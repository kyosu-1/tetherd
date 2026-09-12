package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
