package agent

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kyosu-1/tetherd/internal/proto"
)

func sess(user, tok string, enabled bool) *Session {
	return &Session{
		User:     user,
		token:    tok,
		open:     nopOpener{},
		Incoming: proto.Incoming{Enabled: enabled, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"},
	}
}

func hdr(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// parseRequest returns a request read off the wire, so that tests see the
// header names as net/http actually presents them rather than as a map
// literal spells them.
func parseRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("parsing the request: %v", err)
	}
	return r
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
	return parseRequest(t, raw).Header.Get
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

// TestMatchSessionRejectsAPrefixOrExtensionOfTheToken pins the length half
// of the comparison. A regression that compares only as far as the shorter
// string pays an attacker who learns a prefix: they would then steal with
// <prefix> plus anything.
func TestMatchSessionRejectsAPrefixOrExtensionOfTheToken(t *testing.T) {
	s := sess("shota", "tok-shota", true)
	for _, presented := range []string{"tok-shotaX", "tok-shota ", "tok-sho", "tok", "", "tok-shotatok-shota"} {
		if got := MatchSession([]*Session{s}, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": presented})); got != nil {
			t.Errorf("token %q must not match %q: %v", presented, "tok-shota", got)
		}
	}
}

// TestMatchSessionIsCaseSensitiveAboutTheUserName keeps the name comparison
// exact. Folding it is not a hole by itself - the token is still required -
// but "Shota" and "shota" are two distinct registry keys, so a folding
// match would make routing between two legitimately attached sessions
// depend on map iteration order.
func TestMatchSessionIsCaseSensitiveAboutTheUserName(t *testing.T) {
	s := sess("shota", "tok-shota", true)
	for _, name := range []string{"Shota", "SHOTA", "sHoTa"} {
		if got := MatchSession([]*Session{s}, hdr(map[string]string{"X-Dev-User": name, "X-Dev-Token": "tok-shota"})); got != nil {
			t.Errorf("user %q must not match session %q: %v", name, s.User, got)
		}
	}
	// Same over the wire, where only the header *name* is canonicalised.
	wire := requestLookup(t, "GET / HTTP/1.1\r\nHost: dev.example.com\r\nX-Dev-User: Shota\r\nX-Dev-Token: tok-shota\r\n\r\n")
	if got := MatchSession([]*Session{s}, wire); got != nil {
		t.Errorf("a differently cased user name must not match on the wire: %v", got)
	}
}

// TestMatchSessionSkipsASessionWithNoOpener covers the session the proxy
// could not serve anyway: without an Opener there is no stream to push the
// request down, so claiming the request would turn into a nil-interface
// panic on the ALB path instead of a quiet fallback to the application.
func TestMatchSessionSkipsASessionWithNoOpener(t *testing.T) {
	s := sess("shota", "tok-shota", true)
	s.open = nil
	if got := MatchSession([]*Session{s}, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok-shota"})); got != nil {
		t.Errorf("a session with no opener cannot be served and must not match: %v", got)
	}
	// It must not shadow a session that can be served either.
	ok := sess("shota", "tok-shota", true)
	if got := MatchSession([]*Session{s, ok}, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok-shota"})); got != ok {
		t.Errorf("an openerless session must not shadow a serviceable one: %v", got)
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

// TestTheTokenIsOnlyEverComparedInConstantTime guards the one property of
// this package that no behavioural test can observe: == and
// subtle.ConstantTimeCompare return the same answers, so the difference
// lives in the source and only a structural check can see it.
//
// It reads the package directory rather than a named file, so moving the
// comparison to another file neither passes nor fails it for the wrong
// reason, and it asserts a rule instead of a spelling: every syntactic
// reference to a session's token field must be an argument to tokenEqual
// (or to len, which leaks nothing), and tokenEqual itself must compare with
// crypto/subtle and nothing else.
func TestTheTokenIsOnlyEverComparedInConstantTime(t *testing.T) {
	fset, files := productionFiles(t)

	allowed := map[token.Pos]bool{}  // may see the token at all
	compared := map[token.Pos]bool{} // actually compares it, in constant time
	var refs []token.Pos
	tokenEqualSeen := false
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				maySee, comparison := calleeAllowsTheToken(x.Fun)
				if maySee {
					for _, arg := range x.Args {
						if pos, ok := tokenFieldPos(arg); ok {
							allowed[pos] = true
							if comparison {
								compared[pos] = true
							}
						}
					}
				}
			case *ast.SelectorExpr:
				if x.Sel.Name == "token" {
					refs = append(refs, x.Pos())
				}
			case *ast.FuncDecl:
				if x.Name.Name == "tokenEqual" {
					tokenEqualSeen = true
					checkConstantTimeHelper(t, fset, x)
				}
			}
			return true
		})
	}

	if !tokenEqualSeen {
		t.Fatal("no tokenEqual helper: the token comparison must live in one constant-time function")
	}
	if len(refs) == 0 {
		t.Fatal("no reference to a session's token field was found; this test has stopped checking anything")
	}
	comparisons := 0
	for _, pos := range refs {
		if !allowed[pos] {
			t.Errorf("%s: the token field is used outside a constant-time comparison "+
				"(it may only be an argument to tokenEqual or len)", fset.Position(pos))
			continue
		}
		if compared[pos] {
			comparisons++
		}
	}
	if comparisons == 0 {
		t.Error("the token field is never passed to tokenEqual; nothing compares it in constant time")
	}
}

// productionFiles parses every non-test file of this package.
func productionFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) < 2 {
		t.Fatalf("parsed %d files: the package directory was not read", len(files))
	}
	return fset, files
}

// calleeAllowsTheToken reports whether a call may be handed the token
// field, and whether that call is a comparison of it. The constant-time
// helper and crypto/subtle compare it; len only sees its length, which is
// how the "no empty token" guard is written and leaks nothing.
func calleeAllowsTheToken(fun ast.Expr) (maySee, comparison bool) {
	switch f := fun.(type) {
	case *ast.Ident:
		if f.Name == "tokenEqual" {
			return true, true
		}
		return f.Name == "len", false
	case *ast.SelectorExpr:
		if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "subtle" {
			return true, true
		}
	}
	return false, false
}

// tokenFieldPos returns the position of a `x.token` selector, seeing
// through a []byte(...) conversion.
func tokenFieldPos(e ast.Expr) (token.Pos, bool) {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		if x.Sel.Name == "token" {
			return x.Pos(), true
		}
	case *ast.CallExpr:
		if _, isConversion := x.Fun.(*ast.ArrayType); isConversion && len(x.Args) == 1 {
			return tokenFieldPos(x.Args[0])
		}
	}
	return 0, false
}

// checkConstantTimeHelper asserts that the helper every token comparison
// goes through compares with crypto/subtle and with nothing else - the rule
// above is worth nothing if tokenEqual's own body says got == want.
//
// The rule is a whitelist of what a constant-time comparison may look like,
// not a list of ways to get it wrong, so it does not have to anticipate the
// next evasion:
//
//   - every == / != in the body must have an operand that is a call to
//     subtle.ConstantTimeCompare or to len. That admits the two shapes a
//     correct implementation needs - `subtle.ConstantTimeCompare(a, b) == 1`
//     and a `len(a) != len(b)` guard - and rejects everything else;
//   - at least one == / != must have a subtle.ConstantTimeCompare operand,
//     so a call whose result is discarded does not count as the comparison.
//
// Both clauses are about the *shape* of the comparison and never about
// identifiers, which is what makes them indifferent to renamed parameters
// and to values laundered through locals: `g, w := got, want; return g == w`
// is rejected by the first clause no matter what the locals are called, and
// `_ = subtle.ConstantTimeCompare(...)` as a decoy is rejected by the
// second.
func checkConstantTimeHelper(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) {
	t.Helper()
	comparisons, constantTime := 0, 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			switch callee := calleeName(x); callee {
			case "bytes.Equal", "strings.Compare", "strings.EqualFold", "strings.Contains", "strings.HasPrefix", "strings.HasSuffix":
				t.Errorf("%s: %s compares the token with %s, which is not constant time",
					fset.Position(x.Pos()), fn.Name.Name, callee)
			}
		case *ast.BinaryExpr:
			if x.Op != token.EQL && x.Op != token.NEQ {
				return true
			}
			comparisons++
			ct, allowed := false, false
			for _, side := range []ast.Expr{x.X, x.Y} {
				switch calleeName(side) {
				case "subtle.ConstantTimeCompare":
					ct, allowed = true, true
				case "len":
					allowed = true
				}
			}
			if ct {
				constantTime++
			}
			if !allowed {
				t.Errorf("%s: %s compares with %s on operands that are not a "+
					"subtle.ConstantTimeCompare result or a len; a constant-time helper may only "+
					"compare subtle.ConstantTimeCompare(a, b) == 1 or len(a) != len(b)",
					fset.Position(x.Pos()), fn.Name.Name, x.Op)
			}
		}
		return true
	})
	if comparisons == 0 || constantTime == 0 {
		t.Errorf("%s: %s must compare with crypto/subtle.ConstantTimeCompare and use its "+
			"result, as in subtle.ConstantTimeCompare(a, b) == 1 (a call whose result is "+
			"discarded compares nothing)", fset.Position(fn.Pos()), fn.Name.Name)
	}
}

// calleeName names the function a call expression calls ("len",
// "subtle.ConstantTimeCompare"), or "" when the expression is not a call to
// a plain identifier or a pkg.Func selector.
func calleeName(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok {
			return pkg.Name + "." + fun.Sel.Name
		}
	}
	return ""
}
