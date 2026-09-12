package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// Opener opens a new stream toward the peer. Serve hands one to the handler
// at hello time so the agent can push an http stream to this CLI when a
// request for that user arrives; nothing else opens streams from the agent
// side.
//
// yamux is symmetric, so either end can open a stream. Until v0.3 only the
// CLI did (dial, resolve), which is why the CLI's accept loop is new.
type Opener interface {
	OpenStream() (net.Conn, error)
}

// muxOpener adapts yamux's concrete return type (*yamux.Stream) to Opener.
type muxOpener struct{ mux *yamux.Session }

func (m muxOpener) OpenStream() (net.Conn, error) { return m.mux.OpenStream() }

// Handler is implemented by the agent.
type Handler interface {
	// Hello validates the hello and returns welcome, or an error to reject.
	// open is the session's Opener: hello is the moment the agent learns
	// which session belongs to which user, so it is where the handler is
	// given the means to push streams back at that user's CLI.
	Hello(h proto.Hello, remote string, open Opener) (proto.Welcome, *proto.Error)
	// Dial opens a TCP connection to addr from the agent's network.
	Dial(ctx context.Context, addr string) (net.Conn, error)
	// Resolve looks name up with the agent's own resolver (the task's
	// resolv.conf), which is what makes Cloud Map names and private hosted
	// zones resolvable from the laptop.
	Resolve(ctx context.Context, name string) (addrs []string, ttl int, err error)
	// Closed is called once when the session ends for any reason.
	Closed()
}

// ServeOptions tunes Serve.
type ServeOptions struct {
	// ControlTimeout is the longest the control stream may be silent. The
	// client pings every 5s, so 20s means ~4 missed pings.
	ControlTimeout time.Duration
	// ResolveTimeout bounds how long a single resolve stream's lookup may
	// take before serveStream gives up and answers with an error, so a
	// handler that hangs cannot leak a stream forever. Default 5s.
	ResolveTimeout time.Duration
}

// Serve runs the agent side of one session until the client says bye, the
// control stream goes silent, ctx is cancelled, or conn dies.
func Serve(ctx context.Context, conn net.Conn, h Handler, opts ServeOptions) error {
	if opts.ControlTimeout == 0 {
		opts.ControlTimeout = 20 * time.Second
	}
	if opts.ResolveTimeout == 0 {
		opts.ResolveTimeout = 5 * time.Second
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	mux, err := yamux.Server(conn, cfg)
	if err != nil {
		return err
	}
	defer mux.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		mux.Close()
	}()

	control, err := mux.AcceptStream()
	if err != nil {
		return fmt.Errorf("session: accept control stream: %w", err)
	}
	enc := proto.NewEncoder(control)
	dec := proto.NewDecoder(control)

	control.SetReadDeadline(time.Now().Add(opts.ControlTimeout))
	typ, raw, err := dec.Decode()
	if err != nil {
		return fmt.Errorf("session: read hello: %w", err)
	}
	if typ != proto.TypeHello {
		enc.Encode(proto.TypeError, proto.Error{Code: proto.CodeBadHello, Message: "expected hello, got " + typ})
		return errors.New("session: expected hello")
	}
	var hello proto.Hello
	if err := proto.Unmarshal(raw, &hello); err != nil {
		enc.Encode(proto.TypeError, proto.Error{Code: proto.CodeBadHello, Message: err.Error()})
		return err
	}
	if hello.Version != proto.Version {
		enc.Encode(proto.TypeError, proto.Error{Code: proto.CodeVersionMismatch, Message: "agent speaks protocol " + proto.Version})
		return errors.New("session: version mismatch")
	}
	welcome, rej := h.Hello(hello, conn.RemoteAddr().String(), muxOpener{mux: mux})
	if rej != nil {
		enc.Encode(proto.TypeError, *rej)
		return fmt.Errorf("session: rejected: %s", rej.Code)
	}
	defer h.Closed()
	if err := enc.Encode(proto.TypeWelcome, welcome); err != nil {
		return err
	}

	// Data streams.
	go func() {
		for {
			s, err := mux.AcceptStream()
			if err != nil {
				return
			}
			go serveStream(ctx, s, h, opts.ResolveTimeout)
		}
	}()

	// Control loop.
	for {
		control.SetReadDeadline(time.Now().Add(opts.ControlTimeout))
		typ, _, err := dec.Decode()
		if err != nil {
			return fmt.Errorf("session: control stream: %w", err)
		}
		switch typ {
		case proto.TypePing:
			if err := enc.Encode(proto.TypePong, nil); err != nil {
				return err
			}
		case proto.TypeBye:
			return nil
		}
	}
}

func serveStream(ctx context.Context, s net.Conn, h Handler, resolveTimeout time.Duration) {
	defer s.Close()
	typ, raw, err := proto.ReadHeader(s)
	if err != nil {
		return
	}
	enc := proto.NewEncoder(s)
	switch typ {
	case proto.TypeDial:
		var hd proto.DialHeader
		if err := proto.Unmarshal(raw, &hd); err != nil {
			enc.Encode(proto.TypeDial, proto.DialReply{Error: err.Error()})
			return
		}
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		target, err := h.Dial(dctx, hd.Addr)
		cancel()
		if err != nil {
			enc.Encode(proto.TypeDial, proto.DialReply{Error: err.Error()})
			return
		}
		defer target.Close()
		if err := enc.Encode(proto.TypeDial, proto.DialReply{OK: true}); err != nil {
			return
		}
		Pipe(s, target)
	case proto.TypeResolve:
		var hd proto.ResolveHeader
		if err := proto.Unmarshal(raw, &hd); err != nil {
			enc.Encode(proto.TypeResolve, proto.ResolveReply{Error: err.Error()})
			return
		}
		if hd.QType != "A" {
			enc.Encode(proto.TypeResolve, proto.ResolveReply{Error: fmt.Sprintf("unsupported query type %q: only A is supported", hd.QType)})
			return
		}
		rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
		addrs, ttl, err := h.Resolve(rctx, hd.Name)
		cancel()
		if err != nil {
			enc.Encode(proto.TypeResolve, proto.ResolveReply{Error: err.Error(), NotFound: errors.Is(err, ErrNameNotFound)})
			return
		}
		enc.Encode(proto.TypeResolve, proto.ResolveReply{OK: true, Addrs: addrs, TTL: ttl})
	default:
		enc.Encode(proto.TypeError, proto.Error{Code: proto.CodeBadHello, Message: "unknown stream type " + typ})
	}
}

// Pipe copies bytes in both directions until both sides are done. When one
// direction ends, the write side of the other conn is half-closed if it
// supports it, so a graceful FIN propagates.
func Pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
