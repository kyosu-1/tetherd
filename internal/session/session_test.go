package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
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

	mu   sync.Mutex
	open Opener
}

func (f *fakeHandler) Hello(h proto.Hello, remote string, open Opener) (proto.Welcome, *proto.Error) {
	f.mu.Lock()
	f.open = open
	f.mu.Unlock()
	if f.reject != nil {
		return proto.Welcome{}, f.reject
	}
	w := f.welcome
	w.Version = proto.Version
	return w, nil
}

// opener returns the Opener Serve handed to Hello, the way the agent's
// session registry will hold on to it (Task 3).
func (f *fakeHandler) opener() Opener {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
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

// TestResolveNotFoundThroughSession pins the two hops of the not_found
// plumbing that live in this package: serveStream must set
// ResolveReply.NotFound when the handler's error satisfies
// errors.Is(err, ErrNameNotFound), and Client.Resolve must in turn wrap
// ErrNameNotFound (via %w) into the error it returns whenever the reply
// says NotFound - not just echo the error text. dnsproxy answers NXDOMAIN
// based on errors.Is alone, so a text-only propagation would silently
// break it.
func TestResolveNotFoundThroughSession(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), resolveErr: fmt.Errorf("lookup nope.internal: %w", ErrNameNotFound)}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _, err = c.Resolve(context.Background(), "nope.internal")
	if !errors.Is(err, ErrNameNotFound) {
		t.Fatalf("err = %v, want it to satisfy errors.Is(err, ErrNameNotFound)", err)
	}
	// The sentinel goes in the chain, not into the text a second time: the
	// handler's own message already ends in "name not found", and wrapping
	// it with %w printed it twice ("... name not found: name not found"),
	// which is what `tetherd doctor` was showing an operator.
	if n := strings.Count(err.Error(), ErrNameNotFound.Error()); n != 1 {
		t.Errorf("%q says %q %d times, want once", err.Error(), ErrNameNotFound.Error(), n)
	}
	// And the handler's own wording survives, so the reason is not lost to
	// the tidy-up.
	if !strings.Contains(err.Error(), "lookup nope.internal") {
		t.Errorf("the handler's message must survive: %q", err.Error())
	}
}

// TestResolveOtherFailuresAreNotErrNameNotFound guards the other direction:
// an ordinary resolve error (no ErrNameNotFound anywhere in its chain) must
// not come back satisfying errors.Is(err, ErrNameNotFound) - otherwise
// every transient failure would read as NXDOMAIN instead of SERVFAIL.
func TestResolveOtherFailuresAreNotErrNameNotFound(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), resolveErr: errors.New("agent's resolv.conf is broken")}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _, err = c.Resolve(context.Background(), "nope.internal")
	if errors.Is(err, ErrNameNotFound) {
		t.Fatalf("an ordinary failure must not satisfy errors.Is(err, ErrNameNotFound): %v", err)
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

// The reverse direction: the agent opens the stream. Nothing before v0.3
// did, so the CLI had no accept loop at all. The stream must carry bytes
// both ways after the header, because a stolen HTTP request is a request
// and a response.
func TestAgentCanOpenAnHTTPStreamToTheCLI(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})

	got := make(chan string, 1)
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{
		OnHTTP: func(s net.Conn) {
			defer s.Close()
			b := make([]byte, 5)
			if _, err := io.ReadFull(s, b); err != nil {
				return
			}
			got <- string(b)
			s.Write([]byte("PONG!"))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// The handler received an Opener at hello time; use it the way the L7
	// proxy will.
	open := h.opener()
	if open == nil {
		t.Fatal("Hello was not given an Opener")
	}
	s, err := open.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("PING!")); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != "PING!" {
			t.Fatalf("the CLI read %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the CLI never saw the stream")
	}
	b := make([]byte, 5)
	if _, err := io.ReadFull(s, b); err != nil || string(b) != "PONG!" {
		t.Fatalf("agent read %q err %v: the stream must carry both directions", b, err)
	}
}

// `tetherd run --no-incoming` leaves OnHTTP nil. An http stream pushed at
// such a CLI must be answered with an error that says why, so the agent's
// proxy can return a real status instead of hanging on a stream nobody will
// ever read - and with CodeNoIncoming, not CodeBadHello, so the proxy can
// tell "expected, serve it from the application" from "the CLI did not know
// the stream type" without parsing the other side's wording.
func TestAnHTTPStreamIsRefusedWhenTheCLIDoesNotAcceptSteal(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s, err := h.opener().OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"}); err != nil {
		t.Fatal(err)
	}
	s.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, raw, err := proto.ReadHeader(s)
	if err != nil {
		t.Fatal(err)
	}
	if typ != proto.TypeError {
		t.Fatalf("type = %q, want an error reply", typ)
	}
	var e proto.Error
	if err := proto.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != proto.CodeNoIncoming {
		t.Errorf("code = %q, want %q: the proxy routes on the code, not on the wording", e.Code, proto.CodeNoIncoming)
	}
	if !strings.Contains(e.Message, "incoming") {
		t.Errorf("the refusal must say the CLI is not accepting incoming requests: %q", e.Message)
	}
}

// The other half of the additive-stream-types contract documented in
// package proto: the agent already answers TypeError for a stream type it
// does not know, and now the CLI must too - without taking the run down.
func TestAnUnknownInboundStreamTypeIsRefusedAndDoesNotKillTheSession(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), addrs: []string{"10.0.0.7"}, ttl: 30}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{
		OnHTTP: func(s net.Conn) { s.Close() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s, err := h.opener().OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.NewEncoder(s).Encode("mirror", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	s.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, raw, err := proto.ReadHeader(s)
	if err != nil {
		t.Fatal(err)
	}
	if typ != proto.TypeError {
		t.Fatalf("type = %q", typ)
	}
	var e proto.Error
	if err := proto.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != proto.CodeBadHello {
		t.Errorf("code = %q, want %q: an unrecognised stream type is a fault, not a --no-incoming refusal", e.Code, proto.CodeBadHello)
	}
	if !strings.Contains(e.Message, "mirror") {
		t.Errorf("the refusal must name the type it did not know: %q", e.Message)
	}
	s.Close()
	// The session must still work: a future agent opening a stream type this
	// CLI does not know must not take the run down.
	if _, _, err := c.Resolve(context.Background(), "api.myapp.internal"); err != nil {
		t.Fatalf("the session died after an unknown stream: %v", err)
	}
	select {
	case <-c.Done():
		t.Fatal("the session ended after an unknown inbound stream")
	default:
	}
}

// Each inbound stream gets its own goroutine: one slow stolen request (a
// laptop handler that takes a second, or a developer sitting in a debugger)
// must not stop the next request from reaching the laptop at all. Serving
// inbound streams inline in the accept loop passes every other test in this
// file and fails this one.
func TestASlowInboundStreamDoesNotBlockTheNextOne(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), addrs: []string{"10.0.0.7"}, ttl: 30}
	go Serve(context.Background(), sc, h, ServeOptions{})

	release := make(chan struct{})
	started := make(chan string, 2)
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{
		OnHTTP: func(s net.Conn) {
			defer s.Close()
			b := make([]byte, 1)
			if _, err := io.ReadFull(s, b); err != nil {
				return
			}
			started <- string(b)
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defer close(release)

	push := func(payload string) {
		t.Helper()
		s, err := h.opener().OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if err := proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}

	push("A")
	select {
	case v := <-started:
		if v != "A" {
			t.Fatalf("first inbound stream carried %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first inbound stream never reached OnHTTP")
	}

	push("B")
	select {
	case v := <-started:
		if v != "B" {
			t.Fatalf("second inbound stream carried %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a slow inbound stream blocked the next one: inbound streams must each get their own goroutine")
	}

	// And the session as a whole is still usable while one steal is stuck.
	if _, _, err := c.Resolve(context.Background(), "api.myapp.internal"); err != nil {
		t.Fatalf("the session was unusable while an inbound stream was in flight: %v", err)
	}
}

// muxOpener must not hand back a typed nil. `return m.mux.OpenStream()`
// compiles - *yamux.Stream is assignable to net.Conn - and on error yields a
// NON-nil net.Conn wrapping a nil *yamux.Stream, so the ordinary cleanup
// shape `if s != nil { s.Close() }` panics. That panic happens inside
// tetherd-agent, which has no recover, so it would kill the sidecar and drop
// every developer's session on that task - not just the one whose laptop
// went to sleep while an ALB request was in flight.
func TestMuxOpenerReturnsANilConnOnError(t *testing.T) {
	_, sc := pair(t)
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	mux, err := yamux.Server(sc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux.Close()

	var o Opener = muxOpener{mux: mux}
	s, err := o.OpenStream()
	if err == nil {
		t.Fatal("want an error opening a stream on a closed session")
	}
	if s != nil {
		t.Fatalf("OpenStream returned a non-nil net.Conn (%T) alongside err=%v: a typed nil here panics the agent on any `if s != nil { s.Close() }` cleanup", s, err)
	}
}

// assertStreamClosedPromptly is the agent's side of a refusal: having read
// the refusal, the next read must reach io.EOF at once. A refusal the CLI
// answers but does not close leaves Task 4's proxy blocked until its own
// deadline - it would see "i/o deadline reached" instead of EOF and turn an
// immediate, correct refusal into a hang.
func assertStreamClosedPromptly(t *testing.T, s net.Conn) {
	t.Helper()
	s.SetReadDeadline(time.Now().Add(2 * time.Second))
	var b [1]byte
	if _, err := s.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("after the refusal the agent read err=%v, want io.EOF: a refusal must close its stream", err)
	}
}

// Every branch of serveInbound that refuses a stream must also close it.
// Dropping the s.Close() from any one of them used to survive the whole
// package.
func TestEveryRefusedInboundStreamIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		onHTTP      func(net.Conn)
		send        func(t *testing.T, s net.Conn)
		wantRefusal bool
	}{
		{
			name: "http refused because this CLI takes no incoming requests",
			send: func(t *testing.T, s net.Conn) {
				if err := proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"}); err != nil {
					t.Fatal(err)
				}
			},
			wantRefusal: true,
		},
		{
			name:   "a stream type this CLI does not know",
			onHTTP: func(s net.Conn) { s.Close() },
			send: func(t *testing.T, s net.Conn) {
				if err := proto.NewEncoder(s).Encode("mirror", map[string]any{}); err != nil {
					t.Fatal(err)
				}
			},
			wantRefusal: true,
		},
		{
			// A header that does not parse: no reply is possible (there is no
			// stream type to answer on), but the stream must still be closed
			// rather than parked forever.
			name:   "a header that does not parse",
			onHTTP: func(s net.Conn) { s.Close() },
			send: func(t *testing.T, s net.Conn) {
				if _, err := s.Write([]byte("not json\n")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc, sc := pair(t)
			h := &fakeHandler{closed: make(chan struct{})}
			go Serve(context.Background(), sc, h, ServeOptions{})
			c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{OnHTTP: tc.onHTTP})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()

			s, err := h.opener().OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			tc.send(t, s)
			if tc.wantRefusal {
				s.SetReadDeadline(time.Now().Add(2 * time.Second))
				if typ, _, err := proto.ReadHeader(s); err != nil {
					t.Fatal(err)
				} else if typ != proto.TypeError {
					t.Fatalf("reply type = %q, want an error", typ)
				}
			}
			assertStreamClosedPromptly(t, s)
		})
	}
}

// An inbound stream whose header never arrives must not park a goroutine for
// the life of the session. Task 4 opens the stream when a request arrives and
// can then fail before writing the header - a marshal error, a cancelled
// context, a client that hung up - and over a day's `tetherd run` those
// accumulate with nothing capping the count. The timeout is exercised at
// 50ms here rather than waiting out the production default (10s, set in Dial
// when Options.InboundHeaderTimeout is left zero), the same way
// ServeOptions.ResolveTimeout is exercised on the agent side.
func TestAnInboundStreamWhoseHeaderNeverArrivesIsClosed(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), addrs: []string{"10.0.0.7"}, ttl: 30}
	go Serve(context.Background(), sc, h, ServeOptions{})
	const headerTimeout = 50 * time.Millisecond
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{
		OnHTTP:               func(s net.Conn) { s.Close() },
		InboundHeaderTimeout: headerTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	start := time.Now()
	s, err := h.opener().OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Deliberately write nothing at all.
	assertStreamClosedPromptly(t, s)
	if elapsed := time.Since(start); elapsed < headerTimeout {
		t.Fatalf("the stream was closed after %s, before the %s header timeout could have fired", elapsed, headerTimeout)
	}

	// Closing a parked stream must not cost the session.
	if _, _, err := c.Resolve(context.Background(), "api.myapp.internal"); err != nil {
		t.Fatalf("the session died when an inbound header timed out: %v", err)
	}
}

// The header read must stay bounded only until the handler takes over: once
// OnHTTP owns the stream, a long-lived stolen request (a websocket, an SSE
// stream, a slow endpoint) must not be cut off by the header deadline.
func TestTheHeaderTimeoutDoesNotApplyOnceOnHTTPOwnsTheStream(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})
	const headerTimeout = 50 * time.Millisecond
	echoed := make(chan error, 1)
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{
		InboundHeaderTimeout: headerTimeout,
		OnHTTP: func(s net.Conn) {
			defer s.Close()
			// Read well after the header deadline would have fired.
			b := make([]byte, 4)
			_, err := io.ReadFull(s, b)
			echoed <- err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s, err := h.opener().OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * headerTimeout)
	if _, err := s.Write([]byte("late")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-echoed:
		if err != nil {
			t.Fatalf("OnHTTP's read failed %v: the header deadline must be cleared before the handler takes over", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnHTTP never completed its read")
	}
}

// `tetherd run` is not just the session: it is the DNS proxy, the packet
// capture, the credential endpoint and the developer's wrapped child
// process. A panic in the steal handler - over a stream whose bytes a remote
// party shaped, parsed by http.ReadRequest in Task 5 - must not take all of
// that down. net/http.Server recovers per connection for the same reason,
// and since the session layer owns this goroutine it owns the recover.
func TestAPanickingOnHTTPDoesNotTakeDownTheSession(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), addrs: []string{"10.0.0.7"}, ttl: 30}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{
		OnHTTP: func(s net.Conn) { panic("handler blew up on a remote-shaped request") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s, err := h.opener().OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"}); err != nil {
		t.Fatal(err)
	}
	// The stream is closed rather than left parked, so the agent's proxy
	// learns the request is not coming back.
	assertStreamClosedPromptly(t, s)

	// And the rest of the session is untouched: the next steal, a resolve
	// and the control stream all still work.
	if _, _, err := c.Resolve(context.Background(), "api.myapp.internal"); err != nil {
		t.Fatalf("the session died after the handler panicked: %v", err)
	}
	select {
	case <-c.Done():
		t.Fatal("the session ended after the handler panicked")
	default:
	}
}

// oldCLIDial emulates a pre-v0.3a CLI: a real yamux client that completes
// hello/welcome and answers pings, but has no accept loop, because no
// released CLI before this milestone had one. It is the measured basis for
// what CodeNoIncoming's doc comment may claim about older peers.
func oldCLIDial(t *testing.T, conn net.Conn) *yamux.Session {
	t.Helper()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	mux, err := yamux.Client(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mux.Close() })
	control, err := mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	enc := proto.NewEncoder(control)
	if err := enc.Encode(proto.TypeHello, proto.Hello{Version: proto.Version, User: "shota"}); err != nil {
		t.Fatal(err)
	}
	dec := proto.NewDecoder(control)
	typ, _, err := dec.Decode()
	if err != nil || typ != proto.TypeWelcome {
		t.Fatalf("old CLI: welcome: typ=%q err=%v", typ, err)
	}
	go func() {
		for {
			typ, _, err := dec.Decode()
			if err != nil {
				return
			}
			if typ == proto.TypePing {
				enc.Encode(proto.TypePong, nil)
			}
		}
	}()
	return mux
}

// A CLI that predates the accept loop answers an http stream with NOTHING -
// not CodeBadHello, not any other code. The agent can open the stream and
// write the header successfully; the reply simply never comes. This is the
// fact CodeNoIncoming's doc comment rests on, and it is why the opener must
// bound its own read of the reply and why Hello.Incoming.Enabled (false by
// zero value on such a CLI) is the signal that decides whether to steal at
// all. Believing the older peer replies would make Task 4's proxy block
// until the ALB's 504 on a request the application should have served.
func TestAPreAcceptLoopCLIAnswersAnHTTPStreamWithNothing(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})
	oldCLIDial(t, cc)

	s, err := h.opener().OpenStream()
	if err != nil {
		t.Fatalf("opening the stream toward an older CLI must succeed: %v", err)
	}
	defer s.Close()
	if err := proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"}); err != nil {
		t.Fatalf("writing the header toward an older CLI must succeed: %v", err)
	}
	s.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	typ, _, err := proto.ReadHeader(s)
	if err == nil {
		t.Fatalf("an older CLI answered %q; the doc comment and Task 4 assume it answers nothing at all", typ)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want a timeout: an older CLI does not close the stream either, it simply never reads it", err)
	}
}
