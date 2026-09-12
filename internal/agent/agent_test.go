package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{Env: "dev", TaskARN: "arn:test"}, nil) // nil: sessions log after the test ends
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Serve(ctx, ln)
	return ln.Addr().String()
}

func connect(t *testing.T, addr, user string) (*session.Client, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return session.Dial(context.Background(), conn, proto.Hello{Version: proto.Version, User: user, Token: "tok"}, session.Options{})
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
