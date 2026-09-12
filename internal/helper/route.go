// What this file does, in full:
//   - it pins one address (169.254.170.2) to lo0 for the life of a session;
//   - it displaces only a host route for that address that is not on lo0;
//   - it removes only the routes it recorded pinning.
//
// Why the pin: connect()'s route lookup runs before pf's output rules, so pf
// cannot rescue a destination the kernel already considers unreachable.
// macOS ARPs for 169.254.170.2 on the LAN, gets nothing back (the address
// only exists inside the ECS task), and leaves a rejecting host route behind
// on the LAN interface - `169.254.170.2 link#15 UHLSW en0 !` in netstat -rn;
// `route -n get` shows that entry on `interface: en0` with flags
// `<UP,HOST,DONE,LLINFO,STATIC,b016,WASCLONED>` and a negative expire, and
// never names the reject bit at all. While it is live connect() returns
// EHOSTUNREACH in about a millisecond and the rdr rule never sees a packet,
// which is why the same child at the same gid reaches a VPC address fine and
// the credential endpoint intermittently not at all. Pinning the address to
// lo0 makes the lookup succeed, and `rdr pass on lo0` then does the work.
//
// Why so little inference: deciding to delete a root-owned route by reading
// a tool's output has already produced two wrong rules here. The interface
// is the one field that has been measured on both sides - en0 for the entry
// this displaces, lo0 for a deliberate route (the ECS credential emulator's
// `ifconfig lo0 alias`, or tetherd's own pin) - so it is the only thing
// classification rests on, and everything else refuses.
//
// No build tag: only darwin runs this (DarwinPlatform is the only caller),
// but the code is portable, which keeps it inside GOOS=linux go vet.
package helper

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
)

// DefaultPinFile records the addresses currently pinned, so a helper that
// was killed rather than stopped can remove its own pins at the next start.
// It lives next to the socket and is written 0600 by the root daemon.
const DefaultPinFile = "/var/run/tetherd-pins"

// Router adds and removes host routes through route(8).
type Router struct {
	// Run executes route(8). nil means the real one.
	Run func(name string, args ...string) ([]byte, error)
	// Logf is the helper's log. Every route(8) delete goes through it: this
	// code can remove an entry it did not create, and a developer whose
	// routing table changed has to be able to find out why.
	Logf func(string, ...any)
	// PinFile is where the pinned addresses are recorded ("" disables the
	// record, which is what the unit tests use).
	PinFile string

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
			// The record is not proof: Clear deliberately keeps a host
			// whose delete failed, and something else may have removed the
			// route in between. Returning success for an address with no
			// route to lo0 would hand the child back the EHOSTUNREACH this
			// file exists to remove - silently, for the helper's lifetime.
			if r.reachesLoopback(h) {
				continue
			}
			r.logf("route: %s is recorded as pinned but has no route to lo0; pinning it again", h)
		}
		if err := r.pin(h); err != nil {
			return err
		}
		if !r.pinned(h) {
			r.set = append(r.set, h)
			r.savePins()
		}
	}
	return nil
}

// pin adds the host route, displacing an unusable one if that is what stands
// in the way.
func (r *Router) pin(h netip.Addr) error {
	// -n keeps route(8) from resolving names, which would make this depend
	// on DNS while DNS is being rearranged.
	out, err := r.run("-n", "add", "-host", h.String(), "-interface", "lo0")
	if err == nil {
		return nil
	}
	if !destinationExists(out) {
		return fmt.Errorf("route.set %s: %w: %s", h, err, strings.TrimSpace(string(out)))
	}
	// Something already occupies this destination. It is most likely the
	// stale entry this file is about, but it can also be a route a
	// developer's own tooling installed - and this runs as root, so it must
	// find out which before deleting anything.
	if err := r.displaceConflict(h); err != nil {
		return err
	}
	if out, err := r.run("-n", "add", "-host", h.String(), "-interface", "lo0"); err != nil {
		return fmt.Errorf("route.set %s: %w: %s", h, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// reachesLoopback reports whether a host route for h to lo0 is in the table
// right now. That is what the pin is for, so it - not the record - is the
// question Set has to ask.
func (r *Router) reachesLoopback(h netip.Addr) bool {
	out, err := r.run("-n", "get", h.String())
	if err != nil {
		return false
	}
	return classifyRouteGet(string(out), h) == routeLoopback
}

// displaceConflict removes the route already occupying h, but only when
// route(8) positively describes a host route for h on an interface that is
// not lo0 - the measured shape of the entry a failed ARP leaves behind.
// Every other answer refuses, including one this parser cannot read: a route
// on lo0 is somebody's deliberate route (the ECS credential emulator serves
// this very address by aliasing it onto lo0, docs/design.md), and an answer
// we do not understand is not grounds for a root daemon to delete anything.
func (r *Router) displaceConflict(h netip.Addr) error {
	got, err := r.run("-n", "get", h.String())
	if err != nil {
		// Measured on macOS 25.6: `route -n get` exits 0 even for an
		// address with no host route of its own - it answers with the
		// network route that covers it. So a failure here is not "there is
		// nothing there": it means there is no route to the block at all,
		// and on such a machine nothing occupies the destination either,
		// so the add above would have succeeded and this code would not be
		// running. That leaves a state tetherd cannot explain, which is
		// exactly when a root daemon must not delete. Do not turn this
		// into a silent delete.
		return fmt.Errorf("route.set %s: a route for it already exists and `route -n get %s` could not describe it (%w: %s); remove it yourself (sudo route -n delete -host %s) and run tetherd again", h, h, err, summarize(got), h)
	}
	switch classifyRouteGet(string(got), h) {
	case routeLoopback:
		return fmt.Errorf("route.set %s: %s is already routed to lo0 by something else and tetherd will not replace it (amazon-ecs-local-container-endpoints does this with an lo0 alias; it can also be a pin a previous helper could not remove - the helper log says so). Stop that, or run `sudo route -n delete -host %s` yourself, or run without capturing %s: %s", h, h, h, h, summarize(got))
	case routeNoHost:
		return fmt.Errorf("route.set %s: route(8) refused the add but reports no host route for %s - it answered about %s instead; tetherd will not delete a route it cannot identify: %s", h, h, routeGetDestination(string(got)), summarize(got))
	case routeUnreadable:
		return fmt.Errorf("route.set %s: route(8) refused the add and `route -n get %s` did not describe a host route this helper can read, so nothing was deleted; remove the route yourself (sudo route -n delete -host %s) and run tetherd again: %s", h, h, h, summarize(got))
	}
	r.logf("route: %s already had an unusable route on a non-loopback interface, deleting it to pin the address to lo0: %s", h, summarize(got))
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

// routeGetState is what `route -n get <h>` says about h.
type routeGetState int

const (
	// routeUnreadable is the answer to anything this parser cannot
	// positively identify: no `interface:` field, no `destination:` field,
	// output in a shape that has not been measured. displaceConflict
	// refuses it, which is what makes a misread fail closed instead of
	// deleting a route as root.
	//
	// It is declared first so the zero value of the type is that refusal
	// too. Nothing today produces a routeGetState without going through
	// classifyRouteGet, so that ordering is insurance for a future caller
	// rather than a live rule - renumbering these constants changes no
	// behaviour, and a mutation that does so survives the suite.
	routeUnreadable routeGetState = iota
	// routeNoHost: the answer describes a different destination, i.e. the
	// network route that covers h, so h has no host route of its own.
	// `route -n get 169.254.99.99` answers `destination: 169.254.0.0,
	// mask: 255.255.0.0` and exits 0.
	routeNoHost
	// routeLoopback: a host route for h on lo0. Deliberate, and not ours to
	// delete - the ECS credential emulator installs one with `ifconfig lo0
	// alias`, and tetherd's own pin looks the same.
	routeLoopback
	// routeDisplaceable: a host route for h on a named interface that is
	// not lo0. Measured on the machine this feature exists for:
	// `interface: en0`, flags `<UP,HOST,DONE,LLINFO,STATIC,b016,WASCLONED>`,
	// `expire -9813`. Note what route(8) does *not* print: REJECT. netstat
	// renders this entry with a trailing "!" and one of its flag bits has
	// no name at all (b016), so nothing here keys on a flag name.
	routeDisplaceable
)

// classifyRouteGet reads the two fields that decide this: `destination:`,
// which says whether the answer is even about h, and `interface:`, which
// says whether the route is somebody's deliberate lo0 route or the stale
// entry on a LAN interface. An answer missing either is unreadable.
func classifyRouteGet(out string, h netip.Addr) routeGetState {
	var iface string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "interface:" {
			iface = f[1]
		}
	}
	dest := routeGetDestination(out)
	switch {
	case dest == "":
		return routeUnreadable
	case dest != h.String():
		return routeNoHost
	case iface == "":
		return routeUnreadable
	case iface == "lo0":
		return routeLoopback
	}
	return routeDisplaceable
}

// routeGetDestination is the `destination:` field, or "" if there is none.
func routeGetDestination(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "destination:" {
			return f[1]
		}
	}
	return ""
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
	r.savePins()
	return firstErr
}

// ClearRecorded removes the pins listed in PinFile and then empties it. It is
// for helper startup and nothing else: a helper killed with SIGKILL leaves
// 169.254.170.2 pointing at lo0 with nothing listening, every AWS SDK on the
// machine then hangs on the credential endpoint, and the file is the only
// record of that pin - Router.set died with the process.
//
// It deletes only what the file lists. Deleting the address unconditionally
// would take out a running emulator's lo0 alias before any session had
// started, and would log it as a previous helper's leftover, which would be
// false. A malformed or unreadable record is logged and skipped: the daemon
// must still start.
func (r *Router) ClearRecorded() {
	if r.PinFile == "" {
		return
	}
	b, err := os.ReadFile(r.PinFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.logf("route: could not read the pin record %s (%v); leaving any recorded pins in place", r.PinFile, err)
		}
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		a, perr := netip.ParseAddr(line)
		if perr != nil || !hostAllowed(a) {
			r.logf("route: ignoring %q in the pin record %s: not an address tetherd pins", line, r.PinFile)
			continue
		}
		if out, err := r.run("-n", "delete", "-host", a.String()); err != nil {
			r.logf("route: a leftover pin for %s could not be removed (%v: %s); remove it yourself with `sudo route -n delete -host %s`", a, err, summarize(out), a)
			continue
		}
		r.logf("route: removed a leftover pin for %s recorded in %s (a previous helper did not shut down cleanly)", a, r.PinFile)
	}
	r.set = nil
	r.savePins()
}

// savePins writes the current pins, so a helper that is killed leaves a
// record of what it installed. Failing to write is logged and tolerated -
// the pin itself is what the session needs.
func (r *Router) savePins() {
	if r.PinFile == "" {
		return
	}
	var b strings.Builder
	for _, h := range r.set {
		b.WriteString(h.String())
		b.WriteByte('\n')
	}
	if err := os.WriteFile(r.PinFile, []byte(b.String()), 0o600); err != nil {
		r.logf("route: could not record the pinned routes in %s (%v); a helper that is killed will not know to remove them", r.PinFile, err)
	}
}

// Active reports what is currently pinned.
func (r *Router) Active() []netip.Addr { return append([]netip.Addr(nil), r.set...) }
