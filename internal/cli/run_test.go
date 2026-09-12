package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/kyosu-1/tetherd/internal/env"
	"github.com/kyosu-1/tetherd/internal/proto"
)

func TestParseRemoteCIDRs(t *testing.T) {
	got, err := ParseRemoteCIDRs([]string{"10.0.0.0/16", "169.254.170.0/24"})
	if err != nil || len(got) != 2 || got[1].String() != "169.254.170.0/24" {
		t.Fatalf("got %v, err %v", got, err)
	}
	if _, err := ParseRemoteCIDRs([]string{"10.0.0.0"}); err == nil {
		t.Fatal("bare address must fail")
	}
	if _, err := ParseRemoteCIDRs([]string{"fd00::/8"}); err == nil {
		t.Fatal("ipv6 must fail in v1")
	}
}

func TestRunCommandDirectFlags(t *testing.T) {
	var captured RunOptions
	runFn = func(opts RunOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { runFn = defaultRun })

	root := NewRootCommand()
	root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "127.0.0.1:9900",
		"--remote-cidr", "172.20.0.0/16", "--remote-cidr", "10.1.0.0/16", "--user", "shota",
		"--", "curl", "-s", "http://172.20.0.11:8081/"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.AgentAddr != "127.0.0.1:9900" || len(captured.RemoteCIDRs) != 2 || captured.User != "shota" {
		t.Fatalf("opts = %+v", captured)
	}
	if len(captured.Command) != 3 || captured.Command[0] != "curl" {
		t.Fatalf("command = %v", captured.Command)
	}
}

func TestRunCommandParsesFlagsAndCommand(t *testing.T) {
	var captured RunOptions
	runFn = func(opts RunOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { runFn = defaultRun })

	root := NewRootCommand()
	root.SetArgs([]string{"run", "--profile", "personal", "--region", "ap-northeast-1", "--cluster", "tetherd-dev", "--service", "api",
		"--env", "dev", "--remote-cidr", "10.1.0.0/16", "--user", "shota", "--no-env",
		"--", "psql", "-h", "db"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Transport != "ssm" || captured.Profile != "personal" || captured.Cluster != "tetherd-dev" || captured.Service != "api" || captured.TargetEnv != "dev" || !captured.NoEnv {
		t.Fatalf("opts = %+v", captured)
	}
	if len(captured.RemoteCIDRs) != 1 || captured.RemoteCIDRs[0] != "10.1.0.0/16" || len(captured.Command) != 3 || captured.Command[0] != "psql" {
		t.Fatalf("opts = %+v", captured)
	}
}

func TestLocalOverlaps(t *testing.T) {
	cidrs := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("169.254.170.0/24")}
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("10.0.5.7"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("192.168.1.20"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
	}
	got := LocalOverlaps(cidrs, addrs)
	if len(got) != 1 || !strings.Contains(got[0], "10.0.0.0/16") || !strings.Contains(got[0], "10.0.5.7") {
		t.Fatalf("got %v", got)
	}
}

func TestRunSSMRequiresClusterAndService(t *testing.T) {
	code, err := Run(context.Background(), RunOptions{Transport: "ssm", Command: []string{"true"}}, io.Discard)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--cluster") {
		t.Fatalf("code %d err %v", code, err)
	}
}

func TestExitCodeMapping(t *testing.T) {
	t.Run("child exit code", func(t *testing.T) {
		runFn = func(opts RunOptions) (int, error) { return 7, nil }
		t.Cleanup(func() { runFn = defaultRun })

		root := NewRootCommand()
		root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1", "--", "true"})
		root.SetErr(&bytes.Buffer{})
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := ExitCode(err); got != 7 {
			t.Fatalf("ExitCode = %d, want 7", got)
		}
		if !IsChildExit(err) {
			t.Fatal("IsChildExit = false, want true")
		}
	})

	t.Run("run error with explicit usage code", func(t *testing.T) {
		runFn = func(opts RunOptions) (int, error) { return 2, errors.New("bad flag") }
		t.Cleanup(func() { runFn = defaultRun })

		root := NewRootCommand()
		root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1", "--", "true"})
		root.SetErr(&bytes.Buffer{})
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := ExitCode(err); got != 2 {
			t.Fatalf("ExitCode = %d, want 2", got)
		}
		if IsChildExit(err) {
			t.Fatal("IsChildExit = true, want false")
		}
		if !strings.Contains(err.Error(), "bad flag") {
			t.Fatalf("err = %v, want it to contain %q", err, "bad flag")
		}
	})

	t.Run("run error with code 1", func(t *testing.T) {
		runFn = func(opts RunOptions) (int, error) { return 1, errors.New("connect failed") }
		t.Cleanup(func() { runFn = defaultRun })

		root := NewRootCommand()
		root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1", "--", "true"})
		root.SetErr(&bytes.Buffer{})
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := ExitCode(err); got != 1 {
			t.Fatalf("ExitCode = %d, want 1", got)
		}
		if IsChildExit(err) {
			t.Fatal("IsChildExit = true, want false")
		}
	})

	t.Run("cobra usage error", func(t *testing.T) {
		root := NewRootCommand()
		root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1"})
		root.SetErr(&bytes.Buffer{})
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := ExitCode(err); got != 2 {
			t.Fatalf("ExitCode = %d, want 2", got)
		}
		if IsChildExit(err) {
			t.Fatal("IsChildExit = true, want false")
		}
	})
}

func TestResolveTaskEnv(t *testing.T) {
	t.Run("no-env", func(t *testing.T) {
		env, status, err := resolveTaskEnv(proto.Welcome{AppEnv: map[string]string{"X": "1"}}, RunOptions{NoEnv: true})
		if err != nil || env != nil || status != "env      skipped (--no-env)" {
			t.Fatalf("env=%v status=%q err=%v", env, status, err)
		}
	})
	t.Run("env error over direct is tolerated", func(t *testing.T) {
		env, status, err := resolveTaskEnv(proto.Welcome{EnvError: "boom"}, RunOptions{Transport: "direct"})
		if err != nil || env != nil || !strings.Contains(status, "boom") {
			t.Fatalf("env=%v status=%q err=%v", env, status, err)
		}
	})
	t.Run("env error over ssm is fatal", func(t *testing.T) {
		env, _, err := resolveTaskEnv(proto.Welcome{EnvError: "boom"}, RunOptions{Transport: "ssm"})
		if env != nil || err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "--no-env") {
			t.Fatalf("env=%v err=%v", env, err)
		}
	})
	t.Run("app env", func(t *testing.T) {
		env, status, err := resolveTaskEnv(proto.Welcome{AppEnv: map[string]string{"A": "1", "B": "2"}}, RunOptions{Transport: "ssm"})
		if err != nil || len(env) != 2 || !strings.Contains(status, "2 vars") {
			t.Fatalf("env=%v status=%q err=%v", env, status, err)
		}
	})
}

func TestCheckTargetEnv(t *testing.T) {
	t.Run("ssm mismatch", func(t *testing.T) {
		err := checkTargetEnv(proto.Welcome{Env: "dev"}, RunOptions{Transport: "ssm", TargetEnv: "prod"})
		if err == nil || !strings.Contains(err.Error(), "refusing to attach") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("ssm match", func(t *testing.T) {
		if err := checkTargetEnv(proto.Welcome{Env: "dev"}, RunOptions{Transport: "ssm", TargetEnv: "dev"}); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("direct mismatch is not checked", func(t *testing.T) {
		if err := checkTargetEnv(proto.Welcome{Env: "dev"}, RunOptions{Transport: "direct", TargetEnv: "prod"}); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRunRequiresCommand(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1"})
	root.SetErr(&bytes.Buffer{})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Fatal("run without -- <command> must fail")
	}
}

func TestTaskRoleEnv(t *testing.T) {
	got := taskRoleEnv("ap-northeast-1", "/tmp/empty")
	want := map[string]string{
		"AWS_CONFIG_FILE":             "/tmp/empty",
		"AWS_SHARED_CREDENTIALS_FILE": "/tmp/empty",
		"AWS_REGION":                  "ap-northeast-1",
		"AWS_DEFAULT_REGION":          "ap-northeast-1",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// Without a region only the shared-config pair is set.
	noRegion := taskRoleEnv("", "/tmp/empty")
	if len(noRegion) != 2 || noRegion["AWS_CONFIG_FILE"] != "/tmp/empty" {
		t.Fatalf("no-region case = %v", noRegion)
	}
}

func TestEmptyAWSConfigFile(t *testing.T) {
	path, cleanup, err := emptyAWSConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file must be readable: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("the file must be empty, got %d bytes", len(b))
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the file, stat err = %v", err)
	}
}

// The shared-config override has to beat the developer's own value, or the
// task role stays shadowed (observed on a real machine: a `default` profile
// with a credential source wins over the container credentials).
func TestTaskRoleEnvOverridesLocalSharedConfig(t *testing.T) {
	local := []string{"AWS_CONFIG_FILE=/Users/dev/.aws/config", "PORT=3000"}
	task := map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}
	out := env.Merge(local, task, env.Options{
		StripLocal: env.LocalAWSCredentialVars,
		Override:   taskRoleEnv("ap-northeast-1", "/tmp/empty"),
	})
	got := map[string]string{}
	for _, kv := range out {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["AWS_CONFIG_FILE"] != "/tmp/empty" {
		t.Errorf("AWS_CONFIG_FILE = %q, want the empty file", got["AWS_CONFIG_FILE"])
	}
	if got["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] != "/v2/credentials/x" {
		t.Errorf("the task's credential URI must survive: %q", got["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"])
	}
	if got["PORT"] != "3000" {
		t.Errorf("unrelated local vars must survive: PORT=%q", got["PORT"])
	}
}
