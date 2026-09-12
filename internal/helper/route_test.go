package helper

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
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

// The three fixtures below are `route -n get` output measured on a real Mac
// (macOS 25.6) that had the stale entry this feature exists for. They are
// verbatim, because the classifier reads this text and a fixture written
// from a model of the output would only test the model.
//
// Note what staleEntry does NOT contain: the word REJECT. netstat renders
// this entry with a trailing "!", route get does not, and one of its flag
// bits has no name at all (b016). A classifier keyed on "REJECT" could
// never fire against real output.
const staleEntry = `   route to: 169.254.170.2
destination: 169.254.170.2
  interface: en0
      flags: <UP,HOST,DONE,LLINFO,STATIC,b016,WASCLONED>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      1500     -9813`

// loopbackEntry is a live lo0 host route for a local address, measured for
// 127.0.0.1. (The numeric row is elided as it was in the measurement.)
const loopbackEntry = `   route to: 127.0.0.1
destination: 127.0.0.1
  interface: lo0
      flags: <UP,HOST,DONE,LOCAL>`

// aliasEntry is what `route -n get 169.254.170.2` prints once something
// aliases the address onto lo0 - amazon-ecs-local-container-endpoints does
// exactly this, and tetherd's own pin looks the same to this parser.
//
// Derived: an lo0 alias cannot be created without root, so the shape is
// taken from route(8)'s documented output for `ifconfig lo0 alias` - which
// installs `169.254.170.2 127.0.0.1 UHS lo0`, i.e. a gateway of 127.0.0.1
// and no LOCAL flag, not the local-address shape of loopbackEntry above. The
// difference matters: a classifier that keyed on `gateway: lo0` would call
// this route somebody else's and this one nobody's.
const aliasEntry = `   route to: 169.254.170.2
destination: 169.254.170.2
    gateway: 127.0.0.1
  interface: lo0
      flags: <UP,HOST,DONE,STATIC>`

// unreadableEntry stands for any answer this parser cannot identify - here,
// output that names no interface at all. The point is not this exact text:
// it is that an answer we cannot read must refuse rather than delete.
const unreadableEntry = `   route to: 169.254.170.2
destination: 169.254.170.2
    something route(8) has never printed`

// coveringNetEntry is what route(8) answers - with exit status 0 - for an
// address that has no host route: the network route that covers it.
// Measured for 169.254.99.99.
const coveringNetEntry = `   route to: 169.254.99.99
destination: 169.254.0.0
       mask: 255.255.0.0
  interface: en0
      flags: <UP,DONE,CLONING,STATIC>`

// coveringNetForEndpoint is coveringNetEntry as it would arrive for the
// credential endpoint: same answer, different question.
const coveringNetForEndpoint = `   route to: 169.254.170.2
destination: 169.254.0.0
       mask: 255.255.0.0
  interface: en0
      flags: <UP,DONE,CLONING,STATIC>`

// TestClassifyRouteGetAgainstRealOutput is the parser's contract, pinned to
// the measured text. The three cases are the only ones route(8) produces
// here, and each drives a different decision in Set: displace, refuse
// because somebody else owns the address, refuse because there is no host
// route to displace at all.
func TestClassifyRouteGetAgainstRealOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		h    string
		want routeGetState
	}{
		{"the stale entry a failed ARP left behind", staleEntry, "169.254.170.2", routeDisplaceable},
		{"a live lo0 route", loopbackEntry, "127.0.0.1", routeLoopback},
		{"the emulator's lo0 alias", aliasEntry, "169.254.170.2", routeLoopback},
		{"no host route: the covering net route", coveringNetEntry, "169.254.99.99", routeNoHost},
		{"no host route, asked about the endpoint", coveringNetForEndpoint, "169.254.170.2", routeNoHost},
		// Fail closed: anything not positively identified refuses, so a
		// parser that breaks cannot turn into "delete it as root".
		{"an answer with no interface", unreadableEntry, "169.254.170.2", routeUnreadable},
		{"an answer with no destination", "route to: 169.254.170.2\n  interface: en0", "169.254.170.2", routeUnreadable},
		{"empty output", "", "169.254.170.2", routeUnreadable},
	} {
		if got := classifyRouteGet(tc.out, netip.MustParseAddr(tc.h)); got != tc.want {
			t.Errorf("%s: classified as %v, want %v", tc.name, got, tc.want)
		}
	}
}

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
		out:  map[string]string{addCmd: fileExists, getCmd: staleEntry},
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
	// Refusing is only half of it: this is a developer whose tool tetherd
	// has just declined to break, so the message has to name the address,
	// the likely owner, and the exact command that clears it by hand.
	for _, want := range []string{
		"169.254.170.2", "lo0",
		"amazon-ecs-local-container-endpoints",
		"route -n delete -host 169.254.170.2",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q: %v", want, err)
		}
	}
	want := []string{addCmd, getCmd}
	if strings.Join(f.joined(), " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls = %v, want %v - nothing may be deleted", f.joined(), want)
	}
	if len(r.Active()) != 0 {
		t.Errorf("active = %v, want empty", r.Active())
	}
}

// TestRouterSetRefusesWhenThereIsNoHostRouteToDisplace: route(8) answers a
// host with no route of its own by describing the network route that covers
// it - with exit status 0. Treating that as "the thing in the way" would
// have tetherd delete on the strength of an answer about a different
// destination, so the mismatch is the refusal.
func TestRouterSetRefusesWhenThereIsNoHostRouteToDisplace(t *testing.T) {
	f := &fakeRun{
		err: map[string]error{addCmd: errors.New("exit 1")},
		out: map[string]string{addCmd: fileExists, getCmd: coveringNetForEndpoint},
	}
	r := &Router{Run: f.run, Logf: f.logf}
	err := r.Set([]netip.Addr{addr2()})
	if err == nil {
		t.Fatal("route(8) refusing the add while reporting no host route must be refused, not guessed at")
	}
	if !strings.Contains(err.Error(), "169.254.0.0") {
		t.Errorf("the error must show what route(8) actually described: %v", err)
	}
	want := []string{addCmd, getCmd}
	if strings.Join(f.joined(), " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls = %v, want %v - nothing may be deleted", f.joined(), want)
	}
	if len(r.Active()) != 0 {
		t.Errorf("active = %v, want empty", r.Active())
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
	// The two refusals agree on behaviour, so the message is what tells a
	// developer which one they hit: route(8) failing outright, or answering
	// something this helper could not read.
	for _, want := range []string{
		"route -n delete -host 169.254.170.2",
		"could not describe it",
		"not in table",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q: %v", want, err)
		}
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
		out: map[string]string{addCmd: fileExists, getCmd: staleEntry},
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
// rather than an error - and Clear must then delete it once, not twice. The
// repeat costs one `route get`, which is the confirmation that the pin is
// still in the table.
func TestRouterSetIsIdempotent(t *testing.T) {
	f := &fakeRun{out: map[string]string{getCmd: aliasEntry}}
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Set([]netip.Addr{addr2(), addr2()}); err != nil {
		t.Fatal(err)
	}
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatal(err)
	}
	want := []string{addCmd, getCmd, getCmd}
	if strings.Join(f.joined(), " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls = %v, want one add and a confirmation per repeat", f.joined())
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

// TestRouterSetRepinsWhenTheRecordedRouteIsGone: Clear deliberately keeps a
// host whose delete failed, so the record can name an address that has no
// route. Trusting it would have Set report success having run nothing, pf
// rules would load, and the child would get EHOSTUNREACH in a millisecond -
// the exact symptom this file exists to remove, silently, for the rest of
// the helper's life.
func TestRouterSetRepinsWhenTheRecordedRouteIsGone(t *testing.T) {
	f := &fakeRun{
		once: map[string]error{delCmd: errors.New("exit 1")},
		// route(8) reports no host route: the pin is gone, whatever the
		// record says.
		out: map[string]string{getCmd: coveringNetForEndpoint},
	}
	r := &Router{Run: f.run, Logf: f.logf}
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatal(err)
	}
	if err := r.Clear(); err == nil {
		t.Fatal("the failed delete must be reported")
	}
	if got := r.Active(); len(got) != 1 {
		t.Fatalf("active = %v, want the host kept for the retry", got)
	}
	f.calls = nil
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatal(err)
	}
	want := []string{getCmd, addCmd}
	if strings.Join(f.joined(), " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls = %v, want %v: a recorded pin with no route must be pinned again", f.joined(), want)
	}
	if got := r.Active(); len(got) != 1 {
		t.Fatalf("active = %v, want one entry and no duplicate", got)
	}
}

// TestRouterSetRefusesAnAnswerItCannotRead is the fail-closed direction: the
// parser has been wrong twice about this output, so an answer it cannot
// positively identify must refuse. Deleting on a misread is a root daemon
// destroying a route nobody asked it to touch.
func TestRouterSetRefusesAnAnswerItCannotRead(t *testing.T) {
	f := &fakeRun{
		err: map[string]error{addCmd: errors.New("exit 1")},
		out: map[string]string{addCmd: fileExists, getCmd: unreadableEntry},
	}
	r := &Router{Run: f.run, Logf: f.logf}
	err := r.Set([]netip.Addr{addr2()})
	if err == nil {
		t.Fatal("an answer the helper cannot read must be refused")
	}
	if !strings.Contains(err.Error(), "route -n delete -host 169.254.170.2") {
		t.Errorf("the error must tell the developer how to clear it: %v", err)
	}
	want := []string{addCmd, getCmd}
	if strings.Join(f.joined(), " | ") != strings.Join(want, " | ") {
		t.Fatalf("calls = %v, want %v - nothing may be deleted", f.joined(), want)
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

// TestRouterRecordsAndForgetsPins: the record is what a killed helper leaves
// behind for the next one. Router.set dies with the process, so without the
// file nothing can tell the difference between a pin tetherd installed and
// somebody else's route - which is how the previous round ended up deleting
// the emulator's route at every startup.
func TestRouterRecordsAndForgetsPins(t *testing.T) {
	f := &fakeRun{}
	pins := filepath.Join(t.TempDir(), "pins")
	r := &Router{Run: f.run, Logf: f.logf, PinFile: pins}
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatal(err)
	}
	if got := readPins(t, pins); got != "169.254.170.2\n" {
		t.Fatalf("pin record = %q, want the pinned address", got)
	}
	if err := r.Clear(); err != nil {
		t.Fatal(err)
	}
	if got := readPins(t, pins); got != "" {
		t.Fatalf("pin record after clear = %q, want empty", got)
	}
}

// TestRouterClearRecordedRemovesOnlyWhatTheFileLists is the crashed-helper
// case, and the whole reason the record exists: the pin is removed because
// tetherd wrote down that it made it, not because the address is on a list.
func TestRouterClearRecordedRemovesOnlyWhatTheFileLists(t *testing.T) {
	f := &fakeRun{}
	pins := filepath.Join(t.TempDir(), "pins")
	if err := os.WriteFile(pins, []byte("169.254.170.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Router{Run: f.run, Logf: f.logf, PinFile: pins}
	r.ClearRecorded()
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != delCmd {
		t.Fatalf("calls = %v, want %q", f.joined(), delCmd)
	}
	if !strings.Contains(f.log(), "169.254.170.2") {
		t.Errorf("a leftover that was removed must be logged: %q", f.log())
	}
	if got := readPins(t, pins); got != "" {
		t.Fatalf("the record must be emptied, got %q", got)
	}
}

// TestRouterClearRecordedWithNoRecordDeletesNothing: on a machine where the
// helper shut down cleanly - or has never run - startup must not touch the
// routing table at all. The emulator's lo0 alias survives a helper restart.
func TestRouterClearRecordedWithNoRecordDeletesNothing(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run, Logf: f.logf, PinFile: filepath.Join(t.TempDir(), "absent")}
	r.ClearRecorded()
	if len(f.calls) != 0 {
		t.Fatalf("startup with no record ran %v, want nothing", f.joined())
	}
	if f.log() != "" {
		t.Errorf("a missing record is the normal case and needs no log: %q", f.log())
	}
	// An empty record - what a clean shutdown leaves - is the same.
	pins := filepath.Join(t.TempDir(), "pins")
	if err := os.WriteFile(pins, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f2 := &fakeRun{}
	r2 := &Router{Run: f2.run, Logf: f2.logf, PinFile: pins}
	r2.ClearRecorded()
	if len(f2.calls) != 0 {
		t.Fatalf("startup with an empty record ran %v, want nothing", f2.joined())
	}
}

// TestRouterClearRecordedIgnoresWhatItWouldNotPin: the record is a file, and
// a file can be wrong. An address tetherd would never pin must not become a
// delete just because it appears there, a malformed line must not stop the
// daemon, and a good line alongside them must still be honoured.
func TestRouterClearRecordedIgnoresWhatItWouldNotPin(t *testing.T) {
	f := &fakeRun{}
	pins := filepath.Join(t.TempDir(), "pins")
	if err := os.WriteFile(pins, []byte("8.8.8.8\nnot-an-address\n\n169.254.170.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Router{Run: f.run, Logf: f.logf, PinFile: pins}
	r.ClearRecorded()
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != delCmd {
		t.Fatalf("calls = %v, want only the allowlisted address deleted", f.joined())
	}
	for _, want := range []string{"8.8.8.8", "not-an-address"} {
		if !strings.Contains(f.log(), want) {
			t.Errorf("the ignored entry %q must be logged: %q", want, f.log())
		}
	}
}

// TestRouterClearRecordedKeepsQuietWithoutAPinFile: the unit tests and any
// caller that has not configured a record must not read or write anything.
func TestRouterClearRecordedKeepsQuietWithoutAPinFile(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run, Logf: f.logf}
	r.ClearRecorded()
	if err := r.Set([]netip.Addr{addr2()}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != addCmd {
		t.Fatalf("calls = %v, want just the add", f.joined())
	}
}

func readPins(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
