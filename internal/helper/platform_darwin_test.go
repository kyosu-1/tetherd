//go:build darwin

package helper

import (
	"net/netip"
	"os"
	"path/filepath"
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
	p.route.PinFile = "" // the record is exercised in route_test.go

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
	p.route.PinFile = ""
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

// TestDarwinClearLeftoversRemovesTheRecordedPin is the crashed-helper case.
// Router.set lives in memory, so a helper killed with SIGKILL leaves
// 169.254.170.2 pointing at lo0 and every AWS SDK on the machine then hangs
// on the credential endpoint. The pin record is what outlives the process,
// and startup is the only place that can act on it.
func TestDarwinClearLeftoversRemovesTheRecordedPin(t *testing.T) {
	rec := &recorder{}
	pins := filepath.Join(t.TempDir(), "pins")
	if err := os.WriteFile(pins, []byte("169.254.170.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewDarwinPlatform(rec.pfctl, t.TempDir(), nil)
	p.route.Run = rec.route // nothing pinned in memory: this is a fresh process
	p.route.PinFile = pins
	if err := p.ClearLeftovers(); err != nil {
		t.Fatal(err)
	}
	if rec.indexOf(delCmd) < 0 {
		t.Fatalf("startup cleanup ran %v, want %q for the recorded pin", rec.calls, delCmd)
	}
	// Ordinary Shutdown must not do this: it removes only what this process
	// pinned, so it cannot delete a route someone else installed.
	rec.calls = nil
	if err := p.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if rec.indexOf(delCmd) >= 0 {
		t.Fatalf("Shutdown deleted a route it never pinned: %v", rec.calls)
	}
}

// TestDarwinPlatformConfiguresThePinRecord: the tests above set PinFile
// themselves, so without this the production wiring - the only place that
// decides a real helper keeps a record at all - would be unasserted. A
// helper with no record cannot clean up after being killed.
func TestDarwinPlatformConfiguresThePinRecord(t *testing.T) {
	p := NewDarwinPlatform((&recorder{}).pfctl, t.TempDir(), nil)
	if p.route.PinFile != DefaultPinFile {
		t.Fatalf("PinFile = %q, want %q", p.route.PinFile, DefaultPinFile)
	}
	if !filepath.IsAbs(DefaultPinFile) || filepath.Dir(DefaultPinFile) != filepath.Dir(DefaultSocket) {
		t.Errorf("%q should live beside the socket %q", DefaultPinFile, DefaultSocket)
	}
}

// TestDarwinClearLeftoversTouchesNoRouteWithoutARecord: the common startup.
// Deleting 169.254.170.2 unconditionally here would take out a running
// emulator's lo0 alias before any session began - and log it as a previous
// helper's leftover, which would be false.
func TestDarwinClearLeftoversTouchesNoRouteWithoutARecord(t *testing.T) {
	rec := &recorder{}
	p := NewDarwinPlatform(rec.pfctl, t.TempDir(), nil)
	p.route.Run = rec.route
	p.route.PinFile = filepath.Join(t.TempDir(), "absent")
	if err := p.ClearLeftovers(); err != nil {
		t.Fatal(err)
	}
	if rec.indexOf("route ") >= 0 {
		t.Fatalf("startup cleanup with no record ran %v, want no route(8) call", rec.calls)
	}
	if rec.indexOf("pfctl") < 0 {
		t.Fatalf("it must still flush pf: %v", rec.calls)
	}
}
