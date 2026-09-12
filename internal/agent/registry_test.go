package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

// nopOpener stands in for a CLI's stream opener. It fails rather than
// returning a nil net.Conn with a nil error: a test that actually tries to
// open a stream should see an error it can report, not a nil connection
// that panics three frames later.
type nopOpener struct{}

func (nopOpener) OpenStream() (net.Conn, error) {
	return nil, errors.New("nopOpener: this test session has no stream")
}

// TestASessionNeverPrintsItsToken is the containment invariant, measured.
// Unexported fields keep encoding/json out but not fmt, which reads them by
// reflection: %+v on a *Session would otherwise put a plaintext token into
// the agent's stdout, which is CloudWatch, which colleagues can read - and
// the token is the only thing between the public ALB and a laptop. The
// natural log line in the proxy is p.Logf("... %v", s).
func TestASessionNeverPrintsItsToken(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	h := proto.Hello{Version: proto.Version, User: "shota", Token: "s3cret-token",
		Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
	if e := a.register(h, "10.0.0.1:5000", nopOpener{}); e != nil {
		t.Fatal(e)
	}
	s := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "s3cret-token"}))
	if s == nil {
		t.Fatal("the registered session must be matchable")
	}
	subjects := map[string]any{
		"Session":    *s,
		"*Session":   s,
		"[]*Session": []*Session{s},
	}
	for _, verb := range []string{"%v", "%s", "%+v", "%#v"} {
		for shape, arg := range subjects {
			out := fmt.Sprintf(verb, arg)
			if strings.Contains(out, "s3cret-token") {
				t.Errorf("%s of %s discloses the token: %s", verb, shape, out)
			}
			// A redacted rendering is only useful if it still names the
			// session, otherwise the next person prints the struct fields
			// by hand to get something readable.
			if !strings.Contains(out, "shota") {
				t.Errorf("%s of %s says nothing about the session: %s", verb, shape, out)
			}
		}
	}
}

// TestMatchRequestSuppliesACaseInsensitiveLookup covers the entry point the
// proxy uses. MatchRequest exists so that no caller picks the lookup: a
// direct r.Header[name] index works for canonically spelled configured
// names and silently stops matching a lowercase incoming.match.header,
// which internal/config accepts verbatim, and the failure would first
// appear on a real ALB as a request quietly served by the application.
func TestMatchRequestSuppliesACaseInsensitiveLookup(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	lowerCfg := proto.Hello{Version: proto.Version, User: "shota", Token: "tok-shota",
		Incoming: proto.Incoming{Enabled: true, Header: "x-dev-user", TokenHeader: "x-dev-token"}}
	if e := a.register(lowerCfg, "10.0.0.1:5000", nopOpener{}); e != nil {
		t.Fatal(e)
	}
	canonicalWire := parseRequest(t, "GET /orders HTTP/1.1\r\nHost: dev.example.com\r\nX-Dev-User: shota\r\nX-Dev-Token: tok-shota\r\n\r\n")
	if got := a.MatchRequest(canonicalWire); got == nil {
		t.Error("a lowercase configured header name must match what the wire canonicalises to")
	}
	lowerWire := parseRequest(t, "GET /orders HTTP/1.1\r\nHost: dev.example.com\r\nx-dev-user: shota\r\nx-dev-token: tok-shota\r\n\r\n")
	if got := a.MatchRequest(lowerWire); got == nil {
		t.Error("a lowercase header on the wire must match too")
	}
	// And the security property survives the convenience wrapper.
	wrongToken := parseRequest(t, "GET /orders HTTP/1.1\r\nHost: dev.example.com\r\nX-Dev-User: shota\r\nX-Dev-Token: guess\r\n\r\n")
	if got := a.MatchRequest(wrongToken); got != nil {
		t.Errorf("a wrong token must not steal through MatchRequest: %v", got)
	}
	health := parseRequest(t, "GET /healthz HTTP/1.1\r\nHost: 10.0.1.23:8080\r\nUser-Agent: ELB-HealthChecker/2.0\r\n\r\n")
	if got := a.MatchRequest(health); got != nil {
		t.Errorf("a health check must not steal through MatchRequest: %v", got)
	}
}

// TestRegisterWarnsWhenIncomingCannotWork covers the silent misconfiguration:
// a hello that asks for incoming requests but cannot match one attaches
// successfully, prints an "attached" line, and then has every request go to
// the application with nothing naming the cause.
//
// The cases below are hand-built proto.Hello values because that is the only
// thing that reaches the branch. tetherd's own CLI always fills both header
// names and refuses an empty token (internal/cli/steal.go's stealSettings),
// so incomingGap guards an older or third-party client - do not simplify it
// away on the belief that `tetherd run` exercises it.
func TestRegisterWarnsWhenIncomingCannotWork(t *testing.T) {
	full := proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}
	cases := []struct {
		name  string
		hello proto.Hello
		warn  string
	}{
		{"no token", proto.Hello{User: "shota", Incoming: full}, "no token"},
		{"no header", proto.Hello{User: "shota", Token: "tok-shota",
			Incoming: proto.Incoming{Enabled: true, TokenHeader: "X-Dev-Token"}}, "incoming.match.header is empty"},
		{"no token header", proto.Hello{User: "shota", Token: "tok-shota",
			Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User"}}, "incoming.match.token_header is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines strings.Builder
			a := New(Config{Env: "dev"}, func(f string, args ...any) { lines.WriteString(fmt.Sprintf(f, args...) + "\n") })
			if e := a.register(tc.hello, "10.0.0.1:5000", nopOpener{}); e != nil {
				t.Fatal(e)
			}
			got := lines.String()
			if !strings.Contains(got, tc.warn) || !strings.Contains(got, "shota") {
				t.Errorf("want a warning naming %q and the user, got:\n%s", tc.warn, got)
			}
			if strings.Contains(got, "tok-shota") {
				t.Errorf("the warning must not carry the token:\n%s", got)
			}
		})
	}
	// A usable session says nothing, and a disabled one says nothing even
	// though it has no token: --no-incoming is a choice, not a mistake.
	for _, h := range []proto.Hello{
		{User: "shota", Token: "tok-shota", Incoming: full},
		{User: "shota", Incoming: proto.Incoming{Enabled: false}},
	} {
		var lines strings.Builder
		a := New(Config{Env: "dev"}, func(f string, args ...any) { lines.WriteString(fmt.Sprintf(f, args...) + "\n") })
		if e := a.register(h, "10.0.0.1:5000", nopOpener{}); e != nil {
			t.Fatal(e)
		}
		if strings.Contains(lines.String(), "cannot receive incoming requests") {
			t.Errorf("hello %+v must not be warned about:\n%s", h.Incoming, lines.String())
		}
	}
}

// TestSessionsAreSortedByUser pins the order, which others() turns into
// Welcome.Others: unsorted, the welcome a developer sees would differ from
// one attach to the next for no reason.
func TestSessionsAreSortedByUser(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	in := []string{"yuki", "taro", "shota", "rin", "mei", "ken", "hanako", "akira"}
	for _, u := range in {
		h := proto.Hello{Version: proto.Version, User: u, Token: "tok-" + u,
			Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
		if e := a.register(h, "10.0.0.1:5000", nopOpener{}); e != nil {
			t.Fatal(e)
		}
	}
	want := []string{"akira", "hanako", "ken", "mei", "rin", "shota", "taro", "yuki"}
	var got []string
	for _, s := range a.Sessions() {
		got = append(got, s.User)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Sessions() = %v, want %v", got, want)
	}
	// Welcome.Others is this exact composition in handler.Hello, so the
	// test asks for it the same way rather than through a parallel helper
	// that could drift from what the wire carries.
	if others := sessionUsers(a.sessionInfos("shota")); len(others) != len(want)-1 || others[0] != "akira" {
		t.Errorf("Welcome.Others for shota = %v", others)
	}
}

func TestRegisterKeepsTheTokenOutOfTheVisibleView(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	h := proto.Hello{Version: proto.Version, User: "shota", Token: "s3cret-token",
		Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
	if e := a.register(h, "127.0.0.1:1", nopOpener{}); e != nil {
		t.Fatal(e)
	}
	infos := a.Sessions()
	if len(infos) != 1 || infos[0].User != "shota" {
		t.Fatalf("sessions = %+v", infos)
	}
	// SessionInfo is what `tetherd status` will print and what the agent
	// logs; a token must not be reachable through it.
	if strings.Contains(fmtSessions(infos), "s3cret-token") {
		t.Fatal("the token must not appear in the visible session view")
	}
	// Nor through %+v on the view, which is how a debug log line would
	// render it.
	if strings.Contains(fmt.Sprintf("%+v", infos), "s3cret-token") {
		t.Fatal("the token must not appear in a formatted SessionInfo")
	}
	if got := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "s3cret-token"})); got == nil {
		t.Fatal("the registered session must be matchable")
	}
	a.unregister("shota")
	if got := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "s3cret-token"})); got != nil {
		t.Fatal("a detached session must stop taking requests")
	}
	if len(a.Sessions()) != 0 {
		t.Fatalf("sessions after unregister = %+v", a.Sessions())
	}
}

func TestRegisterStoresTheOpenerForTheProxy(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	var open session.Opener = nopOpener{}
	h := proto.Hello{Version: proto.Version, User: "shota", Token: "t",
		Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
	if e := a.register(h, "127.0.0.1:1", open); e != nil {
		t.Fatal(e)
	}
	s := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "t"}))
	if s == nil || s.open == nil {
		t.Fatal("the proxy needs the opener to reach this CLI")
	}
	// The rest of the hello has to survive too: the proxy reads the header
	// names and the enabled flag off the session it matched.
	if s.From != "127.0.0.1:1" || s.Since.IsZero() || s.Incoming.Header != "X-Dev-User" || !s.Incoming.Enabled {
		t.Fatalf("session = %+v", s)
	}
}

func TestRegisterRejectsADuplicateUser(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	h := proto.Hello{Version: proto.Version, User: "shota", Token: "tok-1",
		Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
	if e := a.register(h, "10.0.0.1:5000", nopOpener{}); e != nil {
		t.Fatal(e)
	}
	second := h
	second.Token = "tok-2"
	e := a.register(second, "10.0.0.2:5000", nopOpener{})
	if e == nil || e.Code != proto.CodeDuplicateUser {
		t.Fatalf("want duplicate_user, got %+v", e)
	}
	// The refusal tells the developer where the other session is, and must
	// not leak either token while doing it.
	if e.From != "10.0.0.1:5000" || e.Since == "" {
		t.Fatalf("refusal = %+v", e)
	}
	if strings.Contains(fmt.Sprintf("%+v", e), "tok-") {
		t.Fatalf("the refusal must not carry a token: %+v", e)
	}
	// The first session is the one that is still attached, unchanged: a
	// rejected hello must not have replaced its token or its opener.
	if got := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok-1"})); got == nil {
		t.Fatal("the first session must survive a rejected duplicate")
	}
	if got := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok-2"})); got != nil {
		t.Fatal("a rejected hello's token must not become matchable")
	}
	// A different user attaches fine.
	other := h
	other.User, other.Token = "taro", "tok-taro"
	if e := a.register(other, "10.0.0.3:5000", nopOpener{}); e != nil {
		t.Fatal(e)
	}
	if len(a.Sessions()) != 2 {
		t.Fatalf("sessions = %+v", a.Sessions())
	}
}

// TestMatchWhileSessionsAttachAndDetach is the concurrency case: a request
// is routed on the proxy's goroutine while a session detaches on its own.
// Run under -race, which is what makes it an assertion at all.
func TestMatchWhileSessionsAttachAndDetach(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	const users = 8
	const rounds = 50
	var wg sync.WaitGroup
	for i := 0; i < users; i++ {
		user := fmt.Sprintf("dev%d", i)
		token := "tok-" + user
		wg.Add(2)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				h := proto.Hello{Version: proto.Version, User: user, Token: token,
					Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
				a.register(h, "127.0.0.1:1", nopOpener{})
				a.unregister(user)
			}
		}()
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				// Either this user's session is attached right now or it is
				// not; both answers are correct. What must never happen is
				// a torn read, a wrong user, or a match on a bad token.
				if s := a.Match(hdr(map[string]string{"X-Dev-User": user, "X-Dev-Token": token})); s != nil && s.User != user {
					t.Errorf("matched %q for a request naming %q", s.User, user)
				}
				if s := a.Match(hdr(map[string]string{"X-Dev-User": user, "X-Dev-Token": "wrong"})); s != nil {
					t.Errorf("a wrong token matched %q", s.User)
				}
				a.Sessions()
			}
		}()
	}
	wg.Wait()
}

// TestAttachOverTheWireStoresTheSessionAndNeverLogsTheToken drives a whole
// session over loopback: what handler.Hello stored is what the proxy will
// match against, and every line the agent logged is read back. `tetherd
// status` prints SessionInfo and the agent logs attach/detach; neither may
// carry the token a request would have to present.
func TestAttachOverTheWireStoresTheSessionAndNeverLogsTheToken(t *testing.T) {
	var mu sync.Mutex
	var logged strings.Builder
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged.WriteString(fmt.Sprintf(format, args...) + "\n")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{Env: "dev", TaskARN: "arn:test"}, logf)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Serve(ctx, ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	hello := proto.Hello{Version: proto.Version, User: "shota", Token: "s3cret-token",
		Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
	c, err := session.Dial(context.Background(), conn, hello, session.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The session the proxy would find, over a real hello.
	s := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "s3cret-token"}))
	if s == nil {
		t.Fatal("a session attached over the wire must be matchable")
	}
	// The Opener has to survive the handshake, not just register: this is
	// the stream the proxy opens back toward the CLI.
	if s.open == nil {
		t.Fatal("a session attached over the wire must carry its opener")
	}
	if s := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "guess"})); s != nil {
		t.Fatal("a guessed token must not match a session attached over the wire")
	}
	c.Close()

	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logged.String(), "s3cret-token") {
		t.Fatalf("the agent logged the token:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "shota") {
		t.Fatalf("the agent logged nothing about the session:\n%s", logged.String())
	}
}

// fmtSessions renders the view the way a log line or `status` would.
func fmtSessions(in []SessionInfo) string {
	var b strings.Builder
	for _, s := range in {
		b.WriteString(s.User + " " + s.From + " " + s.Since.String() + " ")
	}
	return b.String()
}
