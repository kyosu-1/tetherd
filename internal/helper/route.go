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
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// Router adds and removes host routes through route(8).
type Router struct {
	// Run executes route(8). nil means the real one.
	Run func(name string, args ...string) ([]byte, error)

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
var allowedHosts = map[string]bool{"169.254.170.2": true}

func (r *Router) run(args ...string) ([]byte, error) {
	run := r.Run
	if run == nil {
		run = func(name string, a ...string) ([]byte, error) {
			return exec.Command(name, a...).CombinedOutput()
		}
	}
	return run("route", args...)
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
	for _, h := range hosts {
		if !allowedHosts[h.String()] {
			return fmt.Errorf("route.set: %s is not a host tetherd pins (only 169.254.170.2)", h)
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
			// The likeliest failure is the reject route this whole file is
			// about: it occupies the same destination, so route(8) refuses
			// the add with "File exists". Take it out of the way and try
			// once more. Deleting it costs nothing - the kernel recreates
			// that entry on demand, and it is exactly the entry that makes
			// the address unreachable.
			if _, derr := r.run("-n", "delete", "-host", h.String()); derr == nil {
				out, err = r.run("-n", "add", "-host", h.String(), "-interface", "lo0")
			}
		}
		if err != nil {
			return fmt.Errorf("route.set %s: %w: %s", h, err, strings.TrimSpace(string(out)))
		}
		r.set = append(r.set, h)
	}
	return nil
}

// Clear removes the routes this Router added, and nothing else. Safe to call
// more than once: the helper's disconnect cleanup calls it unconditionally.
func (r *Router) Clear() error {
	var firstErr error
	for _, h := range r.set {
		if out, err := r.run("-n", "delete", "-host", h.String()); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("route.clear %s: %w: %s", h, err, strings.TrimSpace(string(out)))
		}
	}
	r.set = nil
	return firstErr
}

// Active reports what is currently pinned.
func (r *Router) Active() []netip.Addr { return append([]netip.Addr(nil), r.set...) }
