package session

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
)

type fakeHandler struct {
	welcome proto.Welcome
	reject  *proto.Error
	closed  chan struct{}
}

func (f *fakeHandler) Hello(h proto.Hello, remote string) (proto.Welcome, *proto.Error) {
	if f.reject != nil {
		return proto.Welcome{}, f.reject
	}
	w := f.welcome
	w.Version = proto.Version
	return w, nil
}

func (f *fakeHandler) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

func (f *fakeHandler) Closed() { close(f.closed) }

// pair returns a connected TCP pair on loopback.
func pair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Error(err)
			return
		}
		done <- c
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c, <-done
}

// echoServer returns the address of a TCP server that echoes bytes.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func TestHelloWelcome(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{welcome: proto.Welcome{Env: "dev", TaskARN: "arn:task"}, closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})

	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if w := c.Welcome(); w.Env != "dev" || w.TaskARN != "arn:task" {
		t.Fatalf("welcome = %+v", w)
	}
	c.Close()
	select {
	case <-h.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("handler.Closed not called after bye")
	}
}

func TestRejected(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{reject: &proto.Error{Code: proto.CodeDuplicateUser, Message: "already attached"}, closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})

	_, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Err.Code != proto.CodeDuplicateUser {
		t.Fatalf("want RejectedError(duplicate_user), got %v", err)
	}
}

func TestDialTCPThroughSession(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	conn, err := c.DialTCP(context.Background(), echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo = %q, err = %v", buf, err)
	}
}

func TestDialTCPRefused(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// port 1 on loopback is closed
	if _, err := c.DialTCP(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatal("want error dialing closed port")
	}
}

func TestClientDetectsDeadPeer(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go Serve(ctx, sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{PingInterval: 50 * time.Millisecond, MaxMissed: 2})
	if err != nil {
		t.Fatal(err)
	}
	cancel()   // server stops answering
	sc.Close() // and the transport dies
	select {
	case <-c.Done():
		if c.Err() == nil {
			t.Fatal("Err must be set after Done")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not notice dead peer")
	}
}
