package agent

import (
	"context"
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
	if err != nil || cfg.Env != "dev" || cfg.Control != "127.0.0.1:9900" {
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
