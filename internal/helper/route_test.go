package helper

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

type fakeRun struct {
	calls [][]string
	// err fails a command every time it is run; once fails it only the
	// first time, which is how a stale route that route(8) refuses to
	// overwrite behaves once it has been deleted.
	err  map[string]error
	once map[string]error
}

func (f *fakeRun) run(name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	key := strings.Join(call, " ")
	if e, ok := f.once[key]; ok {
		delete(f.once, key)
		return []byte("boom"), e
	}
	if e, ok := f.err[key]; ok {
		return []byte("boom"), e
	}
	return nil, nil
}

func (f *fakeRun) joined() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func TestRouterSetAddsAHostRouteToLoopback(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run}
	if err := r.Set([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	want := "route -n add -host 169.254.170.2 -interface lo0"
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != want {
		t.Fatalf("calls = %v, want %q", f.joined(), want)
	}
	if got := r.Active(); len(got) != 1 || got[0].String() != "169.254.170.2" {
		t.Fatalf("active = %v", got)
	}
}

func TestRouterClearRemovesOnlyWhatItAdded(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run}
	if err := r.Set([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := r.Clear(); err != nil {
		t.Fatal(err)
	}
	want := "route -n delete -host 169.254.170.2"
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != want {
		t.Fatalf("calls = %v, want %q", f.joined(), want)
	}
	if len(r.Active()) != 0 {
		t.Fatalf("active after clear = %v", r.Active())
	}
	// A second Clear does nothing: the helper's disconnect cleanup calls it
	// unconditionally, and it must not delete a route it never added.
	f.calls = nil
	if err := r.Clear(); err != nil || len(f.calls) != 0 {
		t.Fatalf("second clear ran %v (err %v)", f.joined(), err)
	}
}

// TestRouterClearOnARouterThatNeverSetAnything is the same guard from the
// other side: a Router that was never used must not run route(8) at all.
func TestRouterClearOnARouterThatNeverSetAnything(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run}
	if err := r.Clear(); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("clear on an unused Router ran %v", f.joined())
	}
}

// TestRouterSetDeletesAStaleRouteAndRetries is the case this whole feature
// exists for. The reject route macOS leaves behind after ARPing for
// 169.254.170.2 on the LAN occupies the same destination, so route(8)
// answers the add with "File exists". Without the retry the pin fails on
// exactly the machines that need it.
func TestRouterSetDeletesAStaleRouteAndRetries(t *testing.T) {
	f := &fakeRun{once: map[string]error{
		"route -n add -host 169.254.170.2 -interface lo0": errors.New("exit 1"),
	}}
	r := &Router{Run: f.run}
	if err := r.Set([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatalf("a stale route must be cleared out of the way, not reported: %v", err)
	}
	want := []string{
		"route -n add -host 169.254.170.2 -interface lo0",
		"route -n delete -host 169.254.170.2",
		"route -n add -host 169.254.170.2 -interface lo0",
	}
	if strings.Join(f.joined(), " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls = %v, want %v", f.joined(), want)
	}
	if got := r.Active(); len(got) != 1 || got[0].String() != "169.254.170.2" {
		t.Fatalf("active = %v, want the address pinned after the retry", got)
	}
}

func TestRouterSetLeavesNothingPinnedWhenTheAddKeepsFailing(t *testing.T) {
	// A host recorded as pinned but not actually in the routing table would
	// be deleted on Clear (harmless) while the session believed it had a
	// route it never got - so a failed Set must report and record nothing.
	f := &fakeRun{err: map[string]error{
		"route -n add -host 169.254.170.2 -interface lo0": errors.New("exit 1"),
	}}
	r := &Router{Run: f.run}
	err := r.Set([]netip.Addr{netip.MustParseAddr("169.254.170.2")})
	if err == nil {
		t.Fatal("a failed add must be reported")
	}
	if !strings.Contains(err.Error(), "169.254.170.2") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("the error must name the address and what route said: %v", err)
	}
	if len(r.Active()) != 0 {
		t.Errorf("active = %v, want empty after a failed Set", r.Active())
	}
	f.calls = nil
	if err := r.Clear(); err != nil || len(f.calls) != 0 {
		t.Errorf("Clear after a failed Set ran %v (err %v)", f.joined(), err)
	}
}

// TestRouterSetIsIdempotent: route(8) refuses to add a route that is already
// there, so a repeated Set (or a repeated host in one call) must be a no-op
// rather than an error - and Clear must then delete it once, not twice.
func TestRouterSetIsIdempotent(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run}
	addr := netip.MustParseAddr("169.254.170.2")
	if err := r.Set([]netip.Addr{addr, addr}); err != nil {
		t.Fatal(err)
	}
	if err := r.Set([]netip.Addr{addr}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %v, want a single add", f.joined())
	}
	if got := r.Active(); len(got) != 1 {
		t.Fatalf("active = %v, want one entry", got)
	}
	f.calls = nil
	if err := r.Clear(); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("clear ran %v, want a single delete", f.joined())
	}
}

func TestRouterSetRejectsAnythingButTheCredentialEndpoint(t *testing.T) {
	// Pinning an address to lo0 sends every process on the machine to a
	// local listener for it. This is an operation a root daemon accepts, so
	// the range it can redirect is one address - not "any", and not the
	// rest of the link-local block either.
	f := &fakeRun{}
	r := &Router{Run: f.run}
	for _, bad := range []string{"10.0.0.1", "0.0.0.0", "8.8.8.8", "fd00::1", "169.254.170.3", "169.254.169.254"} {
		if err := r.Set([]netip.Addr{netip.MustParseAddr(bad)}); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused host must not reach route(8): %v", f.joined())
	}
	if len(r.Active()) != 0 {
		t.Errorf("active = %v, want empty", r.Active())
	}
}

// TestRouterSetRefusesTheWholeCallIfAnyHostIsRefused: the check runs over
// every host before the first add, so a refused host cannot ride along
// behind an allowed one.
func TestRouterSetRefusesTheWholeCallIfAnyHostIsRefused(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run}
	err := r.Set([]netip.Addr{
		netip.MustParseAddr("169.254.170.2"),
		netip.MustParseAddr("10.0.0.1"),
	})
	if err == nil {
		t.Fatal("a call carrying a refused host must fail")
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing may be added before the whole call is checked: %v", f.joined())
	}
	if len(r.Active()) != 0 {
		t.Errorf("active = %v, want empty", r.Active())
	}
}
