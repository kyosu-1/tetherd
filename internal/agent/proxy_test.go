package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

// appServer stands in for the application container on :8081. It counts
// hits so that a test can assert the application was *not* asked, which is
// the half of "the laptop's error is its own answer" that a status code
// alone does not pin.
type appServer struct {
	*httptest.Server
	hits atomic.Int64
}

func appStub(t *testing.T, body string) *appServer {
	t.Helper()
	a := &appServer{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s path=%s body=%q len=%d upgrade=%q xff=%q host=%s",
			body, r.URL.Path, string(b), len(b), r.Header.Get("Upgrade"), r.Header.Get("X-Forwarded-For"), r.Host)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *appServer) addr() string { return a.Listener.Addr().String() }

// laptop stands in for an attached CLI: OpenStream hands back one end of a
// pipe, and the other end is spoken HTTP/1.1 on the way the CLI does.
//
// It is a stand-in, so it can only prove the proxy agrees with this file's
// idea of the CLI. TestProxyStealsThroughTheCLIsOwnSessionCode and
// TestRunServesBothTheControlPortAndTheALBPort drive the same proxy against
// internal/session's own client instead, which is what keeps this
// stand-in's shape honest.
type laptop struct {
	mu      sync.Mutex
	streams []net.Conn      // the CLI's ends, for a test that reads raw bytes
	eofs    []chan struct{} // closed when the agent closes its end
	opens   int

	// Exactly one of these describes what the CLI does with a stream:
	// handler serves HTTP on it, refuse answers with a stream-level
	// refusal, raw answers with exactly these bytes, partial answers with
	// these bytes and then stops reading the request, silent reads
	// everything and never answers (a CLI with no accept loop), and all of
	// them zero leaves the stream untouched for the test to drive.
	handler http.Handler
	refuse  *proto.Error
	raw     string
	partial string
	silent  bool

	fail bool // OpenStream fails: the laptop is gone
}

func (l *laptop) OpenStream() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail {
		// As session.Opener requires: a nil net.Conn with the error.
		return nil, fmt.Errorf("the laptop is gone")
	}
	l.opens++
	mine, theirs := net.Pipe()
	eof := make(chan struct{})
	cli := &eofConn{Conn: mine, eof: eof}
	l.streams = append(l.streams, cli)
	l.eofs = append(l.eofs, eof)
	handler, refuse, silent := l.handler, l.refuse, l.silent
	raw, partial := l.raw, l.partial
	switch {
	case silent:
		// A CLI that reads the request and never answers it. Measured
		// behaviour for a CLI older than the accept loop, and the reason
		// the proxy has to bound its own read of the reply.
		go io.Copy(io.Discard, cli)
	case raw != "":
		go func() {
			if typ, _, err := proto.ReadHeader(cli); err != nil || typ != proto.TypeHTTP {
				cli.Close()
				return
			}
			// Drain on the side: net.Pipe is unbuffered, so a proxy still
			// writing the request would block against this write.
			go io.Copy(io.Discard, cli)
			cli.Write([]byte(raw))
		}()
	case partial != "":
		// A CLI that starts a reply and then stops reading. The proxy's
		// request write has nowhere to go and no deadline of its own, so
		// this is the input that parks a proxy which joins that write
		// without aborting it first.
		go func() {
			if typ, _, err := proto.ReadHeader(cli); err != nil || typ != proto.TypeHTTP {
				cli.Close()
				return
			}
			cli.Write([]byte(partial))
		}()
	case handler != nil || refuse != nil:
		go func() {
			// The CLI reads the stream header first and only then hands
			// the stream to its HTTP server. Reading it here is what makes
			// this stand-in disagree with a proxy that writes the request
			// first.
			typ, raw, err := proto.ReadHeader(cli)
			if err != nil || typ != proto.TypeHTTP {
				cli.Close()
				return
			}
			var h proto.HTTPHeader
			if err := proto.Unmarshal(raw, &h); err != nil {
				cli.Close()
				return
			}
			if refuse != nil {
				proto.NewEncoder(cli).Encode(proto.TypeError, *refuse)
				cli.Close()
				return
			}
			srv := &http.Server{Handler: handler}
			srv.Serve(&oneConnListener{c: cli})
		}()
	}
	return theirs, nil
}

func (l *laptop) opened() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opens
}

// eofConn reports when the other end goes away, so a test can tell whether
// the proxy closed the stream it opened.
//
// Only a real end of stream counts. Signalling on any read error at all
// made the close assertion pass without the proxy closing anything:
// net/http's own connReader aborts its background read by setting the read
// deadline to the past (abortPendingRead) as soon as a handler returns, so
// every served request produces an "i/o timeout" here whether the stream
// was closed or not. net.Pipe reports a closed peer as io.EOF and a closed
// local end as io.ErrClosedPipe, and nothing else does.
type eofConn struct {
	net.Conn
	once sync.Once
	eof  chan struct{}
}

func (c *eofConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		c.once.Do(func() { close(c.eof) })
	}
	return n, err
}

// blockingBody yields n bytes and then blocks until hold is closed: a
// client that stopped sending in the middle of an upload. It is what makes
// the request-writing goroutine block on a *read* it owns nothing of, which
// no deadline on the stream can abort.
type blockingBody struct {
	left int
	hold chan struct{}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if b.left > 0 {
		n := len(p)
		if n > b.left {
			n = b.left
		}
		for i := 0; i < n; i++ {
			p[i] = 'x'
		}
		b.left -= n
		return n, nil
	}
	<-b.hold
	return 0, io.EOF
}

func (b *blockingBody) Close() error { return nil }

type oneConnListener struct {
	c    net.Conn
	done bool
}

// Pointer receiver, and done really is set: a value receiver would hand the
// same connection back on every Accept and http.Server would serve it in a
// loop.
func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.done {
		return nil, io.EOF
	}
	l.done = true
	return l.c, nil
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

// logLines collects the agent's log output and stops at the end of the
// test: sessions log from goroutines that outlive the test function, and
// t.Logf after that panics.
type logLines struct {
	t     *testing.T
	mu    sync.Mutex
	done  bool
	lines []string
}

func newLog(t *testing.T) *logLines {
	l := &logLines{t: t}
	t.Cleanup(func() {
		l.mu.Lock()
		l.done = true
		l.mu.Unlock()
	})
	return l
}

func (l *logLines) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
	if !l.done {
		l.t.Log(line)
	}
}

func (l *logLines) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// albClient is what stands in for the ALB when a test drives a real
// listener. It has a timeout because these tests are about a proxy on the
// availability path: a request that never completes is the failure being
// tested for, and http.DefaultClient would report it as a whole-package
// timeout with a goroutine dump instead of as a failed assertion.
func albClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(what)
}

func newProxy(t *testing.T, app string, sessions ...*Session) (*Proxy, *logLines) {
	t.Helper()
	a := New(Config{Env: "dev"}, nil)
	for _, s := range sessions {
		a.mu.Lock()
		a.sessions[s.User] = s
		a.mu.Unlock()
	}
	lg := newLog(t)
	// Two seconds rather than DefaultStealTimeout: a test that waits 30s
	// for a laptop that is never going to answer is a test nobody runs.
	return &Proxy{Agent: a, AppAddr: app, Logf: lg.logf, StealTimeout: 2 * time.Second}, lg
}

func proxyFor(t *testing.T, app string, sessions ...*Session) http.Handler {
	t.Helper()
	p, _ := newProxy(t, app, sessions...)
	return p.Handler()
}

func do(t *testing.T, h http.Handler, method, path string, body string, hdrs map[string]string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdrs {
		r.Header.Set(k, v)
	}
	return serve(t, h, r)
}

func serve(t *testing.T, h http.Handler, r *http.Request) *http.Response {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	resp.Body.Close()
	return string(b)
}

func stealHeaders() map[string]string {
	return map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok"}
}

func TestProxyPassesEverythingToTheAppWithNoSession(t *testing.T) {
	app := appStub(t, "APP")
	h := proxyFor(t, app.addr())

	// The ALB health check, which after the Terraform change reaches the
	// application only through here: nobody is attached, and it still has
	// to be a 200 from the app or ECS replaces the task.
	resp := do(t, h, "GET", "/", "", nil)
	b := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, "APP path=/") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}

	// And an ordinary request from someone who is not attached, with the
	// forwarding headers the ALB writes.
	resp = do(t, h, "POST", "/api/orders", "hello", map[string]string{
		"X-Forwarded-For": "203.0.113.5", "X-Forwarded-Proto": "https",
	})
	b = readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, `APP path=/api/orders body="hello"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	// httputil.ReverseProxy deletes the X-Forwarded-* headers whenever
	// Rewrite is set. tetherd is not the hop that owns them: the
	// application has to see what the ALB sent (spec §5.1).
	if !strings.Contains(b, `xff="203.0.113.5"`) {
		t.Errorf("X-Forwarded-For must reach the application unchanged: %s", b)
	}
}

// TestProxyAnswers502WhenTheApplicationIsDown is the pass-through's own
// failure mode, and the one that decides whether ECS notices. Once the ALB
// target group points at :8080 the health check reaches the application
// only through here, so a proxy that answered an unreachable application
// with 200 and an empty body would keep a task with a dead app in service.
func TestProxyAnswers502WhenTheApplicationIsDown(t *testing.T) {
	// A port on loopback that nothing is listening on: the app container is
	// still starting, which is an ordinary moment in an ECS task's life.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := ln.Addr().String()
	ln.Close()

	p, lg := newProxy(t, down)
	resp := do(t, p.Handler(), "GET", "/", "", nil)
	b := readAll(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: an unreachable application must not read as healthy (body=%q)", resp.StatusCode, b)
	}
	if !strings.Contains(lg.all(), "GET /") {
		t.Errorf("the agent's own log must say the application could not be reached: %s", lg.all())
	}
}

func TestProxyStealsAMatchingRequestAndPassesTheRest(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "LAPTOP path=%s body=%q xff=%q host=%s", r.URL.Path, string(b), r.Header.Get("X-Forwarded-For"), r.Host)
	})}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)

	match := map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok", "X-Forwarded-For": "203.0.113.5"}
	resp := do(t, h, "POST", "/api/orders", "hello", match)
	b := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, `LAPTOP path=/api/orders body="hello"`) {
		t.Fatalf("a matching request must reach the laptop: status=%d body=%s", resp.StatusCode, b)
	}
	// X-Forwarded-* and tracing headers pass through untouched (spec §5.1):
	// the developer debugging a stolen request is looking at the real
	// client's address.
	if !strings.Contains(b, `xff="203.0.113.5"`) {
		t.Errorf("X-Forwarded-For must survive: %s", b)
	}
	// And the Host the ALB sent, not the proxy's placeholder authority.
	if !strings.Contains(b, "host=example.com") {
		t.Errorf("the laptop must see the inbound Host: %s", b)
	}
	if app.hits.Load() != 0 {
		t.Errorf("the application was asked %d times about a stolen request", app.hits.Load())
	}

	// A health check on the same proxy still goes to the app.
	resp = do(t, h, "GET", "/", "", nil)
	b = readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, "APP path=/") {
		t.Fatalf("health checks must not be stolen: status=%d body=%s", resp.StatusCode, b)
	}
	// So does the same path with the wrong token.
	resp = do(t, h, "GET", "/api/orders", "", map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "nope"})
	b = readAll(t, resp)
	if !strings.Contains(b, "APP path=/api/orders") {
		t.Fatalf("a wrong token must fall through to the app: %s", b)
	}
	// Neither of those may even have opened a stream toward the laptop.
	if got := l.opened(); got != 1 {
		t.Errorf("streams opened = %d, want 1: only the matching request may reach the laptop", got)
	}
}

// TestProxyMatchesALowercaseConfiguredHeaderName drives the proxy with a
// request read off the wire against a session whose configured header names
// are lowercase, which internal/config accepts verbatim. A proxy that
// indexed r.Header itself would pass every other test in this file - they
// all set headers through Header.Set, which canonicalises - and steal
// nothing on a real ALB.
func TestProxyMatchesALowercaseConfiguredHeaderName(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "LAPTOP path=%s", r.URL.Path)
	})}
	s := sess("shota", "tok", true)
	s.Incoming.Header, s.Incoming.TokenHeader = "x-dev-user", "x-dev-token"
	s.open = l
	h := proxyFor(t, app.addr(), s)

	wire := parseRequest(t, "GET /api/orders HTTP/1.1\r\nHost: dev.example.com\r\nX-Dev-User: shota\r\nX-Dev-Token: tok\r\n\r\n")
	b := readAll(t, serve(t, h, wire))
	if !strings.Contains(b, "LAPTOP path=/api/orders") {
		t.Fatalf("a lowercase configured header name must still match what the wire canonicalises to: %s", b)
	}
}

func TestProxyNeverStealsAnUpgrade(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an upgrade must never reach the laptop")
	})}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)
	resp := do(t, h, "GET", "/ws", "", map[string]string{
		"X-Dev-User": "shota", "X-Dev-Token": "tok", "Upgrade": "websocket", "Connection": "Upgrade",
	})
	b := readAll(t, resp)
	if !strings.Contains(b, `APP path=/ws`) || !strings.Contains(b, `upgrade="websocket"`) {
		t.Fatalf("a websocket upgrade must go to the app untouched: %s", b)
	}
	// Not even a stream may be opened: there is no way to carry an upgrade
	// through one request/response over a yamux stream.
	if got := l.opened(); got != 0 {
		t.Errorf("streams opened = %d, want 0 for an upgrade", got)
	}
}

func TestProxyFallsBackToTheAppWhenTheLaptopIsGone(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{fail: true}
	s := sess("shota", "tok", true)
	s.open = l
	h, lg := newProxy(t, app.addr(), s)
	resp := do(t, h.Handler(), "POST", "/api/orders", "important", stealHeaders())
	b := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("a vanished laptop must not fail the request: status=%d body=%s", resp.StatusCode, b)
	}
	// The body must arrive intact: it was already read off the wire before
	// the dial failed, so it has to be replayed.
	if !strings.Contains(b, `APP path=/api/orders body="important"`) {
		t.Fatalf("the body must be replayed to the app: %s", b)
	}
	// The fallback is silent to the client but must not be silent in the
	// log, and must name the user so it can be acted on.
	if !strings.Contains(lg.all(), `"shota"`) {
		t.Errorf("the fallback must say whose session it was: %s", lg.all())
	}
	// And why. session.Opener promises a nil net.Conn alongside the error,
	// so a nil check alone would route this request identically and lose
	// the only account of what went wrong; the error has to be looked at.
	if !strings.Contains(lg.all(), "the laptop is gone") {
		t.Errorf("the fallback must carry the Opener's own error: %s", lg.all())
	}
}

// TestProxyFallsBackWhenAnOpenerBreaksItsContract covers the guard behind
// the guard. session.Opener promises a nil net.Conn on error, which is what
// makes `c == nil` reliable (it was a typed-nil non-nil interface until
// b663526); an Opener that answered nil with no error at all would be a nil
// dereference on the ALB's data path, so the proxy treats it as a laptop
// that is not there.
func TestProxyFallsBackWhenAnOpenerBreaksItsContract(t *testing.T) {
	app := appStub(t, "APP")
	s := sess("shota", "tok", true)
	s.open = nilOpener{}
	p, lg := newProxy(t, app.addr(), s)
	resp := do(t, p.Handler(), "POST", "/api/orders", "important", stealHeaders())
	b := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, `APP path=/api/orders body="important"`) {
		t.Fatalf("an Opener that returns nothing must not panic or fail the request: status=%d body=%s", resp.StatusCode, b)
	}
	if !strings.Contains(lg.all(), "shota") {
		t.Errorf("the log must name the session: %s", lg.all())
	}
}

// nilOpener returns nil for both results, which session.Opener forbids.
type nilOpener struct{}

func (nilOpener) OpenStream() (net.Conn, error) { return nil, nil }

// TestProxyKeepsABodyItCanStillReplay pins the other side of the cap: a
// body of exactly MaxReplayBody is buffered and replayed, so the limit is
// the documented one and not, say, a few kilobytes.
func TestProxyKeepsABodyItCanStillReplay(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{fail: true}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)
	resp := do(t, h, "POST", "/upload", strings.Repeat("x", MaxReplayBody), stealHeaders())
	b := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, fmt.Sprintf("len=%d", MaxReplayBody)) {
		t.Fatalf("a body of exactly MaxReplayBody must still be replayable: status=%d", resp.StatusCode)
	}
}

func TestProxyRefusesToFallBackWithAnUnreplayableBody(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{fail: true}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)
	big := strings.Repeat("x", MaxReplayBody+1)
	resp := do(t, h, "POST", "/upload", big, stealHeaders())
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: a body too large to buffer cannot be replayed", resp.StatusCode)
	}
	// Not a truncated request either: the application must not see a
	// request whose body the proxy could only partly reproduce.
	if n := app.hits.Load(); n != 0 {
		t.Errorf("the application was asked %d times with a body that cannot be replayed", n)
	}
}

func TestProxyServesConcurrentRequestsOnOneSession(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, "LAPTOP %s", r.URL.Path)
	})}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	start := time.Now()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := do(t, h, "GET", fmt.Sprintf("/p%d", i), "", stealHeaders())
			b := readAll(t, resp)
			want := fmt.Sprintf("LAPTOP /p%d", i)
			if !strings.Contains(b, want) {
				errs <- fmt.Sprintf("got %q, want %q", b, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	// One stream per request, served concurrently: eight 50ms handlers must
	// not take 400ms.
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Errorf("requests were serialised: %s", el)
	}
	if got := l.opened(); got != 8 {
		t.Errorf("streams opened = %d, want one per request", got)
	}
}

// TestProxyWritesTheStreamHeaderBeforeTheRequest reads the raw bytes the
// proxy puts on the stream. The CLI reads {"type":"http"} first and only
// then hands the stream to its HTTP server, so a proxy that wrote the
// request first would have the CLI read a request line as a stream header
// and close.
func TestProxyWritesTheStreamHeaderBeforeTheRequest(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{} // raw: nothing on the CLI side reads but this test
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)

	replied := make(chan string, 1)
	go func() {
		resp := do(t, h, "POST", "/x", "payload", stealHeaders())
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		replied <- fmt.Sprintf("%d %s", resp.StatusCode, b)
	}()
	waitFor(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.streams) > 0
	}, "the proxy never opened a stream")
	l.mu.Lock()
	c := l.streams[0]
	l.mu.Unlock()

	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, `"type":"http"`) {
		t.Fatalf("first line = %q, want the http stream header", line)
	}
	req, err := http.ReadRequest(br)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Path != "/x" {
		t.Fatalf("path = %q", req.URL.Path)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil || string(body) != "payload" {
		t.Fatalf("body = %q, err = %v", body, err)
	}
	// And what the CLI writes back is what the ALB gets.
	if _, err := c.Write([]byte("HTTP/1.1 201 Created\r\nContent-Length: 2\r\n\r\nyo")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-replied:
		if got != "201 yo" {
			t.Fatalf("the ALB saw %q, want %q", got, "201 yo")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the proxy never returned the laptop's reply")
	}
}

// TestProxyStealsARequestThatExpectsAContinue covers a header curl sets by
// itself for any body over 1KB. The proxy reads the reply off the stream
// directly, so a "100 Continue" the CLI's own http.Server emits would be
// read as the reply - and the developer would see a 100 with no body. The
// body is buffered by then, so there is nothing left to negotiate and the
// header does not travel.
func TestProxyStealsARequestThatExpectsAContinue(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "LAPTOP body=%q expect=%q", string(b), r.Header.Get("Expect"))
	})}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)

	resp := do(t, h, "POST", "/api/orders", "hello", map[string]string{
		"X-Dev-User": "shota", "X-Dev-Token": "tok", "Expect": "100-continue",
	})
	b := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, `LAPTOP body="hello"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	// And the header does not travel. The 1xx loop in readFinalResponse
	// would discard the CLI's "100 Continue" anyway, so this is the only
	// thing that keeps the deletion honest: there is nothing left to
	// negotiate once the body is buffered, and a round trip spent saying so
	// is one the developer waits for.
	if !strings.Contains(b, `expect=""`) {
		t.Errorf("Expect must not reach the laptop: %s", b)
	}
}

// TestProxyClosesTheStreamWhenTheResponseIsDone keeps one request to one
// stream. Leaking a stream per request leaks a goroutine in the CLI and a
// yamux stream in the agent, and the symptom appears days later.
func TestProxyClosesTheStreamWhenTheResponseIsDone(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "LAPTOP")
	})}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)

	resp := do(t, h, "GET", "/x", "", stealHeaders())
	if b := readAll(t, resp); !strings.Contains(b, "LAPTOP") {
		t.Fatalf("body = %s", b)
	}
	l.mu.Lock()
	eof := l.eofs[0]
	l.mu.Unlock()
	select {
	case <-eof:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream was never closed: the CLI end is still waiting for another request")
	}
}

// TestProxyReturnsTheLaptopsOwnFailure is the line between "the request
// never arrived" and "the request arrived and failed". Only the first is
// replayable; retrying the second against the application would run a POST
// twice and show the developer an answer their own code did not give.
func TestProxyReturnsTheLaptopsOwnFailure(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "LAPTOP-BROKE", http.StatusServiceUnavailable)
	})}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)

	resp := do(t, h, "POST", "/api/orders", "once", stealHeaders())
	b := readAll(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(b, "LAPTOP-BROKE") {
		t.Fatalf("the laptop's own answer must be returned verbatim: status=%d body=%s", resp.StatusCode, b)
	}
	if n := app.hits.Load(); n != 0 {
		t.Errorf("the application served %d requests the laptop had already handled", n)
	}
}

// TestProxyAnswers502WhenTheCLINeverReplies is the bound session.Opener
// makes the caller's duty. A CLI older than the accept loop answers an http
// stream with nothing at all - not a refusal, not an EOF - so without a
// deadline this request never completes, and with this proxy on the ALB's
// data path that is an outage rather than a slow request.
func TestProxyAnswers502WhenTheCLINeverReplies(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{silent: true}
	s := sess("shota", "tok", true)
	s.open = l
	p, _ := newProxy(t, app.addr(), s)
	p.StealTimeout = 200 * time.Millisecond
	h := p.Handler()

	done := make(chan int, 1)
	go func() {
		resp := do(t, h, "GET", "/api/orders", "", stealHeaders())
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		if code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a CLI that never answers must not hang the request: no deadline on the reply")
	}
	// Not replayed: the request was written to a CLI that accepted the
	// stream, so it may well have acted on it.
	if n := app.hits.Load(); n != 0 {
		t.Errorf("the application served %d requests the laptop may already have run", n)
	}
}

// TestProxyFallsBackWhenTheCLIRefusesTheStream covers the refusal the CLI
// sends instead of a response: one JSON line, which http.ReadResponse
// cannot parse. It is sent before the CLI runs anything, so the request can
// still be served honestly by the application - that is what the code on
// the wire is for (proto.CodeNoIncoming).
func TestProxyFallsBackWhenTheCLIRefusesTheStream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   proto.Error
		inLog string
	}{
		{"no incoming", proto.Error{Code: proto.CodeNoIncoming, Message: "not accepting incoming requests"}, "not accepting incoming requests"},
		{"unknown stream type", proto.Error{Code: proto.CodeBadHello, Message: "unknown stream type http"}, proto.CodeBadHello},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := appStub(t, "APP")
			e := tc.err
			l := &laptop{refuse: &e}
			s := sess("shota", "tok", true)
			s.open = l
			p, lg := newProxy(t, app.addr(), s)
			resp := do(t, p.Handler(), "POST", "/api/orders", "important", stealHeaders())
			b := readAll(t, resp)
			if resp.StatusCode != 200 || !strings.Contains(b, `APP path=/api/orders body="important"`) {
				t.Fatalf("a refused stream must be served by the application, body included: status=%d body=%s", resp.StatusCode, b)
			}
			if !strings.Contains(lg.all(), tc.inLog) {
				t.Errorf("the log must say why the request was not stolen (%q): %s", tc.inLog, lg.all())
			}
		})
	}
}

// TestProxyNeverLogsTheToken is the containment invariant on the one code
// path that logs about a session at all. Proxy.Logf is the agent's stdout,
// which is CloudWatch, which the developer's colleagues can read, and the
// token is the only thing between the public ALB and their laptop.
func TestProxyNeverLogsTheToken(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{fail: true}
	s := sess("shota", "s3cret-token", true)
	s.open = l
	p, lg := newProxy(t, app.addr(), s)
	resp := do(t, p.Handler(), "POST", "/api/orders", "important", map[string]string{
		"X-Dev-User": "shota", "X-Dev-Token": "s3cret-token",
	})
	resp.Body.Close()
	if got := lg.all(); strings.Contains(got, "s3cret-token") {
		t.Errorf("the proxy log discloses the session token: %s", got)
	}
	if got := lg.all(); !strings.Contains(got, "shota") || !strings.Contains(got, "/api/orders") {
		t.Errorf("the log must still be usable: %s", got)
	}
	// Session.String() redacts the token, so printing a whole session is
	// not a disclosure today - it is one field away from being one. The
	// rule the proxy follows is to log s.User and nothing else about the
	// session, and this is what makes that rule fail out loud rather than
	// lean on the backstop.
	if got := lg.all(); strings.Contains(got, "Session{") {
		t.Errorf("the proxy logs a whole Session; log s.User instead: %s", got)
	}
}

// TestProxyDoesNotParkOnAWriteItNeverAbortedAfterAFailedReply is the
// deadlock that the code's own comment used to deny. The proxy clears the
// write deadline before writing the request, gives up on the reply when the
// read deadline fires, and then joins the write goroutine - and a deferred
// close cannot unblock that join, because a defer runs only after the
// function has returned. A CLI that sends half a status line and stops
// reading is enough, and a goroutine parked here with the proxy on the
// ALB's data path is an outage rather than a slow request.
//
// The bound on this test is its own, so that a regression reports a failed
// assertion instead of stalling the whole package for ten minutes.
func TestProxyDoesNotParkOnAWriteItNeverAbortedAfterAFailedReply(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{partial: "HTTP/1.1 20"}
	s := sess("shota", "tok", true)
	s.open = l
	p, _ := newProxy(t, app.addr(), s)
	p.StealTimeout = 200 * time.Millisecond
	h := p.Handler()

	done := make(chan int, 1)
	go func() {
		resp := do(t, h, "POST", "/api/orders", strings.Repeat("x", 64<<10), stealHeaders())
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		if code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy never answered: it is parked joining a request write it did not abort")
	}
	// A half-written reply means the CLI had the request. Not replayable.
	if n := app.hits.Load(); n != 0 {
		t.Errorf("the application served %d requests the laptop may already have run", n)
	}
}

// TestProxyAbortsTheWriteRatherThanWaitingOutTheGrace pins the abort
// itself, not just the bound on the join. The bound alone would answer this
// request too - a whole WriteAbortGrace later - so the grace here is set far
// above the steal timeout, which makes "did it abort the write, or did it
// sit out the grace?" the only thing the elapsed time can be measuring.
func TestProxyAbortsTheWriteRatherThanWaitingOutTheGrace(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{partial: "HTTP/1.1 20"}
	s := sess("shota", "tok", true)
	s.open = l
	p, _ := newProxy(t, app.addr(), s)
	p.StealTimeout = 200 * time.Millisecond
	p.WriteAbortGrace = 4 * time.Second
	h := p.Handler()

	done := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		resp := do(t, h, "POST", "/api/orders", strings.Repeat("x", 64<<10), stealHeaders())
		resp.Body.Close()
		done <- time.Since(start)
	}()
	select {
	case took := <-done:
		// The reply is given up on at 200ms; aborting the write costs
		// nothing after that. Sitting out the grace would cost four
		// seconds.
		if took > 2*time.Second {
			t.Fatalf("the request took %s: the write was not aborted, only waited out", took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the proxy never answered at all")
	}
}

// TestProxyDoesNotParkOnAWriteBlockedOnTheInboundBody is the same hazard by
// the route no deadline can reach. Here the write goroutine is blocked
// reading the *inbound* request body - the case for a body too large to
// buffer, where that body is still the ALB's connection - so aborting the
// stream does nothing and only a bound on the join returns the request.
func TestProxyDoesNotParkOnAWriteBlockedOnTheInboundBody(t *testing.T) {
	app := appStub(t, "APP")
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	l := &laptop{silent: true} // drains the request, never answers
	s := sess("shota", "tok", true)
	s.open = l
	p, _ := newProxy(t, app.addr(), s)
	p.StealTimeout = 200 * time.Millisecond
	h := p.Handler()

	r := httptest.NewRequest("POST", "/upload", &blockingBody{left: MaxReplayBody + 1, hold: hold})
	for k, v := range stealHeaders() {
		r.Header.Set(k, v)
	}
	done := make(chan int, 1)
	go func() {
		resp := serve(t, h, r)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		if code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502: a body past the cap cannot be replayed", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy never answered: it is parked on a body read it cannot abort")
	}
}

// TestProxyReadsPastInformationalResponses covers 103 Early Hints, which a
// local app may send before the real answer and which the CLI's own reverse
// proxy forwards. This transport reads the exchange by hand, so nothing
// discards them for it: relaying the 103 would hand the browser an empty
// reply and leave the real response unread on the stream.
func TestProxyReadsPastInformationalResponses(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{raw: "HTTP/1.1 103 Early Hints\r\nLink: </s.css>; rel=preload\r\n\r\n" +
		"HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nLAPTOP"}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)

	resp := do(t, h, "GET", "/api/orders", "", stealHeaders())
	b := readAll(t, resp)
	if resp.StatusCode != 200 || b != "LAPTOP" {
		t.Fatalf("status=%d body=%q, want 200 LAPTOP: the 103 must be discarded, not relayed", resp.StatusCode, b)
	}
	if got := resp.Header.Get("Link"); got != "" {
		t.Errorf("the informational response's headers leaked into the answer: Link=%q", got)
	}
}

// TestProxyRefusesA101OnTheStealPath keeps the upgrade rule whole from the
// other end. An Upgrade request never reaches the steal path, so a 101 here
// is a reply this transport cannot finish - and it is not replayable,
// because the request reached the developer's process.
func TestProxyRefusesA101OnTheStealPath(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{raw: "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"}
	s := sess("shota", "tok", true)
	s.open = l
	p, lg := newProxy(t, app.addr(), s)

	resp := do(t, p.Handler(), "POST", "/ws", "hello", stealHeaders())
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a 101 on the steal path", resp.StatusCode)
	}
	if n := app.hits.Load(); n != 0 {
		t.Errorf("the application served %d requests the developer's process had already taken", n)
	}
	if !strings.Contains(lg.all(), "101") {
		t.Errorf("the log must name the reply it could not use: %s", lg.all())
	}
}

// TestProxyServesFromTheAppWhenTheDevelopersServerIsNotRunning is
// docs/e2e-aws.md row 24: the developer forgot to start their own server,
// the CLI cannot dial it, and the client must get the task's answer rather
// than a 502 from someone's laptop.
//
// It turns on an explicit header and nothing else. The CLI sets
// proto.NoListenerHeader only when that dial failed - before any byte of
// the request reached any application - which is what makes serving the
// request a second time safe. Guessing from the status alone would re-run a
// POST the developer's app had already handled.
func TestProxyServesFromTheAppWhenTheDevelopersServerIsNotRunning(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{raw: "HTTP/1.1 502 Bad Gateway\r\n" + proto.NoListenerHeader + ": 1\r\n" +
		"Content-Length: 33\r\n\r\nnothing is listening on 127.0.0.1"}
	s := sess("shota", "tok", true)
	s.open = l
	p, lg := newProxy(t, app.addr(), s)

	resp := do(t, p.Handler(), "POST", "/api/orders", "important", stealHeaders())
	b := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, `APP path=/api/orders body="important"`) {
		t.Fatalf("row 24: the task must answer, body included: status=%d body=%s", resp.StatusCode, b)
	}
	if resp.Header.Get(proto.NoListenerHeader) != "" {
		t.Errorf("the internal header reached the client: %v", resp.Header)
	}
	if !strings.Contains(lg.all(), "shota") {
		t.Errorf("the log must name the session: %s", lg.all())
	}
}

// TestProxyRelaysA502TheDevelopersAppProduced is the other half of that
// rule, and the one that keeps it safe. A 502 without the header may be the
// developer's own application answering a request it has already run;
// serving that request from the task as well would run a POST twice.
func TestProxyRelaysA502TheDevelopersAppProduced(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{raw: "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 12\r\n\r\nLAPTOP-BROKE"}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)

	resp := do(t, h, "POST", "/api/orders", "once", stealHeaders())
	b := readAll(t, resp)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(b, "LAPTOP-BROKE") {
		t.Fatalf("a 502 the laptop itself produced must be relayed verbatim: status=%d body=%s", resp.StatusCode, b)
	}
	if n := app.hits.Load(); n != 0 {
		t.Errorf("the application re-ran %d requests the developer's app had already answered", n)
	}
}

// TestProxyStripsTheNoListenerHeaderFromARelayedResponse keeps the internal
// header off the internet whatever it is attached to. The rule above looks
// only at 502s, so a response with any other status carrying it is relayed
// - and it must not be relayed with it.
func TestProxyStripsTheNoListenerHeaderFromARelayedResponse(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{raw: "HTTP/1.1 200 OK\r\n" + proto.NoListenerHeader + ": 1\r\n" +
		"Content-Length: 6\r\n\r\nLAPTOP"}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.addr(), s)

	resp := do(t, h, "GET", "/api/orders", "", stealHeaders())
	b := readAll(t, resp)
	if resp.StatusCode != 200 || b != "LAPTOP" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, b)
	}
	if resp.Header.Get(proto.NoListenerHeader) != "" {
		t.Errorf("%s must never reach the client: %v", proto.NoListenerHeader, resp.Header)
	}
}

// attachRealCLI attaches a session with internal/session's own client, so
// the proxy is exercised against the CLI's measured behaviour - its accept
// loop, its header read and the Opener the registry really stores - rather
// than against this file's laptop stand-in.
func attachRealCLI(ctx context.Context, t *testing.T, controlAddr, user, tok string, h http.Handler) *session.Client {
	t.Helper()
	conn, err := net.Dial("tcp", controlAddr)
	if err != nil {
		t.Fatal(err)
	}
	hello := proto.Hello{Version: proto.Version, User: user, Token: tok,
		Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
	c, err := session.Dial(ctx, conn, hello, session.Options{
		OnHTTP: func(stream net.Conn) {
			// What `tetherd run` does with a stolen request: serve
			// HTTP/1.1 on the stream it was handed.
			srv := &http.Server{Handler: h}
			srv.Serve(&oneConnListener{c: stream})
		},
	})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestProxyStealsThroughTheCLIsOwnSessionCode is the answer to "the
// stand-in agrees with me because I wrote both ends". Nothing about the
// laptop type in this file takes part: the stream comes from the registry's
// own Opener over yamux, and the CLI end is internal/session's client.
func TestProxyStealsThroughTheCLIsOwnSessionCode(t *testing.T) {
	app := appStub(t, "APP")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	lg := newLog(t)
	a := New(Config{Env: "dev"}, lg.logf)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Serve(ctx, ln)

	attachRealCLI(ctx, t, ln.Addr().String(), "shota", "tok", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "REAL-CLI path=%s body=%q xff=%q", r.URL.Path, string(b), r.Header.Get("X-Forwarded-For"))
	}))
	waitFor(t, func() bool { return len(a.Sessions()) == 1 }, "the session never registered")

	p := &Proxy{Agent: a, AppAddr: app.addr(), Logf: lg.logf, StealTimeout: 5 * time.Second}
	h := p.Handler()
	resp := do(t, h, "POST", "/api/orders", "hello", map[string]string{
		"X-Dev-User": "shota", "X-Dev-Token": "tok", "X-Forwarded-For": "203.0.113.5",
	})
	b := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, `REAL-CLI path=/api/orders body="hello"`) {
		t.Fatalf("the real CLI must receive the stolen request: status=%d body=%s", resp.StatusCode, b)
	}
	if !strings.Contains(b, `xff="203.0.113.5"`) {
		t.Errorf("X-Forwarded-For must survive to the real CLI too: %s", b)
	}
	// The same proxy, the same attached session: a health check is still
	// the application's.
	resp = do(t, h, "GET", "/", "", nil)
	if b := readAll(t, resp); !strings.Contains(b, "APP path=/") {
		t.Fatalf("health check body = %s", b)
	}
	if strings.Contains(lg.all(), "tok") {
		t.Errorf("the log mentions the token: %s", lg.all())
	}
}

// TestServeProxyServesTheALBPortUntilTheContextEnds covers the listener
// half: a real TCP connection, a real HTTP client, and a shutdown that
// stops serving when the agent is asked to stop.
func TestServeProxyServesTheALBPortUntilTheContextEnds(t *testing.T) {
	app := appStub(t, "APP")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{Env: "dev", AppAddr: app.addr()}, newLog(t).logf)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- a.ServeProxy(ctx, ln) }()

	resp, err := albClient().Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("the ALB port must answer: %v", err)
	}
	b := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(b, "APP path=/") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("a clean shutdown must not be an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeProxy did not return when the context ended")
	}
	if _, err := albClient().Get("http://" + ln.Addr().String() + "/"); err == nil {
		t.Error("the ALB port still answers after shutdown")
	}
}

// TestRunServesBothTheControlPortAndTheALBPort is what main starts. The
// agent is useless without either half: no control port and nobody can
// attach, no ALB port and the dev environment is down.
func TestRunServesBothTheControlPortAndTheALBPort(t *testing.T) {
	app := appStub(t, "APP")
	lg := newLog(t)
	a := New(Config{Env: "dev", Control: "127.0.0.1:0", Proxy: "127.0.0.1:0", AppAddr: app.addr()}, lg.logf)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()

	var control, proxy string
	listening := regexp.MustCompile(`control listening on (\S+), proxy listening on (\S+)`)
	waitFor(t, func() bool {
		m := listening.FindStringSubmatch(lg.all())
		if m == nil {
			return false
		}
		control, proxy = m[1], m[2]
		return true
	}, "Run never reported both listeners")

	resp, err := albClient().Get("http://" + proxy + "/")
	if err != nil {
		t.Fatalf("the ALB port must answer while nobody is attached: %v", err)
	}
	if b := readAll(t, resp); !strings.Contains(b, "APP path=/") {
		t.Fatalf("health check body = %s", b)
	}

	attachRealCLI(ctx, t, control, "shota", "tok", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "REAL-CLI path=%s", r.URL.Path)
	}))
	waitFor(t, func() bool { return len(a.Sessions()) == 1 }, "the session never registered")

	req, err := http.NewRequest("GET", "http://"+proxy+"/api/orders", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Dev-User", "shota")
	req.Header.Set("X-Dev-Token", "tok")
	resp, err = albClient().Do(req)
	if err != nil {
		t.Fatalf("the stolen request failed: %v", err)
	}
	if b := readAll(t, resp); !strings.Contains(b, "REAL-CLI path=/api/orders") {
		t.Fatalf("a matching request on the ALB port must reach the laptop: %s", b)
	}

	cancel()
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return when the context ended")
	}
}

// TestRunDrainsInFlightALBRequestsBeforeReturning is what SIGTERM does to
// a deployment. Run owns two goroutines, and the control accept loop
// returns the moment its listener closes while the proxy is still draining
// the ALB's in-flight requests through http.Server.Shutdown. Returning on
// whichever finishes first has main exit mid-drain and severs exactly the
// requests the drain exists to finish - so Run must wait for both.
func TestRunDrainsInFlightALBRequestsBeforeReturning(t *testing.T) {
	entered := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		time.Sleep(time.Second)
		fmt.Fprint(w, "APP-DRAINED")
	}))
	t.Cleanup(slow.Close)

	lg := newLog(t)
	a := New(Config{Env: "dev", Control: "127.0.0.1:0", Proxy: "127.0.0.1:0",
		AppAddr: slow.Listener.Addr().String()}, lg.logf)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()

	var proxy string
	listening := regexp.MustCompile(`proxy listening on (\S+)`)
	waitFor(t, func() bool {
		m := listening.FindStringSubmatch(lg.all())
		if m == nil {
			return false
		}
		proxy = m[1]
		return true
	}, "Run never reported its ALB listener")

	answered := make(chan string, 1)
	go func() {
		resp, err := albClient().Get("http://" + proxy + "/")
		if err != nil {
			answered <- "the request failed: " + err.Error()
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			answered <- "reading the response failed: " + err.Error()
			return
		}
		answered <- string(b)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the application")
	}

	cancel()
	select {
	case err := <-ran:
		t.Fatalf("Run returned while an ALB request was still in flight (err=%v): main exits on that, severing it", err)
	case <-time.After(500 * time.Millisecond):
	}
	select {
	case got := <-answered:
		if !strings.Contains(got, "APP-DRAINED") {
			t.Fatalf("the in-flight request was not served to completion: %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight request never completed")
	}
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("a clean shutdown must not be an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return once the drain was done")
	}
}

// TestRunRefusesToStartWithoutBothPorts keeps startup honest: an agent that
// came up with only its control port would look healthy and have no ALB
// path at all.
func TestRunRefusesToStartWithoutBothPorts(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	a := New(Config{Env: "dev", Control: "127.0.0.1:0", Proxy: taken.Addr().String(), AppAddr: "127.0.0.1:8081"}, newLog(t).logf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = a.Run(ctx)
	if err == nil {
		t.Fatal("Run must fail when the ALB port cannot be listened on")
	}
	if !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Errorf("the error must name the port: %v", err)
	}
}

// TestConfigDefaultsForTheProxyAndTheApp pins the two addresses the ECS
// task definition and the ALB target group depend on. Getting either wrong
// is a task that comes up and serves nothing.
func TestConfigDefaultsForTheProxyAndTheApp(t *testing.T) {
	cfg, err := ConfigFromEnv(func(k string) string {
		if k == "TETHERD_ENV" {
			return "dev"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy != "0.0.0.0:8080" {
		t.Errorf("default proxy = %q, want 0.0.0.0:8080: the ALB reaches the task over the VPC, not loopback", cfg.Proxy)
	}
	if cfg.AppAddr != "127.0.0.1:8081" {
		t.Errorf("default app = %q, want 127.0.0.1:8081", cfg.AppAddr)
	}
	env := map[string]string{
		"TETHERD_ENV":      "dev",
		"TETHERD_PROXY":    "0.0.0.0:9090",
		"TETHERD_APP_ADDR": "127.0.0.1:3000",
	}
	cfg, err = ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy != "0.0.0.0:9090" || cfg.AppAddr != "127.0.0.1:3000" {
		t.Errorf("cfg = %+v: both addresses must be overridable", cfg)
	}
}
