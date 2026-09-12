package helper

import (
	"bufio"
	"context"
	"encoding/json"
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

	mu     sync.Mutex
	active *connState
}

type connState struct {
	peer        Peer
	since       time.Time
	applied     bool
	resolverSet bool
	routeSet    bool
}

// ListenAndServe listens on a UNIX socket (mode 0666) until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, socketPath string) error {
	os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(socketPath, 0o666); err != nil {
		ln.Close()
		return err
	}
	defer os.Remove(socketPath)
	return s.Serve(ctx, ln)
}

// Serve accepts on ln until ctx is done. ln may come from launchd socket
// activation (v0.4) or from ListenAndServe.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	peerFn := s.PeerFunc
	if peerFn == nil {
		peerFn = PeerCredentials
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
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
		go s.ServeConn(conn, peer)
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
	if err := s.Platform.PfClear(); err != nil {
		return response{Code: CodePlatform, Error: err.Error()}
	}
	st.applied = false
	if st.resolverSet {
		s.Platform.ResolverClear()
		st.resolverSet = false
	}
	// pf.clear ends the session and hands the machine to the next CLI, so
	// the pinned route goes too. Left behind it would point 169.254.170.2
	// at lo0 with no rdr rule to catch it - worse than the unreachable-host
	// error it was installed to fix - and the disconnect cleanup, which
	// only runs for the active connection, would no longer remove it.
	if st.routeSet {
		if err := s.Platform.RouteClear(); err != nil {
			s.logf("route clear on pf.clear: %v", err)
		}
		st.routeSet = false
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
