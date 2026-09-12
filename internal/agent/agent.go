package agent

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

// Agent serves control sessions.
type Agent struct {
	cfg    Config
	logf   func(string, ...any)
	env    EnvReader // nil outside ECS
	dial   func(ctx context.Context, addr string) (net.Conn, error)
	lookup func(ctx context.Context, name string) ([]net.IPAddr, error)

	mu       sync.Mutex
	sessions map[string]*Session
}

// New returns an Agent. logf receives one line per event (nil = silent).
func New(cfg Config, logf func(string, ...any)) *Agent {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	a := &Agent{cfg: cfg, logf: logf, sessions: map[string]*Session{}}
	if cfg.MetadataURL != "" {
		a.env = &ProcEnvReader{MetadataURL: cfg.MetadataURL, ProcRoot: "/proc", AppContainer: cfg.AppContainer}
	}
	return a
}

// SetEnvReader replaces the env source (tests).
func (a *Agent) SetEnvReader(r EnvReader) { a.env = r }

// SetDialer replaces what a dial stream connects to. Production leaves it
// unset and the agent dials the address itself; tests point it at a stub so
// the CLI side can be exercised without a VPC.
func (a *Agent) SetDialer(dial func(ctx context.Context, addr string) (net.Conn, error)) {
	a.dial = dial
}

// ListenAndServe listens on cfg.Control and serves until ctx is done.
func (a *Agent) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.Control)
	if err != nil {
		return err
	}
	a.logf("control listening on %s (env=%s)", ln.Addr(), a.cfg.Env)
	return a.Serve(ctx, ln)
}

// Serve accepts sessions on ln.
func (a *Agent) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			h := &handler{a: a}
			if err := session.Serve(ctx, conn, h, session.ServeOptions{}); err != nil {
				a.logf("session from %s ended: %v", conn.RemoteAddr(), err)
			}
			conn.Close()
		}()
	}
}

func (a *Agent) others(user string) []string {
	var out []string
	for _, s := range a.Sessions() {
		if s.User != user {
			out = append(out, s.User)
		}
	}
	return out
}

// handler implements session.Handler for one connection.
type handler struct {
	a    *Agent
	user string
}

// Hello registers the user's session: the user and where they attached
// from, plus the token and incoming rules a request is matched against and
// the Opener the L7 proxy uses to push a stolen request at this user's CLI.
func (h *handler) Hello(hello proto.Hello, remote string, open session.Opener) (proto.Welcome, *proto.Error) {
	if hello.User == "" {
		return proto.Welcome{}, &proto.Error{Code: proto.CodeBadHello, Message: "hello.user is empty"}
	}
	if e := h.a.register(hello, remote, open); e != nil {
		return proto.Welcome{}, e
	}
	h.user = hello.User
	h.a.logf("user %q attached from %s", hello.User, remote)
	w := proto.Welcome{Version: proto.Version, TaskARN: h.a.cfg.TaskARN, Env: h.a.cfg.Env, Others: h.a.others(hello.User)}
	if h.a.env == nil {
		w.EnvError = "no ECS metadata endpoint (agent is not running in ECS)"
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		env, arn, err := h.a.env.Read(ctx)
		cancel()
		if err != nil {
			w.EnvError = err.Error()
			h.a.logf("env for %q: %v", hello.User, err)
		} else {
			w.AppEnv = env
			if arn != "" {
				w.TaskARN = arn
			}
			h.a.logf("env for %q: %d vars from container %q", hello.User, len(env), h.a.cfg.AppContainer)
		}
	}
	return w, nil
}

func (h *handler) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if h.a.dial != nil {
		return h.a.dial(ctx, addr)
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

func (h *handler) Closed() {
	if h.user != "" {
		h.a.unregister(h.user)
		h.a.logf("user %q detached", h.user)
	}
}
