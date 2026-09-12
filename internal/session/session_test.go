package session

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
)

type fakeHandler struct {
	welcome    proto.Welcome
	reject     *proto.Error
	closed     chan struct{}
	addrs      []string
	ttl        int
	resolveErr error
	resolved   string
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

func (f *fakeHandler) Resolve(ctx context.Context, name string) ([]string, int, error) {
	f.resolved = name
	if f.resolveErr != nil {
		return nil, 0, f.resolveErr
	}
	return f.addrs, f.ttl, nil
}

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

func TestResolveThroughSession(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), addrs: []string{"10.0.0.7"}, ttl: 30}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	addrs, ttl, err := c.Resolve(context.Background(), "api.myapp.internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0] != "10.0.0.7" || ttl != 30 {
		t.Fatalf("addrs=%v ttl=%d", addrs, ttl)
	}
	if h.resolved != "api.myapp.internal" {
		t.Fatalf("the agent saw %q", h.resolved)
	}
}

func TestResolveFailurePropagates(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), resolveErr: errors.New("NXDOMAIN")}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.Resolve(context.Background(), "nope.internal"); err == nil || !strings.Contains(err.Error(), "NXDOMAIN") {
		t.Fatalf("err = %v", err)
	}
}

// A failed resolve must not take the session down: the control loop and its
// dial path keep working afterwards.
func TestResolveFailureDoesNotBreakSession(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), resolveErr: errors.New("NXDOMAIN")}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, _, err := c.Resolve(context.Background(), "nope.internal"); err == nil {
		t.Fatal("want resolve error")
	}

	// The session (control stream + stream acceptor) must still be alive.
	conn, err := c.DialTCP(context.Background(), echoServer(t))
	if err != nil {
		t.Fatalf("session unusable after failed resolve: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("echo = %q, err = %v", buf, err)
	}

	select {
	case <-c.Done():
		t.Fatal("session ended after a failed resolve")
	default:
	}
}

// A resolve stream whose payload doesn't parse into ResolveHeader must still
// get an error reply, and serveStream must return (closing the stream)
// rather than hang.
func TestServeStreamResolveMalformedPayloadRepliesAndCloses(t *testing.T) {
	client, server := net.Pipe()
	h := &fakeHandler{closed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		serveStream(context.Background(), server, h)
		close(done)
	}()

	// "name" should be a string; this fails proto.Unmarshal into ResolveHeader.
	if _, err := client.Write([]byte(`{"type":"resolve","name":123,"qtype":"A"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, raw, err := proto.ReadHeader(client)
	if err != nil {
		t.Fatal(err)
	}
	if typ != proto.TypeResolve {
		t.Fatalf("reply type = %q", typ)
	}
	var reply proto.ResolveReply
	if err := proto.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.OK || reply.Error == "" {
		t.Fatalf("reply = %+v, want a failure with a message", reply)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveStream leaked: did not return after a malformed header")
	}
}

// slowResolveHandler blocks Resolve until its context is done, to exercise
// the server's per-stream timeout.
type slowResolveHandler struct{ fakeHandler }

func (h *slowResolveHandler) Resolve(ctx context.Context, name string) ([]string, int, error) {
	<-ctx.Done()
	return nil, 0, ctx.Err()
}

// A lookup that never returns on its own must still be cut off by
// serveStream's own timeout (not merely by a deadline the caller happens to
// set on ctx), get an error reply, and not leak the stream. ctx here is
// context.Background() — undeadlined — so the cutoff can only come from the
// 5s bound serveStream applies internally.
func TestServeStreamResolveTimeoutClosesStream(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the real 5s server-side resolve timeout")
	}
	client, server := net.Pipe()
	h := &slowResolveHandler{fakeHandler{closed: make(chan struct{})}}
	done := make(chan struct{})
	start := time.Now()
	go func() {
		serveStream(context.Background(), server, h)
		close(done)
	}()

	if err := proto.NewEncoder(client).Encode(proto.TypeResolve, proto.ResolveHeader{Name: "slow.internal", QType: "A"}); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, raw, err := proto.ReadHeader(client)
	if err != nil {
		t.Fatal(err)
	}
	if typ != proto.TypeResolve {
		t.Fatalf("reply type = %q", typ)
	}
	var reply proto.ResolveReply
	if err := proto.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.OK || reply.Error == "" {
		t.Fatalf("reply = %+v, want a failure with a message", reply)
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second {
		t.Fatalf("reply arrived after %s, before the 5s server timeout could have fired", elapsed)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveStream leaked: did not return after the lookup timed out")
	}
}
