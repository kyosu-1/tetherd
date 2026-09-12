package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kyosu-1/tetherd/internal/env"
	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/transport"
)

func TestParseRemoteCIDRs(t *testing.T) {
	got, err := ParseRemoteCIDRs("--remote-cidr", []string{"10.0.0.0/16", "169.254.170.0/24"})
	if err != nil || len(got) != 2 || got[1].String() != "169.254.170.0/24" {
		t.Fatalf("got %v, err %v", got, err)
	}
	if _, err := ParseRemoteCIDRs("--remote-cidr", []string{"10.0.0.0"}); err == nil {
		t.Fatal("bare address must fail")
	}
	if _, err := ParseRemoteCIDRs("--remote-cidr", []string{"fd00::/8"}); err == nil {
		t.Fatal("ipv6 must fail in v1")
	}
}

// TestParseRemoteCIDRsNamesItsSource pins that the error names the caller's
// label (a flag or a config key), not a hardcoded "--remote-cidr": with only
// one label ever used, a local_cidrs parse failure would misleadingly read
// as a --remote-cidr problem.
func TestParseRemoteCIDRsNamesItsSource(t *testing.T) {
	_, err := ParseRemoteCIDRs("network.local_cidrs", []string{"not-a-cidr"})
	if err == nil || !strings.Contains(err.Error(), "network.local_cidrs") || strings.Contains(err.Error(), "--remote-cidr") {
		t.Fatalf("err = %v, want it to name network.local_cidrs and not --remote-cidr", err)
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

// addrIn reports whether any prefix in set contains addr - used instead of a
// hardcoded prefix list so a test survives Subtract choosing a different
// (but equally correct) split into pieces.
func addrIn(set []netip.Prefix, addr string) bool {
	a := netip.MustParseAddr(addr)
	for _, p := range set {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func sortedPrefixStrings(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	slices.Sort(out)
	return out
}

func TestSubtract(t *testing.T) {
	t.Run("no overlap leaves the prefix untouched", func(t *testing.T) {
		all := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
		got := Subtract(all, []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")})
		if len(got) != 1 || got[0] != all[0] {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("exact equality removes it", func(t *testing.T) {
		p := netip.MustParsePrefix("10.0.5.0/24")
		got := Subtract([]netip.Prefix{p}, []netip.Prefix{p})
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("a wider exclude removes it", func(t *testing.T) {
		got := Subtract([]netip.Prefix{netip.MustParsePrefix("10.0.5.0/24")}, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	// The primary use case (spec §4.1, and the sample in both config test
	// fixtures): a narrower exclude must carve an exact hole out of a wider
	// remote range, not be a no-op (old containment-only bug) and not drop
	// the whole range either.
	t.Run("a narrower exclude carves an exact hole out of a wider range", func(t *testing.T) {
		all := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("10.1.0.0/16")}
		got := Subtract(all, []netip.Prefix{netip.MustParsePrefix("10.0.5.0/24")})
		if addrIn(got, "10.0.5.42") {
			t.Fatalf("10.0.5.42 (inside the excluded /24) must not be covered: %v", got)
		}
		if !addrIn(got, "10.0.4.42") {
			t.Fatalf("10.0.4.42 (same /16, outside the excluded /24) must still be covered: %v", got)
		}
		if !addrIn(got, "10.1.0.1") {
			t.Fatalf("10.1.0.1 (an unrelated remote range) must still be covered: %v", got)
		}
		// 10.0.0.0/16 minus 10.0.5.0/24 splits into exactly 8 canonical
		// prefixes (24-16 halvings), plus the untouched 10.1.0.0/16.
		if len(got) != 9 {
			t.Fatalf("got %d prefixes, want 9 (8 from the split + the untouched /16): %v", len(got), got)
		}
	})

	t.Run("several excludes at once", func(t *testing.T) {
		all := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
		got := Subtract(all, []netip.Prefix{
			netip.MustParsePrefix("10.0.5.0/24"),
			netip.MustParsePrefix("10.0.9.0/24"),
		})
		for _, bad := range []string{"10.0.5.1", "10.0.9.1"} {
			if addrIn(got, bad) {
				t.Fatalf("%s must be excluded: %v", bad, got)
			}
		}
		if !addrIn(got, "10.0.1.1") {
			t.Fatalf("10.0.1.1 must still be covered: %v", got)
		}
	})

	t.Run("empty exclude list changes nothing", func(t *testing.T) {
		all := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("169.254.170.0/24")}
		got := Subtract(all, nil)
		if len(got) != 2 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("duplicate excludes are idempotent", func(t *testing.T) {
		all := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
		e := netip.MustParsePrefix("10.0.5.0/24")
		got := Subtract(all, []netip.Prefix{e, e})
		if addrIn(got, "10.0.5.1") {
			t.Fatalf("10.0.5.1 must be excluded: %v", got)
		}
		if !addrIn(got, "10.0.1.1") {
			t.Fatalf("10.0.1.1 must still be covered: %v", got)
		}
	})
}

// TestSubtractDoesNotAliasInput pins that Subtract, even with no exclusions
// at all, hands back a slice the caller can freely append to without
// corrupting all's backing array - the same bug class as remoteSet's own
// VPCCIDRs aliasing (TestRemoteSetDoesNotMutateTheProvidersVPCSlice below).
func TestSubtractDoesNotAliasInput(t *testing.T) {
	all := make([]netip.Prefix, 1, 4) // spare capacity an append could reuse
	all[0] = netip.MustParsePrefix("10.0.0.0/16")
	full := all[:cap(all)] // exposes any write past len(all) into its backing array

	got := Subtract(all, nil)
	got = append(got, netip.MustParsePrefix("192.168.0.0/16"))

	if full[1].IsValid() {
		t.Fatalf("Subtract's result aliases the input slice: appending to it wrote into all's backing array: %v", full[1])
	}
}

// TestRemoteSet pins the whole set remoteSet assembles: the VPC CIDRs, the
// extra --remote-cidr ranges and any network.remote_services prefixes -
// minus what network.local_cidrs claims for the laptop. Deleting any one
// term from remoteSet's assembly (the VPC append, the extra append, the
// ServiceCIDRs call, or the closing Subtract) changes this set and fails the
// test. Comparing sorted slices, not a map keyed by string, means a
// duplicated prefix cannot mask a missing one the way an equal len(map)
// could.
//
// 169.254.170.0/24 is not in it: the credential endpoint is served on
// loopback (credproxy.go), so it is captured only when the operator asks for
// the machine-wide route pin that needs it.
func TestRemoteSet(t *testing.T) {
	newProvider := func() *fakeProvider {
		return &fakeProvider{
			vpc: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
			svc: []netip.Prefix{netip.MustParsePrefix("52.219.0.0/20")},
		}
	}
	opts := RunOptions{
		RemoteCIDRs:    []string{"10.9.0.0/16"},
		RemoteServices: []string{"s3"},
		// 10.9.0.0/16 is an extra remote range but also the laptop's own
		// network in this scenario: local_cidrs must remove it again.
		LocalCIDRs: []string{"10.9.0.0/16"},
	}
	got, err := remoteSet(context.Background(), opts, newProvider(), transport.Task{SubnetID: "subnet-a"}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/16", "52.219.0.0/20"}
	slices.Sort(want)
	if !slices.Equal(sortedPrefixStrings(got), want) {
		t.Fatalf("got %v, want %v", sortedPrefixStrings(got), want)
	}

	// With the route pin asked for, the endpoint joins the set: the pin is
	// only usable while pf's rdr rule covers the address.
	opts.PinCredentialRoute = true
	got, err = remoteSet(context.Background(), opts, newProvider(), transport.Task{SubnetID: "subnet-a"}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, "169.254.170.0/24")
	slices.Sort(want)
	if !slices.Equal(sortedPrefixStrings(got), want) {
		t.Fatalf("got %v, want %v", sortedPrefixStrings(got), want)
	}
}

// TestRemoteSetTaskRoleCIDRSurvivesLocalCIDRs pins that TaskRoleCIDR cannot
// be removed via network.local_cidrs *while the route pin is on*, even by
// the most extreme possible entry: with the pin asked for, the endpoint has
// to be redirected by pf or the pinned route is a dead end, so it is
// required infrastructure rather than part of the operator-tunable set.
// Before this became a floor, TaskRoleCIDR was unioned in *before* the
// subtraction, so "0.0.0.0/0" - or the more plausible "169.254.0.0/16" the
// review called out - silently dropped it.
//
// With the pin off (the default, v0.3a) the address is nothing special: it
// is served on loopback instead, so local_cidrs is simply honoured.
func TestRemoteSetTaskRoleCIDRSurvivesLocalCIDRs(t *testing.T) {
	// "keep link-local on the laptop" is the plausible spelling of this
	// mistake, and it covers 169.254.170.0/24 exactly.
	p := &fakeProvider{vpc: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}
	opts := RunOptions{LocalCIDRs: []string{"169.254.0.0/16"}, PinCredentialRoute: true}
	got, err := remoteSet(context.Background(), opts, p, transport.Task{}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if !addrIn(got, "169.254.170.2") {
		t.Fatalf("the pinned endpoint must survive local_cidrs 169.254.0.0/16: %v", got)
	}
	if !addrIn(got, "10.0.0.42") {
		t.Fatalf("the VPC must still be captured: %v", got)
	}
	// "0.0.0.0/0" is the one spelling the floor must *not* rescue: it
	// leaves nothing else at all, and a run that captured only the
	// credential endpoint would print a green network line while every VPC
	// connection left over the laptop's own route (see
	// TestRemoteSetRejectsLocalCIDRsThatRemoveEverything - emptiness is
	// judged before the floor goes back on).
	p = &fakeProvider{vpc: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}
	if _, err := remoteSet(context.Background(), RunOptions{LocalCIDRs: []string{"0.0.0.0/0"}, PinCredentialRoute: true}, p, transport.Task{}, func(string, ...any) {}); err == nil {
		t.Fatal("local_cidrs 0.0.0.0/0 must be refused, not reduced to the floor")
	}

	// Without the pin there is no floor to defend: the operator asked for
	// link-local to stay on the laptop and nothing in tetherd needs it.
	p = &fakeProvider{vpc: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}
	got, err = remoteSet(context.Background(), RunOptions{LocalCIDRs: []string{"169.254.0.0/16"}}, p, transport.Task{}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if addrIn(got, "169.254.170.2") {
		t.Fatalf("local_cidrs must be honoured with no route pin asked for: %v", got)
	}
}

// TestRemoteSetDoesNotMutateTheProvidersVPCSlice pins the aliasing fix: the
// old append(append(vpc, ecsprov.TaskRoleCIDR), extra...) wrote into
// VPCCIDRs' own returned slice whenever it had spare capacity, silently
// corrupting a provider that reuses or caches that slice across calls.
func TestRemoteSetDoesNotMutateTheProvidersVPCSlice(t *testing.T) {
	vpc := make([]netip.Prefix, 1, 4) // spare capacity an aliasing append could reuse
	vpc[0] = netip.MustParsePrefix("10.0.0.0/16")
	full := vpc[:cap(vpc)] // exposes any write past len(vpc) into vpc's backing array

	p := &fakeProvider{vpc: vpc}
	opts := RunOptions{RemoteCIDRs: []string{"10.9.0.0/16"}}
	if _, err := remoteSet(context.Background(), opts, p, transport.Task{}, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if full[1].IsValid() {
		t.Fatalf("remoteSet wrote into the provider's VPC slice's spare capacity: %v", full[1])
	}
}

// TestRemoteSetRejectsBadLocalCIDRs pins the decision that a local_cidrs
// parse failure is reported with the "network.local_cidrs" prefix, so an
// operator can tell it apart from a bad --remote-cidr, and that it is a
// usage error: a value that is wrong however it arrived, flag or committed
// config file, which is what makes Run exit 2 for it the way it always has
// for a bad --remote-cidr (see TestRunExitsTwoForEveryBadCIDRValue).
func TestRemoteSetRejectsBadLocalCIDRs(t *testing.T) {
	p := &fakeProvider{}
	opts := RunOptions{LocalCIDRs: []string{"not-a-cidr"}}
	_, err := remoteSet(context.Background(), opts, p, transport.Task{}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "network.local_cidrs") {
		t.Fatalf("err = %v, want it to mention network.local_cidrs", err)
	}
	if !isUsageError(err) {
		t.Errorf("a local_cidrs typo is a usage error (exit 2), got %v", err)
	}
}

// TestRemoteSetParsesLocalCIDRsBeforeAnyAWSCall pins that a local_cidrs typo
// is caught before VPCCIDRs (or ServiceCIDRs) is ever called, so it costs no
// AWS round trip.
func TestRemoteSetParsesLocalCIDRsBeforeAnyAWSCall(t *testing.T) {
	p := &fakeProvider{vpc: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}
	opts := RunOptions{LocalCIDRs: []string{"not-a-cidr"}, RemoteServices: []string{"s3"}}
	if _, err := remoteSet(context.Background(), opts, p, transport.Task{}, func(string, ...any) {}); err == nil {
		t.Fatal("expected an error")
	}
	if p.vpcCalls != 0 {
		t.Fatalf("VPCCIDRs called %d times, want 0: a local_cidrs typo must be caught first", p.vpcCalls)
	}
	if p.svcCalls != 0 {
		t.Fatalf("ServiceCIDRs called %d times, want 0: a local_cidrs typo must be caught first", p.svcCalls)
	}
}

// TestRemoteSetRejectsLocalCIDRsThatRemoveEverything is the other half of
// TestRemoteSetTaskRoleCIDRSurvivesLocalCIDRs. The task-role floor must not
// double as a reason to accept a local_cidrs that leaves nothing else: the
// realistic version of this is "10.0.0.0/8" written to mean a home LAN
// against a 10.0.0.0/16 VPC, which removes the whole VPC. Judged after the
// floor went back on, that run started normally with a green network line
// and every VPC connection quietly left over the laptop's own route.
func TestRemoteSetRejectsLocalCIDRsThatRemoveEverything(t *testing.T) {
	// Under both settings: with the route pin on, the floor must not double
	// as a reason to accept it either.
	for _, pin := range []bool{false, true} {
		p := &fakeProvider{vpc: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}
		opts := RunOptions{LocalCIDRs: []string{"10.0.0.0/8"}, PinCredentialRoute: pin}
		_, err := remoteSet(context.Background(), opts, p, transport.Task{}, func(string, ...any) {})
		if err == nil {
			t.Fatalf("pin=%v: a local_cidrs that removes every remote range must be reported, not hidden by the task-role floor", pin)
		}
		if !strings.Contains(err.Error(), "local_cidrs") {
			t.Fatalf("pin=%v: the error must name local_cidrs: %v", pin, err)
		}
		if !isUsageError(err) {
			t.Errorf("pin=%v: a local_cidrs that removes everything is a usage error (exit 2), got %v", pin, err)
		}
	}
}

// TestRemoteSetRejectsAnEmptyRemoteSetWithoutBlamingLocalCIDRs: with the
// task-role floor gone from the default path, a run whose remote set is
// empty for some other reason - an IPv6-only VPC, whose IPv4 prefixes are
// none - reaches the same "nothing would be captured" guard. Until v0.2b
// the floor hid that case; blaming network.local_cidrs for it would send the
// operator to a key they never set.
func TestRemoteSetRejectsAnEmptyRemoteSetWithoutBlamingLocalCIDRs(t *testing.T) {
	p := &fakeProvider{} // VPCCIDRs returns nothing
	_, err := remoteSet(context.Background(), RunOptions{}, p, transport.Task{}, func(string, ...any) {})
	if err == nil {
		t.Fatal("capturing nothing at all must be reported")
	}
	if strings.Contains(err.Error(), "local_cidrs") {
		t.Fatalf("local_cidrs was never set; the error must not blame it: %v", err)
	}
	if !isUsageError(err) {
		t.Errorf("pointing tetherd at a VPC it cannot capture is a usage error (exit 2), got %v", err)
	}
}
