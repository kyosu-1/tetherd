package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// localApp is the developer's own process on 127.0.0.1:<port>.
func localApp(t *testing.T, h http.Handler) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func bufioReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

// agentSide writes one request onto a stream the way the agent's proxy does
// and returns the response it reads back.
//
// No proto header is written: the session layer reads and discards it
// before calling OnHTTP, so the first bytes a StealServer ever sees are the
// request line (session.Options.OnHTTP). The e2e tests in run_e2e_test.go
// go through a real session and so do write one.
func agentSide(t *testing.T, s *StealServer, req *http.Request) *http.Response {
	t.Helper()
	mine, theirs := net.Pipe()
	t.Cleanup(func() { mine.Close() })
	go s.Serve(theirs)
	if err := req.Write(mine); err != nil {
		t.Fatal(err)
	}
	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufioReader(mine), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// logSink collects the CLI's log lines the way run.go's logf writes them.
type logSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", args...)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestStealServerProxiesToTheLocalPort(t *testing.T) {
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "LOCAL %s %s body=%q xff=%q host=%q", r.Method, r.URL.Path, string(b), r.Header.Get("X-Forwarded-For"), r.Host)
	}))
	var logs logSink
	s := &StealServer{LocalPort: port, Logf: logs.logf}

	req := httptest.NewRequest("POST", "http://tetherd.example/api/orders", strings.NewReader("hi"))
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	resp := agentSide(t, s, req)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `LOCAL POST /api/orders body="hi"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `xff="203.0.113.5"`) {
		t.Errorf("X-Forwarded-For must reach the developer's process: %s", b)
	}
	// The request has to look the way it looked at the ALB, not the way
	// this hop dialled it: an application that routes on Host must see the
	// caller's host, never 127.0.0.1:<port>.
	if !strings.Contains(string(b), `host="tetherd.example"`) {
		t.Errorf("the inbound Host must reach the developer's process: %s", b)
	}
	// The request log is the CLI's job, not the agent's (spec §5.2).
	if !strings.Contains(logs.String(), "POST") || !strings.Contains(logs.String(), "/api/orders") ||
		!strings.Contains(logs.String(), "200") || !strings.Contains(logs.String(), "203.0.113.5") {
		t.Errorf("the log line must carry method, path, status and the caller: %q", logs.String())
	}
}

// A request that arrived through several proxies names the client first;
// that is who the log line is about, and the whole list is what the
// developer's process must receive.
func TestStealServerKeepsTheWholeForwardedChain(t *testing.T) {
	got := make(chan http.Header, 1)
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
	}))
	var logs logSink
	s := &StealServer{LocalPort: port, Logf: logs.logf}

	req := httptest.NewRequest("GET", "http://tetherd/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.1.20")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "api.example.com")
	agentSide(t, s, req)

	h := <-got
	if h.Get("X-Forwarded-For") != "203.0.113.5, 10.0.1.20" {
		t.Errorf("X-Forwarded-For = %q, want the chain unchanged", h.Get("X-Forwarded-For"))
	}
	if h.Get("X-Forwarded-Proto") != "https" || h.Get("X-Forwarded-Host") != "api.example.com" {
		t.Errorf("the other X-Forwarded-* must pass through too: %v", h)
	}
	if !strings.Contains(logs.String(), "(from 203.0.113.5)") {
		t.Errorf("the log must name the original caller, not the last proxy: %q", logs.String())
	}
}

func TestStealServerReportsALocalAppThatIsNotListening(t *testing.T) {
	// The developer has not started their server yet. The agent must get a
	// response, not a hang, and the log must say what to do.
	var logs logSink
	s := &StealServer{LocalPort: 1, Logf: logs.logf}
	resp := agentSide(t, s, httptest.NewRequest("GET", "http://tetherd/", nil))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "127.0.0.1:1") {
		t.Errorf("the log must name the port nothing is listening on: %q", logs.String())
	}
	// The body is asserted separately from the log because the dialler's
	// own error happens to contain the address too: without this, a handler
	// that named nothing itself would still pass the line above.
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "127.0.0.1:1") {
		t.Errorf("the body must name the port too: %q", b)
	}
}

// A failed dial is the one 502 the agent may answer by serving the request
// from the application instead, because nothing reached an application and
// so nothing can have acted on it (docs/e2e-aws.md row 24). The header is
// how the agent is told, and it must be on this response and no other.
func TestStealServerMarksAFailedDialAsSafeToReplay(t *testing.T) {
	var logs logSink
	s := &StealServer{LocalPort: 1, Logf: logs.logf}
	resp := agentSide(t, s, httptest.NewRequest("POST", "http://tetherd/api/orders", strings.NewReader("{}")))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	// The literal spelling, not the constant, because the constant is what
	// is under test: both ends read proto.NoListenerHeader so they cannot
	// drift, and this is the one place the wire name is written out.
	if got := resp.Header.Get("X-Tetherd-No-Listener"); got != "1" {
		t.Errorf("X-Tetherd-No-Listener = %q, want \"1\" so the agent falls back to the app", got)
	}
	if proto.NoListenerHeader != "X-Tetherd-No-Listener" {
		t.Errorf("the header name is a protocol agreement with the agent, got %q", proto.NoListenerHeader)
	}
	// The agent keys on the header paired with a 502, so a dial failure
	// answered with any other status would turn the fallback off.
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want the 502 the agent pairs the header with", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "nothing is listening on 127.0.0.1:1") {
		t.Errorf("the log is the developer's only clue their server is down: %q", logs.String())
	}
}

// The case that decides whether a POST runs twice: the developer's process
// accepted the connection and then failed, so the request did reach an
// application. That 502 is the application's answer as far as anyone else
// can tell, and it must be relayed, never replayed.
func TestStealServerDoesNotMarkAFailureAfterTheAppAccepted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Read what was sent - this stands for an application that has
			// the request and may have acted on it - then hang up without
			// answering.
			io.CopyN(io.Discard, c, 1)
			select {
			case accepted <- struct{}{}:
			default:
			}
			c.Close()
		}
	}()

	var logs logSink
	s := &StealServer{LocalPort: ln.Addr().(*net.TCPAddr).Port, Logf: logs.logf}
	resp := agentSide(t, s, httptest.NewRequest("POST", "http://tetherd/api/orders", strings.NewReader("{}")))
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the stand-in app never saw the connection, so this is not the case under test")
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Tetherd-No-Listener"); got != "" {
		t.Errorf("X-Tetherd-No-Listener = %q on a request that reached an application: replaying it would run the POST twice", got)
	}
}

// The header is this hop's claim about its own dial. The developer's
// process must not be able to make it on this hop's behalf - a local app
// that sets it would otherwise have every one of its own responses
// replayed against the task's application.
func TestStealServerStripsTheReplayClaimFromTheLocalApp(t *testing.T) {
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tetherd-No-Listener", "1")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "the app's own 502")
	}))
	s := &StealServer{LocalPort: port}
	resp := agentSide(t, s, httptest.NewRequest("POST", "http://tetherd/api/orders", strings.NewReader("{}")))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want the app's own 502 relayed", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Tetherd-No-Listener"); got != "" {
		t.Errorf("X-Tetherd-No-Listener = %q: only a failed dial may claim a request is safe to replay", got)
	}
}

func TestStealServerHandlesSeveralRequestsOnSeparateStreams(t *testing.T) {
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "LOCAL %s", r.URL.Path)
	}))
	s := &StealServer{LocalPort: port}
	for _, p := range []string{"/a", "/b", "/c"} {
		resp := agentSide(t, s, httptest.NewRequest("GET", "http://tetherd"+p, nil))
		b, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(b), "LOCAL "+p) {
			t.Fatalf("%s: %s", p, b)
		}
	}
}

// Serve owns the stream, including closing it (session.Options.OnHTTP). An
// agent's proxy that is left holding a stream nobody will read waits out
// its own deadline instead of passing the request to the application.
func TestStealServerClosesTheStreamWhenTheRequestIsDone(t *testing.T) {
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	s := &StealServer{LocalPort: port}
	mine, theirs := net.Pipe()
	served := make(chan struct{})
	go func() { s.Serve(theirs); close(served) }()

	req := httptest.NewRequest("GET", "http://tetherd/", nil)
	// Close: net/http's server keeps a keep-alive connection open for the
	// next request, so it is the agent hanging up - or asking not to keep
	// it - that ends the stream.
	req.Close = true
	if err := req.Write(mine); err != nil {
		t.Fatal(err)
	}
	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufioReader(mine), req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the request was answered; the stream is leaked")
	}
	// The far end must see the close, not a stream that stays open.
	//
	// io.EOF specifically, not merely "some error that is not a timeout":
	// net.Pipe reports a peer that closed as io.EOF and its own end being
	// closed as io.ErrClosedPipe, so requiring EOF is what stops this
	// assertion passing on an artefact of the stand-in rather than on
	// Serve having closed anything. (net/http also pushes the read
	// deadline into the past after every handler returns, via
	// connReader.abortPendingRead, which is why a deadline firing is not
	// evidence of a close either - but that happens on Serve's end of the
	// pipe, not on this one.)
	mine.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = mine.Read(make([]byte, 1))
	switch {
	case err == nil:
		t.Error("the stream must be closed once Serve returns")
	case errors.Is(err, io.EOF):
		// The close, which is what this test is about.
	default:
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Error("reading the stream timed out, so Serve left it open")
		} else {
			t.Errorf("reading the stream gave %v, want io.EOF - a close by the other end", err)
		}
	}
}

// Serve must not return - and so must not close the stream - while a
// request is still being served. The listener it hands net/http is a single
// connection, and returning when net/http's own accept loop finds nothing
// more to accept would cut every stolen request off mid-response.
func TestStealServerDoesNotCloseTheStreamWhileTheRequestIsInFlight(t *testing.T) {
	release := make(chan struct{})
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, "SLOW ok")
	}))
	s := &StealServer{LocalPort: port}
	mine, theirs := net.Pipe()
	t.Cleanup(func() { mine.Close() })
	go s.Serve(theirs)

	req := httptest.NewRequest("GET", "http://tetherd/slow", nil)
	if err := req.Write(mine); err != nil {
		t.Fatal(err)
	}
	// Long enough that a Serve which returns as soon as net/http's accept
	// loop is done would have closed the stream by now.
	time.Sleep(200 * time.Millisecond)
	close(release)
	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufioReader(mine), req)
	if err != nil {
		t.Fatalf("the response never came back: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "SLOW ok" {
		t.Fatalf("body = %q", b)
	}
}

// A streamed response (server-sent events, a progress log) must reach the
// caller as it is produced, not when the handler finally returns. What
// carries the flush through this hop is the wrapper around the
// ResponseWriter: httputil flushes through http.ResponseController and
// nothing else, so a wrapper the controller cannot see past would buffer
// the whole stream.
func TestStealServerStreamsAFlushedResponse(t *testing.T) {
	release := make(chan struct{})
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("the local app could not flush: %v", err)
		}
		<-release
		fmt.Fprint(w, "data: second\n\n")
	}))
	s := &StealServer{LocalPort: port}
	mine, theirs := net.Pipe()
	t.Cleanup(func() { mine.Close() })
	go s.Serve(theirs)

	req := httptest.NewRequest("GET", "http://tetherd/events", nil)
	if err := req.Write(mine); err != nil {
		t.Fatal(err)
	}
	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufioReader(mine)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Read the first event while the handler is still inside the channel
	// receive: this is what a buffered hop would make impossible. The read
	// is off resp.Body so that the chunked framing is net/http's problem.
	body := bufioReader(resp.Body)
	line, err := body.ReadString('\n')
	if err != nil {
		t.Fatalf("the first event never arrived while the response was still open: %v", err)
	}
	if line != "data: first\n" {
		t.Fatalf("first line = %q", line)
	}
	close(release)
	rest, _ := io.ReadAll(body)
	if !strings.Contains(string(rest), "data: second") {
		t.Errorf("the rest of the stream did not follow: %q", rest)
	}
}

// A stolen upgrade (a websocket) must reach the developer's process and
// then carry bytes both ways for as long as they keep it open. The agent
// does not steal upgrades today, but the CLI is what decides whether it
// ever can - and the failure mode is quiet: a wrapper that cannot give up
// the connection turns the switch into a 502.
func TestStealServerProxiesAnUpgrade(t *testing.T) {
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "not an upgrade: "+r.Header.Get("Upgrade"), http.StatusBadRequest)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("the local app could not hijack: %v", err)
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		// Echo one line, then another unprompted, to prove both directions
		// keep flowing after the switch.
		line, err := brw.ReadString('\n')
		if err != nil {
			t.Errorf("the local app read nothing after the switch: %v", err)
			return
		}
		brw.WriteString("echo:" + line)
		brw.Flush()
	}))
	var logs logSink
	s := &StealServer{LocalPort: port, Logf: logs.logf}

	mine, theirs := net.Pipe()
	t.Cleanup(func() { mine.Close() })
	go s.Serve(theirs)

	req := httptest.NewRequest("GET", "http://tetherd/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	if err := req.Write(mine); err != nil {
		t.Fatal(err)
	}
	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufioReader(mine)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 101 (body %s)", resp.StatusCode, b)
	}
	if _, err := mine.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("nothing came back over the upgraded stream: %v", err)
	}
	if got != "echo:ping\n" {
		t.Fatalf("got %q over the upgraded stream", got)
	}
	// The line is written when the copying stops, which is after the local
	// app hangs up - so it is polled for, not read straight away.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "101") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "101") {
		t.Errorf("the log line must report the protocol switch: %q", logs.String())
	}
}

// The header bound is a bound on the header and on nothing else. The
// session layer clears the read deadline before handing the stream over
// precisely so that a stolen websocket can sit idle (session.Options.
// OnHTTP), and a whole-stream ReadTimeout here would undo that: the
// upgraded stream would die the moment the developer stopped typing.
func TestStealServerLetsAnUpgradedStreamIdlePastTheHeaderBound(t *testing.T) {
	restore := stealReadHeaderTimeout
	stealReadHeaderTimeout = 150 * time.Millisecond
	t.Cleanup(func() { stealReadHeaderTimeout = restore })

	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("the local app could not hijack: %v", err)
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		line, err := brw.ReadString('\n')
		if err != nil {
			t.Errorf("the local app read nothing after the idle stretch: %v", err)
			return
		}
		brw.WriteString("echo:" + line)
		brw.Flush()
	}))
	s := &StealServer{LocalPort: port}

	mine, theirs := net.Pipe()
	t.Cleanup(func() { mine.Close() })
	go s.Serve(theirs)

	req := httptest.NewRequest("GET", "http://tetherd/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	if err := req.Write(mine); err != nil {
		t.Fatal(err)
	}
	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufioReader(mine)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	// Longer than the header bound, which is what a whole-stream deadline
	// would have been set to.
	time.Sleep(4 * stealReadHeaderTimeout)
	if _, err := mine.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("the upgraded stream died while it was idle: %v", err)
	}
	if got != "echo:ping\n" {
		t.Fatalf("got %q after the idle stretch", got)
	}
}

// The bound on the header must not become a bound on the body. A stolen
// upload arrives as fast as the original caller sends it, and a
// whole-stream read deadline here would cut off any request that takes
// longer to arrive than a header does - which is the deadline the session
// layer deliberately cleared before handing the stream over.
//
// This is the case that catches a whole-stream deadline: net/http clears
// every deadline when a handler hijacks, so an idle websocket survives one
// either way, and a slow request body is what does not.
func TestStealServerLetsASlowRequestBodyArrivePastTheHeaderBound(t *testing.T) {
	restore := stealReadHeaderTimeout
	stealReadHeaderTimeout = 150 * time.Millisecond
	t.Cleanup(func() { stealReadHeaderTimeout = restore })

	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "the body never finished: "+err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "LOCAL body=%q", string(b))
	}))
	s := &StealServer{LocalPort: port}
	mine, theirs := net.Pipe()
	t.Cleanup(func() { mine.Close() })
	go s.Serve(theirs)

	// Written by hand rather than with Request.Write so that the body can
	// be left unfinished for longer than the header bound.
	mine.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := mine.Write([]byte("POST /upload HTTP/1.1\r\nHost: tetherd\r\nTransfer-Encoding: chunked\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * stealReadHeaderTimeout)
	if _, err := mine.Write([]byte("4\r\nslow\r\n0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufioReader(mine), nil)
	if err != nil {
		t.Fatalf("no response came back: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `LOCAL body="slow"`) {
		t.Fatalf("status=%d body=%s, want the whole body delivered", resp.StatusCode, b)
	}
}

// A stream the agent keeps open after its request is answered is a pooled
// stream, and one nobody comes back to has to be reclaimed: without a bound
// the connection goroutine parks in its next read for the life of the
// session, which is the leak the stream-close contract exists to prevent,
// only slower.
func TestStealServerReclaimsAnIdleStream(t *testing.T) {
	restore := stealIdleTimeout
	stealIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { stealIdleTimeout = restore })

	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	s := &StealServer{LocalPort: port}
	mine, theirs := net.Pipe()
	t.Cleanup(func() { mine.Close() })
	served := make(chan struct{})
	go func() { s.Serve(theirs); close(served) }()

	// No Close on the request: the agent is keeping the stream for a second
	// request it never sends.
	req := httptest.NewRequest("GET", "http://tetherd/", nil)
	if err := req.Write(mine); err != nil {
		t.Fatal(err)
	}
	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufioReader(mine), req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve never returned for a stream left idle; the stream is leaked")
	}
}

// A proxy must not ask for an encoding the caller did not ask for: with
// net/http's default transport behaviour the request would reach the
// developer's process with an Accept-Encoding they never sent, and the
// response would come back decompressed with its Content-Encoding
// stripped - a body the application meant to be gzip, silently changed by
// a hop that has no business deciding that.
func TestStealServerDoesNotInventAnAcceptEncoding(t *testing.T) {
	got := make(chan string, 2)
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Accept-Encoding")
	}))
	s := &StealServer{LocalPort: port}

	agentSide(t, s, httptest.NewRequest("GET", "http://tetherd/", nil))
	if ae := <-got; ae != "" {
		t.Errorf("Accept-Encoding = %q, want it absent as the caller sent it", ae)
	}
	// And one the caller did send is passed on untouched.
	req := httptest.NewRequest("GET", "http://tetherd/", nil)
	req.Header.Set("Accept-Encoding", "br")
	agentSide(t, s, req)
	if ae := <-got; ae != "br" {
		t.Errorf("Accept-Encoding = %q, want the caller's own", ae)
	}
}

// The log line reports 200 for a handler that wrote a body without ever
// naming a status, which is what net/http itself sends. A recorder that
// started at zero would put "0" in the developer's log for a response the
// caller saw as a 200.
func TestStealServerLogsTheImpliedStatus(t *testing.T) {
	var logs logSink
	s := &StealServer{LocalPort: 1, Logf: logs.logf}
	rec := httptest.NewRecorder()
	s.logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "no WriteHeader call at all")
	})).ServeHTTP(rec, httptest.NewRequest("GET", "http://tetherd/implied", nil))
	if !strings.Contains(logs.String(), "/implied  200") {
		t.Errorf("the log must report the 200 net/http implies: %q", logs.String())
	}
}

// net/http's own complaints - a panic in a handler above all - must land in
// the CLI's log, where the developer is looking, and not on the standard
// logger in among their child process's output. The wiring is checked
// rather than provoked: a panic is what this mostly exists for, and there
// is deliberately nothing here that can panic.
func TestStealServerRoutesNetHTTPComplaintsIntoTheCLILog(t *testing.T) {
	var logs logSink
	s := &StealServer{LocalPort: 1, Logf: logs.logf}
	srv := s.server()
	if srv.ErrorLog == nil {
		t.Fatal("net/http must log through the CLI's logf, not the standard logger")
	}
	srv.ErrorLog.Printf("http: panic serving 127.0.0.1:1: boom")
	if !strings.Contains(logs.String(), "panic serving") || !strings.Contains(logs.String(), "boom") {
		t.Errorf("the complaint did not reach the CLI's log: %q", logs.String())
	}
	if strings.HasSuffix(logs.String(), "\n\n") {
		t.Errorf("one log line per complaint, not two: %q", logs.String())
	}
	// With nowhere better to put them, they stay on the standard logger
	// rather than going to a logf that would panic.
	if (&StealServer{LocalPort: 1}).server().ErrorLog != nil {
		t.Error("with no Logf there is nothing to route net/http's complaints into")
	}
}

// deadlineWriter is a ResponseWriter that supports one of the control
// methods statusRecorder does not implement itself.
type deadlineWriter struct {
	http.ResponseWriter
	set time.Time
}

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.set = t
	return nil
}

// The log line has to say what the caller was actually sent. net/http puts
// the first status on the wire and ignores any later one with a
// "superfluous WriteHeader" complaint, so a recorder that kept the last
// would report a status nobody received.
func TestStatusRecorderReportsTheStatusThatWasSent(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.WriteHeader(http.StatusCreated)
	rec.WriteHeader(http.StatusTeapot)
	if rec.status != http.StatusCreated {
		t.Errorf("status = %d, want the 201 that went on the wire", rec.status)
	}
}

// http.ResponseController reaches the real ResponseWriter's control methods
// by following Unwrap. Without it every method statusRecorder does not
// implement by hand answers ErrNotSupported instead - silently, since
// nothing in httputil reports a control method it could not reach.
func TestStatusRecorderPassesResponseControlThrough(t *testing.T) {
	inner := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	rec := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}
	want := time.Now().Add(time.Minute)
	if err := http.NewResponseController(rec).SetWriteDeadline(want); err != nil {
		t.Fatalf("SetWriteDeadline through the wrapper: %v", err)
	}
	if !inner.set.Equal(want) {
		t.Errorf("the deadline reached %v, want it passed through to the real writer", inner.set)
	}
}

// A malformed request must not panic or hang: net/http answers it and the
// stream is closed either way. `tetherd run` is also the DNS proxy, the
// capture and the developer's child process, and bytes a remote party
// shaped must not be able to take any of that down.
func TestStealServerAnswersRubbishWithoutPanicking(t *testing.T) {
	// The last case below never finishes a header line, so what ends it is
	// the header bound - shortened here because 20 seconds is the right
	// number in production and a hung test in a suite.
	restore := stealReadHeaderTimeout
	stealReadHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { stealReadHeaderTimeout = restore })

	var logs logSink
	s := &StealServer{LocalPort: 1, Logf: logs.logf}
	for _, junk := range []string{
		"NOT-HTTP-AT-ALL\r\n\r\n",
		"GET /\r\nHost: x\r\n\r\n",
		"\x00\x01\x02\x03",
		"GET / HTTP/1.1\r\nHost: x\r\nContent-Length: nonsense\r\n\r\n",
	} {
		mine, theirs := net.Pipe()
		done := make(chan struct{})
		go func() { s.Serve(theirs); close(done) }()
		mine.SetWriteDeadline(time.Now().Add(2 * time.Second))
		mine.Write([]byte(junk))
		mine.SetReadDeadline(time.Now().Add(5 * time.Second))
		io.Copy(io.Discard, mine) // whatever it answers, it must end
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("Serve never returned for %q", junk)
		}
		mine.Close()
	}
}

// A StealServer with no Logf must not panic: Logf is optional on the type
// and nothing in the package guarantees Run is the only caller.
func TestStealServerWithoutALoggerIsSilentNotFatal(t *testing.T) {
	s := &StealServer{LocalPort: 1}
	resp := agentSide(t, s, httptest.NewRequest("GET", "http://tetherd/", nil))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestStealSettingsDefaultsTheMatchHeaders(t *testing.T) {
	opts := ssmOpts("true")
	opts.NoIncoming = false
	opts.LocalPort = 3000
	opts.Token = "tok"
	st, err := stealSettings(opts)
	if err != nil {
		t.Fatal(err)
	}
	// internal/config applies no defaults to incoming.match, and the agent
	// treats an empty header name as "matches nothing", silently. So these
	// two values have to come from here or from nowhere.
	if !st.Incoming.Enabled || st.Incoming.Header != "X-Dev-User" || st.Incoming.TokenHeader != "X-Dev-Token" {
		t.Fatalf("incoming = %+v, want the two default header names", st.Incoming)
	}
	if st.LocalPort != 3000 {
		t.Errorf("LocalPort = %d, want the configured port", st.LocalPort)
	}
	if DefaultMatchHeader != "X-Dev-User" || DefaultMatchTokenHeader != "X-Dev-Token" {
		t.Fatalf("the defaults are part of the protocol with the browser extension: %q / %q", DefaultMatchHeader, DefaultMatchTokenHeader)
	}
	// The literal, not just "whatever the constant says": docs/config.md
	// documents all three of these as the values a repository that
	// configures nothing gets, and every other assertion in the suite reads
	// the constant, so nothing else would notice it changing.
	if DefaultLocalPort != 8080 {
		t.Errorf("DefaultLocalPort = %d, want the 8080 docs/config.md documents", DefaultLocalPort)
	}
}

func TestStealSettingsKeepsConfiguredMatchHeaders(t *testing.T) {
	opts := ssmOpts("true")
	opts.NoIncoming = false
	opts.LocalPort = 3000
	opts.Token = "tok"
	opts.MatchHeader = "x-dev"
	opts.MatchTokenHeader = "x-secret"
	st, err := stealSettings(opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Incoming.Header != "x-dev" || st.Incoming.TokenHeader != "x-secret" {
		t.Fatalf("incoming = %+v, want the configured names verbatim", st.Incoming)
	}
}

func TestStealSettingsRefusesAnEnabledSessionWithNoToken(t *testing.T) {
	opts := ssmOpts("true")
	opts.NoIncoming = false
	opts.LocalPort = 3000
	_, err := stealSettings(opts)
	if err == nil {
		t.Fatal("a session that takes requests with no token must not start: the agent can never match it")
	}
	if !isUsageError(err) {
		t.Errorf("a missing token is a configuration mistake (exit 2), got %T", err)
	}
	if !strings.Contains(err.Error(), "token") || !strings.Contains(err.Error(), "--no-incoming") {
		t.Errorf("the error must name the token and the way out: %v", err)
	}
}

// --no-incoming is the only way to turn steal off. Not naming a port is not
// one: the port has a default, because a request only ever leaves the
// application if it carries this developer's name and their token.
func TestStealSettingsOffOnlyForNoIncoming(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*RunOptions)
		off  bool
	}{
		{"--no-incoming", func(o *RunOptions) { o.NoIncoming = true; o.LocalPort = 3000; o.Token = "tok" }, true},
		{"--no-incoming with nothing else", func(o *RunOptions) { o.NoIncoming = true }, true},
		{"no local port", func(o *RunOptions) { o.Token = "tok" }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			opts := ssmOpts("true")
			opts.NoIncoming = false
			c.mut(&opts)
			st, err := stealSettings(opts)
			if err != nil {
				t.Fatalf("stealSettings: %v", err)
			}
			if c.off {
				if st != (stealConfig{}) {
					t.Fatalf("steal = %+v, want the zero value so the agent never steals", st)
				}
				return
			}
			if !st.Incoming.Enabled || st.LocalPort != DefaultLocalPort {
				t.Fatalf("steal = %+v, want it on and on the default port %d", st, DefaultLocalPort)
			}
		})
	}
}

func TestStealSettingsRefusesAPortThatIsNotAPort(t *testing.T) {
	for _, port := range []int{-1, 70000} {
		opts := ssmOpts("true")
		opts.NoIncoming = false
		opts.LocalPort = port
		opts.Token = "tok"
		if _, err := stealSettings(opts); err == nil || !isUsageError(err) {
			t.Errorf("local port %d must be a usage error, got %v", port, err)
		}
	}
}

// The token is the one secret in RunOptions and the CLI's log goes to the
// developer's terminal (and their scrollback). fmt reaches struct fields by
// reflection, so the protection has to live on the type.
func TestStealTokenIsNeverPrinted(t *testing.T) {
	opts := ssmOpts("true")
	opts.NoIncoming = false
	opts.LocalPort = 3000
	opts.Token = "s3cret-token-value"
	for _, s := range []string{
		fmt.Sprintf("%v", opts.Token),
		fmt.Sprintf("%s", opts.Token),
		fmt.Sprintf("%q", opts.Token),
		fmt.Sprintf("%#v", opts.Token),
		fmt.Sprintf("%v", opts),
		fmt.Sprintf("%+v", opts),
		fmt.Sprintf("%#v", opts),
		fmt.Sprintf("%v", EnvOptions{RunOptions: opts}),
		fmt.Sprintf("%+v", DoctorOptions{RunOptions: opts}),
	} {
		if strings.Contains(s, "s3cret-token-value") {
			t.Errorf("the token leaked into %q", s)
		}
	}
	// And the real value is still reachable where it is needed.
	if string(opts.Token) != "s3cret-token-value" {
		t.Fatalf("string(Token) = %q", string(opts.Token))
	}
}
