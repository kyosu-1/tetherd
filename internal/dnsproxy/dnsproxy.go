// Package dnsproxy answers DNS queries for the domains that only the VPC can
// resolve. macOS sends a child's lookups through mDNSResponder, which pf
// cannot scope to a process, so the names are routed per-domain by
// /etc/resolver files pointing here, and this server forwards each question
// to the agent (spec §3.4).
package dnsproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/kyosu-1/tetherd/internal/session"
)

// Resolver answers one name, normally session.Client.Resolve.
type Resolver func(ctx context.Context, name string) (addrs []string, ttl int, err error)

// Server is the loopback resolver. Only A records are answered: the captured
// set is IPv4-only in v1, so handing a child an AAAA would send it somewhere
// tetherd cannot carry.
//
// Every question is served on its own goroutine (miekg/dns's own model) and
// - by way of session.Client.Resolve - its own yamux stream to the agent.
// Nothing here caps how many run at once: the number of distinct names one
// `tetherd run` session ever looks up is small enough (the app's own
// hostnames) that a concurrency limit would add complexity without a real
// problem to solve.
type Server struct {
	Resolve Resolver
	Logf    func(string, ...any)

	// QueryTimeout bounds how long one question waits on Resolve before the
	// server answers SERVFAIL on its own. Zero means defaultQueryTimeout. A
	// field (not a hardcoded production duration) so a test can shrink it
	// instead of waiting out the real value to observe the timeout.
	QueryTimeout time.Duration

	// listenPacket and listenTCP substitute the OS bind calls in tests, so a
	// rare "kernel-chosen UDP port already taken on TCP" race (the reason
	// StartPreferring retries even when it was already asked for port 0),
	// or a half-successful bind leaking its UDP socket, can be forced
	// deterministically instead of trying to predict which port the kernel
	// will hand out. Zero value in production: Start falls back to
	// net.ListenPacket / net.Listen.
	listenPacket func(network, address string) (net.PacketConn, error)
	listenTCP    func(network, address string) (net.Listener, error)

	mu        sync.Mutex
	pc        net.PacketConn
	ln        net.Listener
	udp       *dns.Server
	tcp       *dns.Server
	addr      netip.AddrPort
	closed    bool
	doneCh    chan struct{}
	reqCtx    context.Context
	reqCancel context.CancelFunc
}

// DefaultPort is the port the helper writes into /etc/resolver/<domain>
// (spec §3.4). It is fixed so a stale resolver file still points somewhere
// predictable, and so doctor can say what to look for.
const DefaultPort = 53530

// defaultQueryTimeout is the production value of QueryTimeout: long enough
// for a real agent round trip, short enough that a dead one does not wedge
// the child's own DNS timeout on top of tetherd's.
const defaultQueryTimeout = 5 * time.Second

// StartPreferring binds port if it is free and any free port otherwise: a
// session killed hard can leave a resolver holding DefaultPort, and the
// helper is told whichever port was actually bound, so the fallback is
// invisible to the child. The retry always targets a fresh kernel-assigned
// port (0), even when port was already 0: the two ListenPacket/Listen
// calls inside Start are not atomic, so on rare occasions something else
// can grab the TCP half of a kernel-chosen UDP port between them, and that
// deserves the same one-shot retry as a taken DefaultPort does.
func (s *Server) StartPreferring(ctx context.Context, port int) (netip.AddrPort, error) {
	addr, err := s.Start(ctx, port)
	if err == nil {
		return addr, err
	}
	return s.Start(ctx, 0)
}

// Start binds 127.0.0.1:port for UDP and TCP (port 0 picks a free one) and
// serves until ctx is done or Close is called.
func (s *Server) Start(ctx context.Context, port int) (netip.AddrPort, error) {
	listenPacket := s.listenPacket
	if listenPacket == nil {
		listenPacket = net.ListenPacket
	}
	listenTCP := s.listenTCP
	if listenTCP == nil {
		listenTCP = net.Listen
	}
	pc, err := listenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return netip.AddrPort{}, err
	}
	udpLocal := pc.LocalAddr().(*net.UDPAddr)
	bound := udpLocal.Port
	ln, err := listenTCP("tcp4", fmt.Sprintf("127.0.0.1:%d", bound))
	if err != nil {
		pc.Close()
		return netip.AddrPort{}, err
	}
	h := dns.HandlerFunc(s.handle)
	// Derived from what was actually bound, not the literal passed to
	// ListenPacket: the two must never drift apart, or Addr (and the port
	// StartPreferring hands the helper for /etc/resolver) could claim
	// loopback while the socket underneath was opened on every interface.
	udpIP, ok := netip.AddrFromSlice(udpLocal.IP)
	if !ok {
		pc.Close()
		ln.Close()
		return netip.AddrPort{}, fmt.Errorf("dnsproxy: unexpected local address %v", udpLocal.IP)
	}
	addr := netip.AddrPortFrom(udpIP.Unmap(), uint16(bound))

	// reqCtx is what each question's timeout is derived from (see handle):
	// it is cancelled the instant ctx is (automatic, via WithCancel's
	// parent link) or Close runs (explicitly, via reqCancel), so an
	// in-flight resolve is cut short at teardown instead of only ever
	// bounded by QueryTimeout - which would otherwise make Close (and so
	// `tetherd run`'s exit) wait up to QueryTimeout per stuck question,
	// since miekg's Shutdown waits for every handler goroutine to return.
	reqCtx, reqCancel := context.WithCancel(ctx)

	s.mu.Lock()
	if s.closed {
		// Close ran first (it can race a Start called from another
		// goroutine): leave nothing listening rather than starting a
		// server nobody will ever be able to stop through this Server.
		s.mu.Unlock()
		reqCancel()
		pc.Close()
		ln.Close()
		return netip.AddrPort{}, net.ErrClosed
	}
	if s.doneCh == nil {
		s.doneCh = make(chan struct{})
	}
	s.pc, s.ln = pc, ln
	s.udp = &dns.Server{PacketConn: pc, Handler: h}
	s.tcp = &dns.Server{Listener: ln, Handler: h}
	s.addr = addr
	s.reqCtx, s.reqCancel = reqCtx, reqCancel
	udp, tcp, done := s.udp, s.tcp, s.doneCh
	s.mu.Unlock()

	go func() {
		if err := udp.ActivateAndServe(); err != nil {
			s.logf("dns udp: %v", err)
		}
	}()
	go func() {
		if err := tcp.ActivateAndServe(); err != nil {
			s.logf("dns tcp: %v", err)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-done:
		}
	}()
	return addr, nil
}

// Close stops both listeners, cancels any in-flight resolve and unblocks
// the goroutine Start left watching ctx. It is safe to call more than
// once, and safe to call concurrently with (before, during or after)
// Start.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	pc, ln, udp, tcp, reqCancel := s.pc, s.ln, s.udp, s.tcp, s.reqCancel
	s.pc, s.ln, s.udp, s.tcp = nil, nil, nil, nil
	s.addr = netip.AddrPort{}
	if s.doneCh == nil {
		s.doneCh = make(chan struct{})
	}
	close(s.doneCh)
	s.mu.Unlock()

	if reqCancel != nil {
		// Cancel in-flight resolves first: this is what lets the
		// Shutdown calls below return promptly instead of blocking on a
		// handler goroutine that is still waiting out QueryTimeout.
		reqCancel()
	}
	// Shut the dns.Server down first (it is a no-op error, not a panic, if
	// ActivateAndServe never got as far as marking itself started), then
	// close the raw listeners directly: that unblocks ActivateAndServe even
	// when it raced Close and had not yet reached that point, so nothing is
	// left listening either way.
	if udp != nil {
		udp.Shutdown()
	}
	if tcp != nil {
		tcp.Shutdown()
	}
	if pc != nil {
		pc.Close()
	}
	if ln != nil {
		ln.Close()
	}
	return nil
}

func (s *Server) handle(w dns.ResponseWriter, req *dns.Msg) {
	// This runs on miekg/dns's own per-request goroutine. A panic here
	// (a malformed query tripping some assumption below, or in a future
	// change) would otherwise take the whole tetherd process down, skip
	// every CLI defer, and leave hc.ResolverClear() never called - so the
	// stale /etc/resolver files it would have removed become the helper's
	// disconnect backstop's problem instead of never existing.
	defer func() {
		if r := recover(); r != nil {
			s.logf("dns: recovered from a panic handling a query: %v", r)
			resp := new(dns.Msg)
			if req != nil {
				resp.SetReply(req)
			}
			resp.Rcode = dns.RcodeServerFailure
			w.WriteMsg(resp)
		}
	}()

	resp := new(dns.Msg)
	resp.SetReply(req)
	if len(req.Question) != 1 {
		// No question (or more than one, which this server never asks
		// itself and does not support): answering success would mean
		// silently ignoring a malformed query, and indexing
		// req.Question[0] below with zero questions would panic. Any
		// local process can send tetherd's loopback resolver whatever it
		// likes, so this has to be a real check, not an assumption.
		resp.Rcode = dns.RcodeFormatError
		w.WriteMsg(resp)
		return
	}
	q := req.Question[0]
	if q.Qtype != dns.TypeA || q.Qclass != dns.ClassINET {
		// Anything but an A record is out of scope: tetherd only carries IPv4.
		resp.Rcode = dns.RcodeNotImplemented
		w.WriteMsg(resp)
		return
	}
	name := q.Name
	timeout := s.QueryTimeout
	if timeout <= 0 {
		timeout = defaultQueryTimeout
	}
	ctx, cancel := context.WithTimeout(s.requestCtx(), timeout)
	defer cancel()
	addrs, ttl, err := s.resolveBounded(ctx, trimDot(name))
	if err != nil {
		if errors.Is(err, session.ErrNameNotFound) {
			// The agent (or, in a direct/offline test, the Resolver
			// itself) says this name does not exist. NXDOMAIN, not
			// SERVFAIL: macOS retries SERVFAIL, so a mistyped hostname
			// would otherwise read as a hang instead of "host not found".
			s.logf("dns %s: not found", trimDot(name))
			resp.Rcode = dns.RcodeNameError
			w.WriteMsg(resp)
			return
		}
		s.logf("dns %s: %v", trimDot(name), err)
		resp.Rcode = dns.RcodeServerFailure
		w.WriteMsg(resp)
		return
	}
	if ttl <= 0 {
		ttl = 30
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			continue
		}
		resp.Answer = append(resp.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: uint32(ttl)},
			A:   ip.To4(),
		})
	}
	if len(resp.Answer) == 0 {
		// Defensive fallback: a Resolver that returns no addresses without
		// an error (production's session.Client.Resolve never does - the
		// agent always returns ErrNameNotFound for that case, handled
		// above - but a hand-written Resolver might) must still read as
		// "no such name", not success with zero answers.
		resp.Rcode = dns.RcodeNameError
	}
	w.WriteMsg(resp)
}

// resolveBounded runs Resolve on its own goroutine and returns as soon as
// ctx's deadline passes, even if Resolve itself never returns: a hung agent
// must SERVFAIL only the question stuck on it. miekg/dns already serves each
// request on its own goroutine, but without this a slow Resolve would still
// hold this handler goroutine (and the query's connection) open forever,
// leaking one goroutine per stuck name.
func (s *Server) resolveBounded(ctx context.Context, name string) ([]string, int, error) {
	type result struct {
		addrs []string
		ttl   int
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		// A panic here would cross a goroutine boundary handle's own
		// recover cannot reach - Resolve is arbitrary code (production's
		// session.Client.Resolve, or a hand-written stub) and a bug in it
		// must not take the whole tetherd process down.
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("dnsproxy: recovered from a panic in Resolve: %v", r)}
			}
		}()
		addrs, ttl, err := s.Resolve(ctx, name)
		ch <- result{addrs, ttl, err}
	}()
	select {
	case r := <-ch:
		return r.addrs, r.ttl, r.err
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

// requestCtx is what handle derives each question's timeout from: the ctx
// Start was given (so teardown cancels in-flight work), or
// context.Background() if handle is somehow invoked before Start ever ran
// (not reachable through the real dns.Server, but a nil-safe fallback costs
// nothing).
func (s *Server) requestCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reqCtx == nil {
		return context.Background()
	}
	return s.reqCtx
}

func trimDot(name string) string {
	if len(name) > 1 && name[len(name)-1] == '.' {
		return name[:len(name)-1]
	}
	return name
}

// Addr reports the bound address (zero before Start, and after Close).
func (s *Server) Addr() netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
