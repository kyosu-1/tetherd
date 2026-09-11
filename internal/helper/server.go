package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
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
		return response{Code: CodeBusy, Error: "another tetherd session is active on this machine", Busy: &busyWire{PID: s.active.peer.PID, Since: s.active.since}}
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
	s.active = nil
	s.logf("pf cleared for pid %d", st.peer.PID)
	return response{OK: true}
}

func (s *Server) resolverSet(st *connState, req request) response {
	if req.Resolver == nil || len(req.Resolver.Domains) == 0 {
		return response{Code: CodePlatform, Error: "resolver.set: missing domains"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != st {
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
	if st.resolverSet {
		if err := s.Platform.ResolverClear(); err != nil {
			s.logf("resolver clear on disconnect: %v", err)
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
