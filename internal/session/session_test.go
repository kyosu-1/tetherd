package session

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/kyosu-1/tetherd/internal/proto"
)

type fakeHandler struct {
	welcome    proto.Welcome
	reject     *proto.Error
	closed     chan struct{}
	addrs      []string
	ttl        int
	resolveErr error
	// resolveErrOnce, when set, makes resolveErr fire on the first Resolve
	// call only; subsequent calls fall through to addrs/ttl.
	resolveErrOnce bool
	resolved       string
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
		err := f.resolveErr
		if f.resolveErrOnce {
			f.resolveErr = nil
		}
		return nil, 0, err
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

// oldAgentServe emulates a pre-v0.2 agent: it completes hello/welcome like
// the real Serve, but knows nothing about the resolve stream type, so every
// stream it's handed gets the same "unknown stream type" TypeError the real
// server's serveStream default case sends. This is the actual wire shape a
// deployed v0.1 agent produces for a resolve request — proto.Version is
// still "1", so it accepts the hello.
func oldAgentServe(t *testing.T, conn net.Conn) {
	t.Helper()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	mux, err := yamux.Server(conn, cfg)
	if err != nil {
		t.Error(err)
		return
	}
	defer mux.Close()
	control, err := mux.AcceptStream()
	if err != nil {
		t.Error(err)
		return
	}
	dec := proto.NewDecoder(control)
	enc := proto.NewEncoder(control)
	typ, _, err := dec.Decode()
	if err != nil || typ != proto.TypeHello {
		t.Errorf("old agent: hello: typ=%q err=%v", typ, err)
		return
	}
	if err := enc.Encode(proto.TypeWelcome, proto.Welcome{Version: proto.Version}); err != nil {
		t.Error(err)
		return
	}
	go func() {
		for {
			s, err := mux.AcceptStream()
			if err != nil {
				return
			}
			go func(s net.Conn) {
				defer s.Close()
				typ, _, err := proto.ReadHeader(s)
				if err != nil {
					return
				}
				proto.NewEncoder(s).Encode(proto.TypeError, proto.Error{Code: proto.CodeBadHello, Message: "unknown stream type " + typ})
			}(s)
		}
	}()
	for {
		typ, _, err := dec.Decode()
		if err != nil {
			return
		}
		switch typ {
		case proto.TypePing:
			enc.Encode(proto.TypePong, nil)
		case proto.TypeBye:
			return
		}
	}
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

// Resolving against a pre-v0.2 agent must produce a real, actionable
// message, not the blank "resolve name via agent: " you get from
// unmarshalling a TypeError reply into ResolveReply and printing its
// (zero-value) Error field. Version skew is the single most likely failure
// mode for this feature, and its consumer is `tetherd doctor`.
func TestResolveAgainstOlderAgentIsAFriendlyError(t *testing.T) {
	cc, sc := pair(t)
	go oldAgentServe(t, sc)
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _, err = c.Resolve(context.Background(), "api.myapp.internal")
	if err == nil {
		t.Fatal("want an error resolving against an agent that doesn't support it")
	}
	msg := err.Error()
	if strings.HasSuffix(strings.TrimSpace(msg), ":") {
		t.Fatalf("err must not end in a bare colon: %q", msg)
	}
	if !strings.Contains(msg, "upgrade the sidecar") {
		t.Fatalf("err = %q, want a message telling the operator to upgrade the sidecar", msg)
	}
}

// A ResolveReply with OK:false and no Error text (the zero value) must still
// produce a real message, never "resolve name via agent: " with nothing
// after the colon.
func TestResolveFailureWithNoMessageGetsAFallback(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), resolveErr: errors.New("")}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _, err = c.Resolve(context.Background(), "nope.internal")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if strings.HasSuffix(strings.TrimSpace(msg), ":") {
		t.Fatalf("err must not end in a bare colon: %q", msg)
	}
}

// A failed resolve must not take the session down: a subsequent, successful
// resolve on the same session must still get its answer, and dial must
// still work. (A weaker version of this test — one that only checked for
// *some* error after the failed resolve — would also pass if the server
// silently dropped the stream instead of replying, or if the whole resolve
// case were deleted, since the client still errors on the next call in both
// of those scenarios; the follow-up success assertions are what catch that.)
func TestResolveFailureDoesNotBreakSession(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{
		closed:         make(chan struct{}),
		resolveErr:     errors.New("NXDOMAIN"),
		resolveErrOnce: true,
		addrs:          []string{"10.0.0.7"},
		ttl:            30,
	}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, _, err := c.Resolve(context.Background(), "nope.internal"); err == nil {
		t.Fatal("want resolve error")
	}

	// A later resolve on the same session must succeed and get real answers.
	addrs, ttl, err := c.Resolve(context.Background(), "api.myapp.internal")
	if err != nil {
		t.Fatalf("resolve after a prior failure: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != "10.0.0.7" || ttl != 30 {
		t.Fatalf("addrs=%v ttl=%d", addrs, ttl)
	}

	// Dial on the same session must still work too.
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

// A resolve stream asking for anything but an A record must be rejected
// before the handler is ever invoked (v1 is A-only), and the rejection must
// name the query type that was asked for.
func TestServeStreamResolveRejectsNonAQueries(t *testing.T) {
	client, server := net.Pipe()
	h := &fakeHandler{closed: make(chan struct{}), addrs: []string{"10.0.0.7"}, ttl: 30}
	done := make(chan struct{})
	go func() {
		serveStream(context.Background(), server, h, 5*time.Second)
		close(done)
	}()

	if err := proto.NewEncoder(client).Encode(proto.TypeResolve, proto.ResolveHeader{Name: "api.myapp.internal", QType: "AAAA"}); err != nil {
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
	if reply.OK || !strings.Contains(reply.Error, "AAAA") {
		t.Fatalf("reply = %+v, want a failure naming AAAA", reply)
	}
	if h.resolved != "" {
		t.Fatalf("handler.Resolve must not be called for an unsupported query type, but it saw %q", h.resolved)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveStream leaked: did not return after rejecting the query type")
	}
}

// A caller that cancels its context mid-flight (Ctrl-C, an errgroup sibling
// failing) must get its Resolve call back promptly, not block for the
// client's 10s default read deadline.
func TestResolveContextCancellationReturnsPromptly(t *testing.T) {
	cc, sc := pair(t)
	h := &slowResolveHandler{fakeHandler{closed: make(chan struct{})}}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, _, err = c.Resolve(ctx, "slow.internal")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want an error when the caller cancels")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Resolve took %s to notice cancellation, want well under the 10s default deadline", elapsed)
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
		serveStream(context.Background(), server, h, 5*time.Second)
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
// resolveTimeout serveStream is given, exercised here at 50ms so the test
// stays fast instead of waiting out the real production default (5s, set
// in Serve when ServeOptions.ResolveTimeout is left zero).
func TestServeStreamResolveTimeoutClosesStream(t *testing.T) {
	client, server := net.Pipe()
	h := &slowResolveHandler{fakeHandler{closed: make(chan struct{})}}
	const resolveTimeout = 50 * time.Millisecond
	done := make(chan struct{})
	start := time.Now()
	go func() {
		serveStream(context.Background(), server, h, resolveTimeout)
		close(done)
	}()

	if err := proto.NewEncoder(client).Encode(proto.TypeResolve, proto.ResolveHeader{Name: "slow.internal", QType: "A"}); err != nil {
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
	if elapsed := time.Since(start); elapsed < resolveTimeout {
		t.Fatalf("reply arrived after %s, before the %s server timeout could have fired", elapsed, resolveTimeout)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveStream leaked: did not return after the lookup timed out")
	}
}
