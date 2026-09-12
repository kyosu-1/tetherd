package cli

import (
	"os"
	"path/filepath"
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
