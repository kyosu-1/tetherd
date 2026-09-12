//go:build darwin

package helper

import (
	"net/netip"
	"strings"
	"testing"
)

// recorder taps route(8) and pfctl into one ordered log, so a test can pin
// which of them ran first. Neither real binary is ever executed: pfctl needs
// root and would touch this machine's pf, route(8) its routing table.
type recorder struct{ calls []string }

func (r *recorder) route(name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return nil, nil
}

func (r *recorder) pfctl(args []string, _ string) (string, error) {
	r.calls = append(r.calls, "pfctl "+strings.Join(args, " "))
	return "", nil
}

func (r *recorder) indexOf(prefix string) int {
	for i, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// TestDarwinPlatformPinsThroughRoute8 pins the wiring: the Platform methods
// the server calls have to reach route(8), not just exist. A DarwinPlatform
// whose RouteSet did nothing would pass every socket-level test in this
// package (they use fakePlatform) and fail on the only machine that matters.
func TestDarwinPlatformPinsThroughRoute8(t *testing.T) {
	f := &fakeRun{}
	p := NewDarwinPlatform((&recorder{}).pfctl, t.TempDir(), nil)
	p.route.Run = f.run

	if err := p.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	if got := f.joined(); len(got) != 1 || got[0] != addCmd {
		t.Fatalf("calls = %v, want %q", got, addCmd)
	}

	f.calls = nil
	if err := p.RouteClear(); err != nil {
		t.Fatal(err)
	}
	if got := f.joined(); len(got) != 1 || got[0] != delCmd {
		t.Fatalf("calls = %v, want %q", got, delCmd)
	}
}

// TestDarwinShutdownUnpinsTheRouteBeforeTouchingPf: Shutdown runs on every
// helper exit, and a host route pointing 169.254.170.2 at lo0 survives the
// process that made it. It must also come down *before* the rdr rule it
// depends on: in between, that address is pinned to lo0 with no rule to
// catch it, and a packet to a non-local address on lo0 is dropped.
func TestDarwinShutdownUnpinsTheRouteBeforeTouchingPf(t *testing.T) {
	rec := &recorder{}
	p := NewDarwinPlatform(rec.pfctl, t.TempDir(), nil)
	p.route.Run = rec.route
	if err := p.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil
	if err := p.Shutdown(); err != nil {
		t.Fatal(err)
	}
	del, pfctl := rec.indexOf(delCmd), rec.indexOf("pfctl")
	if del < 0 {
		t.Fatalf("shutdown ran %v, want %q", rec.calls, delCmd)
	}
	if pfctl < 0 {
		t.Fatalf("shutdown ran %v, want a pfctl call too", rec.calls)
	}
	if del > pfctl {
		t.Fatalf("the route came down after pf: %v", rec.calls)
	}
	// And a Shutdown with nothing pinned - the one on a helper that never
	// served a session - must not touch the routing table at all.
	rec.calls = nil
	if err := p.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if rec.indexOf("route ") >= 0 {
		t.Fatalf("a shutdown with nothing pinned ran %v", rec.calls)
	}
}

// TestDarwinClearLeftoversRemovesAPinThisProcessNeverMade is the
// crashed-helper case. Router.set lives in memory, so a helper killed with
// SIGKILL leaves 169.254.170.2 pointing at lo0 with no record of it, and
// from then on every AWS SDK on the machine hangs on the credential
// endpoint. Startup is the only place that can notice.
func TestDarwinClearLeftoversRemovesAPinThisProcessNeverMade(t *testing.T) {
	rec := &recorder{}
	p := NewDarwinPlatform(rec.pfctl, t.TempDir(), nil)
	p.route.Run = rec.route // nothing pinned: this is a fresh process
	if err := p.ClearLeftovers(); err != nil {
		t.Fatal(err)
	}
	if rec.indexOf(delCmd) < 0 {
		t.Fatalf("startup cleanup ran %v, want %q for the allowlisted host", rec.calls, delCmd)
	}
	// Ordinary Shutdown must not do this: it only removes what this process
	// pinned, so it cannot delete a route someone else installed.
	rec.calls = nil
	if err := p.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if rec.indexOf(delCmd) >= 0 {
		t.Fatalf("Shutdown deleted a route it never pinned: %v", rec.calls)
	}
}
