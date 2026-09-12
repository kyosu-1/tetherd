package agent

import (
	"fmt"
	"sort"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

// Session is one attached CLI as the agent needs it internally: who is
// attached, how to decide whether a request belongs to them, and how to
// reach them.
//
// token and open are unexported so that nothing outside this package can
// print, log or serialise them by accident. Sessions() hands out
// SessionInfo instead, which cannot carry a token at all - that is the
// point of having two types rather than one.
type Session struct {
	User     string
	From     string
	Since    time.Time
	Incoming proto.Incoming

	token string         // compared only, never logged and never in SessionInfo
	open  session.Opener // how the proxy pushes a stolen request at this CLI
}

// SessionInfo is the public view of a session: what the agent logs and what
// `tetherd status` prints. It deliberately has no token field.
type SessionInfo struct {
	User  string
	From  string
	Since time.Time
}

// Sessions lists attached users, sorted by user.
func (a *Agent) Sessions() []SessionInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]SessionInfo, 0, len(a.sessions))
	for _, s := range a.sessions {
		out = append(out, SessionInfo{User: s.User, From: s.From, Since: s.Since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out
}

// Match returns the session a request belongs to, or nil for the
// application. Held under the lock: a session can detach while a request is
// being routed.
func (a *Agent) Match(header func(string) string) *Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	all := make([]*Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		all = append(all, s)
	}
	return MatchSession(all, header)
}

// register records the session described by h. One user may hold one
// session at a time; a second hello for the same user is refused and
// changes nothing about the session that is already attached.
func (a *Agent) register(h proto.Hello, from string, open session.Opener) *proto.Error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.sessions[h.User]; ok {
		return &proto.Error{
			Code:    proto.CodeDuplicateUser,
			Message: fmt.Sprintf("another session for user %q is already attached", h.User),
			From:    s.From,
			Since:   s.Since.UTC().Format(time.RFC3339),
		}
	}
	a.sessions[h.User] = &Session{
		User:     h.User,
		From:     from,
		Since:    time.Now(),
		Incoming: h.Incoming,
		token:    h.Token,
		open:     open,
	}
	return nil
}

func (a *Agent) unregister(user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, user)
}
