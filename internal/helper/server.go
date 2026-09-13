package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// Server serves the helper protocol. Exactly one connection may hold the pf
// session at a time; it is torn down when that connection closes.
type Server struct {
	Platform Platform
	// Allow decides whether a peer may change state. nil allows everyone.
	Allow func(Peer) bool
	// PeerFunc extracts peer credentials. nil uses PeerCredentials.
	PeerFunc func(net.Conn) (Peer, error)
	Logf     func(string, ...any)
	// IdleTimeout, when positive, makes Serve return nil once no
	// connection has been open for that long - the idle exit spec §8's
	// socket activation needs, so that no root process exists while
	// tetherd is not in use.
	//
	// It is a field rather than a constant because no test can wait
	// DefaultIdleTimeout's 30 seconds. cmd/tetherd-helper sets it only for
	// the listener launchd handed over: on the foreground path there is
	// nothing to restart the helper on the next connection, and
	// hack/e2e-local.sh expects its `sudo tetherd-helper` to stay up.
	//
	// Zero means never, which is what every caller that binds its own
	// socket wants.
	IdleTimeout time.Duration

	mu     sync.Mutex
	active *connState

	// conns tracks live connections so Serve can shut them down and wait
	// for their cleanup before returning. main calls platform.Shutdown() as
	// soon as Serve returns, and that lock is not this one: a Serve that
	// returned with a connection still being served would let an in-flight
	// route.set pin 169.254.170.2 just after Shutdown looked.
	connMu  sync.Mutex
	conns   map[net.Conn]struct{}
	closing bool
	serving sync.WaitGroup
	// idleLn and idleFor are the idle exit's state, set once by Serve and
	// read by untrackConn. They live under connMu because the decision is
	// exactly "did conns just become empty".
	idleLn  deadlineListener
	idleFor time.Duration
}

// deadlineListener is the part of *net.UnixListener that the idle exit
// needs.
//
// Accept's own deadline, rather than a timer that closes the listener, is
// what makes the decision race-free: it is Accept that reports the timeout,
// so a connection that arrived during the idle window is returned instead of
// being dropped by a listener that is already closing.
type deadlineListener interface {
	net.Listener
	SetDeadline(t time.Time) error
}

type connState struct {
	peer        Peer
	since       time.Time
	applied     bool
	resolverSet bool
	routeSet    bool
}

// socketMode is the mode of the helper's UNIX socket. Plist writes the same
// value into SockPathMode (in decimal, as launchd requires), so that the
// socket launchd binds and the one this function binds are identical: the
// permissions are part of the protocol - internal/doctor and the CLI's "not
// running" advice both assume any admin user can connect, and the actual
// authorization is Server.Allow, not the file mode.
const socketMode = 0o666

// ListenAndServe binds its own UNIX socket and serves until ctx is done.
//
// This is the path taken when launchd handed over no listener: the
// foreground `sudo tetherd-helper` of hack/e2e-local.sh, and a launchd start
// from a plist with no Sockets entry. Under socket activation the caller
// passes the inherited listener to Serve instead and nothing here runs -
// binding would unlink the socket launchd owns.
func (s *Server) ListenAndServe(ctx context.Context, socketPath string) error {
	os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(socketPath, socketMode); err != nil {
		ln.Close()
		return err
	}
	defer os.Remove(socketPath)
	return s.Serve(ctx, ln)
}

// Serve accepts on ln until ctx is done, or - when IdleTimeout is positive
// - until no connection has been open for that long. ln may be the listener
// launchd handed over (see InheritedListener) or one ListenAndServe bound.
//
// Both endings return nil, so that the helper's process exits 0 and the
// plist's KeepAlive: {SuccessfulExit: false} leaves it alone. Only a serve
// failure returns an error, which is the exit launchd does retry.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
		// Closing the listener does not touch established connections, and
		// a ServeConn parked on its scanner would never return - so the
		// clients are disconnected here, which is what makes the wait
		// below finite.
		s.stopConns()
	}()
	idle := s.armIdleExit(ln)
	peerFn := s.PeerFunc
	if peerFn == nil {
		peerFn = PeerCredentials
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				// Every connection's cleanup has run by the time this
				// returns, so the caller's platform shutdown sees the
				// machine as the sessions left it.
				s.serving.Wait()
				return nil
			}
			if idle > 0 && errors.Is(err, os.ErrDeadlineExceeded) {
				if !s.noConnections() {
					// A connection is open, so this window belongs to a
					// session that is still running - `tetherd run` holds
					// one connection for hours and may send nothing on it
					// for most of that. The deadline is taken off until
					// untrackConn arms the next window, when the last
					// connection closes.
					//
					// This is the only place the deadline is cleared. An
					// earlier version also cleared it on every accepted
					// connection, and the two made each other dead code:
					// with the clear in place the deadline never expired
					// while a connection was open, so removing this guard
					// changed nothing any test could see, and vice versa.
					// Both mutations survived. One mechanism, exercised
					// by every connection that outlives a window.
					s.clearIdleDeadline()
					continue
				}
				s.logf("no connection for %s; exiting (launchd starts the helper again on the next one)", idle)
				// Same wait as the cancel path: the caller's platform
				// shutdown runs the moment this returns, and it must not
				// race a session's cleanup.
				s.stopConns()
				s.serving.Wait()
				return nil
			}
			return err
		}
		peer, err := peerFn(conn)
		if err != nil {
			s.logf("peer credentials: %v", err)
			conn.Close()
			continue
		}
		if !s.trackConn(conn) {
			conn.Close() // shutting down; do not start a new session
			continue
		}
		s.serving.Add(1)
		go func() {
			defer s.serving.Done()
			defer s.untrackConn(conn)
			s.ServeConn(conn, peer)
		}()
	}
}

// trackConn registers a connection, or reports false once shutdown started.
func (s *Server) trackConn(c net.Conn) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.closing {
		return false
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[c] = struct{}{}
	return true
}

// untrackConn deregisters a connection and, when it was the last one, starts
// the idle window.
//
// It runs from the serving goroutine's defer, after ServeConn's own
// `defer s.cleanup(st)`, so the session's pf rules, resolver files and route
// are already off the machine before the clock that ends the process starts.
func (s *Server) untrackConn(c net.Conn) {
	s.connMu.Lock()
	delete(s.conns, c)
	var dl deadlineListener
	if len(s.conns) == 0 {
		dl = s.idleLn
	}
	d := s.idleFor
	s.connMu.Unlock()
	if dl != nil && d > 0 {
		// SetDeadline wakes an Accept that is already blocked, which is
		// what turns "the last client left" into the accept loop's
		// decision rather than a separate timer's.
		dl.SetDeadline(time.Now().Add(d))
	}
}

// armIdleExit records the idle exit's state and starts the first window,
// returning the timeout actually in force.
//
// The window is armed before the first connection rather than after the
// first disconnect: launchd starts this daemon speculatively as well as on
// demand - `man launchd.plist` (Darwin 25.6.0): the use of KeepAlive
// "implicitly implies RunAtLoad, causing launchd to speculatively launch the
// job" - and a start nobody ever connects to has to end by itself, or the
// helper is resident again by the back door.
func (s *Server) armIdleExit(ln net.Listener) time.Duration {
	if s.IdleTimeout <= 0 {
		return 0
	}
	dl, ok := ln.(deadlineListener)
	if !ok {
		// Every listener in production is a *net.UnixListener, which has
		// SetDeadline. Refusing to serve over this would be worse than
		// staying up.
		s.logf("idle exit after %s is unavailable on a %T listener; staying up until the context is cancelled", s.IdleTimeout, ln)
		return 0
	}
	s.connMu.Lock()
	s.idleLn, s.idleFor = dl, s.IdleTimeout
	s.connMu.Unlock()
	dl.SetDeadline(time.Now().Add(s.IdleTimeout))
	return s.IdleTimeout
}

// noConnections reports whether nothing is connected right now.
func (s *Server) noConnections() bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return len(s.conns) == 0
}

// clearIdleDeadline takes the idle window off the listener.
func (s *Server) clearIdleDeadline() {
	s.connMu.Lock()
	dl := s.idleLn
	s.connMu.Unlock()
	if dl != nil {
		dl.SetDeadline(time.Time{})
	}
}

// stopConns disconnects every live client and refuses new ones.
func (s *Server) stopConns() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.closing = true
	for c := range s.conns {
		c.Close()
	}
}

// ServeConn handles one connection; it returns when the peer disconnects.
func (s *Server) ServeConn(conn net.Conn, peer Peer) {
	defer conn.Close()
	st := &connState{peer: peer, since: time.Now()}
	defer s.cleanup(st)

	enc := json.NewEncoder(conn)
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			enc.Encode(response{OK: false, Code: CodeUnknownOp, Error: "bad request: " + err.Error()})
			return
		}
		resp := s.handle(st, req)
		resp.ID = req.ID
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) handle(st *connState, req request) response {
	if req.Op == OpVersion {
		return response{OK: true, Version: ProtocolVersion}
	}
	if s.Allow != nil && !s.Allow(st.peer) {
		return response{Code: CodeForbidden, Error: fmt.Sprintf("uid %d is not allowed to use tetherd-helper (must be in the admin group)", st.peer.UID)}
	}
	switch req.Op {
	case OpPfApply:
		return s.pfApply(st, req)
	case OpPfClear:
		return s.pfClear(st)
	case OpResolverSet:
		return s.resolverSet(st, req)
	case OpResolverClear:
		return s.resolverClear(st)
	case OpRouteSet:
		return s.routeSet(st, req)
	case OpRouteClear:
		return s.routeClear(st)
	case OpNatLook:
		return s.natLook(req)
	}
	return response{Code: CodeUnknownOp, Error: "unknown op " + req.Op}
}

func (s *Server) pfApply(st *connState, req request) response {
	if req.Pf == nil {
		return response{Code: CodePlatform, Error: "pf.apply: missing pf"}
	}
	spec := PfSpec{RedirectPort: req.Pf.RedirectPort}
	for _, c := range req.Pf.RemoteCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return response{Code: CodePlatform, Error: "pf.apply: bad cidr " + c}
		}
		spec.RemoteCIDRs = append(spec.RemoteCIDRs, p)
	}
	if spec.RedirectPort <= 0 || spec.RedirectPort > 65535 {
		return response{Code: CodePlatform, Error: "pf.apply: bad redirect_port"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active != st {
		return s.busyResponse()
	}
	if st.applied {
		return response{Code: CodeAlreadyApplied, Error: "pf.apply was already called on this connection"}
	}
	if err := s.Platform.PfApply(spec); err != nil {
		return response{Code: CodePlatform, Error: err.Error()}
	}
	st.applied = true
	s.active = st
	s.logf("pf applied for pid %d: %d cidrs -> 127.0.0.1:%d", st.peer.PID, len(spec.RemoteCIDRs), spec.RedirectPort)
	return response{OK: true}
}

func (s *Server) pfClear(st *connState) response {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != st {
		return response{Code: CodeNoSession, Error: "this connection holds no pf session"}
	}
	// pf.clear ends the session and hands the machine to the next CLI, so
	// everything this session installed goes with it, in the same order as
	// the disconnect cleanup: resolver, then route, then pf. The route must
	// come down *before* the rules, not after - while it is pinned with no
	// rdr rule behind it, 169.254.170.2 resolves to lo0 and a packet to a
	// non-local address on lo0 is dropped, so a child hangs where before
	// the pin it got EHOSTUNREACH in a millisecond.
	stuck := false
	if st.resolverSet {
		if err := s.Platform.ResolverClear(); err != nil {
			s.logf("resolver clear on pf.clear: %v", err)
			stuck = true
		} else {
			st.resolverSet = false
		}
	}
	if st.routeSet {
		if err := s.Platform.RouteClear(); err != nil {
			s.logf("route clear on pf.clear: %v", err)
			stuck = true
		} else {
			st.routeSet = false
		}
	}
	if err := s.Platform.PfClear(); err != nil {
		return response{Code: CodePlatform, Error: err.Error()}
	}
	st.applied = false
	if stuck {
		// Something this session installed is still on the machine. The
		// connection keeps the session - the disconnect cleanup is the
		// retry, and it only runs for the active connection - and the next
		// CLI stays locked out until the machine is clean.
		s.logf("pf cleared for pid %d, but some state could not be removed; keeping the session for the disconnect retry", st.peer.PID)
		return response{OK: true}
	}
	s.active = nil
	s.logf("pf cleared for pid %d", st.peer.PID)
	return response{OK: true}
}

// busyResponse says who holds the machine-wide session. Called with s.mu
// held and s.active non-nil.
func (s *Server) busyResponse() response {
	return response{Code: CodeBusy, Error: "another tetherd session is active on this machine", Busy: &busyWire{PID: s.active.peer.PID, Since: s.active.since}}
}

// routeSet pins host routes for this connection, claiming the session the
// same way pf.apply does: the CLI pins the credential endpoint before it
// installs pf rules, so this is where a second `tetherd run` finds out the
// machine is taken.
func (s *Server) routeSet(st *connState, req request) response {
	if len(req.Hosts) == 0 {
		return response{Code: CodePlatform, Error: "route.set: missing hosts"}
	}
	// Bounded before anything is allocated or scanned: Router.Set checks
	// each host against what it has already pinned, which is linear, so an
	// unbounded list from a client is quadratic work in a root daemon.
	if len(req.Hosts) > maxHosts {
		return response{Code: CodePlatform, Error: fmt.Sprintf("route.set: %d hosts is more than tetherd pins (max %d)", len(req.Hosts), maxHosts)}
	}
	hosts := make([]netip.Addr, 0, len(req.Hosts))
	for _, h := range req.Hosts {
		a, err := netip.ParseAddr(h)
		if err != nil {
			return response{Code: CodePlatform, Error: "route.set: bad host " + h}
		}
		hosts = append(hosts, a)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active != st {
		return s.busyResponse()
	}
	if err := s.Platform.RouteSet(hosts); err != nil {
		// Nothing was pinned, so nothing claims the session either: a
		// refused address must not lock the machine for the next CLI.
		return response{Code: CodePlatform, Error: err.Error()}
	}
	st.routeSet = true
	s.active = st
	s.logf("routes pinned to lo0 for pid %d: %s", st.peer.PID, strings.Join(req.Hosts, ", "))
	return response{OK: true}
}

// routeClear removes what this connection pinned. It is not an error when
// there is nothing to remove: the CLI clears the route from a defer that
// also runs on the paths where pinning never happened, and on the ordinary
// one where pf.clear already took it down.
func (s *Server) routeClear(st *connState) response {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !st.routeSet {
		return response{OK: true}
	}
	if err := s.Platform.RouteClear(); err != nil {
		return response{Code: CodePlatform, Error: err.Error()}
	}
	st.routeSet = false
	// Unlike pf.clear this does not end the session: a connection that
	// still holds pf rules keeps the machine. One that held only a route
	// now holds nothing, and must let the next CLI in. (st.routeSet is only
	// ever true while s.active is this connection, so there is nobody
	// else's session to drop here.)
	if !st.applied {
		s.active = nil
	}
	return response{OK: true}
}

func (s *Server) resolverSet(st *connState, req request) response {
	if req.Resolver == nil || len(req.Resolver.Domains) == 0 {
		return response{Code: CodePlatform, Error: "resolver.set: missing domains"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// pf specifically, not just any session: route.set opens one of its own
	// now, and /etc/resolver files pointing at a DNS proxy while no capture
	// is installed would send the whole machine's lookups for those domains
	// at a port nothing is serving.
	if s.active != st || !st.applied {
		return response{Code: CodeNoSession, Error: "resolver.set requires an active pf session on this connection"}
	}
	if err := s.Platform.ResolverSet(req.Resolver.Domains, req.Resolver.Port); err != nil {
		return response{Code: CodePlatform, Error: err.Error()}
	}
	st.resolverSet = true
	return response{OK: true}
}

func (s *Server) resolverClear(st *connState) response {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != st {
		return response{Code: CodeNoSession, Error: "this connection holds no session"}
	}
	if err := s.Platform.ResolverClear(); err != nil {
		return response{Code: CodePlatform, Error: err.Error()}
	}
	st.resolverSet = false
	return response{OK: true}
}

func (s *Server) natLook(req request) response {
	if req.NatLook == nil {
		return response{Code: CodePlatform, Error: "natlook: missing args"}
	}
	src, err1 := netip.ParseAddrPort(req.NatLook.Src)
	dst, err2 := netip.ParseAddrPort(req.NatLook.Dst)
	if err1 != nil || err2 != nil {
		return response{Code: CodePlatform, Error: "natlook: bad src/dst"}
	}
	orig, err := s.Platform.NatLook(req.NatLook.Proto, src, dst)
	if err != nil {
		return response{Code: CodePlatform, Error: err.Error()}
	}
	return response{OK: true, Dst: orig.String()}
}

// cleanup runs when a connection ends: whatever it left behind is removed.
func (s *Server) cleanup(st *connState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != st {
		return
	}
	// resolver, then route, then pf: the reverse of what depends on what.
	// The resolver files are useless without the route and the rules, and
	// the route is only correct while the rdr rule is loaded.
	if st.resolverSet {
		if err := s.Platform.ResolverClear(); err != nil {
			s.logf("resolver clear on disconnect: %v", err)
		}
	}
	if st.routeSet {
		if err := s.Platform.RouteClear(); err != nil {
			s.logf("route clear on disconnect: %v", err)
		}
	}
	if st.applied {
		if err := s.Platform.PfClear(); err != nil {
			s.logf("pf clear on disconnect: %v", err)
		}
	}
	s.active = nil
	s.logf("session of pid %d ended; state cleaned", st.peer.PID)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
