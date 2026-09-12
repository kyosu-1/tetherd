package helper

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

type fakeRun struct {
	calls [][]string
	// err fails a command every time it is run; once fails it only the
	// first time, which is how a stale route that route(8) refuses to
	// overwrite behaves once it has been deleted. out is what a command
	// prints (route(8) writes its diagnostics to the same stream).
	err  map[string]error
	once map[string]error
	out  map[string]string
	logs []string
}

func (f *fakeRun) run(name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	key := strings.Join(call, " ")
	out := []byte(f.out[key])
	if e, ok := f.once[key]; ok {
		delete(f.once, key)
		return f.failOutput(key, out), e
	}
	if e, ok := f.err[key]; ok {
		return f.failOutput(key, out), e
	}
	return out, nil
}

// failOutput keeps the old "boom" default for commands the test did not
// give explicit output, so an error message is still checked for carrying
// what route(8) said.
func (f *fakeRun) failOutput(key string, out []byte) []byte {
	if _, ok := f.out[key]; ok {
		return out
	}
	return []byte("boom")
}

func (f *fakeRun) logf(format string, args ...any) {
	f.logs = append(f.logs, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (f *fakeRun) joined() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func (f *fakeRun) log() string { return strings.Join(f.logs, "\n") }

const (
	addCmd = "route -n add -host 169.254.170.2 -interface lo0"
	getCmd = "route -n get 169.254.170.2"
	delCmd = "route -n delete -host 169.254.170.2"
)

// fileExists is what route(8) prints when the destination is already in the
// table, which is the only failure that may lead to a delete.
const fileExists = "add host 169.254.170.2: gateway lo0: File exists"

// rejectEntry is `route -n get` on the entry a failed ARP leaves behind:
// the one this whole feature exists to displace.
const rejectEntry = `   route to: 169.254.170.2
destination: 169.254.170.2
  interface: en0
      flags: <UP,HOST,DONE,LLINFO,WASCLONED,IFSCOPE,IFREF>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0     16384       -18`

// aliasEntry is `route -n get` when something else - typically
// amazon-ecs-local-container-endpoints, which aliases 169.254.170.2 onto
// lo0 - already serves the address locally.
const aliasEntry = `   route to: 169.254.170.2
destination: 169.254.170.2
  interface: lo0
      flags: <UP,HOST,DONE,LOCAL,IFSCOPE>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0     16384         0`

func addr2() netip.Addr { return netip.MustParseAddr("169.254.170.2") }

func TestRouterSetAddsAHostRouteToLoopback(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != addCmd {
		t.Fatalf("calls = %v, want %q", f.joined(), addCmd)
	}
	if got := r.Active(); len(got) != 1 || got[0].String() != "169.254.170.2" {
		t.Fatalf("active = %v", got)
	}
}

func TestRouterClearRemovesOnlyWhatItAdded(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := r.Clear(); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != delCmd {
		t.Fatalf("calls = %v, want %q", f.joined(), delCmd)
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

// TestRouterClearKeepsAHostWhoseDeleteFailed is the sibling of the test
// above, which only covers a *successful* clear. Forgetting a host whose
// delete failed leaves the address pinned to lo0 while the Router believes
// nothing is pinned - and the disconnect cleanup, which is the retry, then
// runs no commands at all.
func TestRouterClearKeepsAHostWhoseDeleteFailed(t *testing.T) {
	f := &fakeRun{once: map[string]error{delCmd: errors.New("exit 1")}}
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatal(err)
	}
	if err := r.Clear(); err == nil {
		t.Fatal("a failed delete must be reported")
	}
	if got := r.Active(); len(got) != 1 || got[0] != addr2() {
		t.Fatalf("active = %v, want the host kept for the retry", got)
	}
	f.calls = nil
	if err := r.Clear(); err != nil {
		t.Fatalf("the retry must succeed: %v", err)
	}
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != delCmd {
		t.Fatalf("the retry ran %v, want %q", f.joined(), delCmd)
	}
	if len(r.Active()) != 0 {
		t.Fatalf("active = %v after a successful retry", r.Active())
	}
}

// TestRouterClearOnARouterThatNeverSetAnything is the same guard from the
// other side: a Router that was never used must not run route(8) at all.
func TestRouterClearOnARouterThatNeverSetAnything(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Clear(); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("clear on an unused Router ran %v", f.joined())
	}
}

// TestRouterSetDisplacesTheStaleRouteAndRetries is the case this whole
// feature exists for. The reject route macOS leaves behind after ARPing for
// 169.254.170.2 on the LAN occupies the same destination, so route(8)
// answers the add with "File exists". Without displacing it the pin fails on
// exactly the machines that need it - but the delete has to be reported,
// because it removes an entry tetherd did not create.
func TestRouterSetDisplacesTheStaleRouteAndRetries(t *testing.T) {
	f := &fakeRun{
		once: map[string]error{addCmd: errors.New("exit 1")},
		out:  map[string]string{addCmd: fileExists, getCmd: rejectEntry},
	}
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatalf("a stale route must be displaced, not reported: %v", err)
	}
	want := []string{addCmd, getCmd, delCmd, addCmd}
	if strings.Join(f.joined(), " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls = %v, want %v", f.joined(), want)
	}
	if got := r.Active(); len(got) != 1 || got[0] != addr2() {
		t.Fatalf("active = %v, want the address pinned after the retry", got)
	}
	if !strings.Contains(f.log(), "169.254.170.2") || !strings.Contains(f.log(), "deleting") {
		t.Errorf("the delete must be logged, naming the address; log = %q", f.log())
	}
}

// TestRouterSetRefusesToDisplaceALiveLoopbackRoute:
// amazon-ecs-local-container-endpoints serves this exact address on macOS by
// aliasing it onto lo0 (docs/design.md). Deleting that would break the
// developer's tool for the rest of the boot - silently, and with tetherd's
// own pin standing in its place until Clear takes that away too.
func TestRouterSetRefusesToDisplaceALiveLoopbackRoute(t *testing.T) {
	f := &fakeRun{
		err: map[string]error{addCmd: errors.New("exit 1")},
		out: map[string]string{addCmd: fileExists, getCmd: aliasEntry},
	}
	r := &Router{Run: f.run, Logf: f.logf}
	err := r.Set([]netip.Addr{addr2()})
	if err == nil {
		t.Fatal("a live lo0 route must not be taken over")
	}
	if !strings.Contains(err.Error(), "lo0") || !strings.Contains(err.Error(), "169.254.170.2") {
		t.Errorf("the error must name the conflict: %v", err)
	}
	want := []string{addCmd, getCmd}
	if strings.Join(f.joined(), " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls = %v, want %v - nothing may be deleted", f.joined(), want)
	}
	if len(r.Active()) != 0 {
		t.Errorf("active = %v, want empty", r.Active())
	}
}

// A rejecting route is never a working one, whatever interface it names, so
// an entry on lo0 that rejects is still ours to displace.
func TestRouterSetDisplacesARejectingLoopbackRoute(t *testing.T) {
	rejectOnLo0 := strings.Replace(strings.Replace(rejectEntry, "en0", "lo0", 1),
		"<UP,HOST,DONE,LLINFO", "<UP,HOST,REJECT,DONE,LLINFO", 1)
	f := &fakeRun{
		once: map[string]error{addCmd: errors.New("exit 1")},
		out:  map[string]string{addCmd: fileExists, getCmd: rejectOnLo0},
	}
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatalf("a rejecting lo0 entry must be displaced: %v", err)
	}
	if got := len(f.calls); got != 4 {
		t.Fatalf("calls = %v, want add, get, delete, add", f.joined())
	}
}

// TestRouterSetDeletesNothingForAnUnrelatedFailure: the delete exists for
// "File exists" and nothing else. A permission failure or a bad interface
// must be reported as-is - deleting the destination's route in response
// would destroy something tetherd cannot even put back.
func TestRouterSetDeletesNothingForAnUnrelatedFailure(t *testing.T) {
	f := &fakeRun{
		err: map[string]error{addCmd: errors.New("exit 1")},
		out: map[string]string{addCmd: "route: writing to routing socket: Permission denied"},
	}
	r := &Router{Run: f.run, Logf: f.logf}
	err := r.Set([]netip.Addr{addr2()})
	if err == nil {
		t.Fatal("a failed add must be reported")
	}
	if !strings.Contains(err.Error(), "Permission denied") || !strings.Contains(err.Error(), "169.254.170.2") {
		t.Errorf("the error must name the address and what route said: %v", err)
	}
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != addCmd {
		t.Fatalf("calls = %v, want the add and nothing else", f.joined())
	}
	if len(r.Active()) != 0 {
		t.Errorf("active = %v, want empty after a failed Set", r.Active())
	}
}

// TestRouterSetRefusesWhenTheConflictCannotBeDescribed: if `route get`
// cannot say what occupies the destination, the safe answer is to refuse and
// tell the developer how to clear it, not to delete blind.
func TestRouterSetRefusesWhenTheConflictCannotBeDescribed(t *testing.T) {
	f := &fakeRun{
		err: map[string]error{addCmd: errors.New("exit 1"), getCmd: errors.New("exit 1")},
		out: map[string]string{addCmd: fileExists, getCmd: "route: writing to routing socket: not in table"},
	}
	r := &Router{Run: f.run, Logf: f.logf}
	err := r.Set([]netip.Addr{addr2()})
	if err == nil {
		t.Fatal("an undescribable conflict must be refused")
	}
	if !strings.Contains(err.Error(), "route -n delete -host 169.254.170.2") {
		t.Errorf("the error must tell the developer how to clear it: %v", err)
	}
	want := []string{addCmd, getCmd}
	if strings.Join(f.joined(), " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls = %v, want %v - nothing may be deleted", f.joined(), want)
	}
}

func TestRouterSetLeavesNothingPinnedWhenTheAddKeepsFailing(t *testing.T) {
	// A host recorded as pinned but not actually in the routing table would
	// be deleted on Clear (harmless) while the session believed it had a
	// route it never got - so a failed Set must report and record nothing.
	f := &fakeRun{
		err: map[string]error{addCmd: errors.New("exit 1")},
		out: map[string]string{addCmd: fileExists, getCmd: rejectEntry},
	}
	r := &Router{Run: f.run, Logf: f.logf}
	err := r.Set([]netip.Addr{addr2()})
	if err == nil {
		t.Fatal("a failed add must be reported")
	}
	if !strings.Contains(err.Error(), "169.254.170.2") || !strings.Contains(err.Error(), "File exists") {
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
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Set([]netip.Addr{addr2(), addr2()}); err != nil {
		t.Fatal(err)
	}
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
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
	r := &Router{Run: f.run, Logf: f.logf}
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

// TestOnlyOneHostIsEverPinnable guards the invariant Set's doc comment
// describes: with one allowed address a failed Set cannot leave a partial
// pin behind, so there is no rollback. Widening the list without adding one
// turns that comment into the leak it warns about, so this fails loudly on
// the day it happens.
func TestOnlyOneHostIsEverPinnable(t *testing.T) {
	if len(allowedHosts) != 1 || allowedHosts[0].String() != "169.254.170.2" {
		t.Fatalf("allowedHosts = %v; Set has no rollback because at most one route can be added per call - add one (and a test for it) before growing this list", allowedHosts)
	}
}

// TestRouterSetRefusesTheWholeCallIfAnyHostIsRefused: the check runs over
// every host before the first add, so a refused host cannot ride along
// behind an allowed one.
func TestRouterSetRefusesTheWholeCallIfAnyHostIsRefused(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run, Logf: f.logf}
	err := r.Set([]netip.Addr{addr2(), netip.MustParseAddr("10.0.0.1")})
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

// TestRouterSetRejectsAnEmptyOrOversizedCall keeps the check where the
// allow-list lives, rather than only in the server that happens to call it.
func TestRouterSetRejectsAnEmptyOrOversizedCall(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Set(nil); err == nil {
		t.Error("Set with no hosts must fail")
	}
	many := make([]netip.Addr, 64)
	for i := range many {
		many[i] = addr2()
	}
	if err := r.Set(many); err == nil {
		t.Error("Set with more hosts than tetherd pins must fail")
	}
	if len(f.calls) != 0 {
		t.Errorf("neither may reach route(8): %v", f.joined())
	}
}

// TestRouterClearAllRemovesALeftoverItNeverPinned is the crashed-helper
// case: Router.set lives in memory, so a helper killed with SIGKILL leaves
// 169.254.170.2 pointing at lo0 with nothing listening and no record of it.
// Every AWS SDK on the machine then hangs on the credential endpoint.
func TestRouterClearAllRemovesALeftoverItNeverPinned(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run, Logf: f.logf}
	r.ClearAll()
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != delCmd {
		t.Fatalf("calls = %v, want %q for every allowlisted host", f.joined(), delCmd)
	}
	if !strings.Contains(f.log(), "169.254.170.2") {
		t.Errorf("a leftover that was removed must be logged: %q", f.log())
	}
	// Nothing to delete is the normal outcome on a clean machine, and must
	// not be reported as a problem or logged as a removal.
	f2 := &fakeRun{err: map[string]error{delCmd: errors.New("exit 1")}}
	r2 := &Router{Run: f2.run, Logf: f2.logf}
	r2.ClearAll()
	if f2.log() != "" {
		t.Errorf("nothing was removed, so nothing may be logged: %q", f2.log())
	}
}
