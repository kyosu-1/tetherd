package agent

import "crypto/subtle"

// MatchSession picks the session a request belongs to: the one whose user
// name and token both match the request's headers. Returns nil when no
// session claims it, which is every health check and every request from a
// user who is not attached - those go to the application.
//
// header looks a header up by name and must be case-insensitive, the way
// http.Header.Get is: the name travels in a config file at one end and in a
// browser extension at the other, and the two do not agree on case. Pass
// r.Header.Get and nothing else.
//
// The token comparison is constant time. The user name's is not, and does
// not need to be: user names are not secret (they travel in a header a
// browser extension sets, and the agent's own welcome lists who else is
// attached), while the token is the only thing standing between a public
// ALB and a developer's laptop (spec §5.2, §11).
func MatchSession(sessions []*Session, header func(string) string) *Session {
	for _, s := range sessions {
		// An empty token or user name can never be matched: otherwise a
		// request that simply omits a header would steal from a session
		// that attached without one.
		if !s.Incoming.Enabled || s.token == "" || s.User == "" {
			continue
		}
		if header(s.Incoming.Header) != s.User {
			continue
		}
		got := header(s.Incoming.TokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1 {
			return s
		}
	}
	return nil
}
