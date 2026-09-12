package agent

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

// Session is one attached CLI as the agent needs it internally: who is
// attached, how to decide whether a request belongs to them, and how to
// reach them.
//
// token and open are unexported so that no other package can reach them,
// and String/GoString redact the token so that fmt cannot print it either -
// unexported is not enough on its own, because fmt reads unexported fields
// by reflection and `%+v` on this type would otherwise put a plaintext
// token in the agent's log.
//
// A Session is never mutated after it is inserted into the registry:
// register writes a fresh *Session and unregister deletes the map entry, so
// nothing ever writes to one that a reader may be holding. That invariant
// is what makes it safe for Match to return the pointer after releasing the
// lock; a future field that changes over the session's life needs its own
// synchronisation rather than an assignment here.
type Session struct {
	User     string
	From     string
	Since    time.Time
	Incoming proto.Incoming

	token string         // compared only, through tokenEqual; never printed
	open  session.Opener // how the proxy pushes a stolen request at this CLI
}

// String renders the session without its token. It exists so that an
// innocent log line - p.Logf("steal for %v", s) - cannot disclose the one
// secret that stands between the public ALB and a developer's laptop.
func (s Session) String() string {
	return fmt.Sprintf("Session{User:%s From:%s Since:%s Incoming:%+v token:<redacted> open:%t}",
		s.User, s.From, s.Since.UTC().Format(time.RFC3339), s.Incoming, s.open != nil)
}

// GoString renders the session without its token under %#v, which does not
// go through String.
func (s Session) GoString() string {
	return fmt.Sprintf("agent.Session{User:%q, From:%q, Since:%q, Incoming:%#v, token:%q, open:%t}",
		s.User, s.From, s.Since.UTC().Format(time.RFC3339), s.Incoming, "<redacted>", s.open != nil)
}

// SessionInfo is the public view of a session: what the agent logs and what
// `tetherd status` prints. It deliberately has no token field.
type SessionInfo struct {
	User  string
	From  string
	Since time.Time
}

// Sessions lists attached users, sorted by user. The sort is part of the
// contract, not tidiness: others() derives Welcome.Others from this slice,
// so an unsorted result makes a welcome message differ from one attach to
// the next.
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

// MatchRequest returns the session the request belongs to, or nil for the
// application. This is the entry point the proxy uses: it supplies the
// header lookup itself, so no caller has to know that the lookup must be
// case-insensitive in both directions (what a config file spelled and what
// the client sent). http.Header.Get reads the first occurrence of a header
// sent more than once, which is the documented rule for a duplicated
// X-Dev-Token: the first one is the one that has to be right.
func (a *Agent) MatchRequest(r *http.Request) *Session {
	return a.Match(r.Header.Get)
}

// Match returns the session a request belongs to, or nil for the
// application. Prefer MatchRequest; this form exists for callers that have
// a lookup but no *http.Request (tests).
//
// The registry lock is held for the whole decision, because a session can
// detach while a request is being routed. header is therefore called under
// that lock and must not call back into the Agent - Sessions(), Match() and
// register() all deadlock from inside the lookup, since sync.Mutex is not
// reentrant.
//
// The returned *Session outlives the lock. That is safe because a Session
// is never mutated after insertion (see the type's comment); the session
// may nevertheless have detached by the time the caller uses it, so a
// failing open is a normal outcome and means "serve from the application".
func (a *Agent) Match(header func(string) string) *Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	all := make([]*Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		all = append(all, s)
	}
	return MatchSession(all, header)
}

// register records the session described by h, if it is one the registry is
// for. One user may hold one *steal* session at a time; a second such hello
// for the same user is refused and changes nothing about the session that is
// already attached.
//
// A session that does not declare Incoming.Enabled is not recorded at all,
// and is never refused. The registry is the steal routing table and nothing
// else - MatchSession skips every session with Incoming disabled, so a
// read-only session could not be matched even when it was recorded - so
// leaving those out costs nothing and buys three things:
//
//   - `tetherd env`, `tetherd doctor` and `tetherd status` stop colliding
//     with this developer's own `tetherd run`. Until this change any of
//     them, run while that developer's run was attached, was refused with
//     duplicate_user - and a live run is exactly when someone reaches for
//     doctor or status. (Shipped in v0.2b and v0.3a; found by `status`.)
//   - Welcome.Others and Welcome.Sessions come to mean "who can receive
//     requests", which is the question `tetherd status` answers. A
//     read-only attach reported as "attached" would be misleading: it can
//     receive nothing and it is gone a moment later.
//   - unregister(user) stays unambiguous, because no two entries can share
//     a name. Recording read-only sessions under a second key would have
//     made the detach path ambiguous instead - and with the map keyed by
//     user, recording them under the same key would have a `status` attach
//     overwrite that developer's run and its detach delete it, which is
//     the developer's requests silently stopping.
//
// Nothing about steal routing changes: a second steal session for one user
// is still refused, which is the ambiguity the rule exists to prevent.
func (a *Agent) register(h proto.Hello, from string, open session.Opener) *proto.Error {
	if !stealSession(h) {
		return nil
	}
	a.mu.Lock()
	if s, ok := a.sessions[h.User]; ok {
		e := &proto.Error{
			Code:    proto.CodeDuplicateUser,
			Message: fmt.Sprintf("another session for user %q is already attached; if you just stopped a session of your own, the agent may not have noticed the disconnect yet - retry in a moment", h.User),
			From:    s.From,
			Since:   s.Since.UTC().Format(time.RFC3339),
		}
		a.mu.Unlock()
		return e
	}
	a.sessions[h.User] = &Session{
		User:     h.User,
		From:     from,
		Since:    time.Now(),
		Incoming: h.Incoming,
		token:    h.Token,
		open:     open,
	}
	a.mu.Unlock()
	// Logged outside the lock: logf belongs to whoever built the Agent and
	// nothing the registry holds should be held while calling out to it.
	//
	// A hello that asks for incoming requests but leaves out the token or
	// either header name produces a successful attach followed by requests
	// that go to the application forever with nothing naming the cause. Say
	// it once, here.
	//
	// tetherd's own CLI cannot send such a hello: internal/cli/steal.go's
	// stealSettings fills both header names from DefaultMatchHeader /
	// DefaultMatchTokenHeader and refuses an empty token before the first
	// AWS call. What can is a hand-built proto.Hello - an older or
	// third-party client - which the protocol is additive enough to admit,
	// so the guard stays.
	if h.Incoming.Enabled {
		if gap := incomingGap(h.Incoming, h.Token); gap != "" {
			a.logf("user %q attached but cannot receive incoming requests: %s; every request will go to the application", h.User, gap)
		}
	}
	return nil
}

// stealSession reports whether a hello asks to receive requests, and so
// whether it belongs in the registry.
//
// It is a named function with two callers on purpose: register records only
// these, and handler.Hello may set handler.user only for these, because
// handler.Closed unregisters whatever handler.user names. A read-only
// session that set it would, on detaching, unregister that developer's own
// steal session - and their requests would stop arriving with nothing
// naming the cause. One predicate, so the two cannot drift apart.
func stealSession(h proto.Hello) bool { return h.Incoming.Enabled }

// incomingGap names what an enabled Incoming is missing, or "" if it is
// usable. It never returns the token itself.
func incomingGap(in proto.Incoming, token string) string {
	switch {
	case token == "":
		return "the hello carries no token"
	case in.Header == "":
		return "incoming.match.header is empty"
	case in.TokenHeader == "":
		return "incoming.match.token_header is empty"
	}
	return ""
}

func (a *Agent) unregister(user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, user)
}
