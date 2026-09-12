package agent

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

type nopOpener struct{}

func (nopOpener) OpenStream() (net.Conn, error) { return nil, nil }

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
