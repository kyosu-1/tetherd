// The steal receiver. The agent opens one yamux stream per stolen request
// and speaks plain HTTP/1.1 on it; the CLI hands each stream to net/http so
// that chunked bodies, Expect: 100-continue, keep-alive and header parsing
// are the standard library's problem, and proxies the result to the
// developer's own process on 127.0.0.1:<local_port> (spec §5.2).
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// The header names the agent matches a request against when
// incoming.match leaves them out. internal/config applies no defaults of
// its own - incoming.match is free text there - and the agent treats an
// empty header name as "matches nothing": it says so once at attach
// (internal/agent/registry.go's incomingGap, logged from register) and then
// sends every request to the application for the life of the session. So
// this is the only place the defaults can live: whatever ends up in
// proto.Hello is what the agent will compare against.
const (
	DefaultMatchHeader      = "X-Dev-User"
	DefaultMatchTokenHeader = "X-Dev-Token"
)

// DefaultLocalPort is where a stolen request goes when neither
// --local-port nor incoming.local_port names a port (docs/config.md).
//
// Steal being on by default is what --no-incoming exists to turn off: the
// agent is on the ALB's data path whether or not anyone is attached, and a
// request only ever leaves it if it carries both this developer's name and
// their token - which nothing but their own browser extension sends. A
// developer who has not started a server on this port gets 502s for their
// own headered requests and nothing else changes.
const DefaultLocalPort = 8080

// StealToken is a developer's steal token: the value the agent compares
// X-Dev-Token against, and the only thing standing between a public ALB and
// this laptop (spec §11).
//
// It is a named type purely so that it cannot be printed by accident.
// RunOptions is a plain struct that any future `logf("%+v", opts)` would
// render field by field, and fmt calls String on an exported field of a
// struct it is rendering - so redacting here redacts everywhere the token
// is carried, including EnvOptions and DoctorOptions, without hijacking the
// formatting of the structs themselves. Use string(t) at the one place the
// real value is needed (the hello message).
type StealToken string

// String redacts the token under %v, %s and %q.
func (t StealToken) String() string {
	if t == "" {
		return ""
	}
	return "<redacted>"
}

// GoString redacts it under %#v too, which does not go through String.
func (t StealToken) GoString() string {
	if t == "" {
		return `""`
	}
	return `"<redacted>"`
}

// errNoStealToken is a startup failure, not a warning. A session that
// advertises incoming requests with no token is one the agent can never
// match: it would attach, print a green line and then leave every request
// with the application forever, with the only clue in the agent's log on
// the task. Refusing here - before any AWS round trip - is what turns that
// into something the developer can read on their own terminal.
var errNoStealToken = errors.New("this machine has no steal token, so the agent could never match a request to it" +
	"\n        tetherd writes a fresh `token:` into ~/.tetherd/config.yml on first run; remove the empty" +
	"\n        one there and run again, or pass --no-incoming to leave every request with the application")

// stealConfig is the resolved steal settings: what the agent must match to
// send a request here, and where it goes when it does.
type stealConfig struct {
	// Incoming is exactly what travels in proto.Hello. Enabled false means
	// this session takes nothing, and every other field is then zero.
	Incoming proto.Incoming
	// LocalPort is the developer's own process, after the default has been
	// applied - never opts.LocalPort, which may be zero.
	LocalPort int
}

// stealSettings decides whether this run accepts stolen requests and what
// the agent has to match to send one. It is the only place any of the three
// defaults is applied: internal/config holds none (docs/config.md says so),
// and the agent treats an empty header name as "matches nothing" - it logs
// that gap once at attach (internal/agent/registry.go's incomingGap) and
// then sends every request to the application.
//
// Because this function always fills both header names and refuses an empty
// token, that warning is unreachable from this CLI: only a hand-built
// proto.Hello - an older or third-party client - can trip it.
//
// The returned Incoming is what the agent matches against, so it is matched
// literally: the token comparison is crypto/subtle.ConstantTimeCompare and
// the user name is compared byte for byte, which makes "Shota" and "shota"
// two different sessions.
//
// Every failure here is a usageError. A value the developer gave is wrong
// in a way no retry can fix, and it is decided before the first AWS call.
func stealSettings(opts RunOptions) (stealConfig, error) {
	if opts.NoIncoming {
		return stealConfig{}, nil
	}
	port := opts.LocalPort
	if port == 0 {
		port = DefaultLocalPort
	}
	if port < 0 || port > 65535 {
		return stealConfig{}, usageError{fmt.Errorf("local port %d is not a port number (1-65535); check --local-port or incoming.local_port", port)}
	}
	if opts.Token == "" {
		return stealConfig{}, usageError{fmt.Errorf("refusing to take requests for %q on localhost:%d: %w", opts.User, port, errNoStealToken)}
	}
	in := proto.Incoming{
		Enabled:     true,
		Header:      opts.MatchHeader,
		TokenHeader: opts.MatchTokenHeader,
	}
	if in.Header == "" {
		in.Header = DefaultMatchHeader
	}
	if in.TokenHeader == "" {
		in.TokenHeader = DefaultMatchTokenHeader
	}
	return stealConfig{Incoming: in, LocalPort: port}, nil
}

// The bounds one stolen stream runs under.
//
// ReadHeaderTimeout applies only to the request line and headers, which the
// agent writes immediately after opening the stream; net/http clears the
// read deadline once they are in, so what the developer's process does
// after that is unbounded here - a report that takes a minute, an SSE
// stream that sits idle between events. Those are the reachable cases and
// the reason this is not a whole-stream ReadTimeout.
//
// An upgraded stream would want the same, but the agent sends every Upgrade
// request to the application instead of stealing it (internal/agent's
// Proxy.Handler), so a stolen websocket cannot arrive here today. Do not
// turn this into a whole-stream bound on the belief that it could not
// matter: the streaming cases above make it matter without any upgrade.
//
// IdleTimeout is a backstop, not a negotiation with the other end. An
// earlier version of this comment said it had to exceed
// net/http.Transport's IdleConnTimeout because "the agent's proxy pools its
// streams"; it does not. internal/agent's streamTransport is a hand-written
// RoundTripper that opens one fresh yamux stream per request and closes it
// with the response, so there is no pool on either side of this stream and
// no number here to keep in step with one. What IdleTimeout actually
// bounds is a stream the agent opened and then left open without sending a
// second request - which nothing does today, so it should never fire. The
// 90s figure in the old comment was net/http's default for a pool that is
// not in play; the transport further down now has no pool of its own
// either, deliberately (see DisableKeepAlives).
var (
	stealReadHeaderTimeout = 20 * time.Second
	stealIdleTimeout       = 120 * time.Second
	stealDialTimeout       = 5 * time.Second
)

// StealServer serves the http streams the agent opens, proxying each one to
// the developer's own process.
type StealServer struct {
	LocalPort int
	Logf      func(string, ...any)

	once sync.Once
	tr   *http.Transport
}

// Serve takes ownership of one stream: it reads HTTP/1.1 from it and
// proxies to 127.0.0.1:LocalPort until the stream ends, then closes it.
//
// The stream arrives with its proto header already consumed and its read
// deadline cleared (see session.Options.OnHTTP), so there is nothing to
// read before the request line and no deadline to undo.
func (s *StealServer) Serve(stream net.Conn) {
	// Every exit closes the stream, and this is the only place that does.
	// net/http and net/http/httputil both close the connection they were
	// handed when they are finished with it, but what they were handed is a
	// doneConn, whose Close only reports that they are finished (see
	// below) - so without this line a stolen request would leave a yamux
	// stream open for the life of the session, one per request, and the
	// agent's proxy would keep offering it to the next request and wait out
	// its own deadline instead of reaching the application.
	defer stream.Close()
	c := &doneConn{Conn: stream, done: make(chan struct{})}
	// One stream is one connection, not a listener. http.Server.Serve
	// returns as soon as it has handed the connection to its own goroutine,
	// so the listener's second Accept is what waits for it to finish:
	// returning any earlier would close the stream from under the request -
	// and for a streaming response (SSE, a slow report written in pieces)
	// that means from under bytes the developer's process is still writing.
	// An upgraded stream, which lives for as long as the developer keeps it
	// open, would need the same; the agent does not steal upgrades today
	// (internal/agent's Proxy.Handler sends them to the application).
	s.server().Serve(&oneConnListener{c: c})
}

// server is the bounds and the logging one stolen stream runs under. It is
// its own method so that a test can see what Serve would be given: the
// wiring here is not visible in the behaviour of a single request.
func (s *StealServer) server() *http.Server {
	return &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: stealReadHeaderTimeout,
		IdleTimeout:       stealIdleTimeout,
		// net/http logs a panic in a handler, a malformed request and a
		// failed protocol switch through here. Without this they go to the
		// standard logger - timestamped, straight to stderr, around the
		// developer's own child process output - and a panic in this file
		// would otherwise be invisible in the CLI's own log.
		ErrorLog: s.errorLog(),
	}
}

// forwardedHeaders are the headers that say who the original caller was and
// what they asked for. The ALB sets them; every hop after it (the agent's
// proxy, this stream) is tetherd's own plumbing and has no business
// appending itself.
var forwardedHeaders = []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"}

func (s *StealServer) handler() http.Handler {
	addr := s.addr()
	rp := &httputil.ReverseProxy{
		Transport: s.transport(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = addr
			// The request must look to the developer's process the way it
			// looked when it arrived at the ALB. Setting Rewrite (rather
			// than Director) makes net/http delete the inbound Forwarded
			// and X-Forwarded-* headers from the outbound request, on the
			// assumption that the proxy will set its own - so an
			// application that logs the caller's IP would log nothing
			// unless they are copied back across.
			//
			// Measured, not assumed, about the Host: net/http leaves
			// Out.Host as request.Clone left it, which is the inbound one,
			// so the line below is belt and braces rather than a
			// correction. It is here because pr.SetURL - which is what a
			// later hand will reach for instead of assigning URL fields -
			// does blank Out.Host, and an application that routes on Host
			// would then see "127.0.0.1:3000".
			pr.Out.Host = pr.In.Host
			for _, h := range forwardedHeaders {
				for _, v := range pr.In.Header.Values(h) {
					pr.Out.Header.Add(h, v)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// proto.NoListenerHeader is this hop's own claim about its own
			// dial (see the error handler below). The developer's process
			// must not be able to make that claim on its behalf: a
			// response that carries the header would otherwise tell the
			// agent to replay a request that has already reached an
			// application and may have acted on it.
			resp.Header.Del(proto.NoListenerHeader)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// errNoListener means the dial to the developer's own process
			// failed, so not one byte of this request reached any
			// application and nothing can have acted on it. That, and only
			// that, is what proto.NoListenerHeader claims: the agent
			// answers it by throwing this response away and serving the
			// request from the application instead, as if no session had
			// matched.
			//
			// Do not widen this. Every other way through here - a timeout
			// part way into the response, a read error after the app
			// accepted the connection, a failed protocol switch, a 502 the
			// app produced itself - means the request did reach an
			// application, and replaying it would run a POST twice. Those
			// 502s are relayed to the caller unchanged, which is why the
			// distinction has to be explicit and has to be made here: this
			// is the only side that knows whether it ever connected.
			//
			// The agent keys on the header paired with a 502, so the
			// status on this path is part of the agreement too: answering
			// a failed dial with anything else turns the fallback off.
			if errors.Is(err, errNoListener) {
				w.Header().Set(proto.NoListenerHeader, "1")
				// The developer's own terminal is the only place this is
				// visible - the caller gets the application's answer - so
				// the line has to name the port nothing is listening on
				// (docs/e2e-aws.md row 24).
				s.logf("✗ steal    %s %s  502  nothing is listening on %s", r.Method, r.URL.Path, addr)
				http.Error(w, fmt.Sprintf("tetherd: nothing is listening on %s", addr), http.StatusBadGateway)
				return
			}
			s.logf("✗ steal    %s %s  502  %s failed part way through: %v", r.Method, r.URL.Path, addr, err)
			http.Error(w, fmt.Sprintf("tetherd: %s did not answer (%v)", addr, err), http.StatusBadGateway)
		},
	}
	return s.logging(rp)
}

// errNoListener marks a dial failure, so the error handler can tell it
// apart from every later failure without reading the error's text. It is
// wrapped around the dialer's own error where the dial happens, which is
// the only place that knows.
//
// The header it ends up as is proto.NoListenerHeader, spelled once in
// internal/proto and read from there by both ends: a typo in a literal on
// either side would turn row 24's fallback off silently, with no error
// anywhere.
var errNoListener = errors.New("nothing is listening on the local port")

// transport dials the developer's process and nothing else. A fixed
// DialContext rather than http.DefaultTransport: the destination is decided
// by --local-port, not by the address in the URL, and the developer's
// HTTP_PROXY has no say in how their own laptop is reached.
func (s *StealServer) transport() http.RoundTripper {
	s.once.Do(func() {
		addr := s.addr()
		s.tr = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := (&net.Dialer{Timeout: stealDialTimeout}).DialContext(ctx, "tcp", addr)
				if err != nil {
					// The dial is the one failure the agent may safely
					// replay, and this is the only place that can tell a
					// failed dial from a connection that was made and then
					// went wrong. Marking it here rather than matching on
					// the error's text in the handler is what keeps the
					// two apart (see the error handler above).
					return nil, fmt.Errorf("%w: %w", errNoListener, err)
				}
				return c, nil
			},
			// A proxy must not add an Accept-Encoding the caller did not
			// send: the response would come back decompressed with the
			// header stripped, and a body the application meant to be gzip
			// is not this hop's decision.
			DisableCompression: true,
			// No connection pool, and this is a correctness requirement
			// rather than a tuning choice.
			//
			// errNoListener is attached per dial, but the claim it makes -
			// proto.NoListenerHeader, "nothing reached an application, so
			// replay this" - is about the request. Those two come apart the
			// moment net/http retries: Transport.roundTrip retries a
			// request whose connection turns out to be gone, and
			// persistConn.shouldRetryRequest allows that for a replayable
			// request (any GET, or a POST with no body and an
			// Idempotency-Key) once the connection has been reused. If the
			// retry's fresh dial then fails, the error handler is handed a
			// dial error - and would stamp the replay claim on a request
			// the developer's live application had already read in full on
			// the first attempt. The agent would run it a second time
			// against the deployed application. A server shutting down is
			// exactly when that happens, and exactly when steal is in use.
			//
			// With keep-alives off the invariant holds again: a request
			// handed a dial error has never been written to any
			// connection. Either it was waiting for its first one, or the
			// only retry net/http has left is the nothingWrittenError case
			// - and that one is sound by construction, because nothing was
			// written on the earlier attempt either.
			//
			// It costs nothing. The agent opens one fresh yamux stream per
			// stolen request, so there is at most one request in flight
			// here and there was never a pool on the other side for this
			// one to amortise.
			DisableKeepAlives: true,
		}
	})
	return s.tr
}

func (s *StealServer) addr() string { return fmt.Sprintf("127.0.0.1:%d", s.LocalPort) }

// logging prints one line per stolen request: the CLI is where the
// developer is looking, and the agent deliberately sends no logs of its own
// (spec §5.2).
func (s *StealServer) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logf("← %-6s %s  %d  %s  (from %s)", r.Method, r.URL.Path, rec.status,
			time.Since(started).Round(time.Millisecond), firstForwardedFor(r))
	})
}

// statusRecorder remembers what was answered so the log line can say so.
//
// It has to pass Flush and Hijack through, not just WriteHeader.
//
// Flush is the reachable one: httputil calls it to push bytes out as the
// developer's process writes them, which is what a streaming response (SSE,
// a slow report) needs to arrive in pieces rather than at the end.
//
// Hijack is for a protocol switch: a ReverseProxy serving one asks the
// ResponseWriter for the raw connection, and one that cannot give it up is
// answered with 502 by the error handler instead. The agent sends every
// Upgrade request to the application rather than stealing it
// (internal/agent's Proxy.Handler), so no switch reaches this wrapper
// today - this is what would make one work the day one does.
//
// Measured, so that the next person weighing up which of these methods
// earns its place has the real answer: httputil asks through
// http.ResponseController, which tries Hijack on this wrapper and then
// follows Unwrap, so either method alone is enough to keep a switch
// working and dropping both is what turns one into a bad gateway. Hijack
// is still needed on its own account - it is the only place the 101 can be
// recorded, because httputil writes that response onto the raw connection
// where WriteHeader never sees it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Hijack hands over the underlying connection, recording the 101 that got
// us here: net/http/httputil only hijacks after the developer's process has
// answered "101 Switching Protocols", and it writes that response onto the
// raw connection itself, where WriteHeader above never sees it.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	if !r.wrote {
		r.status, r.wrote = http.StatusSwitchingProtocols, true
	}
	return hj.Hijack()
}

// Unwrap is what http.ResponseController follows for everything this
// wrapper does not implement itself, and it is doing real work rather than
// satisfying a convention: net/http/httputil flushes a streamed response
// (server-sent events, a slow chunked body) with
// http.NewResponseController(rw).Flush() and nothing else, so without
// Unwrap a stolen event stream would sit in the buffer until the handler
// returned. Write and read deadlines, and any control method a later Go
// adds, arrive the same way.
//
// A hand-written Flush here would be dead code for the same reason - the
// controller would never reach it, because it tries this wrapper's own
// methods first and Unwrap only after.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// firstForwardedFor is the original caller: the ALB appends, so the first
// entry is the client.
func firstForwardedFor(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return "unknown"
	}
	if i := strings.IndexByte(xff, ','); i >= 0 {
		return strings.TrimSpace(xff[:i])
	}
	return strings.TrimSpace(xff)
}

// oneConnListener presents one already-accepted connection as a Listener,
// so net/http can serve a yamux stream. The second Accept blocks until that
// connection is closed - by net/http when the request is done, or by
// net/http/httputil when a hijacked protocol switch finishes copying - and
// that is what keeps Serve from returning while the stream is still in use.
type oneConnListener struct {
	c      *doneConn
	handed bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.handed {
		<-l.c.done
		return nil, io.EOF
	}
	l.handed = true
	return l.c, nil
}

// Close is a no-op: the stream belongs to Serve, which closes it, and
// net/http closes the listener it was handed as soon as Accept fails.
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

// doneConn reports when net/http is finished with the connection.
//
// Close does not close the stream: the stream belongs to Serve, which is
// the OnHTTP contract (the callback owns it, including closing it), and
// Close here only reports that net/http is finished with it - net/http
// closes what it was handed at the end of a connection, and
// net/http/httputil closes a hijacked one when the copying stops.
//
// Measured: passing Close through to the stream as well would end up with
// the stream closed too, because Serve closes it on the way out either
// way. Not passing it through is what keeps one reader of this file from
// having to work that out - the stream is closed in exactly one place -
// and it is the reason the deferred close in Serve is load-bearing rather
// than a second belt.
type doneConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *doneConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (s *StealServer) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// errorLog routes net/http's own complaints into the CLI's log, or leaves
// them on the standard logger when there is nowhere better to put them.
func (s *StealServer) errorLog() *log.Logger {
	if s.Logf == nil {
		return nil
	}
	return log.New(logfWriter{logf: s.Logf}, "", 0)
}

// logfWriter adapts a logf to io.Writer, one log line per Write.
type logfWriter struct{ logf func(string, ...any) }

func (w logfWriter) Write(p []byte) (int, error) {
	w.logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
