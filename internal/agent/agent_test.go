package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

func TestConfigFromEnv(t *testing.T) {
	_, err := ConfigFromEnv(func(string) string { return "" })
	if err == nil {
		t.Fatal("TETHERD_ENV missing must be an error")
	}
	cfg, err := ConfigFromEnv(func(k string) string {
		if k == "TETHERD_ENV" {
			return "dev"
		}
		return ""
	})
	if err != nil || cfg.Env != "dev" || cfg.Control != "127.0.0.1:9900" || cfg.AppContainer != "app" {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
}

func startAgent(t *testing.T) (addr string) {
	t.Helper()
	_, addr = newAgent(t, nil)
	return addr
}

// newAgent is startAgent for a test that has to ask the agent itself
// something - which of its sessions are registered, what it logged - and
// not only reach it over the wire. logf may be nil.
func newAgent(t *testing.T, logf func(string, ...any)) (*Agent, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{Env: "dev", TaskARN: "arn:test"}, logf) // nil: sessions log after the test ends
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go a.Serve(ctx, ln)
	return a, ln.Addr().String()
}

// stealHeaders are the header names a session that takes requests names in
// its hello. The agent treats an empty name as "matches nothing", so a
// hello that asks for incoming requests has to carry them.
const (
	stealHeader      = "X-Dev-User"
	stealTokenHeader = "X-Dev-Token"
)

// connect attaches the way `tetherd run` does: asking to receive incoming
// requests, with a token to match them against. That is what puts the
// session in the registry, so it is what the one-session-per-user rule and
// the welcome's session list are about.
func connect(t *testing.T, addr, user string) (*session.Client, error) {
	t.Helper()
	return dial(t, addr, proto.Hello{Version: proto.Version, User: user, Token: "tok",
		Incoming: proto.Incoming{Enabled: true, Header: stealHeader, TokenHeader: stealTokenHeader}})
}

// connectReadOnly attaches the way `tetherd env`, `tetherd doctor` and
// `tetherd status` do: no incoming requests, no token. Such a session can
// receive nothing, so it is not recorded in the registry at all.
func connectReadOnly(t *testing.T, addr, user string) (*session.Client, error) {
	t.Helper()
	return dial(t, addr, proto.Hello{Version: proto.Version, User: user})
}

func dial(t *testing.T, addr string, hello proto.Hello) (*session.Client, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := session.Dial(context.Background(), conn, hello, session.Options{})
	if err != nil {
		conn.Close()
	}
	return c, err
}

// attachedHeader is the header lookup a request from user with the token
// connect uses would present, so a test can ask the agent whether it would
// still route that request to that session.
func attachedHeader(user string) func(string) string {
	return func(name string) string {
		switch name {
		case stealHeader:
			return user
		case stealTokenHeader:
			return "tok"
		}
		return ""
	}
}

func TestWelcomeCarriesEnv(t *testing.T) {
	c, err := connect(t, startAgent(t), "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if w := c.Welcome(); w.Env != "dev" || w.TaskARN != "arn:test" || w.Version != proto.Version {
		t.Fatalf("welcome = %+v", w)
	}
}

// TestDuplicateUserRejected pins the rule the registry exists for, which
// this milestone narrowed but must not weaken: two sessions that can both
// receive requests for one name would make steal routing between them
// depend on map order, so the second is refused. (A read-only second
// session is a different thing and is allowed - see
// TestAReadOnlySessionAttachesAlongsideTheSameUsersRun.)
func TestDuplicateUserRejected(t *testing.T) {
	addr := startAgent(t)
	c1, err := connect(t, addr, "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	_, err = connect(t, addr, "shota")
	var rej *session.RejectedError
	if !errors.As(err, &rej) || rej.Err.Code != proto.CodeDuplicateUser {
		t.Fatalf("want duplicate_user, got %v", err)
	}
	// A different user is fine.
	c2, err := connect(t, addr, "taro")
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
}

func TestUserFreedAfterBye(t *testing.T) {
	addr := startAgent(t)
	c1, err := connect(t, addr, "shota")
	if err != nil {
		t.Fatal(err)
	}
	c1.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c2, err := connect(t, addr, "shota")
		if err == nil {
			c2.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("user not freed after bye: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDialThroughAgent(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	c, err := connect(t, startAgent(t), "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	conn, err := c.DialTCP(context.Background(), echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("got %q, %v", buf, err)
	}
}

func TestEmptyUserRejected(t *testing.T) {
	_, err := connect(t, startAgent(t), "")
	var rej *session.RejectedError
	if !errors.As(err, &rej) || rej.Err.Code != proto.CodeBadHello {
		t.Fatalf("want bad_hello, got %v", err)
	}
}

type fakeEnv struct {
	env map[string]string
	arn string
	err error
}

func (f fakeEnv) Read(context.Context) (map[string]string, string, error) { return f.env, f.arn, f.err }

func TestWelcomeCarriesAppEnv(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	a := New(Config{Env: "dev"}, nil)
	a.SetEnvReader(fakeEnv{env: map[string]string{"PORT": "8081"}, arn: "arn:task"})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Serve(ctx, ln)
	c, err := connect(t, ln.Addr().String(), "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w := c.Welcome()
	if w.AppEnv["PORT"] != "8081" || w.TaskARN != "arn:task" || w.EnvError != "" {
		t.Fatalf("welcome = %+v", w)
	}
}

func TestWelcomeReportsEnvError(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	a := New(Config{Env: "dev", TaskARN: "arn:cfg"}, nil)
	a.SetEnvReader(fakeEnv{err: errors.New("no process of container \"app\" visible; is pidMode \"task\" set")})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Serve(ctx, ln)
	c, err := connect(t, ln.Addr().String(), "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w := c.Welcome()
	if w.EnvError == "" || w.AppEnv != nil || w.TaskARN != "arn:cfg" {
		t.Fatalf("welcome = %+v", w)
	}
}

func TestWelcomeWithoutMetadata(t *testing.T) {
	c, err := connect(t, startAgent(t), "shota") // startAgent has no MetadataURL
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if w := c.Welcome(); !strings.Contains(w.EnvError, "not running in ECS") {
		t.Fatalf("welcome = %+v", w)
	}
}

// TestWelcomeCarriesAttachedSessions pins what `tetherd status` reads:
// Welcome.Sessions, the same set as Welcome.Others with where each session
// attached from and since when. Without it the registry's SessionInfo
// exists and never reaches the wire, which is the state v0.3a shipped in.
func TestWelcomeCarriesAttachedSessions(t *testing.T) {
	addr := startAgent(t)
	before := time.Now()
	// Attached out of order, because the order Welcome reports them in is
	// Sessions()'s sort and not the order they arrived.
	shota, err := connect(t, addr, "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer shota.Close()
	akira, err := connect(t, addr, "akira")
	if err != nil {
		t.Fatal(err)
	}
	defer akira.Close()

	taro, err := connect(t, addr, "taro")
	if err != nil {
		t.Fatal(err)
	}
	defer taro.Close()

	w := taro.Welcome()
	if len(w.Sessions) != 2 {
		t.Fatalf("welcome.sessions = %+v, want the two other sessions", w.Sessions)
	}
	// Sorted by user, which registry.go documents as a contract: unsorted,
	// the welcome one developer reads - and `tetherd status`'s output -
	// would differ from one attach to the next for no reason.
	if w.Sessions[0].User != "akira" || w.Sessions[1].User != "shota" {
		t.Errorf("welcome.sessions must be sorted by user: %+v", w.Sessions)
	}
	for _, s := range w.Sessions {
		// From and Since are the whole reason the field exists: "is my
		// colleague still attached, and since when?".
		if !strings.HasPrefix(s.From, "127.0.0.1:") {
			t.Errorf("session %q has no usable From: %+v", s.User, s)
		}
		if s.Since.Before(before) || s.Since.After(time.Now()) {
			t.Errorf("session %q has no usable Since: %+v", s.User, s)
		}
	}
	// The asking session is not one of them: it is the set Others has
	// always been, one field richer - and `tetherd status` must not report
	// the session it had to open in order to ask.
	for _, s := range w.Sessions {
		if s.User == "taro" {
			t.Errorf("welcome.sessions must leave the welcomed user out: %+v", w.Sessions)
		}
	}
	// Others is unchanged and describes the same set, so a CLI older than
	// this field keeps working against this agent.
	if len(w.Others) != 2 || w.Others[0] != "akira" || w.Others[1] != "shota" {
		t.Errorf("welcome.others = %v, want the same two names", w.Others)
	}
	// Nothing about a session's token may travel with it. connect attaches
	// with the token "tok".
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"tok"`) {
		t.Errorf("the welcome must not carry any session's token: %s", raw)
	}
}

// TestWelcomeOmitsSessionsWhenNobodyElseIsAttached keeps the wire quiet for
// the common case and keeps the two fields consistent: the first developer
// to attach must see neither, so that `tetherd status`'s fallback ("this
// agent sent Others and no Sessions, so it is older than the field") cannot
// be triggered by an agent that simply has nothing to report.
func TestWelcomeOmitsSessionsWhenNobodyElseIsAttached(t *testing.T) {
	c, err := connect(t, startAgent(t), "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if w := c.Welcome(); len(w.Sessions) != 0 || len(w.Others) != 0 {
		t.Fatalf("welcome = %+v, want no sessions and no others", w)
	}
}

// TestAReadOnlySessionAttachesAlongsideTheSameUsersRun is the defect this
// milestone fixes. `tetherd env`, `tetherd doctor` and `tetherd status`
// attach with Incoming disabled and no token, under the developer's own
// name - and until the registry stopped recording such sessions, every one
// of them was refused with duplicate_user while that developer's own
// `tetherd run` was attached. A live run is exactly when someone reaches
// for doctor or status, so the diagnostic was unavailable precisely when
// it was wanted. (Shipped in v0.2b and v0.3a.)
func TestAReadOnlySessionAttachesAlongsideTheSameUsersRun(t *testing.T) {
	a, addr := newAgent(t, nil)
	run, err := connect(t, addr, "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	ro, err := connectReadOnly(t, addr, "shota")
	if err != nil {
		t.Fatalf("a read-only session under an attached name must be allowed: %v", err)
	}
	defer ro.Close()

	// It reads the same welcome any session gets, which is the whole point:
	// it attached in order to read something.
	if w := ro.Welcome(); w.Env != "dev" {
		t.Errorf("the read-only session must get a usable welcome: %+v", w)
	}
	// And the run is untouched: still the one session for that name, still
	// the session a request for it is routed to.
	if got := a.Sessions(); len(got) != 1 || got[0].User != "shota" {
		t.Errorf("sessions = %+v, want only the run's", got)
	}
	if a.Match(attachedHeader("shota")) == nil {
		t.Error("the run must still be the session a request for shota goes to")
	}
}

// TestAReadOnlySessionIsNotReportedAsAttached pins what Welcome.Others and
// Welcome.Sessions mean: who can receive requests. A read-only session can
// receive nothing and is gone a moment later, so reporting it would tell a
// developer looking for the colleague who is stealing their requests about
// somebody who is not.
func TestAReadOnlySessionIsNotReportedAsAttached(t *testing.T) {
	_, addr := newAgent(t, nil)
	run, err := connect(t, addr, "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	// A name that is not otherwise attached, so its absence below is about
	// the read-only session and not about de-duplication.
	ro, err := connectReadOnly(t, addr, "kenji")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	third, err := connect(t, addr, "taro")
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()

	w := third.Welcome()
	if len(w.Others) != 1 || w.Others[0] != "shota" {
		t.Errorf("welcome.others = %v, want only the session that can receive requests", w.Others)
	}
	if len(w.Sessions) != 1 || w.Sessions[0].User != "shota" {
		t.Errorf("welcome.sessions = %+v, want only the session that can receive requests", w.Sessions)
	}
}

// TestAReadOnlySessionsCloseLeavesTheRunRegistered covers the trap in this
// change. handler.Closed unregisters whatever handler.user names, so a
// read-only session that had set it would - on detaching, which for a
// diagnostic is a second later - unregister that developer's own run. Their
// requests would then go to the application for the rest of the run, with
// nothing naming the cause.
func TestAReadOnlySessionsCloseLeavesTheRunRegistered(t *testing.T) {
	var mu sync.Mutex
	var lines strings.Builder
	a, addr := newAgent(t, func(f string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&lines, f+"\n", args...)
	})
	logged := func() string {
		mu.Lock()
		defer mu.Unlock()
		return lines.String()
	}

	run, err := connect(t, addr, "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	ro, err := connectReadOnly(t, addr, "shota")
	if err != nil {
		t.Fatal(err)
	}
	ro.Close()

	// The agent notices the disconnect on its own goroutine, so this is a
	// window rather than a single check: the mutated version (setting
	// handler.user for a read-only session) unregisters within
	// microseconds of the teardown that Close has already started, and
	// half a second of polling is many times that.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if a.Match(attachedHeader("shota")) == nil {
			t.Fatalf("a read-only session's close unregistered the run:\n%s", logged())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := a.Sessions(); len(got) != 1 || got[0].User != "shota" {
		t.Errorf("sessions = %+v, want the run still attached", got)
	}
	// Structural, and not a window: a read-only session must not produce
	// the registered session's detach line for a name it never held.
	// Compared line by line, because the read-only detach line is that
	// line plus a suffix and strings.Contains would match it.
	if hasLogLine(logged(), `user "shota" detached`) {
		t.Errorf("the read-only session detached somebody else's registration:\n%s", logged())
	}
}

// TestAReadOnlyWelcomeListsEveryAttachedSession pins what a read-only
// attach is told, including the case that matters: a session already
// attached under the *same* name. `tetherd status` attaches read-only as
// this developer, and their own `tetherd run` is the session they are most
// likely asking about - so a welcome that filtered by name would hide it,
// and the report would read as "your run is not attached to this task".
//
// There is nothing of the asking session's own to leave out, because a
// read-only session is not recorded at all.
func TestAReadOnlyWelcomeListsEveryAttachedSession(t *testing.T) {
	_, addr := newAgent(t, nil)
	run, err := connect(t, addr, "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	colleague, err := connect(t, addr, "taro")
	if err != nil {
		t.Fatal(err)
	}
	defer colleague.Close()

	ro, err := connectReadOnly(t, addr, "shota") // the same name as the run
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	w := ro.Welcome()
	if len(w.Sessions) != 2 || w.Sessions[0].User != "shota" || w.Sessions[1].User != "taro" {
		t.Errorf("welcome.sessions = %+v, want both attached sessions including the asking name's own run", w.Sessions)
	}
	if len(w.Others) != 2 {
		t.Errorf("welcome.others = %v, want both attached sessions", w.Others)
	}
	// A steal session still does not see itself: that is what Others has
	// always meant, and it is the session's own registration.
	if w := colleague.Welcome(); len(w.Others) != 1 || w.Others[0] != "shota" {
		t.Errorf("a session that can receive requests must not be listed its own: %v", w.Others)
	}
}

// hasLogLine reports whether the captured log has a line that is exactly
// want. Exact, because the agent's lines are deliberately prefixes of one
// another - "user %q detached" and "user %q detached (read only)" mean
// different things and a substring test cannot tell them apart.
func hasLogLine(log, want string) bool {
	for _, line := range strings.Split(log, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// TestAReadOnlyAttachAndDetachAreLogged pins the one consequence of not
// recording read-only sessions that is not wanted: the agent's log is the
// only place to see that a CLI reached the task at all, and
// `tetherd run --no-incoming` is long-lived *and* read-only (docs
// e2e-aws.md row 25), so a session that logged neither its attach nor its
// detach would leave "my requests are not arriving" with no server-side
// trace to check.
//
// The two wordings must stay distinguishable: a read-only session is in
// nobody's `tetherd status` and can receive nothing, so a line that read
// like a steal attach would have someone hunting for a registry entry
// that never existed.
func TestAReadOnlyAttachAndDetachAreLogged(t *testing.T) {
	var mu sync.Mutex
	var lines strings.Builder
	a, addr := newAgent(t, func(f string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&lines, f+"\n", args...)
	})
	logged := func() string {
		mu.Lock()
		defer mu.Unlock()
		return lines.String()
	}

	ro, err := connectReadOnly(t, addr, "shota")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(logged(), `user "shota" attached`) }, "the read-only attach to be logged")
	attach := logged()
	if !strings.Contains(attach, "to read only") {
		t.Errorf("a read-only attach must not read like a session that takes requests:\n%s", attach)
	}
	// It is logged without being recorded: the log line is not evidence of
	// a registry entry, and must not become one.
	if got := a.Sessions(); len(got) != 0 {
		t.Errorf("a logged read-only attach must still not be registered: %+v", got)
	}

	ro.Close()
	waitFor(t, func() bool { return hasLogLine(logged(), `user "shota" detached (read only)`) }, "the read-only detach to be logged")
	// And it must not be the registered session's line, which would claim
	// an unregister that did not happen.
	if hasLogLine(logged(), `user "shota" detached`) {
		t.Errorf("a read-only detach must not read like a registered session's:\n%s", logged())
	}
}
