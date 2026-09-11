package agent

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

// SessionInfo describes one attached CLI.
type SessionInfo struct {
	User  string
	From  string
	Since time.Time
}

// Agent serves control sessions.
type Agent struct {
	cfg  Config
	logf func(string, ...any)
	env  EnvReader // nil outside ECS

	mu       sync.Mutex
	sessions map[string]SessionInfo
}

// New returns an Agent. logf receives one line per event (nil = silent).
func New(cfg Config, logf func(string, ...any)) *Agent {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	a := &Agent{cfg: cfg, logf: logf, sessions: map[string]SessionInfo{}}
	if cfg.MetadataURL != "" {
		a.env = &ProcEnvReader{MetadataURL: cfg.MetadataURL, ProcRoot: "/proc", AppContainer: cfg.AppContainer}
	}
	return a
}

// SetEnvReader replaces the env source (tests).
func (a *Agent) SetEnvReader(r EnvReader) { a.env = r }

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

// Sessions lists attached users, sorted by user.
func (a *Agent) Sessions() []SessionInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]SessionInfo, 0, len(a.sessions))
	for _, s := range a.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out
}

func (a *Agent) register(user, from string) *proto.Error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.sessions[user]; ok {
		return &proto.Error{
			Code:    proto.CodeDuplicateUser,
			Message: fmt.Sprintf("another session for user %q is already attached", user),
			From:    s.From,
			Since:   s.Since.UTC().Format(time.RFC3339),
		}
	}
	a.sessions[user] = SessionInfo{User: user, From: from, Since: time.Now()}
	return nil
}

func (a *Agent) unregister(user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, user)
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

func (h *handler) Hello(hello proto.Hello, remote string) (proto.Welcome, *proto.Error) {
	if hello.User == "" {
		return proto.Welcome{}, &proto.Error{Code: proto.CodeBadHello, Message: "hello.user is empty"}
	}
	if e := h.a.register(hello.User, remote); e != nil {
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
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

func (h *handler) Closed() {
	if h.user != "" {
		h.a.unregister(h.user)
		h.a.logf("user %q detached", h.user)
	}
}
