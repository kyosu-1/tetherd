package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/agent"
	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// statusToken is the token these tests put in RunOptions. It has to be
// non-empty for TestStatusNeverSendsATokenOrTakesRequests to mean anything:
// "the wire carried no token" is only a claim if there was a token to
// carry.
const statusToken = "s3cret-status-token"

// attachAs opens a session to addr as user and keeps it for the rest of the
// test - a colleague's `tetherd run`, which is what `tetherd status` has to
// report. It asks for incoming requests and carries a token, the way a real
// run does, so the sessions in the report are steal targets and `status`'s
// own session is the only one that is not.
func attachAs(t *testing.T, addr, user string) *session.Client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := session.Dial(context.Background(), conn, proto.Hello{
		Version: proto.Version,
		User:    user,
		Token:   "colleague-token",
		Incoming: proto.Incoming{
			Enabled:     true,
			Header:      DefaultMatchHeader,
			TokenHeader: DefaultMatchTokenHeader,
		},
	}, session.Options{OnHTTP: func(net.Conn) {}})
	if err != nil {
		conn.Close()
		t.Fatalf("attach as %q: %v", user, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// fakeAgentHandler is the agent side of the wire as a test can shape it: it
// records the hello it was sent and answers with the welcome the test
// chose.
//
// Both halves are things a real *agent.Agent cannot do. The hello matters
// because the security assertion has to read what the wire carried, not
// what the CLI put in a variable; the welcome because v0.3a's agent - the
// one deployed right now - fills Others and knows nothing about Sessions,
// and `tetherd status` has to degrade against it rather than fail.
type fakeAgentHandler struct {
	welcome proto.Welcome

	mu    sync.Mutex
	hello proto.Hello
	got   bool
}

func (h *fakeAgentHandler) Hello(hello proto.Hello, remote string, open session.Opener) (proto.Welcome, *proto.Error) {
	h.mu.Lock()
	h.hello = hello
	h.got = true
	h.mu.Unlock()
	w := h.welcome
	if w.Version == "" {
		w.Version = proto.Version
	}
	return w, nil
}

func (h *fakeAgentHandler) Dial(context.Context, string) (net.Conn, error) {
	return nil, errors.New("this agent does not dial")
}

func (h *fakeAgentHandler) Resolve(context.Context, string) ([]string, int, error) {
	return nil, 0, errors.New("this agent does not resolve")
}

func (h *fakeAgentHandler) Closed() {}

// received returns the hello this handler was sent, and whether it was sent
// one at all.
func (h *fakeAgentHandler) received() (proto.Hello, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hello, h.got
}

// startFakeAgent serves h on a loopback listener and returns its address.
// Nothing in here may call t.Fatal: it runs on its own goroutines, where
// Fatal calls Goexit and kills that goroutine instead of failing the test.
func startFakeAgent(t *testing.T, h session.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				session.Serve(ctx, conn, h, session.ServeOptions{})
				conn.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// deadAddr returns a loopback address nothing is listening on, by opening a
// listener and closing it again.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// statusOpts is ssmOpts for `tetherd status`, carrying a token the command
// must never put on the wire.
func statusOpts() StatusOptions {
	opts := StatusOptions{RunOptions: ssmOpts()}
	opts.Token = statusToken
	return opts
}

// sectionFor returns the lines of the report under the heading for task id,
// up to the next heading. The report is nested, so "which task is this
// developer attached to" is a question about grouping, and a test that only
// searched the whole output would pass with every session listed under the
// wrong task.
func sectionFor(t *testing.T, out, id string) string {
	t.Helper()
	head := "  task " + short(id)
	start := strings.Index(out, head)
	if start < 0 {
		t.Fatalf("no section for task %s in:\n%s", short(id), out)
	}
	rest := out[start+len(head):]
	if next := strings.Index(rest, "\n  task "); next >= 0 {
		return rest[:next]
	}
	return rest
}

// TestStatusListsWhoIsAttachedToEachTask is the command's reason to exist.
// Steal is shared: two developers on one dev service, and the ALB decides
// which task each request lands on. "My requests are not arriving" is
// answered by seeing who else is attached and to which task, so this is the
// diagnostic for the most likely support question - which means the report
// has to name the task, the developer, where they attached from and since
// when, and has to group them under the right task.
func TestStatusListsWhoIsAttachedToEachTask(t *testing.T) {
	ag1 := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	ag2 := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	attachAs(t, ag1.addr, "shota")
	attachAs(t, ag1.addr, "taro")
	attachAs(t, ag2.addr, "hanako")
	waitFor(t, func() bool { return len(ag1.a.Sessions()) == 2 && len(ag2.a.Sessions()) == 1 }, "the colleagues to attach")

	const id1, id2 = "fbc1abc4deadbeef", "a17e1234cafef00d"
	p := &fakeProvider{
		region: "r",
		tasks: []transport.Task{
			{ID: id1, Addr: ag1.addr, SubnetID: "subnet-a", StartedAt: time.Now().Add(-2*time.Hour - 12*time.Minute)},
			{ID: id2, Addr: ag2.addr, SubnetID: "subnet-a", StartedAt: time.Now().Add(-6 * time.Minute)},
		},
	}
	var out, logs strings.Builder
	code, err := StatusRunWithDeps(context.Background(), statusOpts(), &out, &logs, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v logs=%s", code, err, logs.String())
	}

	report := out.String()
	first := sectionFor(t, report, id1)
	second := sectionFor(t, report, id2)
	for _, want := range []string{"shota", "taro"} {
		if !strings.Contains(first, want) {
			t.Errorf("%s must be listed under task %s:\n%s", want, short(id1), report)
		}
		if strings.Contains(second, want) {
			t.Errorf("%s is attached to task %s, not %s:\n%s", want, short(id1), short(id2), report)
		}
	}
	if !strings.Contains(second, "hanako") {
		t.Errorf("hanako must be listed under task %s:\n%s", short(id2), report)
	}
	// Who is not enough: the question is whether a colleague is *still*
	// attached and since when, so from where and since when must be there
	// too. Both come from Welcome.Sessions and only from there - an agent
	// that sent Others alone would print the names and neither column.
	if !strings.Contains(first, "from 127.0.0.1:") {
		t.Errorf("every session line must say where the developer attached from:\n%s", report)
	}
	if !regexp.MustCompile(`attached \d+s ago`).MatchString(first) {
		t.Errorf("every session line must say since when:\n%s", report)
	}
	// The task's own age, so a task that has just been rolled out is
	// recognisable as such.
	if !strings.Contains(report, "(started 2h12m ago)") || !strings.Contains(report, "(started 6m ago)") {
		t.Errorf("each task's age must be reported:\n%s", report)
	}
	// `status` had to attach in order to ask, and that session is not a
	// developer working on this service: reporting itself would put a name
	// in the table that vanishes the moment the command exits.
	//
	// Two independent things make this true, so no single mutation
	// falsifies the check and it is a regression guard rather than a pin:
	// the agent records no read-only session at all, so there is nothing
	// of status's own in the welcome (registry.go's register), and the
	// welcome leaves out the asking session besides (handler.Hello). Each
	// mechanism has its own agent-side test -
	// TestAReadOnlySessionIsNotReportedAsAttached and
	// TestAReadOnlyWelcomeListsEveryAttachedSession.
	if strings.Contains(report, "tester") {
		t.Errorf("`status` must not report its own session:\n%s", report)
	}
	if strings.Contains(report, "nobody attached") {
		t.Errorf("both tasks have sessions:\n%s", report)
	}
	// The report is what the developer asked for and belongs on stdout;
	// stderr carries the target line, checked both ways so that a report
	// written to the wrong stream cannot pass.
	if !strings.Contains(logs.String(), "2 tasks") {
		t.Errorf("the target line must reach stderr: %q", logs.String())
	}
	if strings.Contains(report, "tetherd  ") {
		t.Errorf("stdout must carry only the report:\n%s", report)
	}
}

// TestStatusSaysSoWhenNobodyIsAttached pins that an empty list reads as a
// finding. A task heading with nothing under it reads as a broken tool -
// "it printed nothing, did it even work?" - which is the one thing a
// diagnostic must never leave a reader wondering.
//
// The "tester" check below is a regression guard, not this test's point.
// It was written when a `status` attach did register at the agent, so that
// listing every session the agent knew would have printed a developer
// here; since the registry stopped recording read-only sessions that is no
// longer what makes it true. What does: status's session is not in the
// registry, and the welcome excludes the asking session anyway. See
// TestStatusListsWhoIsAttachedToEachTask for where those are pinned.
func TestStatusSaysSoWhenNobodyIsAttached(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: ag.addr}
	var out, logs strings.Builder
	code, err := StatusRunWithDeps(context.Background(), statusOpts(), &out, &logs, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v logs=%s", code, err, logs.String())
	}
	if !strings.Contains(out.String(), "(nobody attached)") {
		t.Fatalf("an empty list must say nobody is attached, in words:\n%s", out.String())
	}
	if strings.Contains(out.String(), "tester") {
		t.Fatalf("`status`'s own session must not be reported as a developer:\n%s", out.String())
	}
}

// TestStatusNeverSendsATokenOrTakesRequests is the security requirement of
// this command, and it is checked at the agent's side of the wire rather
// than from the CLI's own variables.
//
// `status` attaches to read: it reads one welcome per task and exits. If it
// registered as a steal target, the agent would match a colleague's
// request - their name, their token - against this session and push it into
// a process that prints a table and exits, and that request would arrive
// nowhere at all. So the hello must carry Incoming disabled and no token,
// and the agent must not be willing to route anything into the session
// while it is attached.
func TestStatusNeverSendsATokenOrTakesRequests(t *testing.T) {
	// Half one: what the wire carried. The token is in RunOptions, so an
	// empty Hello.Token here is a fact about this command and not about an
	// empty fixture.
	h := &fakeAgentHandler{welcome: proto.Welcome{Env: "dev", TaskARN: "arn:test"}}
	addr := startFakeAgent(t, h)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: addr}
	var out, logs strings.Builder
	code, err := StatusRunWithDeps(context.Background(), statusOpts(), &out, &logs, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v logs=%s", code, err, logs.String())
	}
	hello, got := h.received()
	if !got {
		t.Fatal("the agent was never sent a hello, so this test proves nothing")
	}
	if hello.Incoming.Enabled {
		t.Errorf("status must send Incoming disabled; the agent received %+v", hello.Incoming)
	}
	if hello.Token != "" {
		t.Errorf("status must send no token; the agent received one of %d bytes", len(hello.Token))
	}
	if hello.Incoming != (proto.Incoming{}) {
		t.Errorf("status must send the zero Incoming; the agent received %+v", hello.Incoming)
	}
	if hello.User != "tester" {
		t.Errorf("hello.user = %q, want the configured user", hello.User)
	}
	if strings.Contains(out.String()+logs.String(), statusToken) {
		t.Errorf("the token must not be printed either:\n%s\n%s", out.String(), logs.String())
	}

	// Half two: what the agent would do with a request while that session
	// is attached. Asked of a real *agent.Agent, inside the window the
	// session exists: the gate blocks the agent in the middle of its own
	// hello handling, after it has decided what to do with the hello and
	// before the welcome is sent. Reading the registry back after the
	// command returned would prove nothing - the session is gone by then.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// blockFrom 2, because the colleague attached below goes through the
	// same reader and must not be held in its own handshake.
	gate := &gateEnvReader{entered: make(chan struct{}), release: make(chan struct{}), blockFrom: 2}
	t.Cleanup(gate.open)
	a := agent.New(agent.Config{Env: "dev", TaskARN: "arn:test", AppContainer: "app"}, nil)
	a.SetEnvReader(gate)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go a.Serve(ctx, ln)
	// A colleague who *is* taking requests, so the assertions below are
	// against a registry that is demonstrably readable and non-empty: "the
	// agent has no session for tester" then means the read-only session
	// was left out, not that nothing was observed at all.
	attachAs(t, ln.Addr().String(), "shota")
	waitFor(t, func() bool { return len(a.Sessions()) == 1 }, "the colleague to attach")

	p2 := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: ln.Addr().String()}
	done := make(chan int, 1)
	go func() {
		var out, logs strings.Builder
		code, _ := StatusRunWithDeps(context.Background(), statusOpts(), &out, &logs, depsFor(p2))
		done <- code
	}()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent never reached the env read, so the session window was never observed")
	}
	// A read-only session is not recorded at all, so the only session the
	// agent holds during the window is the colleague's.
	if got := a.Sessions(); len(got) != 1 || got[0].User != "shota" {
		t.Errorf("sessions during the window = %+v, want only the colleague's", got)
	}
	// A request carrying the name status attached under and the token it
	// was given must belong to the application, not to status.
	header := map[string]string{DefaultMatchHeader: "tester", DefaultMatchTokenHeader: statusToken}
	if s := a.Match(func(k string) string { return header[k] }); s != nil {
		t.Errorf("the agent would steal a request into `tetherd status`: %v", s)
	}
	// And so must one carrying no token at all, which is what a session
	// registered with an empty token would otherwise match.
	if s := a.Match(func(string) string { return "" }); s != nil {
		t.Errorf("the agent would steal an unheadered request into `tetherd status`: %v", s)
	}
	gate.open()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("status exited %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("status never returned")
	}
}

// gateEnvReader blocks the agent inside its hello handling - after the
// session is registered, before the welcome is sent - so that a test can
// inspect the registry in the window a session is attached.
type gateEnvReader struct {
	entered chan struct{}
	release chan struct{}
	// blockFrom is which read to start blocking at, counting from 1. A
	// fixture that attaches a session of its own before the one under test
	// sets it to 2: the earlier attach must complete, and it has when the
	// client it returns has its welcome, which the agent sends only after
	// this read returns.
	blockFrom int

	mu    sync.Mutex
	calls int
	in    sync.Once
	out   sync.Once
}

func (g *gateEnvReader) Read(context.Context) (map[string]string, string, error) {
	g.mu.Lock()
	g.calls++
	n := g.calls
	g.mu.Unlock()
	if g.blockFrom == 0 || n >= g.blockFrom {
		g.in.Do(func() { close(g.entered) })
		<-g.release
	}
	return map[string]string{"PORT": "1"}, "arn:test", nil
}

// open lets the blocked agent finish. Idempotent, so a test can both
// release it deliberately and register it as cleanup: a failure before the
// deliberate release would otherwise leave a goroutine blocked for the
// life of the test binary.
func (g *gateEnvReader) open() { g.out.Do(func() { close(g.release) }) }

// TestStatusReportsATaskItCannotReach pins that one unreachable task does
// not hide the others. A service with one task of two unreachable is a
// service that steals half of what a developer asked for, and a command
// that gave up on the first failure would report nothing about the half
// that works - or about the half that does not.
func TestStatusReportsATaskItCannotReach(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	attachAs(t, ag.addr, "shota")
	waitFor(t, func() bool { return len(ag.a.Sessions()) == 1 }, "the colleague to attach")

	const good, bad = "fbc1abc4deadbeef", "a17e1234cafef00d"
	p := &fakeProvider{
		region: "r",
		tasks: []transport.Task{
			{ID: good, Addr: ag.addr, SubnetID: "subnet-a", StartedAt: time.Now().Add(-time.Hour)},
			{ID: bad, Addr: deadAddr(t), SubnetID: "subnet-a", StartedAt: time.Now().Add(-time.Minute)},
		},
	}
	var out, logs strings.Builder
	code, err := StatusRunWithDeps(context.Background(), statusOpts(), &out, &logs, depsFor(p))
	// Reachable tasks were reported, so the command succeeded: the
	// unreachable one is a row, not a failure of the whole report.
	if err != nil || code != 0 {
		t.Fatalf("one unreachable task must not fail the command: code=%d err=%v logs=%s", code, err, logs.String())
	}
	report := out.String()
	if !strings.Contains(sectionFor(t, report, good), "shota") {
		t.Errorf("the reachable task's sessions must still be reported:\n%s", report)
	}
	// The unreachable task gets a row with a reason, so a developer knows
	// it exists and why nothing is known about it.
	broken := sectionFor(t, report, bad)
	if !strings.Contains(broken, "connect to agent") {
		t.Errorf("the unreachable task's row must say why:\n%s", report)
	}
	if strings.Contains(broken, "nobody attached") {
		t.Errorf("a task that could not be read is not a task with nobody attached:\n%s", report)
	}
}

// TestStatusFallsBackToNamesAgainstAnOlderAgent pins the degrade path
// spec §8 asks for. v0.3a's agent is deployed right now: it fills
// Welcome.Others and knows nothing about Welcome.Sessions, so `status` must
// print the names it does have and say plainly that the detail is not
// available - never fail, and never present a name as if the missing
// columns were a fact about the session.
func TestStatusFallsBackToNamesAgainstAnOlderAgent(t *testing.T) {
	old := &fakeAgentHandler{welcome: proto.Welcome{
		Env:     "dev",
		TaskARN: "arn:test",
		Others:  []string{"shota", "taro"},
		// No Sessions: that field does not exist in the agent this stands
		// in for.
	}}
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: startFakeAgent(t, old)}
	var out, logs strings.Builder
	code, err := StatusRunWithDeps(context.Background(), statusOpts(), &out, &logs, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("an older agent must degrade, not fail: code=%d err=%v logs=%s", code, err, logs.String())
	}
	report := out.String()
	for _, want := range []string{"shota", "taro"} {
		if !strings.Contains(report, want) {
			t.Errorf("the names the older agent did send must be reported:\n%s", report)
		}
	}
	if !strings.Contains(report, "but not from where or since when") {
		t.Errorf("the report must say the detail is unavailable:\n%s", report)
	}
	// Nothing may be invented to fill the columns that did not arrive: each
	// name is a line of its own, with no origin and no age beside it.
	for _, name := range []string{"shota", "taro"} {
		if !hasBareLine(report, name) {
			t.Errorf("%s must be reported as a name alone, with nothing claimed about it:\n%s", name, report)
		}
	}
	if strings.Contains(report, "attached ") {
		t.Errorf("no session may be given an age the agent did not send:\n%s", report)
	}
	if strings.Contains(report, "nobody attached") {
		t.Errorf("two developers are attached:\n%s", report)
	}
}

// TestStatusRefusesAnEnvironmentMismatch pins that `status` applies the
// same guard `run` and `env` do: a developer who typo'd --cluster onto a
// production service is told so, not shown its attached sessions. It also
// pins the exit status when not one task could be read - 1, with the
// reasons in the report rather than repeated as an error.
func TestStatusRefusesAnEnvironmentMismatch(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	attachAs(t, ag.addr, "shota")
	waitFor(t, func() bool { return len(ag.a.Sessions()) == 1 }, "the colleague to attach")
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: ag.addr}
	opts := statusOpts()
	opts.TargetEnv = "prod" // the in-process agent reports "dev"
	var out, logs strings.Builder
	code, err := StatusRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if code != 1 || err != nil {
		t.Fatalf("code=%d err=%v, want 1 with the reason in the report", code, err)
	}
	report := out.String()
	if !strings.Contains(report, "TETHERD_ENV") || !strings.Contains(report, "prod") {
		t.Errorf("the row must say the environment did not match:\n%s", report)
	}
	if strings.Contains(report, "shota") {
		t.Errorf("no session may be reported from a task that failed the environment check:\n%s", report)
	}
}

// TestStatusReadsATaskWhileThisDevelopersRunIsAttached is the payoff of
// narrowing the agent's one-session-per-user rule to sessions that can
// receive requests. `tetherd status` run alongside your own `tetherd run`
// is the common case - a live run is exactly when you ask who else is
// attached - and until that change the attach was refused with
// duplicate_user on every task, so the command could answer nothing at the
// one moment it was wanted. It must now read the task and report the run.
func TestStatusReadsATaskWhileThisDevelopersRunIsAttached(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	attachAs(t, ag.addr, "tester") // this developer's own run, same name
	waitFor(t, func() bool { return len(ag.a.Sessions()) == 1 }, "the run to attach")
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: ag.addr}
	var out, logs strings.Builder
	code, err := StatusRunWithDeps(context.Background(), statusOpts(), &out, &logs, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("status must read a task this developer is already attached to: code=%d err=%v logs=%s", code, err, logs.String())
	}
	report := out.String()
	// The run is reported, from where and since when - it is a session that
	// can receive requests, so it is exactly what `status` is asked about,
	// even though it happens to be this developer's own.
	if !strings.Contains(report, "tester") || !strings.Contains(report, "from 127.0.0.1:") {
		t.Errorf("the attached run must be reported:\n%s", report)
	}
	if strings.Contains(report, "nobody attached") || strings.Contains(report, "not read") {
		t.Errorf("nothing may be refused or missing here:\n%s", report)
	}
}

// TestStatusReportsARefusalWithWhatTheAgentSaid covers what is left of the
// duplicate_user path now that a read-only attach is not refused: an agent
// older than that change - v0.3a's, deployed today - refuses any second
// hello for a name, including `status`'s. RejectedError's own text drops
// the refusal's From and Since, so the row prints them instead of the raw
// protocol wording.
func TestStatusReportsARefusalWithWhatTheAgentSaid(t *testing.T) {
	refusing := &refusingAgentHandler{err: proto.Error{
		Code:    proto.CodeDuplicateUser,
		Message: `another session for user "tester" is already attached`,
		From:    "10.0.0.7:51000",
		Since:   "2026-09-12T09:00:00Z",
	}}
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: startFakeAgent(t, refusing)}
	var out, logs strings.Builder
	code, err := StatusRunWithDeps(context.Background(), statusOpts(), &out, &logs, depsFor(p))
	if code != 1 || err != nil {
		t.Fatalf("code=%d err=%v, want 1 with the reason in the report", code, err)
	}
	report := out.String()
	for _, want := range []string{"tester", "10.0.0.7:51000", "2026-09-12T09:00:00Z"} {
		if !strings.Contains(report, want) {
			t.Errorf("the row must carry %q from the refusal:\n%s", want, report)
		}
	}
}

// refusingAgentHandler answers every hello with one error - an agent that
// refuses this attach, whatever the reason it gives.
type refusingAgentHandler struct{ err proto.Error }

func (h *refusingAgentHandler) Hello(proto.Hello, string, session.Opener) (proto.Welcome, *proto.Error) {
	e := h.err
	return proto.Welcome{}, &e
}

func (h *refusingAgentHandler) Dial(context.Context, string) (net.Conn, error) {
	return nil, errors.New("this agent does not dial")
}

func (h *refusingAgentHandler) Resolve(context.Context, string) ([]string, int, error) {
	return nil, 0, errors.New("this agent does not resolve")
}

func (h *refusingAgentHandler) Closed() {}

// TestStatusCommandParsesFlags exercises the cobra layer end to end
// (NewRootCommand -> flag parsing -> statusFn). Without it, forgetting to
// register the command at all, or binding its target flags to a throwaway
// RunOptions, would pass the whole suite.
func TestStatusCommandParsesFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	var captured StatusOptions
	statusFn = func(opts StatusOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { statusFn = defaultStatus })

	root := NewRootCommand()
	root.SetArgs([]string{"status", "--transport", "direct", "--agent-addr", "127.0.0.1:9900", "--task", "abc123"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Transport != "direct" || captured.AgentAddr != "127.0.0.1:9900" || captured.TaskID != "abc123" {
		t.Errorf("the target flags must reach StatusOptions: %+v", captured)
	}
	// No steal setting reaches this command, and in particular not the
	// personal file's token: applyIncoming is `run`'s alone, so there is
	// nothing here for a later change to accidentally put on the wire.
	if captured.Token != "" || captured.LocalPort != 0 || captured.MatchHeader != "" || captured.MatchTokenHeader != "" {
		t.Errorf("`status` must carry no steal settings: %+v", captured.RunOptions)
	}
}

// hasBareLine reports whether the report has a line that is exactly want,
// ignoring the indentation.
func hasBareLine(report, want string) bool {
	for _, line := range strings.Split(report, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// TestHumanAgoReadsAsAReport pins the ages in the report: Duration's own
// String would print "14m0s" and "2h12m3.4s", and a clock skew that made an
// age negative would print "-3m", which reads as a time in the future.
func TestHumanAgoReadsAsAReport(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{14 * time.Minute, "14m"},
		{time.Hour - time.Second, "59m"},
		{2*time.Hour + 12*time.Minute + 3*time.Second, "2h12m"},
		{-5 * time.Second, "0s"},
	} {
		if got := humanAgo(tc.d); got != tc.want {
			t.Errorf("humanAgo(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
