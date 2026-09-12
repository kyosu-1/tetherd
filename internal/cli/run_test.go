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
	"github.com/kyosu-1/tetherd/internal/transport"
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
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	root := NewRootCommand()
	root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1"})
	root.SetErr(&bytes.Buffer{})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Fatal("run without -- <command> must fail")
	}
}

func TestTaskRoleEnv(t *testing.T) {
	got := taskRoleEnv(nil, "ap-northeast-1", "/tmp/empty")
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
	noRegion := taskRoleEnv(nil, "", "/tmp/empty")
	if len(noRegion) != 2 || noRegion["AWS_CONFIG_FILE"] != "/tmp/empty" {
		t.Fatalf("no-region case = %v", noRegion)
	}
	// The task's own region wins over the CLI-resolved one.
	taskRegion := taskRoleEnv(map[string]string{"AWS_REGION": "us-east-1"}, "ap-northeast-1", "/tmp/empty")
	if taskRegion["AWS_REGION"] != "us-east-1" || taskRegion["AWS_DEFAULT_REGION"] != "us-east-1" {
		t.Fatalf("the task's own region must win: %v", taskRegion)
	}
}

func TestTaskRoleReachable(t *testing.T) {
	withURI := map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}
	vpcOnly := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
	withCreds := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("169.254.170.0/24")}

	if taskRoleReachable(withURI, vpcOnly) {
		t.Error("the credential endpoint is not captured, so the task role is not reachable")
	}
	if !taskRoleReachable(withURI, withCreds) {
		t.Error("the credential endpoint is captured, so the task role is reachable")
	}
	if taskRoleReachable(map[string]string{}, withCreds) {
		t.Error("without the URI there is no task role to use")
	}
	// A wider range that contains the endpoint counts.
	if !taskRoleReachable(withURI, []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}) {
		t.Error("a range containing 169.254.170.0/24 must count as captured")
	}
	// A sub-range that overlaps the /24 but misses 169.254.170.2 does not:
	// the child would still have nowhere to fetch credentials from.
	if taskRoleReachable(withURI, []netip.Prefix{netip.MustParsePrefix("169.254.170.16/28")}) {
		t.Error("a range that does not cover 169.254.170.2 must not count as captured")
	}
}

func TestTaskRoleEnvPrefersTheTaskRegion(t *testing.T) {
	got := taskRoleEnv(map[string]string{"AWS_REGION": "us-east-1"}, "ap-northeast-1", "/tmp/empty")
	if got["AWS_REGION"] != "us-east-1" || got["AWS_DEFAULT_REGION"] != "us-east-1" {
		t.Fatalf("the task's own region must win: %v", got)
	}
	got = taskRoleEnv(nil, "ap-northeast-1", "/tmp/empty")
	if got["AWS_REGION"] != "ap-northeast-1" {
		t.Fatalf("without a task region the CLI's region is used: %v", got)
	}
	got = taskRoleEnv(nil, "", "/tmp/empty")
	if _, ok := got["AWS_REGION"]; ok {
		t.Fatalf("with no region at all nothing is injected: %v", got)
	}
	if got["AWS_CONFIG_FILE"] != "/tmp/empty" {
		t.Fatalf("the shared-config pair is always set: %v", got)
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
		Override:   taskRoleEnv(nil, "ap-northeast-1", "/tmp/empty"),
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

func TestSubtract(t *testing.T) {
	all := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/16"),
		netip.MustParsePrefix("10.0.5.0/24"),
		netip.MustParsePrefix("169.254.170.0/24"),
	}
	got := Subtract(all, []netip.Prefix{netip.MustParsePrefix("10.0.5.0/24")})
	if len(got) != 2 || got[0].String() != "10.0.0.0/16" || got[1].String() != "169.254.170.0/24" {
		t.Fatalf("got %v", got)
	}
	// A local range that contains a remote one removes it entirely.
	got = Subtract(all, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	if len(got) != 1 || got[0].String() != "169.254.170.0/24" {
		t.Fatalf("got %v", got)
	}
	if len(Subtract(all, nil)) != 3 {
		t.Fatal("without exclusions nothing changes")
	}
}

// TestRemoteSet pins the whole set remoteSet assembles: the VPC CIDRs, the
// task-role endpoint, the extra --remote-cidr ranges and any
// network.remote_services prefixes - minus what network.local_cidrs claims
// for the laptop. Deleting any one term from remoteSet's assembly (the VPC
// append, the TaskRoleCIDR append, the extra append, the ServiceCIDRs call,
// or the closing Subtract) changes this set and fails the test.
func TestRemoteSet(t *testing.T) {
	p := &fakeProvider{
		vpc: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		svc: []netip.Prefix{netip.MustParsePrefix("52.219.0.0/20")},
	}
	opts := RunOptions{
		RemoteCIDRs:    []string{"10.9.0.0/16"},
		RemoteServices: []string{"s3"},
		// 10.9.0.0/16 is an extra remote range but also the laptop's own
		// network in this scenario: local_cidrs must remove it again.
		LocalCIDRs: []string{"10.9.0.0/16"},
	}
	got, err := remoteSet(context.Background(), opts, p, transport.Task{SubnetID: "subnet-a"}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"10.0.0.0/16": true, "169.254.170.0/24": true, "52.219.0.0/20": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v", got, want)
	}
	for _, p := range got {
		if !want[p.String()] {
			t.Fatalf("unexpected %s in %v", p, got)
		}
	}
}

// TestRemoteSetRejectsBadLocalCIDRs pins the decision that a local_cidrs
// parse failure is reported with the "network.local_cidrs" prefix (so an
// operator can tell it apart from a bad --remote-cidr) and, since local_cidrs
// can now come from a committed config file rather than only a flag, Run
// maps it to exit 1 rather than the usage exit code 2 (see TestRunFailsOnBadLocalCIDRs).
func TestRemoteSetRejectsBadLocalCIDRs(t *testing.T) {
	p := &fakeProvider{}
	opts := RunOptions{LocalCIDRs: []string{"not-a-cidr"}}
	_, err := remoteSet(context.Background(), opts, p, transport.Task{}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "network.local_cidrs") {
		t.Fatalf("err = %v, want it to mention network.local_cidrs", err)
	}
}
