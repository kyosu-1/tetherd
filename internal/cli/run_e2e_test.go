package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/kyosu-1/tetherd/internal/agent"
	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/helper"
	ecsprov "github.com/kyosu-1/tetherd/internal/provider/ecs"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// inProcessAgent runs a real tetherd-agent on a local listener and returns a
// Transport that dials it, so Run's ssm branch can be exercised without AWS.
type inProcessAgent struct {
	addr string
}

func startAgentFor(t *testing.T, env map[string]string, envErr error, dial func(context.Context, string) (net.Conn, error)) *inProcessAgent {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(agent.Config{Env: "dev", TaskARN: "arn:test", AppContainer: "app"}, nil)
	a.SetEnvReader(fakeEnvReader{env: env, arn: "arn:test", err: envErr})
	if dial != nil {
		a.SetDialer(dial)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go a.Serve(ctx, ln)
	return &inProcessAgent{addr: ln.Addr().String()}
}

type fakeEnvReader struct {
	env map[string]string
	arn string
	err error
}

func (f fakeEnvReader) Read(context.Context) (map[string]string, string, error) {
	return f.env, f.arn, f.err
}

type agentTransport struct{ addr string }

func (a agentTransport) Dial(ctx context.Context, _ transport.Task) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", a.addr)
}

// fakeProvider stands in for AWS.
type fakeProvider struct {
	region string
	task   transport.Task
	vpc    []netip.Prefix
	svc    []netip.Prefix
	// discErr and vpcErr are for later tasks; nothing sets them yet.
	discErr   error
	vpcErr    error
	agentAddr string

	// vpcCalls and svcCalls count invocations, so a test can assert an AWS
	// round trip was (or, for a local_cidrs typo, was not) made.
	vpcCalls int
	svcCalls int
}

func (f *fakeProvider) Region() string { return f.region }
func (f *fakeProvider) Discover(context.Context, ecsprov.Target) (transport.Task, error) {
	return f.task, f.discErr
}
func (f *fakeProvider) VPCCIDRs(context.Context, string) ([]netip.Prefix, error) {
	f.vpcCalls++
	return f.vpc, f.vpcErr
}
func (f *fakeProvider) ServiceCIDRs(context.Context, []string) ([]netip.Prefix, error) {
	f.svcCalls++
	return f.svc, nil
}
func (f *fakeProvider) Transport(func(string, ...any)) transport.Transport {
	return agentTransport{addr: f.agentAddr}
}

func ssmOpts(cmd ...string) RunOptions {
	return RunOptions{
		Transport: "ssm", Cluster: "c", Service: "api", TargetEnv: "dev",
		User: "tester", NoNetwork: true, Command: cmd,
	}
}

func depsFor(p *fakeProvider) Deps {
	return Deps{NewAWSProvider: func(context.Context, RunOptions) (awsProvider, error) { return p, nil }}
}

func TestRunInjectsTaskEnvIntoTheChild(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"DB_HOST": "db.internal", "PORT": "8081"}, nil, nil)
	p := &fakeProvider{region: "ap-northeast-1", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: ag.addr}
	var out strings.Builder
	// The child exits non-zero unless DB_HOST really reached it, so this
	// fails if Run stops injecting the task env: the status line below is
	// printed before the child is built and cannot catch that on its own.
	code, err := RunWithDeps(context.Background(), ssmOpts("sh", "-c", `[ "$DB_HOST" = db.internal ]`), &out, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("the child did not see DB_HOST: code=%d err=%v log=%s", code, err, out.String())
	}
	if !strings.Contains(out.String(), "✓ env      2 vars from the task") {
		t.Errorf("status line missing: %s", out.String())
	}
}

func TestRunRefusesAnEnvironmentMismatch(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}
	opts := ssmOpts("true")
	opts.TargetEnv = "prod"
	code, err := RunWithDeps(context.Background(), opts, io.Discard, depsFor(p))
	if code != 1 || err == nil || !strings.Contains(err.Error(), "refusing to attach") {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestRunEnvErrorIsFatalForSSMAndSkippableWithNoEnv(t *testing.T) {
	ag := startAgentFor(t, nil, fmt.Errorf("pidMode task is not set"), nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}

	code, err := RunWithDeps(context.Background(), ssmOpts("true"), io.Discard, depsFor(p))
	if code != 1 || err == nil || !strings.Contains(err.Error(), "pidMode") {
		t.Fatalf("want the env error to be fatal: code=%d err=%v", code, err)
	}

	opts := ssmOpts("true")
	opts.NoEnv = true
	if code, err := RunWithDeps(context.Background(), opts, io.Discard, depsFor(p)); code != 0 || err != nil {
		t.Fatalf("--no-env must run anyway: code=%d err=%v", code, err)
	}
}

// The remote set is the property three live bugs came from, so pin it here.
func TestRunBuildsTheRemoteSet(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{
		region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	cap := newFakeCapturer()
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return cap }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.RemoteCIDRs = []string{"10.9.0.0/16"}
	opts.ExecPath = "/usr/bin/true" // exists, so the pre-flight passes
	var out strings.Builder
	code, err := RunWithDeps(context.Background(), opts, &out, d)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	got := map[string]bool{}
	for _, p := range cap.spec.RemoteCIDRs {
		got[p.String()] = true
	}
	for _, want := range []string{"10.0.0.0/16", "169.254.170.0/24", "10.9.0.0/16"} {
		if !got[want] {
			t.Errorf("%s missing from the captured set: %v", want, cap.spec.RemoteCIDRs)
		}
	}
}

// The task role has to be claimed only when its endpoint is captured.
func TestRunVerifiesTheTaskRoleThroughTheAgent(t *testing.T) {
	// The endpoint fails on purpose. Serving usable credentials would send
	// Run on to awsid.CallerIdentity, which builds its own aws.Config and is
	// given no endpoint override here, so the test would make a real HTTPS
	// call to sts.<region>.amazonaws.com and could block for the full 15s
	// probe timeout. A non-2xx stops the run inside
	// FetchContainerCredentials instead, which is early enough to leave both
	// assertions below meaningful and late enough to prove the credential
	// request travelled through the agent.
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no credentials for you", http.StatusServiceUnavailable)
	})}
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(cl)

	// The agent dials the fake credential endpoint whatever address it is asked for.
	ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil,
		func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", cl.Addr().String())
		})
	p := &fakeProvider{
		region: "ap-northeast-1", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = "/usr/bin/true"
	var out strings.Builder
	// The credential fetch fails (see the handler above), so Run reports the
	// failure; what matters is that tetherd got that far through the agent
	// and said so honestly.
	RunWithDeps(context.Background(), opts, &out, d)
	if !strings.Contains(out.String(), "iam") {
		t.Fatalf("no iam line at all: %s", out.String())
	}
	if strings.Contains(out.String(), "the task advertises a role but") {
		t.Errorf("169.254.170.0/24 is captured, so the not-captured warning must not fire: %s", out.String())
	}
}

// TestRunTaskRoleOverrideBeatsConfigEnvOverride pins item 8b: a committed
// .tetherd.yml naming AWS_CONFIG_FILE in env.override must not be able to
// defeat the task-role hardening that hides the developer's shared AWS
// config from the child - that would hand the child the developer's own
// identity while the status line still prints a green iam line. The
// credential endpoint is made to fail on purpose, exactly as in
// TestRunVerifiesTheTaskRoleThroughTheAgent above: the override is built as
// soon as the task role is *reachable* (before the credential fetch even
// happens), so this failure mode is provable without a real STS call.
//
// With !opts.NoNetwork, Run execs opts.ExecPath (the setgid tetherd-exec in
// production) and passes the real command as trailing args for *it* to run;
// a stand-in like "/usr/bin/true" (used elsewhere in this file to satisfy
// the pre-flight stat) never looks at those args, so it cannot be used to
// observe child.Env here. Instead ExecPath is pointed at a throwaway shell
// script that dumps its own environment - which is exactly child.Env, built
// by the env.Merge call this test is pinning - to a file this test reads
// back directly.
func TestRunTaskRoleOverrideBeatsConfigEnvOverride(t *testing.T) {
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no credentials for you", http.StatusServiceUnavailable)
	})}
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(cl)

	ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil,
		func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", cl.Addr().String())
		})
	p := &fakeProvider{
		region: "ap-northeast-1", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }

	dir := t.TempDir()
	dump := filepath.Join(dir, "env-dump")
	content := filepath.Join(dir, "aws-config-file-content")
	script := filepath.Join(dir, "fake-exec.sh")
	// Both dumps happen from inside the fake exec while it stands in for the
	// child: the real AWS_CONFIG_FILE temp file only lives until Run returns
	// (its cleanup is deferred there), so its content has to be captured
	// synchronously, during this script's run, not read back afterwards.
	fakeExec := "#!/bin/sh\nenv > " + dump + "\ncat \"$AWS_CONFIG_FILE\" > " + content + " 2>/dev/null\nexit 0\n"
	if err := os.WriteFile(script, []byte(fakeExec), 0o755); err != nil {
		t.Fatal(err)
	}

	const poison = "/tmp/tetherd-test-should-not-win"
	opts := ssmOpts("true") // never actually run: the fake exec above ignores its args
	opts.NoNetwork = false
	opts.ExecPath = script
	opts.EnvOverride = map[string]string{"AWS_CONFIG_FILE": poison}
	var out strings.Builder
	code, err := RunWithDeps(context.Background(), opts, &out, d)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}

	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(readFile(t, dump), "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	got, ok := env["AWS_CONFIG_FILE"]
	if !ok {
		t.Fatal("AWS_CONFIG_FILE was not set in the child's environment at all")
	}
	if got == poison {
		t.Fatalf("the task-role AWS_CONFIG_FILE must win over a config env.override of the same name, got the config's value %q", got)
	}
	if b := readFile(t, content); b != "" {
		t.Fatalf("the task-role AWS_CONFIG_FILE must name an empty file (the task-role hiding effect), got %q: %q", got, b)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRunFailsOnBadLocalCIDRs pins the decision that a bad local_cidrs
// entry - which can now arrive from a committed .tetherd.yml, not only a
// flag - exits 1 (an operational/config failure) rather than the usage exit
// code 2 that a bad --remote-cidr still gets before the switch on
// opts.Transport (see TestRunSSMRequiresClusterAndService and
// TestParseRemoteCIDRs elsewhere in this package for that flag-side check).
func TestRunFailsOnBadLocalCIDRs(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{
		region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = "/usr/bin/true"
	opts.LocalCIDRs = []string{"not-a-cidr"}
	code, err := RunWithDeps(context.Background(), opts, io.Discard, d)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "network.local_cidrs") {
		t.Fatalf("code=%d err=%v, want code 1 and a network.local_cidrs error", code, err)
	}
}

// TestRunDirectAppliesLocalCIDRs pins item 6 of the fix-round-1 review:
// --transport direct has no AWS session to compute remote_services or a VPC
// set from, but network.local_cidrs needs neither, so it must still apply.
func TestRunDirectAppliesLocalCIDRs(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	cap := newFakeCapturer()
	d := Deps{
		DialHelper:  func(string) (HelperClient, error) { return &fakeHelperClient{}, nil },
		NewCapturer: func(HelperClient, func(string, ...any)) Capturer { return cap },
	}

	opts := RunOptions{
		Transport: "direct", AgentAddr: ag.addr, TargetEnv: "dev", User: "tester",
		RemoteCIDRs: []string{"10.0.0.0/16"}, LocalCIDRs: []string{"10.0.5.0/24"},
		ExecPath: "/usr/bin/true", Command: []string{"true"},
	}
	var out strings.Builder
	code, err := RunWithDeps(context.Background(), opts, &out, d)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if addrIn(cap.spec.RemoteCIDRs, "10.0.5.42") {
		t.Fatalf("10.0.5.42 (excluded by local_cidrs) must not be captured under --transport direct: %v", cap.spec.RemoteCIDRs)
	}
	if !addrIn(cap.spec.RemoteCIDRs, "10.0.4.42") {
		t.Fatalf("10.0.4.42 (same /16, outside local_cidrs) must still be captured: %v", cap.spec.RemoteCIDRs)
	}
}

// TestRunDirectIgnoresRemoteServicesWithALogLine pins the rest of item 6:
// remote_services needs an AWS session to resolve a managed prefix list, so
// --transport direct cannot honour it - but it must say so, not silently
// drop it with no trace.
func TestRunDirectIgnoresRemoteServicesWithALogLine(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	d := Deps{
		DialHelper:  func(string) (HelperClient, error) { return &fakeHelperClient{}, nil },
		NewCapturer: func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() },
	}

	opts := RunOptions{
		Transport: "direct", AgentAddr: ag.addr, TargetEnv: "dev", User: "tester",
		RemoteCIDRs: []string{"10.0.0.0/16"}, RemoteServices: []string{"s3"},
		ExecPath: "/usr/bin/true", Command: []string{"true"},
	}
	var out strings.Builder
	code, err := RunWithDeps(context.Background(), opts, &out, d)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if !strings.Contains(out.String(), "remote_services") || !strings.Contains(out.String(), "ignored") {
		t.Fatalf("expected a remote_services-ignored line under --transport direct: %s", out.String())
	}
}

// TestRunDirectRejectsLocalCIDRsExcludingEverything pins item 7 (the branch
// where it is actually reachable): --transport direct has no TaskRoleCIDR
// floor the way ssm's remoteSet does, so local_cidrs wiping out every
// --remote-cidr really does leave zero remote ranges, which must be a loud
// error naming local_cidrs rather than a silent hand-off to the helper. This
// fails before the agent is ever dialed, so a bogus address is enough.
func TestRunDirectRejectsLocalCIDRsExcludingEverything(t *testing.T) {
	opts := RunOptions{
		Transport: "direct", AgentAddr: "127.0.0.1:1", TargetEnv: "dev", User: "tester",
		RemoteCIDRs: []string{"10.0.0.0/16"}, LocalCIDRs: []string{"10.0.0.0/8"},
		ExecPath: "/usr/bin/true", Command: []string{"true"},
	}
	code, err := RunWithDeps(context.Background(), opts, io.Discard, Deps{})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "network.local_cidrs") {
		t.Fatalf("code=%d err=%v, want code 1 and a network.local_cidrs error", code, err)
	}
}

func TestRunWarnsWhenTheCredentialEndpointIsNotCaptured(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil, nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", Addr: ag.addr}, agentAddr: ag.addr}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }

	opts := RunOptions{
		Transport: "direct", AgentAddr: ag.addr, TargetEnv: "dev", User: "tester",
		RemoteCIDRs: []string{"10.0.0.0/16"}, ExecPath: "/usr/bin/true", Command: []string{"true"},
	}
	var out strings.Builder
	RunWithDeps(context.Background(), opts, &out, d)
	if !strings.Contains(out.String(), "the task advertises a role but") {
		t.Fatalf("the warning must fire when 169.254.170.0/24 is not captured: %s", out.String())
	}
}

// TestRunPointsRemoteDomainsAtTheAgent pins the wiring in Run's transparent
// branch end to end: network.remote_domains must start a loopback DNS
// resolver, hand the helper the domains and the port it actually bound, and
// wire that resolver's Resolve to the real session (so it reaches this
// in-process agent) - not just record the right-looking fields. A stub
// asserting only hcFake.domains and hcFake.port would still pass with the
// port hardcoded wrong or Resolve replaced by something that always
// errors; querying the resolver at the exact port resolver.set was told
// about is what makes either of those failures visible. ResolverClear
// being called at teardown is also pinned here, alongside the status line.
func TestRunPointsRemoteDomainsAtTheAgent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(agent.Config{Env: "dev", TaskARN: "arn:test", AppContainer: "app"}, nil)
	a.SetEnvReader(fakeEnvReader{env: map[string]string{"A": "1"}, arn: "arn:test"})
	a.SetResolver(func(_ context.Context, name string) ([]net.IPAddr, error) {
		if name == "api.myapp.internal" {
			return []net.IPAddr{{IP: net.ParseIP("10.0.11.229")}}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	})
	actx, acancel := context.WithCancel(context.Background())
	t.Cleanup(func() { acancel(); ln.Close() })
	go a.Serve(actx, ln)

	p := &fakeProvider{
		region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ln.Addr().String(),
	}
	portKnown := make(chan struct{})
	hcFake := &fakeHelperClient{}
	hcFake.onResolverSet = func() { close(portKnown) }
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return hcFake, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }

	// The real setgid tetherd-exec would run opts.Command; this stand-in
	// just sleeps so the resolver stays up long enough for the query below
	// - what actually runs is irrelevant to what this test pins (the same
	// convention TestRunTaskRoleOverrideBeatsConfigEnvOverride uses).
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-exec.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = script
	opts.RemoteDomains = []string{"api.myapp.internal"}
	var out strings.Builder
	runDone := make(chan struct{})
	var code int
	var runErr error
	go func() {
		code, runErr = RunWithDeps(context.Background(), opts, &out, d)
		close(runDone)
	}()

	select {
	case <-portKnown:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver.set was never called")
	}
	if hcFake.port == 0 {
		t.Fatalf("resolver.set was called with port 0")
	}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("api.myapp.internal"), dns.TypeA)
	c := &dns.Client{Net: "udp", Timeout: 5 * time.Second}
	resp, _, err := c.Exchange(m, fmt.Sprintf("127.0.0.1:%d", hcFake.port))
	if err != nil {
		t.Fatalf("querying the resolver at the recorded port %d: %v", hcFake.port, err)
	}
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("rcode=%d answer=%v", resp.Rcode, resp.Answer)
	}
	rec, ok := resp.Answer[0].(*dns.A)
	if !ok || rec.A.String() != "10.0.11.229" {
		t.Fatalf("answer = %v, want 10.0.11.229", resp.Answer[0])
	}

	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("RunWithDeps never returned")
	}
	if runErr != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, runErr, out.String())
	}
	if len(hcFake.domains) != 1 || hcFake.domains[0] != "api.myapp.internal" {
		t.Fatalf("resolver.set was called with %v", hcFake.domains)
	}
	if !strings.Contains(out.String(), "api.myapp.internal via the VPC resolver") {
		t.Errorf("the status line must say what is routed: %s", out.String())
	}
	if !hcFake.cleared {
		t.Fatalf("hc.ResolverClear() must be called during teardown")
	}
}

// TestRunClearsTheResolverEvenWhenResolverSetPartiallyFails pins the fix
// for the worst outcome this task could produce: hc.ResolverClear() must
// run even when hc.ResolverSet itself fails (e.g. it writes
// myapp.internal's file fine and then errors on corp.internal, which some
// other tool already manages). Registering the clear defer only after a
// successful ResolverSet would leave myapp.internal's file pointed at
// 127.0.0.1:<port> with nothing listening there anymore, forever, until
// someone deletes it by hand.
func TestRunClearsTheResolverEvenWhenResolverSetPartiallyFails(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{
		region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	hcFake := &fakeHelperClient{
		resolverSetErr: errors.New("resolver.set: /etc/resolver/corp.internal exists and is not managed by tetherd"),
	}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return hcFake, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = "/usr/bin/true"
	opts.RemoteDomains = []string{"myapp.internal", "corp.internal"}
	code, err := RunWithDeps(context.Background(), opts, io.Discard, d)
	if code != 1 || err == nil {
		t.Fatalf("code=%d err=%v, want Run to report the resolver.set failure", code, err)
	}
	if !hcFake.cleared {
		t.Fatalf("hc.ResolverClear() must run even when resolver.set fails partway through")
	}
}

// TestRunNoNetworkSkipsTheResolverEvenWithRemoteDomains pins that
// --no-network must not touch the helper (and so not /etc/resolver) at all,
// even when the config sets network.remote_domains: --no-network is the
// escape hatch for "no privileged helper involved", and a resolver started
// behind its back would both dial a helper it promised not to and leave a
// stray listener nothing tells the operator about.
func TestRunNoNetworkSkipsTheResolverEvenWithRemoteDomains(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: ag.addr}
	dialed := false
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) {
		dialed = true
		return &fakeHelperClient{}, nil
	}

	opts := ssmOpts("true")
	opts.RemoteDomains = []string{"myapp.internal"}
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if dialed {
		t.Fatalf("--no-network must never dial the helper, even with remote_domains set")
	}
}

// --- fakes for the helper and the capturer ---

type fakeHelperClient struct {
	domains []string
	port    int
	cleared bool
	// resolverSetErr, when set, is what ResolverSet returns - after still
	// recording domains/port, the way the real helper records some
	// domains as written before failing on a later one.
	resolverSetErr error
	// onResolverSet, when set, runs synchronously inside ResolverSet right
	// after recording domains/port, so a test can learn the port without
	// polling.
	onResolverSet func()
}

func (f *fakeHelperClient) PfApply(helper.PfSpec) error { return nil }
func (f *fakeHelperClient) PfClear() error              { return nil }
func (f *fakeHelperClient) NatLook(string, netip.AddrPort, netip.AddrPort) (netip.AddrPort, error) {
	return netip.MustParseAddrPort("10.0.0.1:5432"), nil
}
func (f *fakeHelperClient) ResolverSet(domains []string, port int) error {
	f.domains = domains
	f.port = port
	if f.onResolverSet != nil {
		f.onResolverSet()
	}
	return f.resolverSetErr
}
func (f *fakeHelperClient) ResolverClear() error {
	f.cleared = true
	return nil
}
func (f *fakeHelperClient) Close() error { return nil }

type fakeCapturer struct {
	// spec is what Run asked to capture: capture.Spec is the whole contract
	// between Run and a capturer, so recording it here pins the remote set
	// without the fake reimplementing what pfrdr does with it.
	spec capture.Spec
	// started and closed are recorded for later tasks; nothing asserts on
	// them yet.
	started bool
	closed  bool
	// accept is created up front so Accept never writes to the field: the
	// forwarder goroutine reads it while the test goroutine closes it.
	accept chan capture.Conn
	once   sync.Once
}

func newFakeCapturer() *fakeCapturer {
	return &fakeCapturer{accept: make(chan capture.Conn)}
}

func (f *fakeCapturer) Start(_ context.Context, spec capture.Spec) error {
	f.spec = spec
	f.started = true
	return nil
}
func (f *fakeCapturer) Accept() (capture.Conn, error) {
	cc, ok := <-f.accept
	if !ok {
		return capture.Conn{}, io.EOF
	}
	return cc, nil
}

// Close unblocks Accept, the way the real pfrdr capturer does by shutting its
// listener. Without it every test that installs capture leaks the
// proxy.Forwarder goroutine that is parked in Accept.
func (f *fakeCapturer) Close() error {
	f.closed = true
	f.once.Do(func() { close(f.accept) })
	return nil
}
func (f *fakeCapturer) RedirectPort() int { return 15300 }
