package agent

import (
	"bufio"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kyosu-1/tetherd/internal/proto"
)

func sess(user, token string, enabled bool) *Session {
	return &Session{
		User:     user,
		token:    token,
		Incoming: proto.Incoming{Enabled: enabled, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"},
	}
}

func hdr(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// requestLookup builds the lookup the real proxy hands MatchSession:
// http.Header.Get over a request parsed off the wire. It is here so the
// header lookup is exercised in its measured shape rather than in a model
// of it - net/http canonicalises header names when it parses a request and
// Get canonicalises its argument, so a case difference between the name a
// developer configured and the name a browser extension sent cannot pass
// these tests and then fail on the ALB.
func requestLookup(t *testing.T, raw string) func(string) string {
	t.Helper()
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("parsing the request: %v", err)
	}
	return r.Header.Get
}

func TestMatchSessionNeedsBothUserAndToken(t *testing.T) {
	s := sess("shota", "tok-shota", true)
	all := []*Session{s}

	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok-shota"})); got != s {
		t.Errorf("both matching must steal: %v", got)
	}
	// The token is the whole security property on a public ALB: knowing the
	// user name must not be enough (spec §11).
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "shota"})); got != nil {
		t.Errorf("a missing token must not steal: %v", got)
	}
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "wrong"})); got != nil {
		t.Errorf("a wrong token must not steal: %v", got)
	}
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-Token": "tok-shota"})); got != nil {
		t.Errorf("a token without the user must not steal: %v", got)
	}
	// A health check carries neither header.
	if got := MatchSession(all, hdr(nil)); got != nil {
		t.Errorf("a request with no headers must go to the app: %v", got)
	}
	// Both headers present but empty is a browser extension that is
	// installed and not configured, not a match.
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "", "X-Dev-Token": ""})); got != nil {
		t.Errorf("empty headers must not steal: %v", got)
	}
}

func TestMatchSessionPicksTheRightUserAmongSeveral(t *testing.T) {
	a, b := sess("shota", "tok-a", true), sess("taro", "tok-b", true)
	all := []*Session{a, b}
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "taro", "X-Dev-Token": "tok-b"})); got != b {
		t.Errorf("got %v, want taro's session", got)
	}
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok-a"})); got != a {
		t.Errorf("got %v, want shota's session", got)
	}
	// taro's name with shota's token belongs to nobody.
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "taro", "X-Dev-Token": "tok-a"})); got != nil {
		t.Errorf("a token from another session must not steal: %v", got)
	}
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok-b"})); got != nil {
		t.Errorf("a token from another session must not steal: %v", got)
	}
	// An unknown user with a valid token belongs to nobody either.
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "hanako", "X-Dev-Token": "tok-a"})); got != nil {
		t.Errorf("an unattached user must not steal: %v", got)
	}
}

func TestMatchSessionRespectsIncomingDisabledAndCustomHeaders(t *testing.T) {
	off := sess("shota", "tok", false)
	if got := MatchSession([]*Session{off}, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok"})); got != nil {
		t.Errorf("--no-incoming means this session takes nothing: %v", got)
	}
	custom := sess("shota", "tok", true)
	custom.Incoming.Header = "X-Who"
	custom.Incoming.TokenHeader = "X-Secret"
	if got := MatchSession([]*Session{custom}, hdr(map[string]string{"X-Who": "shota", "X-Secret": "tok"})); got != custom {
		t.Errorf("custom header names must be honoured: %v", got)
	}
	if got := MatchSession([]*Session{custom}, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok"})); got != nil {
		t.Errorf("the default names must not also work: %v", got)
	}
	// The two names must not be read the other way round.
	if got := MatchSession([]*Session{custom}, hdr(map[string]string{"X-Who": "tok", "X-Secret": "shota"})); got != nil {
		t.Errorf("the user and token headers must not be swapped: %v", got)
	}
	// A disabled session must not be reachable under its own custom names
	// either, and must not hide a later session that does match.
	offCustom := sess("taro", "tok-t", false)
	offCustom.Incoming.Header, offCustom.Incoming.TokenHeader = "X-Who", "X-Secret"
	all := []*Session{offCustom, custom}
	if got := MatchSession(all, hdr(map[string]string{"X-Who": "shota", "X-Secret": "tok"})); got != custom {
		t.Errorf("a disabled session must not shadow a matching one: %v", got)
	}
}

func TestMatchSessionRejectsAnEmptyToken(t *testing.T) {
	// A session that somehow attached without a token must not be matchable
	// by a request that also sends no token - that would make every
	// header-only request steal.
	s := sess("shota", "", true)
	if got := MatchSession([]*Session{s}, hdr(map[string]string{"X-Dev-User": "shota"})); got != nil {
		t.Errorf("an empty token must never match: %v", got)
	}
	if got := MatchSession([]*Session{s}, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": ""})); got != nil {
		t.Errorf("an empty token must never match: %v", got)
	}
	// Nor may a session with no user name be matched by a request that
	// sends no user header, for the same reason.
	anon := sess("", "tok", true)
	if got := MatchSession([]*Session{anon}, hdr(map[string]string{"X-Dev-Token": "tok"})); got != nil {
		t.Errorf("a session with no user name must never match: %v", got)
	}
}

// TestMatchSessionOverARealRequest pins the lookup shape: the proxy will
// hand MatchSession an http.Header.Get, and what a browser actually sends
// differs in case from what a config file spells.
func TestMatchSessionOverARealRequest(t *testing.T) {
	// Lowercase on the wire (HTTP/2 clients send it that way, and curl -H
	// takes whatever the user typed), canonical in the config.
	s := sess("shota", "tok-shota", true)
	lower := requestLookup(t, "GET /orders HTTP/1.1\r\nHost: dev.example.com\r\nx-dev-user: shota\r\nx-dev-token: tok-shota\r\n\r\n")
	if got := MatchSession([]*Session{s}, lower); got != s {
		t.Errorf("a lowercase header on the wire must still match the configured name: %v", got)
	}
	// Lowercase in the config (incoming.match.header is free text), as sent
	// by a browser extension that spells the name canonically.
	cfgLower := sess("shota", "tok-shota", true)
	cfgLower.Incoming.Header, cfgLower.Incoming.TokenHeader = "x-dev-user", "x-dev-token"
	canonical := requestLookup(t, "GET /orders HTTP/1.1\r\nHost: dev.example.com\r\nX-Dev-User: shota\r\nX-Dev-Token: tok-shota\r\n\r\n")
	if got := MatchSession([]*Session{cfgLower}, canonical); got != cfgLower {
		t.Errorf("a lowercase configured name must still match what the wire canonicalises to: %v", got)
	}
	// Header *values* are not case folded: a token is a secret, not a name.
	wrongCase := requestLookup(t, "GET /orders HTTP/1.1\r\nHost: dev.example.com\r\nX-Dev-User: shota\r\nX-Dev-Token: TOK-SHOTA\r\n\r\n")
	if got := MatchSession([]*Session{s}, wrongCase); got != nil {
		t.Errorf("a token differing only in case must not match: %v", got)
	}
	// The ALB health check, verbatim in shape: no steal headers at all.
	health := requestLookup(t, "GET /healthz HTTP/1.1\r\nHost: 10.0.1.23:8080\r\nUser-Agent: ELB-HealthChecker/2.0\r\nAccept: */*\r\nConnection: close\r\n\r\n")
	if got := MatchSession([]*Session{s}, health); got != nil {
		t.Errorf("an ALB health check must belong to nobody: %v", got)
	}
}

// TestTokenComparisonIsConstantTime guards the one property of this file
// that no amount of behavioural testing can observe: an equal/unequal
// answer is identical whether it came from subtle.ConstantTimeCompare or
// from ==, so the difference is only visible in the source. The token is
// the only thing between a public ALB and a developer's laptop, and a
// byte-by-byte comparison leaks its prefix to anyone who can time the ALB.
func TestTokenComparisonIsConstantTime(t *testing.T) {
	src, err := os.ReadFile("steal.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "subtle.ConstantTimeCompare") {
		t.Error("the token comparison must use crypto/subtle.ConstantTimeCompare")
	}
	if strings.Contains(string(src), "got == s.token") || strings.Contains(string(src), "s.token == got") {
		t.Error("the token must not be compared with ==")
	}
}
