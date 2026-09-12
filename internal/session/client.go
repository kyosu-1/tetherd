package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// Options tunes the client's liveness check.
type Options struct {
	PingInterval time.Duration // default 5s
	MaxMissed    int           // default 3
}

// Client is the CLI side of one session.
type Client struct {
	mux     *yamux.Session
	control net.Conn
	enc     *proto.Encoder
	welcome proto.Welcome

	pong chan struct{}
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

// Dial performs hello/welcome over conn and starts the ping loop.
func Dial(ctx context.Context, conn net.Conn, hello proto.Hello, opts Options) (*Client, error) {
	if opts.PingInterval == 0 {
		opts.PingInterval = 5 * time.Second
	}
	if opts.MaxMissed == 0 {
		opts.MaxMissed = 3
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	mux, err := yamux.Client(conn, cfg)
	if err != nil {
		return nil, err
	}
	control, err := mux.OpenStream()
	if err != nil {
		mux.Close()
		return nil, err
	}
	c := &Client{mux: mux, control: control, enc: proto.NewEncoder(control), pong: make(chan struct{}, 1), done: make(chan struct{})}
	if err := c.enc.Encode(proto.TypeHello, hello); err != nil {
		mux.Close()
		return nil, err
	}
	dec := proto.NewDecoder(control)
	if dl, ok := ctx.Deadline(); ok {
		control.SetReadDeadline(dl)
	} else {
		control.SetReadDeadline(time.Now().Add(15 * time.Second))
	}
	typ, raw, err := dec.Decode()
	control.SetReadDeadline(time.Time{})
	if err != nil {
		mux.Close()
		return nil, fmt.Errorf("session: waiting for welcome: %w", err)
	}
	switch typ {
	case proto.TypeWelcome:
		if err := proto.Unmarshal(raw, &c.welcome); err != nil {
			mux.Close()
			return nil, err
		}
	case proto.TypeError:
		var e proto.Error
		proto.Unmarshal(raw, &e)
		mux.Close()
		return nil, &RejectedError{Err: e}
	default:
		mux.Close()
		return nil, fmt.Errorf("session: unexpected %s before welcome", typ)
	}
	go c.readLoop(dec)
	go c.pingLoop(opts)
	return c, nil
}

// Welcome returns the agent's welcome message.
func (c *Client) Welcome() proto.Welcome { return c.welcome }

// Done is closed when the session has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the session ended (nil if Close was called).
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// DialTCP asks the agent to connect to addr and returns the stream.
func (c *Client) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	s, err := c.mux.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("session: open stream: %w", err)
	}
	if err := proto.NewEncoder(s).Encode(proto.TypeDial, proto.DialHeader{Addr: addr}); err != nil {
		s.Close()
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		s.SetReadDeadline(dl)
	} else {
		s.SetReadDeadline(time.Now().Add(15 * time.Second))
	}
	_, raw, err := proto.ReadHeader(s)
	s.SetReadDeadline(time.Time{})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("session: dial %s: %w", addr, err)
	}
	var reply proto.DialReply
	if err := proto.Unmarshal(raw, &reply); err != nil {
		s.Close()
		return nil, err
	}
	if !reply.OK {
		s.Close()
		return nil, fmt.Errorf("dial %s via agent: %s", addr, reply.Error)
	}
	return s, nil
}

// Resolve asks the agent to resolve name with the task's resolver.
func (c *Client) Resolve(ctx context.Context, name string) ([]string, int, error) {
	s, err := c.mux.OpenStream()
	if err != nil {
		return nil, 0, fmt.Errorf("session: open stream: %w", err)
	}
	defer s.Close()
	if err := proto.NewEncoder(s).Encode(proto.TypeResolve, proto.ResolveHeader{Name: name, QType: "A"}); err != nil {
		return nil, 0, err
	}
	if dl, ok := ctx.Deadline(); ok {
		s.SetReadDeadline(dl)
	} else {
		s.SetReadDeadline(time.Now().Add(10 * time.Second))
	}
	// A caller that cancels ctx (Ctrl-C on `tetherd doctor`, an errgroup
	// tearing down its siblings) must not stay blocked in ReadHeader until
	// the deadline set above fires.
	stop := context.AfterFunc(ctx, func() { s.SetReadDeadline(time.Now()) })
	defer stop()
	typ, rawReply, err := proto.ReadHeader(s)
	s.SetReadDeadline(time.Time{})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr
		}
		return nil, 0, fmt.Errorf("session: resolve %s: %w", name, err)
	}
	switch typ {
	case proto.TypeResolve:
		var reply proto.ResolveReply
		if err := proto.Unmarshal(rawReply, &reply); err != nil {
			return nil, 0, err
		}
		if !reply.OK {
			msg := reply.Error
			if msg == "" {
				msg = "agent did not say why"
			}
			if reply.NotFound {
				return nil, 0, &notFoundError{msg: fmt.Sprintf("resolve %s via agent: %s", name, msg)}
			}
			return nil, 0, fmt.Errorf("resolve %s via agent: %s", name, msg)
		}
		return reply.Addrs, reply.TTL, nil
	case proto.TypeError:
		// A pre-v0.2 agent doesn't know the resolve stream type and answers
		// TypeError(CodeBadHello, "unknown stream type resolve") instead —
		// version skew is the single most likely failure for this feature,
		// so name it plainly rather than surfacing the routing error as-is.
		var e proto.Error
		proto.Unmarshal(rawReply, &e)
		if e.Code == proto.CodeBadHello {
			return nil, 0, fmt.Errorf("resolve %s via agent: this agent does not support name resolution; upgrade the sidecar", name)
		}
		msg := e.Message
		if msg == "" {
			msg = "agent rejected the resolve request"
		}
		return nil, 0, fmt.Errorf("resolve %s via agent: %s", name, msg)
	default:
		return nil, 0, fmt.Errorf("resolve %s via agent: unexpected reply type %q", name, typ)
	}
}

// Close sends bye and tears the session down.
func (c *Client) Close() error {
	c.enc.Encode(proto.TypeBye, nil)
	c.finish(nil)
	return nil
}

func (c *Client) finish(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		c.mux.Close()
		close(c.done)
	})
}

func (c *Client) readLoop(dec *proto.Decoder) {
	for {
		typ, raw, err := dec.Decode()
		if err != nil {
			c.finish(fmt.Errorf("session: control stream closed: %w", err))
			return
		}
		switch typ {
		case proto.TypePong:
			select {
			case c.pong <- struct{}{}:
			default:
			}
		case proto.TypeError:
			var e proto.Error
			proto.Unmarshal(raw, &e)
			c.finish(&RejectedError{Err: e})
			return
		case proto.TypeBye:
			c.finish(errors.New("session: agent said bye"))
			return
		}
	}
}

func (c *Client) pingLoop(opts Options) {
	t := time.NewTicker(opts.PingInterval)
	defer t.Stop()
	missed := 0
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		if err := c.enc.Encode(proto.TypePing, nil); err != nil {
			c.finish(fmt.Errorf("session: ping: %w", err))
			return
		}
		select {
		case <-c.pong:
			missed = 0
		case <-time.After(opts.PingInterval):
			missed++
			if missed >= opts.MaxMissed {
				c.finish(fmt.Errorf("session: %d pings unanswered", missed))
				return
			}
		case <-c.done:
			return
		}
	}
}
