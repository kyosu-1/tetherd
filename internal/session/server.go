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
//
// On error the returned net.Conn is nil (a plain `s == nil` test is safe).
// The caller must bound its own read of the reply: a CLI older than the
// accept loop never answers an http stream at all, and a session can go
// away between the moment the registry hands out this Opener and the moment
// it is used.
type Opener interface {
	OpenStream() (net.Conn, error)
}

// muxOpener adapts yamux's concrete return type (*yamux.Stream) to Opener.
type muxOpener struct{ mux *yamux.Session }

// OpenStream deliberately does not `return m.mux.OpenStream()`. That
// compiles - *yamux.Stream is assignable to net.Conn - but on error it
// returns a non-nil net.Conn wrapping a nil *yamux.Stream, so the caller's
// `if s != nil { s.Close() }` dereferences nil. That panic would happen
// inside tetherd-agent, which has no recover, taking down the sidecar and
// every developer's session on the task.
func (m muxOpener) OpenStream() (net.Conn, error) {
	s, err := m.mux.OpenStream()
	if err != nil {
		return nil, err
	}
	return s, nil
}

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
	// RefusalReadWait bounds how long a refused session waits for the
	// client to read the refusal before the transport is torn down anyway
	// (see refuse). Default 2s. It only has to cover the refusal's flight
	// time plus one scheduling of the client's dialing goroutine, so it is
	// far shorter than the 20s of silence ControlTimeout already tolerates
	// while waiting for a hello - the same goroutine and the same conn,
	// parked for the same kind of reason.
	//
	// An option rather than a package var so that a test can shorten or
	// lengthen it without writing to state another test's Serve goroutine
	// is still reading: measured, a var here is a data race between one
	// test's pin and the previous test's refusal wait.
	RefusalReadWait time.Duration
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
	if opts.RefusalReadWait == 0 {
		opts.RefusalReadWait = 2 * time.Second
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
		return refuse(control, enc, opts.RefusalReadWait, proto.Error{Code: proto.CodeBadHello, Message: "expected hello, got " + typ},
			errors.New("session: expected hello"))
	}
	var hello proto.Hello
	if err := proto.Unmarshal(raw, &hello); err != nil {
		return refuse(control, enc, opts.RefusalReadWait, proto.Error{Code: proto.CodeBadHello, Message: err.Error()}, err)
	}
	if hello.Version != proto.Version {
		return refuse(control, enc, opts.RefusalReadWait, proto.Error{Code: proto.CodeVersionMismatch, Message: "agent speaks protocol " + proto.Version},
			errors.New("session: version mismatch"))
	}
	welcome, rej := h.Hello(hello, conn.RemoteAddr().String(), muxOpener{mux: mux})
	if rej != nil {
		return refuse(control, enc, opts.RefusalReadWait, *rej, fmt.Errorf("session: rejected: %s", rej.Code))
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

// refuse writes the reason this session is being turned down and then waits
// for the client to read it, because Serve's `defer mux.Close()` runs the
// moment refuse returns and closing the transport under an unread refusal
// is what loses it.
//
// What is lost is not the refusal frame. yamux delivers it and the client's
// stream buffer keeps it across the teardown (measured: with the client's
// read held back 20ms behind the close, 20 of 20 refusals still arrived).
// What the close loses is the client's *hello write*. yamux reports a
// write's success on one channel and the session's death on another, and
// Dial's goroutine - which has been parked in that select since it queued
// the hello - takes whichever is ready. A close that has already landed
// makes it return ErrSessionShutdown for a write that in fact succeeded, so
// session.Dial returns "session shutdown" and never reads the refusal
// already sitting in its buffer. Measured on loopback TCP before this fix:
// 2 to 5 of every 3000 refusals, every one of them at the hello write and
// not one at the read of the refusal itself.
//
// That matters beyond the wire: run.go's duplicate_user retry is an
// errors.As on *RejectedError, so a refusal arriving as a transport error
// silently skips the whole of DefaultAttachRetryBudget, and a
// protocol-version mismatch reaches `tetherd doctor`'s helper row as a
// generic "session shutdown".
//
// The wait ends on the client, not on the clock: session.Dial closes the
// mux the moment it decodes a TypeError, which closes the conn, which is
// what makes this read fail. ctx cancellation ends it too, through the
// goroutine Serve starts to close mux when ctx is done. wait is only the
// backstop for a peer that reads the refusal and then sits there, or never
// reads at all: nothing else in the session waits on this read - the
// refusal is already on the wire, and a refused hello is never registered
// (h.Closed is not deferred until after this point) - but it must not park
// a goroutine and a conn on the agent indefinitely.
func refuse(control net.Conn, enc *proto.Encoder, wait time.Duration, e proto.Error, err error) error {
	if encErr := enc.Encode(proto.TypeError, e); encErr != nil {
		// Nothing reached the client, so there is nothing to wait for.
		return err
	}
	control.SetReadDeadline(time.Now().Add(wait))
	var b [1]byte
	control.Read(b[:]) // for the error: the client going away is the answer
	return err
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
