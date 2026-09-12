// The agent is always an HTTP/1.1 reverse proxy on the ALB's port, whether
// anyone is attached or not: the ALB reuses its connections with
// keep-alive, so routing has to be decided per request, and "pass through"
// is simply every request going to the application (spec §5.1).
//
// That makes this file the dev environment's availability. Once the ALB's
// target group points at :8080, every request and every health check
// reaches the application only through Handler, so a bug here is an outage
// rather than a missing feature.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

// MaxReplayBody is how much of a request body the proxy keeps so it can be
// replayed to the application when the laptop turns out to be gone.
//
// This limit is the whole reason the steal path buffers at all. Falling
// back to the application after a steal attempt fails means sending the
// application the same request, body included - and the body has already
// been read off the ALB's connection by then, so the only copy left is the
// one the proxy kept. Past this size there is no copy and therefore no
// honest retry: a dial failure with a larger body is answered with 502
// rather than with a request the application receives truncated
// (spec §5.2).
const MaxReplayBody = 1 << 20

// DefaultStealTimeout bounds the two halves of the exchange with a laptop
// where a peer that is not there would otherwise hang a request forever:
// writing the request onto the stream, and reading the reply's status line.
//
// It is not paranoia. A CLI older than the accept loop answers an http
// stream with nothing at all - not a refusal and not an EOF, measured in
// internal/session - so an unbounded read of the reply is an ALB request
// that never completes, which with this proxy on the data path is an outage
// rather than a slow request. Such a CLI is normally excluded before we get
// here, because Hello.Incoming.Enabled is false by its zero value, but
// "normally" is not a bound.
//
// The bound is deliberately below an ALB's default 60s idle timeout, so
// that the developer sees the agent's own 502 instead of the ALB's 504. It
// is cleared once the reply's headers have arrived: a stolen response may
// legitimately stream for minutes (SSE, a slow report), and the laptop owns
// its own timing from that point on.
const DefaultStealTimeout = 30 * time.Second

// DefaultWriteAbortGrace bounds the join with the goroutine writing a
// stolen request, once the proxy has given up on the reply.
//
// It exists because that goroutine may be blocked reading the inbound
// request body rather than writing to the stream - the case for a body too
// large to buffer, where r.Body is still the ALB's connection - and that
// read cannot be aborted from here at all. Two seconds is a join, not work:
// a writer blocked on the stream fails the moment the write deadline moves
// into the past, and one blocked on the body ends by itself when the
// handler returns and net/http closes it.
const DefaultWriteAbortGrace = 2 * time.Second

// Proxy routes each request either to the application or to the laptop of
// the user whose name and token it carries.
type Proxy struct {
	Agent   *Agent
	AppAddr string
	Logf    func(string, ...any)
	// StealTimeout overrides DefaultStealTimeout. Zero means the default.
	StealTimeout time.Duration
	// WriteAbortGrace overrides DefaultWriteAbortGrace. Zero means the
	// default, capped by StealTimeout.
	WriteAbortGrace time.Duration
}

// Handler is the http.Handler the ALB talks to.
func (p *Proxy) Handler() http.Handler {
	app := p.appProxy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An upgrade never gets stolen: the steal path is one HTTP/1.1
		// request and its response over a yamux stream, and a hijacked
		// connection is not that. There is no way to carry a websocket
		// through it at all, so the application serves it (spec §5.1).
		if r.Header.Get("Upgrade") != "" {
			app.ServeHTTP(w, r)
			return
		}
		// MatchRequest, not Match: the entry point supplies the header
		// lookup itself so that this caller cannot get the case-folding
		// wrong. Indexing r.Header here would keep working for canonically
		// spelled configured names and silently stop matching a lowercase
		// incoming.match.header, which internal/config accepts verbatim.
		//
		// nil is the common case, not the error case: every ALB health
		// check and every request from someone who is not attached lands
		// here and goes to the application.
		s := p.Agent.MatchRequest(r)
		if s == nil {
			app.ServeHTTP(w, r)
			return
		}
		p.steal(w, r, s, app)
	})
}

// appProxy is the pass-through: the application on AppAddr, reached for
// everything nobody has claimed.
func (p *Proxy) appProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = p.AppAddr
			// pr.Out.Host is left alone on purpose: it is the Host header
			// the ALB sent, and an application may route on it.
			keepForwarded(pr)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The default handler logs to the standard logger; the agent's
			// lines all go through logf so that one stream in CloudWatch
			// tells the whole story.
			p.logf("app %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
}

// forwardedHeaders are the headers httputil.ReverseProxy deletes from the
// outbound request whenever Rewrite is set, on the assumption that Rewrite
// will put back whatever this hop should claim.
//
// tetherd is not the hop that owns them. The ALB has already written
// X-Forwarded-For and friends, and both the application and the laptop must
// see exactly what it wrote (spec §5.1) - the developer debugging a stolen
// request is looking at the real client's address. So they are copied back
// verbatim, and SetXForwarded is deliberately not used: it would append the
// ALB's own address as though the agent were a further proxy the client had
// passed through.
//
// Doing nothing here is not "pass through unchanged". It is deletion, which
// is easy to conclude otherwise from Rewrite's documentation alone.
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

func keepForwarded(pr *httputil.ProxyRequest) {
	for _, name := range forwardedHeaders {
		if v := pr.In.Header.Values(name); len(v) > 0 {
			pr.Out.Header[name] = append([]string(nil), v...)
		}
	}
}

// steal forwards one request to a laptop, falling back to the application
// if the request never reached it.
func (p *Proxy) steal(w http.ResponseWriter, r *http.Request, s *Session, app http.Handler) {
	// Buffer the body before anything reads it: once the steal attempt has
	// consumed it, falling back would hand the application a request with
	// an empty body, which is worse than either honest answer.
	replay, tooBig, err := bufferBody(r)
	if err != nil {
		// Log s.User, never s itself and never the token: this line goes to
		// the agent's stdout, which is CloudWatch, which the developer's
		// colleagues can read.
		p.logf("steal %s %s for %q: reading the request body: %v", r.Method, r.URL.Path, s.User, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			// The authority routes nothing - the stream already goes to
			// exactly one laptop - but it has to be something valid.
			// pr.Out.Host still carries the ALB's Host header, which is
			// what the developer's handler should see.
			pr.Out.URL.Host = "tetherd"
			keepForwarded(pr)
			// The body is buffered, so there is nothing left to negotiate,
			// and a "100 Continue" in front of the reply would be read as
			// the reply by streamTransport's own ReadResponse.
			pr.Out.Header.Del("Expect")
		},
		Transport: &streamTransport{open: s.open, user: s.User, timeout: p.stealTimeout(), grace: p.writeAbortGrace()},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// The request an ErrorHandler is handed is the outbound clone,
			// not the request that arrived, so the fallback replays the one
			// captured here instead.
			var un unserved
			if !errors.As(err, &un) {
				// The laptop had the request. Its silence, its error or its
				// half-written reply is its own answer: replaying a POST to
				// the application could run it a second time.
				p.logf("steal %s %s for %q: %v", r.Method, r.URL.Path, s.User, err)
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			if tooBig {
				p.logf("steal %s %s for %q: %v, and the body is larger than %d bytes so it cannot be replayed: 502",
					r.Method, r.URL.Path, s.User, err, MaxReplayBody)
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			p.logf("steal %s %s for %q: %v; serving this request from the application", r.Method, r.URL.Path, s.User, err)
			r.Body = io.NopCloser(bytes.NewReader(replay))
			r.ContentLength = int64(len(replay))
			r.TransferEncoding = nil
			app.ServeHTTP(w, r)
		},
	}
	rp.ServeHTTP(w, r)
}

// bufferBody reads up to MaxReplayBody+1 bytes so that the request can be
// replayed to the application. tooBig reports that it did not fit, in which
// case r.Body is left streaming what was read plus the rest, and a failure
// to reach the laptop cannot be recovered.
func bufferBody(r *http.Request) (replay []byte, tooBig bool, err error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, false, nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, MaxReplayBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(buf) > MaxReplayBody {
		// Put what was read back in front of the rest and stream it: the
		// steal itself still works for a large body, only the fallback
		// does not.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
		return nil, true, nil
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	// The length is now exact, so say so rather than re-chunking a request
	// that arrived chunked.
	r.TransferEncoding = nil
	return buf, false, nil
}

// unserved marks a failure that happened before the laptop could have acted
// on the request: the stream would not open, the stream header could not be
// written, or the CLI refused the stream outright. Only these are
// replayable - see the ErrorHandler in steal.
type unserved struct{ err error }

func (u unserved) Error() string { return u.err.Error() }
func (u unserved) Unwrap() error { return u.err }

// streamTransport sends one request over one freshly opened yamux stream
// and reads its reply. One stream per request, because a yamux stream is
// cheap and reusing one would serialise a laptop's requests behind each
// other; the stream is closed when the response body is closed.
//
// It speaks the exchange by hand rather than through http.Transport for two
// reasons: the reply has to be bounded by a deadline of the caller's own
// (session.Opener says so, and a CLI that never answers is a measured
// case), and the CLI may refuse the stream with a one-line proto.Error
// instead of an HTTP response, which has to be told apart from a reply
// rather than reported as a protocol violation.
type streamTransport struct {
	open    session.Opener
	user    string
	timeout time.Duration
	grace   time.Duration
}

func (t *streamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.open == nil {
		// MatchSession skips a session with no Opener, so this is a guard
		// rather than a reachable state - but it is the one that would be a
		// nil dereference in the request path.
		return nil, unserved{fmt.Errorf("the session for %q has no way back to its CLI", t.user)}
	}
	c, err := t.open.OpenStream()
	if err != nil {
		// Normal, not exceptional: the session may have detached between
		// the registry handing out this Opener and this moment.
		return nil, unserved{fmt.Errorf("open a stream to %q: %w", t.user, err)}
	}
	if c == nil {
		// session.Opener promises a nil net.Conn on error, so this means a
		// nil conn with a nil error - an Opener that does not keep the
		// contract. Refusing it here beats a panic in the request path.
		return nil, unserved{fmt.Errorf("open a stream to %q: the session returned no stream", t.user)}
	}
	// A client that goes away must not leave the stream open until the
	// deadline: the request context is cancelled then, and also when the
	// handler returns, which is why the response body stops this.
	stopOnCancel := context.AfterFunc(r.Context(), func() { c.Close() })
	var written *pendingWrite
	handedOff := false
	defer func() {
		if handedOff {
			return
		}
		stopOnCancel()
		c.Close()
		stopWrite(c, written, t.grace)
	}()

	deadline := time.Now().Add(t.timeout)
	c.SetWriteDeadline(deadline)
	// The CLI reads this one line before handing the stream to its handler,
	// so it has to be written before the request; a request line read as a
	// stream header is a closed stream.
	if err := proto.NewEncoder(c).Encode(proto.TypeHTTP, proto.HTTPHeader{User: t.user}); err != nil {
		return nil, unserved{fmt.Errorf("write the http stream header to %q: %w", t.user, err)}
	}
	// From here on the CLI may already have handed the request to the
	// developer's handler, so nothing below is replayable.
	//
	// The request is written on its own goroutine while the reply is read
	// here, which is not an optimisation: an HTTP peer is allowed to answer
	// before it has read the body (a 413, a redirect), and the CLI refuses
	// a stream it cannot serve as soon as it has read the stream header,
	// without reading the request at all. Writing to completion first
	// deadlocks against either, and a deadlock on the ALB's data path is an
	// outage.
	//
	// The write carries no deadline of its own on purpose: a large upload
	// may legitimately take longer than the bound on the reply, and a
	// deadline here would cut off an upload the laptop is still reading.
	// Every path that gives up on the reply therefore aborts the write
	// explicitly through stopWrite before joining it - never by relying on
	// the deferred cleanup, which runs only after this function has
	// returned and so cannot unblock anything this function waits for.
	c.SetWriteDeadline(time.Time{})
	written = &pendingWrite{done: make(chan error, 1)}
	go func() { written.done <- r.Write(c) }()

	c.SetReadDeadline(deadline)
	br := bufio.NewReader(c)
	if err := readRefusal(br, t.user); err != nil {
		return nil, err
	}
	resp, err := readFinalResponse(br, r, t.user)
	if err != nil {
		// A request that never got out is the more useful half of the
		// story, so ask whether the write has already failed on its own
		// before aborting it - an abort of ours would otherwise be the
		// error reported. It is still not replayable either way: the CLI
		// took the stream and may have acted on what did arrive.
		werr, done := written.wait(0)
		if !done {
			// And this is where the request path would otherwise park for
			// good. The write has no deadline, the peer is not reading, and
			// the deferred close cannot help because it runs after this
			// return - so abort the write here and bound the join.
			stopWrite(c, written, t.grace)
			werr = nil
		}
		if werr != nil {
			return nil, fmt.Errorf("write the request to %q: %w", t.user, werr)
		}
		return nil, fmt.Errorf("read the reply from %q: %w", t.user, err)
	}
	if resp.StatusCode == http.StatusBadGateway && resp.Header.Get(proto.NoListenerHeader) != "" {
		// The developer's own server is not running. The CLI says so on a
		// header it sets only when the dial to the local port failed, so
		// nothing can have executed and this request is replayable: serve
		// it from the task exactly as if no session had matched
		// (docs/e2e-aws.md row 24). A 502 without the header is relayed -
		// it may be the developer's application answering, and running a
		// POST a second time is worse than a 502.
		resp.Body.Close()
		return nil, unserved{fmt.Errorf("nothing is listening on %q's own port", t.user)}
	}
	// Internal to the CLI-agent hop, and this response leaves through a
	// public ALB. Stripping it is unconditional rather than tied to the
	// rule above, so that narrowing that rule can never start leaking it.
	resp.Header.Del(proto.NoListenerHeader)
	// The headers are in; the body may legitimately take as long as it
	// takes.
	c.SetReadDeadline(time.Time{})
	resp.Body = &streamBody{ReadCloser: resp.Body, stream: c, stop: stopOnCancel, written: written, grace: t.grace}
	handedOff = true
	return resp, nil
}

// maxInformationalResponses bounds how many 1xx replies the CLI may send
// before the final one. A CLI streaming them without end would otherwise
// hold this request until the read deadline with no way to say why.
const maxInformationalResponses = 8

// readFinalResponse reads the CLI's reply, discarding informational (1xx)
// responses until the final one.
//
// net/http's own client does this; this transport speaks the exchange by
// hand, so it has to do it too. A local app answering 103 Early Hints and
// then 200 is ordinary - the CLI serves a stolen request with a reverse
// proxy of its own, which forwards informational responses - and relaying
// the 103 as the answer would hand the browser an empty reply and leave the
// real response unread on the stream.
//
// 101 is the exception. A switch of protocols cannot be carried over one
// request/response on a yamux stream, which is why an Upgrade request never
// reaches the steal path at all (see Handler). One arriving here is a reply
// this transport cannot finish, and it is not replayable either - the
// request reached the developer's process and may have acted on it - so it
// is an error, which steal turns into a 502 rather than a second run
// against the application.
func readFinalResponse(br *bufio.Reader, r *http.Request, user string) (*http.Response, error) {
	for seen := 0; ; seen++ {
		resp, err := http.ReadResponse(br, r)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 100 || resp.StatusCode > 199 {
			return resp, nil
		}
		// A 1xx carries no body (net/http gives it http.NoBody), but close
		// it rather than depend on that.
		resp.Body.Close()
		if resp.StatusCode == http.StatusSwitchingProtocols {
			return nil, fmt.Errorf("the CLI of %q answered 101: an upgrade cannot be carried over a stolen request", user)
		}
		if seen+1 >= maxInformationalResponses {
			return nil, fmt.Errorf("the CLI of %q sent %d informational responses without a final one", user, seen+1)
		}
	}
}

// stopWrite aborts the goroutine writing the request and joins it, bounded.
//
// The abort is an immediate write deadline, which fails a write that is in
// flight straight away; closing the stream does that too for yamux, but the
// deadline makes it true of every net.Conn and costs nothing. The bound is
// for the case neither reaches: the writer may be blocked *reading the
// inbound request body* - which happens whenever the body was too large to
// buffer, so r.Body is still the ALB's connection - and that read belongs
// to the server, not to us. Waiting on it without a bound would park the
// request path for as long as the client cared to stay silent. Such a
// goroutine ends by itself moments later, when the handler returns and
// net/http closes the request body.
func stopWrite(c net.Conn, written *pendingWrite, grace time.Duration) {
	if written == nil {
		return
	}
	c.SetWriteDeadline(time.Now())
	written.wait(grace)
}

// pendingWrite is the outcome of writing the request, which happens on its
// own goroutine. wait may be called more than once. It needs no lock: every
// caller runs on the goroutine that called RoundTrip - either inside
// RoundTrip itself, or later in streamBody.Close, which the same goroutine
// reaches through httputil.ReverseProxy - and the two paths are exclusive
// (a handed-off response never waits inside RoundTrip).
type pendingWrite struct {
	done chan error
	err  error
	got  bool
}

// wait joins the write for at most grace, and reports whether it finished.
// There is deliberately no unbounded form: this runs on the goroutine
// serving an ALB request, and the writer can be blocked on a read that
// nothing here is able to abort (see stopWrite).
func (p *pendingWrite) wait(grace time.Duration) (err error, done bool) {
	if p.got {
		return p.err, true
	}
	if grace <= 0 {
		select {
		case e := <-p.done:
			p.err, p.got = e, true
			return p.err, true
		default:
			return nil, false
		}
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case e := <-p.done:
		p.err, p.got = e, true
		return p.err, true
	case <-timer.C:
		return nil, false
	}
}

// readRefusal reads a stream-level refusal, if that is what the CLI sent
// instead of a response.
//
// The CLI answers an http stream it cannot serve with one JSON line
// (proto.Error) and closes; that is not something http.ReadResponse can
// parse, and reporting it as a malformed reply would turn an expected
// refusal into a 502. The two are told apart by their first byte - a JSON
// object starts with '{', an HTTP status line with 'H' - which costs one
// byte of lookahead on the buffered reader the response is then read from.
func readRefusal(br *bufio.Reader, user string) error {
	first, err := br.Peek(1)
	if err != nil {
		// Nothing came back at all: the deadline fired, or the CLI closed
		// the stream without answering (its handler panicked, say). Either
		// way the request may have been acted on, so this is not unserved.
		return fmt.Errorf("waiting for the reply from %q: %w", user, err)
	}
	if first[0] != '{' {
		return nil
	}
	typ, raw, err := proto.ReadHeader(br)
	if err != nil {
		return fmt.Errorf("reading the refusal from %q: %w", user, err)
	}
	if typ != proto.TypeError {
		return fmt.Errorf("the CLI of %q answered with a %q line instead of a reply", user, typ)
	}
	var e proto.Error
	proto.Unmarshal(raw, &e)
	// A refusal is sent before the CLI runs anything, so the request can
	// still be served by the application - which is the point of having a
	// code on the wire at all (see proto.CodeNoIncoming). The code is
	// compared first and the message is the fallback, never the other way
	// round.
	switch e.Code {
	case proto.CodeNoIncoming:
		return unserved{fmt.Errorf("the CLI of %q is not accepting incoming requests", user)}
	default:
		msg := e.Message
		if msg == "" {
			msg = "the CLI did not say why"
		}
		// Most likely an agent newer than the CLI: the CLI did not
		// recognise the http stream type at all. Still replayable, and
		// worth saying out loud rather than hiding behind a 502.
		return unserved{fmt.Errorf("the CLI of %q refused the http stream (%s): %s", user, e.Code, msg)}
	}
}

// streamBody ties a stolen response to the stream it arrived on: the stream
// carries exactly one request, so closing the body closes it. Forgetting
// that leaks a yamux stream per request at both ends.
type streamBody struct {
	io.ReadCloser
	stream  net.Conn
	stop    func() bool
	written *pendingWrite
	grace   time.Duration
}

func (b *streamBody) Close() error {
	b.stop()
	err := b.ReadCloser.Close()
	// The stream itself, not just the reader over it. http.ReadResponse
	// builds a body that reads from a bufio.Reader and knows nothing about
	// the conn underneath, so closing the body closes nothing on the wire:
	// without this the CLI's http.Server sits waiting for a second request
	// that never comes, one goroutine and one yamux stream per stolen
	// request, at both ends. Nor does stopOnCancel cover it - stop() has
	// just disarmed it, and it is disarmed on purpose, because a response
	// body outlives the handler only when the caller is still reading it.
	if cerr := b.stream.Close(); err == nil {
		err = cerr
	}
	// The goroutine still writing the request has to be stopped here too:
	// a laptop may answer a 413 or a redirect long before it has read the
	// upload. Joining it is what makes "nothing is still reading the
	// inbound request body" true on this path, which matters because the
	// server reuses that connection as soon as the handler returns - and
	// the join is bounded for the same reason it is everywhere else.
	stopWrite(b.stream, b.written, b.grace)
	return err
}

// writeAbortGrace is how long the proxy joins the goroutine writing a
// stolen request after aborting it. It never exceeds the bound already
// placed on the whole exchange: a proxy told to give a laptop 200ms has not
// agreed to spend two seconds tidying up afterwards.
func (p *Proxy) writeAbortGrace() time.Duration {
	if p.WriteAbortGrace > 0 {
		return p.WriteAbortGrace
	}
	if t := p.stealTimeout(); t < DefaultWriteAbortGrace {
		return t
	}
	return DefaultWriteAbortGrace
}

func (p *Proxy) stealTimeout() time.Duration {
	if p.StealTimeout > 0 {
		return p.StealTimeout
	}
	return DefaultStealTimeout
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// ProxyDrainTimeout is how long ServeProxy lets requests already in flight
// finish after ctx is done, before the ALB's port is dropped regardless.
//
// It is comfortably inside the ECS task's own stop timeout, so a rolling
// deployment finishes these requests instead of answering them with a 502
// the developer reads as a bug in their code.
const ProxyDrainTimeout = 5 * time.Second

// ServeProxy serves the ALB's port until ctx is done, then drains.
func (a *Agent) ServeProxy(ctx context.Context, ln net.Listener) error {
	p := &Proxy{Agent: a, AppAddr: a.cfg.AppAddr, Logf: a.logf}
	srv := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 20 * time.Second}
	drained := make(chan error, 1)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), ProxyDrainTimeout)
		defer cancel()
		drained <- srv.Shutdown(sctx)
	}()
	err := srv.Serve(ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("agent: proxy: %w", err)
	}
	// Shutdown closes the listener first, so Serve returns the moment the
	// drain *starts*. Returning here would hand the caller a stopped proxy
	// while requests were still being served, and main exits on that - the
	// drain has to be joined, or "graceful" only describes a goroutine
	// nobody waits for.
	if ctx.Err() == nil {
		return nil // Serve stopped on its own; there is no drain to join.
	}
	if derr := <-drained; derr != nil {
		// Worth a line, not a non-zero exit: the task is being stopped
		// either way, and a Fatal here would read in CloudWatch as a crash.
		a.logf("proxy: requests still in flight after %s: %v", ProxyDrainTimeout, derr)
	}
	return nil
}
