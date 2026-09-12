package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyConfigFillsUnsetFlagsOnly(t *testing.T) {
	dir := t.TempDir()
	body := "version: 1\naws:\n  profile: from-file\n  region: ap-northeast-1\ntarget:\n  cluster: file-cluster\n  service: file-api\n  env: dev\nnetwork:\n  remote_cidrs: [10.9.0.0/16]\n  local_cidrs: [10.0.5.0/24]\n  remote_domains: [myapp.internal]\n  remote_services: [s3]\nenv:\n  override:\n    PORT: \"8080\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".tetherd.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir) // no personal file there

	var captured RunOptions
	runFn = func(opts RunOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { runFn = defaultRun })

	root := NewRootCommand()
	root.SetArgs([]string{"run", "--config", filepath.Join(dir, ".tetherd.yml"), "--cluster", "flag-cluster", "--", "true"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Cluster != "flag-cluster" {
		t.Errorf("the flag must win: %q", captured.Cluster)
	}
	if captured.Service != "file-api" || captured.Profile != "from-file" || captured.Region != "ap-northeast-1" || captured.TargetEnv != "dev" {
		t.Errorf("unset flags must come from the file: %+v", captured)
	}
	if len(captured.RemoteCIDRs) != 1 || captured.RemoteCIDRs[0] != "10.9.0.0/16" ||
		len(captured.LocalCIDRs) != 1 || len(captured.RemoteDomains) != 1 || len(captured.RemoteServices) != 1 ||
		captured.EnvOverride["PORT"] != "8080" {
		t.Errorf("network/env blocks must be applied: %+v", captured)
	}
}

// TestApplyConfigCarriesPinCredentialRoute: the machine-wide route pin has
// no flag on purpose (it is rarely wanted and affects every process on the
// Mac), so .tetherd.yml is the only way to ask for it - and it has to be
// off when the file does not mention it.
func TestApplyConfigCarriesPinCredentialRoute(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want bool
	}{
		{"asked for", "version: 1\nnetwork:\n  pin_credential_route: true\n", true},
		{"not mentioned", "version: 1\nnetwork:\n  remote_cidrs: [10.9.0.0/16]\n", false},
		{"explicitly off", "version: 1\nnetwork:\n  pin_credential_route: false\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".tetherd.yml")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", dir)

			var captured RunOptions
			runFn = func(opts RunOptions) (int, error) { captured = opts; return 0, nil }
			t.Cleanup(func() { runFn = defaultRun })

			root := NewRootCommand()
			root.SetArgs([]string{"run", "--config", path, "--", "true"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if captured.PinCredentialRoute != c.want {
				t.Fatalf("PinCredentialRoute = %v, want %v", captured.PinCredentialRoute, c.want)
			}
		})
	}
}

// TestApplyConfigExplicitMissingConfigErrors pins item 1: --config naming a
// file that does not exist must fail loudly (the run would otherwise fall
// through to "defaults" and die complaining about --cluster instead of
// about the typo'd path).
func TestApplyConfigExplicitMissingConfigErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	runFn = func(opts RunOptions) (int, error) { return 0, nil }
	t.Cleanup(func() { runFn = defaultRun })

	root := NewRootCommand()
	root.SetArgs([]string{"run", "--config", filepath.Join(dir, "typo.yml"), "--cluster", "c", "--service", "s", "--", "true"})
	err := root.Execute()
	if err == nil {
		t.Fatal("an explicitly named missing --config must error")
	}
	if !strings.Contains(err.Error(), "typo.yml") {
		t.Errorf("the error must name the missing path: %v", err)
	}
}

// TestApplyConfigDiscoveredMissingConfigIsFine is the other half of item 1:
// a config that is merely *looked for* (no --config given) and not found is
// not an error - only a path the operator typed themselves must be.
func TestApplyConfigDiscoveredMissingConfigIsFine(t *testing.T) {
	dir := t.TempDir() // empty: no .tetherd.yml here or above it, ideally
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Chdir(dir)

	configPath = ""
	cmd := newRunCommand()
	var opts RunOptions
	if err := applyConfig(cmd, &opts); err != nil {
		t.Fatalf("a missing discovered config must not error: %v", err)
	}
}

// TestApplyConfigUnionsRemoteCIDRsWithFlags pins item 4: --remote-cidr is
// documented as "additional" and repeatable, so a config-committed range and
// a flag-supplied one must both reach RunOptions, not just the flag's.
func TestApplyConfigUnionsRemoteCIDRsWithFlags(t *testing.T) {
	dir := t.TempDir()
	body := "version: 1\nnetwork:\n  remote_cidrs: [10.9.0.0/16, 10.1.0.0/16]\n"
	if err := os.WriteFile(filepath.Join(dir, ".tetherd.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)

	var captured RunOptions
	runFn = func(opts RunOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { runFn = defaultRun })

	root := NewRootCommand()
	root.SetArgs([]string{"run", "--config", filepath.Join(dir, ".tetherd.yml"),
		"--remote-cidr", "10.1.0.0/16", "--remote-cidr", "172.20.0.0/16",
		"--transport", "direct", "--agent-addr", "x:1", "--", "true"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"10.9.0.0/16": true, "10.1.0.0/16": true, "172.20.0.0/16": true}
	if len(captured.RemoteCIDRs) != len(want) {
		t.Fatalf("RemoteCIDRs = %v, want the union of config and flags (deduplicated): %v", captured.RemoteCIDRs, want)
	}
	for _, c := range captured.RemoteCIDRs {
		if !want[c] {
			t.Errorf("unexpected CIDR %q in %v", c, captured.RemoteCIDRs)
		}
	}
	if captured.RemoteCIDRs[0] != "10.9.0.0/16" {
		t.Errorf("config values must come first: %v", captured.RemoteCIDRs)
	}
}

// TestApplyConfigUserRespectsExplicitEmptyFlag pins item 6: Changed("user"),
// not a zero-value check, decides whether the flag was set - `--user ""` is
// an explicit (if odd) choice and must not fall through to the personal
// file or $USER.
func TestApplyConfigUserRespectsExplicitEmptyFlag(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".tetherd"), 0o700); err != nil {
		t.Fatal(err)
	}
	personalBody := "user: shota\ntoken: tok\n"
	if err := os.WriteFile(filepath.Join(home, ".tetherd", "config.yml"), []byte(personalBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USER", "env-user")
	t.Chdir(dir) // no .tetherd.yml here or above it: nothing to discover

	var captured RunOptions
	runFn = func(opts RunOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { runFn = defaultRun })

	root := NewRootCommand()
	root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1", "--user", "", "--", "true"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.User != "" {
		t.Errorf("an explicit --user \"\" must win over the personal file and $USER: %q", captured.User)
	}
}

// TestApplyConfigPersonalOverridesShared closes the mutation gap in item 8a:
// a mutant that reverses precedence (shared beating personal) or that drops
// the `opts.User = cfg.Personal.User` fallback must fail this test.
func TestApplyConfigPersonalOverridesShared(t *testing.T) {
	dir := t.TempDir()
	sharedBody := "version: 1\naws:\n  profile: shared-profile\n  region: ap-northeast-1\n"
	if err := os.WriteFile(filepath.Join(dir, ".tetherd.yml"), []byte(sharedBody), 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".tetherd"), 0o700); err != nil {
		t.Fatal(err)
	}
	personalBody := "user: shota\ntoken: tok\naws:\n  profile: personal-profile\n  region: ap-southeast-2\n"
	if err := os.WriteFile(filepath.Join(home, ".tetherd", "config.yml"), []byte(personalBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// Pinned to something other than "shota" (review item 5): without this,
	// the User assertion below only kills a mutant that deletes the
	// `opts.User = cfg.Personal.User` fallback by accident, on whatever
	// $USER this machine happens to have - on a machine or CI runner where
	// $USER is already "shota" the mutant would survive undetected.
	t.Setenv("USER", "env-user")

	var captured RunOptions
	runFn = func(opts RunOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { runFn = defaultRun })

	root := NewRootCommand()
	root.SetArgs([]string{"run", "--config", filepath.Join(dir, ".tetherd.yml"), "--", "true"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Profile != "personal-profile" || captured.Region != "ap-southeast-2" {
		t.Errorf("the personal file must win over the shared file: %+v", captured)
	}
	if captured.User != "shota" {
		t.Errorf("user must come from the personal file: %q", captured.User)
	}
}

// TestApplyConfigSeedsThePersonalFileWithTheResolvedUser pins item 2: a
// first-ever run must seed ~/.tetherd/config.yml with the user this
// invocation actually resolved to (an explicit --user, here), not
// unconditionally $USER - otherwise `tetherd run --user alice` on a machine
// where $USER=bob would permanently record `user: bob` for every later run.
func TestApplyConfigSeedsThePersonalFileWithTheResolvedUser(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home") // no .tetherd/config.yml yet: first run
	t.Setenv("HOME", home)
	t.Setenv("USER", "bob")
	t.Chdir(dir) // no .tetherd.yml here or above it

	runFn = func(opts RunOptions) (int, error) { return 0, nil }
	t.Cleanup(func() { runFn = defaultRun })

	root := NewRootCommand()
	root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1", "--user", "alice", "--", "true"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(home, ".tetherd", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "user: alice") {
		t.Fatalf("a first run with --user alice must seed the personal file with alice, not $USER=bob: %s", body)
	}
	if strings.Contains(string(body), "bob") {
		t.Fatalf("the personal file must not mention $USER=bob at all: %s", body)
	}
}

// TestChangedPanicsOnAnUnregisteredFlag pins item 4: a guard on a
// misspelled or not-yet-registered flag name must be caught immediately
// (pflag itself would just silently answer false).
func TestChangedPanicsOnAnUnregisteredFlag(t *testing.T) {
	cmd := newRunCommand()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("changed must panic on an unregistered flag name")
		}
	}()
	changed(cmd, "definitely-not-a-flag")
}

// TestApplyConfigRunsUnderTheEnvCommand pins that `tetherd env` shares the
// same discovery/target flags as `run` (addTargetFlags), so applyConfig's
// changed() guards never panic when run against the env command - a
// regression here would only ever surface at runtime, as a panic on the
// first real invocation of `tetherd env`, since no test previously exercised
// applyConfig with any command other than newRunCommand().
func TestApplyConfigRunsUnderTheEnvCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Chdir(dir) // no .tetherd.yml here or above it: nothing to discover

	cmd := newEnvCommand()
	configPath = ""
	var opts EnvOptions
	if err := applyConfig(cmd, &opts.RunOptions); err != nil {
		t.Fatalf("applyConfig must not error under the env command: %v", err)
	}
}

// TestChangedRecognizesEveryRoutedFlag proves changed() resolves every flag
// name applyConfig cares about (or might one day) without panicking - a
// future rename of any of these would be caught by this test failing to
// compile-equivalent-panic, rather than the config silently winning over an
// unrecognized flag.
func TestChangedRecognizesEveryRoutedFlag(t *testing.T) {
	cmd := newRunCommand()
	for _, name := range []string{
		"profile", "region", "cluster", "service", "task", "env",
		"user", "transport", "agent-addr", "remote-cidr",
	} {
		if got := changed(cmd, name); got {
			t.Errorf("%s: Changed() must be false before any flag is parsed", name)
		}
	}
}
