package agent

import "crypto/subtle"

// MatchSession picks the session a request belongs to: the one whose user
// name and token both match the request's headers. Returns nil when no
// session claims it, which is every health check and every request from a
// user who is not attached - those go to the application.
//
// Production does not call this. (*Agent).MatchRequest is the entry point,
// and it supplies the header lookup itself; this function takes the lookup
// as an argument so that tests can drive it without an http.Request, and
// that flexibility is exactly why no production caller should have it - a
// caller that indexes r.Header directly, or reads r.Header[name], keeps
// working for canonically spelled names and silently stops matching a
// lowercase incoming.match.header, which internal/config accepts verbatim.
//
// header is called with a header name and must answer case-insensitively,
// the way http.Header.Get does: the name is written in a config file at one
// end and sent by a browser extension at the other, and the two do not
// agree on case.
//
// header is called while the registry lock is held (see (*Agent).Match), so
// it must be pure. A lookup that calls back into the Agent - Sessions(),
// Match(), register() - deadlocks: sync.Mutex is not reentrant.
//
// The first matching session wins and the order is unspecified (the caller
// builds the slice from a map). That is only reachable by a request that
// carries valid credentials for two sessions at once, which in turn needs
// those sessions to have configured different header names; a request can
// otherwise satisfy at most one session.
//
// The token comparison is constant time and goes through tokenEqual. The
// user name's does not, and should not pretend to: user names are not
// secret - they travel in a header a browser extension sets and the agent's
// own welcome lists who else is attached - while the token is the only
// thing standing between a public ALB and a developer's laptop (spec §5.2,
// §11).
func MatchSession(sessions []*Session, header func(string) string) *Session {
	for _, s := range sessions {
		// A session with no token, no user name, or no way back to its CLI
		// can never be matched. The first two because a request that simply
		// omits a header would otherwise steal from a session that attached
		// without one; the third because a session with no Opener cannot be
		// served at all, and answering "this one" would panic on the ALB
		// path instead of falling through to the application.
		if !s.Incoming.Enabled || len(s.token) == 0 || s.User == "" || s.open == nil {
			continue
		}
		// Case-sensitive on purpose: the registry is keyed by the exact
		// user name, so "Shota" and "shota" are two different sessions and
		// folding them together would make routing between two legitimately
		// attached developers depend on map iteration order.
		if header(s.Incoming.Header) != s.User {
			continue
		}
		if tokenEqual(header(s.Incoming.TokenHeader), s.token) {
			return s
		}
	}
	return nil
}

// tokenEqual reports whether the token a request presented is the session's
// token. It is the only place a session token is ever compared, so that
// "the comparison is constant time" is a property of one three-line
// function instead of a habit spread over the package - and so that a test
// can check structurally that nothing compares the token any other way.
//
// subtle.ConstantTimeCompare answers in time that does not depend on where
// the two strings first differ, which is what keeps the ALB from leaking a
// token's prefix to anyone who can time it. It also returns 0 for strings
// of different lengths, so a token that is a prefix or an extension of the
// real one is rejected like any other wrong token.
func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
