//go:build darwin

package helper

import (
	"net/netip"
	"testing"
)

// noPfctl stands in for pfctl so Shutdown's anchor flush does not shell out
// to the real one (which needs root and would touch this machine's pf).
func noPfctl(args []string, stdin string) (string, error) { return "", nil }

// TestDarwinPlatformPinsThroughRoute8 pins the wiring: the Platform methods
// the server calls have to reach route(8), not just exist. A DarwinPlatform
// whose RouteSet did nothing would pass every socket-level test in this
// package (they use fakePlatform) and fail on the only machine that matters.
func TestDarwinPlatformPinsThroughRoute8(t *testing.T) {
	f := &fakeRun{}
	p := NewDarwinPlatform(noPfctl, t.TempDir(), nil)
	p.route.Run = f.run

	if err := p.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	want := "route -n add -host 169.254.170.2 -interface lo0"
	if got := f.joined(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %v, want %q", got, want)
	}

	f.calls = nil
	if err := p.RouteClear(); err != nil {
		t.Fatal(err)
	}
	want = "route -n delete -host 169.254.170.2"
	if got := f.joined(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %v, want %q", got, want)
	}
}

// TestDarwinShutdownUnpinsTheRoute: Shutdown runs when the helper exits, and
// a host route pointing 169.254.170.2 at lo0 survives the process that made
// it. Left behind, it swallows the credential endpoint for every process on
// the machine with nothing listening on the other side.
func TestDarwinShutdownUnpinsTheRoute(t *testing.T) {
	f := &fakeRun{}
	p := NewDarwinPlatform(noPfctl, t.TempDir(), nil)
	p.route.Run = f.run
	if err := p.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := p.Shutdown(); err != nil {
		t.Fatal(err)
	}
	want := "route -n delete -host 169.254.170.2"
	if got := f.joined(); len(got) != 1 || got[0] != want {
		t.Fatalf("shutdown ran %v, want %q", got, want)
	}
	// And a Shutdown with nothing pinned - the one at startup, which runs
	// on every helper launch - must not touch the routing table at all.
	f.calls = nil
	if err := p.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if got := f.joined(); len(got) != 0 {
		t.Fatalf("a shutdown with nothing pinned ran %v", got)
	}
}
