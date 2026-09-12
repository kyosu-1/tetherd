// Why this file exists: connect()'s route lookup runs before pf's output
// rules, so pf cannot rescue a destination the kernel already considers
// unreachable. macOS ARPs for 169.254.170.2 on the LAN, gets nothing back
// (the address only exists inside the ECS task), and leaves a reject host
// route behind on the LAN interface - `169.254.170.2 link#15 UHLSW en0 !` in
// netstat -rn, LLINFO with a negative expire in `route -n get`. While that
// entry is live connect() returns EHOSTUNREACH in about a millisecond and
// the rdr rule never sees a packet, which is why the same child at the same
// gid reaches a VPC address fine and the credential endpoint intermittently
// not at all. Pinning the address to lo0 for the session makes the lookup
// succeed, and `rdr pass on lo0` then does the work.
//
// No build tag: only darwin runs this (DarwinPlatform is the only caller),
// but the code is portable, which keeps it inside GOOS=linux go vet.
package helper

import (
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// Router adds and removes host routes through route(8).
type Router struct {
	// Run executes route(8). nil means the real one.
	Run func(name string, args ...string) ([]byte, error)
	// Logf is the helper's log. Every route(8) delete goes through it: this
	// code removes entries it did not create, and a developer whose
	// routing table changed has to be able to find out why.
	Logf func(string, ...any)

	set []netip.Addr // what this Router pinned; Clear removes exactly these
}

// allowedHosts is the complete set of addresses Set will pin. 169.254.170.2
// is the ECS task-role credential and task-metadata endpoint (spec §4.1).
// Pinning an address to lo0 sends every process on the machine to a local
// listener for it, and this is an operation a root daemon accepts from any
// admin user, so the list is one address rather than "whatever was asked
// for". It is deliberately spelled out here instead of imported from
// provider/ecs: what a privileged daemon will do must not be derived from
// the client's idea of it.
var allowedHosts = []netip.Addr{netip.MustParseAddr("169.254.170.2")}

// maxHosts bounds one route.set. The allow-list is one address, so a caller
// sending thousands only costs CPU in the pinned-already scan.
const maxHosts = 8

func hostAllowed(h netip.Addr) bool {
	for _, a := range allowedHosts {
		if a == h {
			return true
		}
	}
	return false
}

func (r *Router) run(args ...string) ([]byte, error) {
	run := r.Run
	if run == nil {
		run = func(name string, a ...string) ([]byte, error) {
			return exec.Command(name, a...).CombinedOutput()
		}
	}
	return run("route", args...)
}

func (r *Router) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Router) pinned(h netip.Addr) bool {
	for _, a := range r.set {
		if a == h {
			return true
		}
	}
	return false
}

// Set pins each host to lo0. Every host is checked before the first one is
// added, so a call carrying an address the helper will not pin changes
// nothing; and since allowedHosts holds exactly one address, at most one
// route is ever added, which is why a failure here can leave nothing behind
// to roll back. (If that list ever grows, Set must undo the adds it already
// made before returning: a host route left pointing at lo0 after the session
// ends keeps swallowing that address with nothing listening.)
func (r *Router) Set(hosts []netip.Addr) error {
	if len(hosts) == 0 {
		return errors.New("route.set: no hosts given")
	}
	if len(hosts) > maxHosts {
		return fmt.Errorf("route.set: %d hosts is more than tetherd pins (max %d)", len(hosts), maxHosts)
	}
	for _, h := range hosts {
		if !hostAllowed(h) {
			return fmt.Errorf("route.set: %s is not a host tetherd pins (only %s)", h, allowedHosts[0])
		}
	}
	for _, h := range hosts {
		if r.pinned(h) {
			continue // already ours; route(8) would answer "File exists"
		}
		// -n keeps route(8) from resolving names, which would make this
		// depend on DNS while DNS is being rearranged.
		out, err := r.run("-n", "add", "-host", h.String(), "-interface", "lo0")
		if err != nil {
			if !destinationExists(out) {
				return fmt.Errorf("route.set %s: %w: %s", h, err, strings.TrimSpace(string(out)))
			}
			// Something already occupies this destination. It is most
			// likely the reject entry this file is about, but it can also
			// be a route a developer's own tooling installed - and this
			// runs as root, so it must find out which before deleting
			// anything.
			if err := r.displaceConflict(h); err != nil {
				return err
			}
			out, err = r.run("-n", "add", "-host", h.String(), "-interface", "lo0")
		}
		if err != nil {
			return fmt.Errorf("route.set %s: %w: %s", h, err, strings.TrimSpace(string(out)))
		}
		r.set = append(r.set, h)
	}
	return nil
}

// displaceConflict removes the route already occupying h, but only when it
// is the stale entry that makes h unreachable. A live lo0 route is somebody
// else's: amazon-ecs-local-container-endpoints (docs/design.md) serves this
// very address on macOS by aliasing it onto lo0, and deleting that would
// break the developer's tool for the rest of the boot with tetherd's pin
// silently standing in its place - and then take it away again on Clear.
func (r *Router) displaceConflict(h netip.Addr) error {
	got, err := r.run("-n", "get", h.String())
	if err != nil {
		return fmt.Errorf("route.set %s: a route for it already exists and `route -n get %s` could not describe it (%w: %s); remove it yourself (sudo route -n delete -host %s) and run tetherd again", h, h, err, summarize(got), h)
	}
	if isLiveLoopbackRoute(string(got)) {
		return fmt.Errorf("route.set %s: something else already routes %s to lo0 and tetherd will not replace it (amazon-ecs-local-container-endpoints does this with an lo0 alias; stop it, or run without capturing %s): %s", h, h, h, summarize(got))
	}
	r.logf("route: %s already had an unusable route, deleting it to pin the address to lo0: %s", h, summarize(got))
	if out, err := r.run("-n", "delete", "-host", h.String()); err != nil {
		return fmt.Errorf("route.set %s: could not remove the existing route: %w: %s", h, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// destinationExists reports whether route(8) refused an add because the
// destination is already in the table (EEXIST, "File exists"). Any other
// failure - no permission, a bad interface - must not lead to a delete.
func destinationExists(out []byte) bool {
	return strings.Contains(strings.ToLower(string(out)), "file exists")
}

// isLiveLoopbackRoute reports whether `route get` describes a working route
// to lo0, as opposed to the reject/incomplete entry a failed ARP leaves
// behind. A rejecting route is never working, whatever interface it names.
func isLiveLoopbackRoute(out string) bool {
	if strings.Contains(strings.ToUpper(out), "REJECT") {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && (f[0] == "interface:" || f[0] == "gateway:") && f[1] == "lo0" {
			return true
		}
	}
	return false
}

// summarize flattens route(8)'s multi-line output into something that fits
// in one log line or error message.
func summarize(out []byte) string {
	s := strings.Join(strings.Fields(string(out)), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// Clear removes the routes this Router added, and nothing else. Safe to call
// more than once: the helper's disconnect cleanup calls it unconditionally.
// A host whose delete failed stays in the set, so the next Clear - the
// disconnect cleanup, which is the retry - tries it again instead of leaving
// the address pinned to lo0 while this Router believes nothing is pinned.
func (r *Router) Clear() error {
	var firstErr error
	var left []netip.Addr
	for _, h := range r.set {
		if out, err := r.run("-n", "delete", "-host", h.String()); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("route.clear %s: %w: %s", h, err, strings.TrimSpace(string(out)))
			}
			left = append(left, h)
			continue
		}
	}
	r.set = left
	return firstErr
}

// ClearAll deletes the route for every address tetherd is allowed to pin,
// whether this process pinned it or not, and is for helper startup only. A
// helper killed with SIGKILL leaves 169.254.170.2 pointing at lo0 with
// nothing listening, and from then on every AWS SDK on the machine hangs on
// the credential endpoint instead of failing fast - with nothing in
// Router.set to tell the next helper what to remove. A route that is not
// there is the normal outcome, so a failed delete is not an error here.
func (r *Router) ClearAll() {
	for _, h := range allowedHosts {
		if _, err := r.run("-n", "delete", "-host", h.String()); err == nil {
			r.logf("route: removed a leftover route for %s (a previous helper did not shut down cleanly)", h)
		}
	}
	r.set = nil
}

// Active reports what is currently pinned.
func (r *Router) Active() []netip.Addr { return append([]netip.Addr(nil), r.set...) }
