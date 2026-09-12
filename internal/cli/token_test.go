package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kyosu-1/tetherd/internal/config"
)

// rotate runs `tetherd token rotate` through the real command tree, with
// HOME pointing at a directory of the test's own, and returns what it wrote
// to stdout. Nothing here touches the developer's real ~/.tetherd/config.yml,
// and nothing needs AWS credentials, the network or tetherd-helper: rotating
// a token is a local file operation and this is where that is pinned.
func rotate(t *testing.T, home string) string {
	t.Helper()
	t.Setenv("HOME", home)
	var out, errOut bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"token", "rotate"})
	if err := root.Execute(); err != nil {
		t.Fatalf("token rotate: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
	}
	// A token on two streams is a token in two places; the report is one
	// piece of output, on the stream the developer is reading it from.
	if errOut.Len() != 0 {
		t.Errorf("token rotate wrote to stderr: %s", errOut.String())
	}
	return out.String()
}

// personalPath is where rotate() writes, given the same home.
func personalPath(home string) string {
	return filepath.Join(home, ".tetherd", "config.yml")
}

// readPersonal parses the personal file with the same decoder the rest of
// tetherd uses. Parsing, not grepping: `user: shota` and a token that
// happens to contain "shota" are the same string to strings.Contains, so a
// presence check cannot tell a preserved field from a coincidence, nor from
// two fields written under each other's keys.
func readPersonal(t *testing.T, home string) config.Personal {
	t.Helper()
	cfg, err := config.Load("", personalPath(home))
	if err != nil {
		t.Fatalf("the rotated file must still parse: %v", err)
	}
	return cfg.Personal
}

// printedToken is the token `token rotate` reported, read back off its
// output. The developer pastes this value into their browser extension, so
// "the printed token is the token on disk" is a property worth its own
// assertion rather than an assumption.
func printedToken(t *testing.T, out string) string {
	t.Helper()
	const key = "new token: "
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, key); i >= 0 {
			return strings.TrimSpace(line[i+len(key):])
		}
	}
	t.Fatalf("token rotate printed no new token:\n%s", out)
	return ""
}

func TestTokenRotateReplacesTheTokenAndKeepsEverythingElse(t *testing.T) {
	home := t.TempDir()
	// The personal file also carries `user` and an aws profile. Rotating
	// the token must not drop them - losing `user` would silently change
	// which requests the agent matches, and losing the aws block would
	// change which account the next run talks to.
	writePersonal(t, home, "user: shota\ntoken: the-old-token\naws:\n  profile: personal-profile\n  region: ap-northeast-1\n")

	out := rotate(t, home)
	got := readPersonal(t, home)

	if got.Token == "the-old-token" {
		t.Fatal("the token on disk is still the old one: nothing was rotated")
	}
	if got.Token == "" {
		t.Fatal("the file must come out with a token; an empty one makes every steal impossible")
	}
	if got.User != "shota" {
		t.Errorf("user = %q, want %q: the agent matches requests against this name", got.User, "shota")
	}
	if got.AWS.Profile != "personal-profile" {
		t.Errorf("aws.profile = %q, want %q", got.AWS.Profile, "personal-profile")
	}
	if got.AWS.Region != "ap-northeast-1" {
		t.Errorf("aws.region = %q, want %q", got.AWS.Region, "ap-northeast-1")
	}
	// Printing one token while writing another would send the developer off
	// to paste a value the agent will never match.
	if tok := printedToken(t, out); tok != got.Token {
		t.Errorf("printed token %q, but the file holds %q", tok, got.Token)
	}
	// And the replaced token is not reprinted: the point of rotating is
	// that the old value stops being useful, not that it gets a second
	// airing in a terminal or a CI log.
	if strings.Contains(out, "the-old-token") {
		t.Errorf("the output must not repeat the token it replaced:\n%s", out)
	}
}

func TestTokenRotateWritesZeroSixHundred(t *testing.T) {
	// The token is the only thing between a public ALB and this laptop.
	t.Run("a file someone created by hand", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, ".tetherd")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		// 0644 is what a hand-created file gets from a text editor, and it
		// is the case that matters: the command whose reason for existing
		// is "this token may be known to someone else" must not leave the
		// replacement readable by every process on the machine.
		if err := os.WriteFile(personalPath(home), []byte("user: shota\ntoken: old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Without this the assertion below could pass on a machine whose
		// umask had already made the file 0600, proving nothing.
		if st, err := os.Stat(personalPath(home)); err != nil {
			t.Fatal(err)
		} else if st.Mode().Perm() != 0o644 {
			t.Skipf("this machine's umask wrote the fixture as %v, not 0644", st.Mode().Perm())
		}

		rotate(t, home)

		st, err := os.Stat(personalPath(home))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("the token file must be 0600, got %v", st.Mode().Perm())
		}
	})

	t.Run("a machine that has never run tetherd", func(t *testing.T) {
		home := t.TempDir()
		rotate(t, home)
		st, err := os.Stat(personalPath(home))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("the token file must be 0600, got %v", st.Mode().Perm())
		}
	})
}

func TestTokenRotateProducesADifferentTokenEveryTime(t *testing.T) {
	home := t.TempDir()
	writePersonal(t, home, "user: shota\ntoken: the-old-token\n")

	seen := map[string]bool{"the-old-token": true}
	for i := 0; i < 5; i++ {
		out := rotate(t, home)
		tok := readPersonal(t, home).Token
		if tok != printedToken(t, out) {
			t.Fatalf("round %d: printed %q, on disk %q", i, printedToken(t, out), tok)
		}
		if seen[tok] {
			t.Fatalf("round %d: token %q has been issued before; rotating must not hand back a value that may already have leaked", i, tok)
		}
		seen[tok] = true
		// Distinctness alone would be satisfied by a counter. What the
		// token has to be is unguessable, so check it really is 32 random
		// bytes (base64url, as internal/config mints it).
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("round %d: token %q is not base64url: %v", i, tok, err)
		}
		if len(raw) < 32 {
			t.Fatalf("round %d: token carries %d bytes of randomness, want at least 32", i, len(raw))
		}
	}
}

func TestTokenRotateSaysRunningSessionsKeepTheOldToken(t *testing.T) {
	// A developer who rotates because they think the token leaked needs to
	// know the leak is not closed until every running session is restarted:
	// the agent matches each session against the token that session sent
	// when it attached, so the file changing under it changes nothing.
	home := t.TempDir()
	writePersonal(t, home, "user: shota\ntoken: the-old-token\n")

	out := rotate(t, home)

	// Literal wording, not the constant the code prints: asserting against
	// rotatingSessionsWarning would move with any edit to it, including one
	// that deleted the sentence's meaning.
	if !strings.Contains(out, "sessions already running keep the old token") {
		t.Errorf("the output must say that running sessions keep the old token:\n%s", out)
	}
	// And what to do about it. "Your token is stale somewhere" without
	// "restart them" is a puzzle, not a warning.
	if !strings.Contains(strings.ToLower(out), "restart every") {
		t.Errorf("the output must say to restart the running sessions:\n%s", out)
	}
}

// TestTokenRotateCreatesThePersonalFileOnAFreshMachine: rotate is also a way
// to get a first token (a machine where `tetherd run` has never run has no
// file at all), so it creates rather than reporting the file missing - and
// the file it creates names this developer, because the agent matches on
// that name.
func TestTokenRotateCreatesThePersonalFileOnAFreshMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USER", "fresh-dev")

	out := rotate(t, home)

	got := readPersonal(t, home)
	if got.Token == "" {
		t.Fatal("a fresh machine must come out with a token")
	}
	if got.User != "fresh-dev" {
		t.Errorf("user = %q, want $USER", got.User)
	}
	if tok := printedToken(t, out); tok != got.Token {
		t.Errorf("printed token %q, but the file holds %q", tok, got.Token)
	}
}

// TestRotateTokenIsSerialisedBetweenConcurrentRotations pins what the flock
// in config.RotateToken buys: every caller's token is distinct, the file is
// never left torn or truncated, `user` survives all of them, and the token
// left on disk is one a caller was actually told about. Without the lock the
// last writer's content and the returned values can disagree, which is a
// developer pasting a token no file holds.
func TestRotateTokenIsSerialisedBetweenConcurrentRotations(t *testing.T) {
	home := t.TempDir()
	writePersonal(t, home, "user: shota\ntoken: the-old-token\n")
	path := personalPath(home)

	const n = 8
	tokens := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// No t.Fatal in here: it calls Goexit, which would kill this
			// goroutine and leave the test passing. Errors are collected
			// and reported on the test's own goroutine below.
			p, err := config.RotateToken(path, "ignored")
			tokens[i], errs[i] = p.Token, err
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("rotation %d: %v", i, err)
		}
		if tokens[i] == "" || tokens[i] == "the-old-token" {
			t.Fatalf("rotation %d returned %q", i, tokens[i])
		}
		if seen[tokens[i]] {
			t.Fatalf("rotation %d returned a token another rotation also returned: %q", i, tokens[i])
		}
		seen[tokens[i]] = true
	}
	got := readPersonal(t, home)
	if got.User != "shota" {
		t.Errorf("user = %q, want shota: a concurrent rotation must not drop it", got.User)
	}
	if !seen[got.Token] {
		t.Errorf("the file holds %q, which no rotation returned", got.Token)
	}
}
