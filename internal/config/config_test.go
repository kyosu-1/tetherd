package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const shared = `version: 1
aws:
  profile: myapp-dev
  region: ap-northeast-1
target:
  cluster: myapp-dev
  service: api
  container: app
  env: dev
env:
  override:
    PORT: "8080"
  exclude: [NOISY_VAR]
network:
  remote_cidrs: [10.9.0.0/16]
  local_cidrs: [10.0.5.0/24]
  remote_domains: [myapp.internal]
  remote_services: [s3]
incoming:
  local_port: 8080
  match:
    header: X-Dev-User
    token_header: X-Dev-Token
`

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadShared(t *testing.T) {
	dir := t.TempDir()
	sp := write(t, dir, ".tetherd.yml", shared)
	cfg, err := Load(sp, filepath.Join(dir, "missing.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shared.Target.Cluster != "myapp-dev" || cfg.Shared.Target.Service != "api" || cfg.Shared.Target.Env != "dev" {
		t.Errorf("target = %+v", cfg.Shared.Target)
	}
	if cfg.Shared.AWS.Profile != "myapp-dev" || cfg.Shared.AWS.Region != "ap-northeast-1" {
		t.Errorf("aws = %+v", cfg.Shared.AWS)
	}
	if cfg.Shared.Env.Override["PORT"] != "8080" || len(cfg.Shared.Env.Exclude) != 1 {
		t.Errorf("env = %+v", cfg.Shared.Env)
	}
	if len(cfg.Shared.Network.RemoteCIDRs) != 1 || cfg.Shared.Network.RemoteDomains[0] != "myapp.internal" ||
		cfg.Shared.Network.RemoteServices[0] != "s3" || cfg.Shared.Network.LocalCIDRs[0] != "10.0.5.0/24" {
		t.Errorf("network = %+v", cfg.Shared.Network)
	}
	if cfg.SharedPath != sp {
		t.Errorf("SharedPath = %q", cfg.SharedPath)
	}
}

func TestLoadPersonalOverridesTheProfile(t *testing.T) {
	dir := t.TempDir()
	sp := write(t, dir, ".tetherd.yml", shared)
	pp := write(t, dir, "personal.yml", "user: shota\ntoken: abc\naws:\n  profile: myapp-dev-shota\n")
	cfg, err := Load(sp, pp)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Personal.User != "shota" || cfg.Personal.Token != "abc" || cfg.Personal.AWS.Profile != "myapp-dev-shota" {
		t.Fatalf("personal = %+v", cfg.Personal)
	}
}

func TestLoadMissingFilesAreFine(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "none.yml"), filepath.Join(dir, "none2.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SharedPath != "" || cfg.Shared.Target.Cluster != "" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadRejectsUnknownKeysAndWrongVersion(t *testing.T) {
	dir := t.TempDir()
	bad := write(t, dir, "bad.yml", "version: 1\ntarget:\n  clustr: typo\n")
	if _, err := Load(bad, ""); err == nil || !strings.Contains(err.Error(), "clustr") {
		t.Fatalf("a typo must be reported: %v", err)
	}
	old := write(t, dir, "old.yml", "version: 2\n")
	if _, err := Load(old, ""); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("an unsupported version must be reported: %v", err)
	}
}

func TestFindWalksUp(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".tetherd.yml", shared)
	deep := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := Find(deep)
	if !ok || got != filepath.Join(dir, ".tetherd.yml") {
		t.Fatalf("Find = %q, %v", got, ok)
	}
	if _, ok := Find(t.TempDir()); ok {
		t.Error("an unrelated directory must not find a config")
	}
}

func TestEnsurePersonalCreatesOnceWithAToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.yml")
	got, created, err := EnsurePersonal(p, "shota")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if got.User != "shota" || len(got.Token) < 40 {
		t.Fatalf("personal = %+v", got)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("the token file must be 0600, got %v", st.Mode().Perm())
	}
	again, created, err := EnsurePersonal(p, "someone-else")
	if err != nil || created {
		t.Fatalf("a second call must not rewrite: created=%v err=%v", created, err)
	}
	if again.Token != got.Token || again.User != "shota" {
		t.Fatalf("the existing file must win: %+v", again)
	}
}

// TestEnsurePersonalIsRaceSafe pins the TOCTOU fix: every `tetherd run`
// invocation calls EnsurePersonal on a path that may not exist yet, so
// concurrent first runs (e.g. two terminals) must agree on exactly one
// token, and exactly one of them must report created=true.
func TestEnsurePersonalIsRaceSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yml")
	const n = 8
	var wg sync.WaitGroup
	results := make([]Personal, n)
	createdFlags := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], createdFlags[i], errs[i] = EnsurePersonal(path, "shota")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	createdCount := 0
	token := results[0].Token
	if token == "" {
		t.Fatal("the winning token must not be empty")
	}
	for i := 0; i < n; i++ {
		if createdFlags[i] {
			createdCount++
		}
		if results[i].Token != token {
			t.Errorf("goroutine %d got a different token: %q vs %q", i, results[i].Token, token)
		}
		if results[i].User != "shota" {
			t.Errorf("goroutine %d got a different user: %q", i, results[i].User)
		}
	}
	if createdCount != 1 {
		t.Errorf("created=true count = %d, want exactly 1", createdCount)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("the token file must be 0600, got %v", st.Mode().Perm())
	}
}

// TestLoadRejectsVersionBeforeCheckingUnknownKeys pins the ordering the
// review demanded: a newer schema (version: 2 plus a v2-only block) must be
// reported as an unsupported version, not as an unknown-field error from
// decoding it against the v1 struct - the version gate has to fire first.
func TestLoadRejectsVersionBeforeCheckingUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "v2.yml", "version: 2\ndns:\n  enabled: true\n")
	_, err := Load(p, "")
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("the version must be reported even with an unknown block present: %v", err)
	}
	if strings.Contains(err.Error(), "dns") {
		t.Fatalf("the version error must fire before the unknown-key error: %v", err)
	}
}

// TestLoadMissingVersionKeyNamesTheFileAndSuggestsTheFix covers the
// commonest mistake - a .tetherd.yml that simply forgot the `version` line
// (or is empty, or comments-only) - which must not be reported as
// "unsupported version 0".
func TestLoadMissingVersionKeyNamesTheFileAndSuggestsTheFix(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"noversion.yml": "target:\n  cluster: c\n",
		"empty.yml":     "",
		"comments.yml":  "# just a comment\n",
	} {
		p := write(t, dir, name, body)
		_, err := Load(p, "")
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if !strings.Contains(err.Error(), p) {
			t.Errorf("%s: error must name the file: %v", name, err)
		}
		if !strings.Contains(err.Error(), "version: 1") {
			t.Errorf("%s: error must suggest `version: 1`: %v", name, err)
		}
		if strings.Contains(err.Error(), "version 0") {
			t.Errorf("%s: must not say \"unsupported version 0\": %v", name, err)
		}
	}
}

// TestLoadEmptyPersonalFileIsFine keeps "an empty file is an empty config"
// true for the personal file specifically: it carries no version gate.
func TestLoadEmptyPersonalFileIsFine(t *testing.T) {
	dir := t.TempDir()
	sp := write(t, dir, ".tetherd.yml", shared)
	pp := write(t, dir, "personal.yml", "")
	cfg, err := Load(sp, pp)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Personal.User != "" || cfg.Personal.Token != "" {
		t.Fatalf("an empty personal file must decode to the zero value: %+v", cfg.Personal)
	}
}

// TestEnsurePersonalOmitsEmptyAWSBlock is the one nitpick from review:
// EnsurePersonal writes a file a developer hand-edits, so it should not end
// with an empty `aws: {profile: "", region: ""}` block.
func TestEnsurePersonalOmitsEmptyAWSBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yml")
	if _, _, err := EnsurePersonal(p, "shota"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "aws:") {
		t.Errorf("an empty aws block must be omitted: %s", body)
	}
}

// --- fallback for filesystems without hard-link support (review item 1) ---

func forceLinkUnsupported(t *testing.T) {
	t.Helper()
	orig := linkFile
	linkFile = func(string, string) error {
		return &os.LinkError{Op: "link", Err: syscall.ENOTSUP}
	}
	t.Cleanup(func() { linkFile = orig })
}

// TestEnsurePersonalWorksWhenLinkIsUnsupported proves EnsurePersonal still
// works end to end on a filesystem that rejects hard links (SMB, exFAT,
// some container bind mounts) - the scenario the Link-only version
// regressed, where a developer's very first run could never succeed. The
// error is injected via the linkFile seam rather than an actual such
// filesystem.
func TestEnsurePersonalWorksWhenLinkIsUnsupported(t *testing.T) {
	forceLinkUnsupported(t)

	path := filepath.Join(t.TempDir(), "sub", "config.yml")
	got, created, err := EnsurePersonal(path, "shota")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if got.User != "shota" || len(got.Token) < 40 {
		t.Fatalf("personal = %+v", got)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("the token file must be 0600, got %v", st.Mode().Perm())
	}

	again, created, err := EnsurePersonal(path, "someone-else")
	if err != nil || created {
		t.Fatalf("a second call must not rewrite: created=%v err=%v", created, err)
	}
	if again.Token != got.Token || again.User != "shota" {
		t.Fatalf("the existing file must win: %+v", again)
	}
}

func TestPublishUsesFallbackWhenLinkIsUnsupported(t *testing.T) {
	forceLinkUnsupported(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	won, retry, err := publish(dir, path, []byte("user: a\ntoken: t\n"))
	if err != nil || !won || retry {
		t.Fatalf("won=%v retry=%v err=%v", won, retry, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "user: a\ntoken: t\n" {
		t.Fatalf("content = %q", body)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestPublishFallbackReportsRetryOnLossWhenPathAlreadyExists(t *testing.T) {
	forceLinkUnsupported(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	won, retry, err := publish(dir, path, []byte("new"))
	if err != nil || won || !retry {
		t.Fatalf("won=%v retry=%v err=%v, want won=false retry=true err=nil", won, retry, err)
	}
}

// TestReadPublishedRetriesUntilTheWinnerFinishesWriting pins the retry the
// non-atomic fallback needs: a loser that arrives while the winner's write
// is still in flight (here, an empty file that gains its content a moment
// later) must not report failure on the first look.
func TestReadPublishedRetriesUntilTheWinnerFinishesWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		body, _ := yaml.Marshal(Personal{User: "winner", Token: "winner-token"})
		_ = os.WriteFile(path, body, 0o600)
	}()

	got, err := readPublished(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "winner-token" || got.User != "winner" {
		t.Fatalf("got %+v", got)
	}
}

// TestReadPublishedDoesNotRetryOverTheLinkPath pins the other half: over the
// atomic Link path a loss is only ever reported once the winner's Link has
// already succeeded, so a single read is enough and retry=false skips the
// backoff entirely.
func TestReadPublishedDoesNotRetryOverTheLinkPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPublished(path, false); err == nil {
		t.Fatal("without retry, a token-less file must fail on the first read")
	}
}

func TestReadPublishedFailsClearlyWhenTheTokenNeverAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readPublished(path, true)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("must fail clearly when the token never appears: %v", err)
	}
}

// TestEnsurePersonalIsRaceSafeOverTheFallbackPath is TestEnsurePersonalIsRaceSafe's
// counterpart for the non-atomic fallback: run in rounds against fresh
// paths, since the fallback's race window is narrower and more timing-
// dependent than the Link path's.
func TestEnsurePersonalIsRaceSafeOverTheFallbackPath(t *testing.T) {
	forceLinkUnsupported(t)

	const rounds, n = 20, 16
	for round := 0; round < rounds; round++ {
		path := filepath.Join(t.TempDir(), "sub", "config.yml")
		var wg sync.WaitGroup
		results := make([]Personal, n)
		createdFlags := make([]bool, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], createdFlags[i], errs[i] = EnsurePersonal(path, "shota")
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d goroutine %d: %v", round, i, err)
			}
		}
		createdCount := 0
		token := results[0].Token
		if token == "" {
			t.Fatalf("round %d: the winning token must not be empty", round)
		}
		for i := 0; i < n; i++ {
			if createdFlags[i] {
				createdCount++
			}
			if results[i].Token != token {
				t.Errorf("round %d goroutine %d got a different token: %q vs %q", round, i, results[i].Token, token)
			}
			if results[i].User != "shota" {
				t.Errorf("round %d goroutine %d got a different user: %q", round, i, results[i].User)
			}
		}
		if createdCount != 1 {
			t.Errorf("round %d: created=true count = %d, want exactly 1", round, createdCount)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("round %d: the token file must be 0600, got %v", round, st.Mode().Perm())
		}
	}
}

// --- backfilling a missing token (review item 3) ---

// TestEnsurePersonalBackfillsAMissingToken covers a hand-created (or
// otherwise token-less) personal file: v0.3's X-Dev-Token match needs a
// token on every personal file, so one missing must be minted and written
// back, keeping every other field.
func TestEnsurePersonalBackfillsAMissingToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("user: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, created, err := EnsurePersonal(path, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("filling in a missing token is not creating the file")
	}
	if got.User != "x" || got.Token == "" {
		t.Fatalf("personal = %+v", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "user: x") || !strings.Contains(string(body), got.Token) {
		t.Fatalf("the file must keep user and gain the token: %s", body)
	}
}

// TestEnsurePersonalBackfillIsRaceSafe covers the other half of the token
// race: the file already exists without a token (hand-created, or written
// by an older tetherd), and several runs start at once. Every caller must
// come back with the one token that is on disk - a caller holding a token
// the file does not have would fail the agent's X-Dev-Token match in v0.3
// while looking perfectly healthy locally.
func TestEnsurePersonalBackfillIsRaceSafe(t *testing.T) {
	const callers = 16
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("user: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	tokens := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got, _, err := EnsurePersonal(path, "ignored")
			tokens[i], errs[i] = got.Token, err
		}(i)
	}
	close(start)
	wg.Wait()

	onDisk := Personal{}
	if err := decodeFile(path, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Token == "" || onDisk.User != "x" {
		t.Fatalf("the file must keep user and gain exactly one token: %+v", onDisk)
	}
	for i := range tokens {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if tokens[i] != onDisk.Token {
			t.Fatalf("caller %d got a token that is not on disk: %q vs %q", i, tokens[i], onDisk.Token)
		}
	}
}

// TestEnsurePersonalWithATokenIsByteIdentical is "never overwrite" for the
// case that already has a token: it must not be touched at all.
func TestEnsurePersonalWithATokenIsByteIdentical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	body := []byte("user: x\ntoken: abc\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsurePersonal(path, "ignored"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(body) {
		t.Fatalf("a file with a token must be untouched: got %q want %q", after, body)
	}
}
