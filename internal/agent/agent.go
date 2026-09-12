package agent

import (
	"context"
	"fmt"
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

// Run serves both ports the agent owns - the control port the CLIs attach
// to and the ALB port in front of the application - until ctx is done.
//
// It returns as soon as either one stops, and the caller is expected to
// exit: the sidecar is an essential container, so exiting has ECS replace
// the task, whereas half an agent is a task whose attach path or whose ALB
// path is silently dead. Both listeners are opened before either is served
// so that a port already in use is a startup error naming the port rather
// than a task that comes up serving one half.
func (a *Agent) Run(ctx context.Context) error {
	control, err := net.Listen("tcp", a.cfg.Control)
	if err != nil {
		return fmt.Errorf("agent: listen on the control port %s: %w", a.cfg.Control, err)
	}
	defer control.Close()
	proxy, err := net.Listen("tcp", a.cfg.Proxy)
	if err != nil {
		return fmt.Errorf("agent: listen on the ALB port %s: %w", a.cfg.Proxy, err)
	}
	defer proxy.Close()
	// One line, both addresses: this is what says in CloudWatch that the
	// task is on the ALB's data path at all.
	a.logf("control listening on %s, proxy listening on %s -> app %s (env=%s)",
		control.Addr(), proxy.Addr(), a.cfg.AppAddr, a.cfg.Env)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- a.Serve(ctx, control) }()
	go func() { done <- a.ServeProxy(ctx, proxy) }()

	// Both halves, not whichever stops first. The control accept loop
	// returns the moment its listener closes, while ServeProxy is still
	// draining the ALB's in-flight requests through http.Server.Shutdown -
	// so returning on the first send would have main exit and sever exactly
	// the requests that drain exists to finish. On SIGTERM that is the
	// difference between a rolling deployment and a handful of 502s.
	first := <-done
	// Whichever half stopped, the other one is asked to stop too: half an
	// agent is a task whose attach path or whose ALB path is silently dead.
	cancel()
	second := <-done
	// The first non-nil error, so that a real failure is not hidden behind
	// the nil the other half returns for an ordinary shutdown.
	if first != nil {
		return first
	}
	return second
}

// ListenAndServe listens on cfg.Control and serves until ctx is done. It
// serves the control port only; Run is what production starts.
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

// sessionInfos is what Welcome.Sessions carries: every session attached
// other than user's own, with where it attached from and since when.
//
// It reuses Sessions()'s sort, which is a contract rather than tidiness
// (see registry.go): unsorted, the welcome one developer reads would differ
// from one attach to the next and so would `tetherd status`'s output.
//
// user's own session is left out so that this is exactly the set others()
// returns, one field richer - see Welcome. It is also what makes `tetherd
// status`, which has to attach in order to ask, not report itself as an
// attached developer.
func (a *Agent) sessionInfos(user string) []proto.SessionInfo {
	var out []proto.SessionInfo
	for _, s := range a.Sessions() {
		if s.User != user {
			out = append(out, proto.SessionInfo{User: s.User, From: s.From, Since: s.Since})
		}
	}
	return out
}

// others is Welcome.Others: the user names of sessionInfos, which is what
// every CLI up to v0.3a reads. Derived from the same slice rather than from
// a second snapshot of the registry, so the two fields of one welcome can
// never describe different sets.
func (a *Agent) others(user string) []string {
	return sessionUsers(a.sessionInfos(user))
}

// sessionUsers projects the user names out of infos. It returns nil for an
// empty input, not an empty slice, so that Welcome.Others keeps being
// omitted from the wire when nobody else is attached.
func sessionUsers(infos []proto.SessionInfo) []string {
	var out []string
	for _, s := range infos {
		out = append(out, s.User)
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
	// One snapshot of the registry for both views of it: Others is the
	// names, Sessions the same set with the detail `tetherd status` needs.
	sessions := h.a.sessionInfos(hello.User)
	w := proto.Welcome{
		Version:  proto.Version,
		TaskARN:  h.a.cfg.TaskARN,
		Env:      h.a.cfg.Env,
		Others:   sessionUsers(sessions),
		Sessions: sessions,
	}
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
