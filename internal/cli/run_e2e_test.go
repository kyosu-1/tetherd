package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/miekg/dns"

	"github.com/kyosu-1/tetherd/internal/agent"
	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/env"
	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/proto"
	ecsprov "github.com/kyosu-1/tetherd/internal/provider/ecs"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// inProcessAgent runs a real tetherd-agent on a local listener and returns a
// Transport that dials it, so Run's ssm branch can be exercised without AWS.
//
// proxyAddr is set only by startAgentForWithApp: it is the agent's real
// reverse proxy, which is where the ALB puts a request.
type inProcessAgent struct {
	addr      string
	proxyAddr string
	a         *agent.Agent
}

// waitDetached blocks until the agent has processed the previous session's
// disconnect.
//
// handler.Closed - and so unregister - runs on the agent's own goroutine
// when it notices the connection is gone. Nothing orders that against
// RunWithDeps returning on this one, so a test that attaches twice as the
// same user has to wait in between or the second hello is refused with
// duplicate_user.
//
// This is not a narrow race. Measured: at the instant RunWithDeps returns,
// the agent still held the previous session in 60 of 60 runs. The window is
// always open; whether a test loses it depends only on how much work happens
// before its next hello arrives. That is why this failed in CI and not
// locally - not a faster machine, just a different amount of slack.
func (ag *inProcessAgent) waitDetached(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool { return len(ag.a.Sessions()) == 0 }, "the agent to see the previous session detach")
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
	return &inProcessAgent{addr: ln.Addr().String(), a: a}
}

// startAgentForWithApp is startAgentFor plus the agent's own reverse proxy
// on a second listener, with appAddr standing in for the application
// container. proxyAddr is then where a request from the ALB arrives, so a
// test can put the agent's real matching and real steal transport in front
// of the CLI's real receiver rather than a stand-in's idea of either.
func startAgentForWithApp(t *testing.T, env map[string]string, appAddr string) *inProcessAgent {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(agent.Config{Env: "dev", TaskARN: "arn:test", AppContainer: "app"}, nil)
	a.SetEnvReader(fakeEnvReader{env: env, arn: "arn:test"})
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &http.Server{Handler: (&agent.Proxy{Agent: a, AppAddr: appAddr}).Handler()}
	t.Cleanup(func() { cancel(); proxy.Close(); ln.Close(); proxyLn.Close() })
	go a.Serve(ctx, ln)
	go proxy.Serve(proxyLn)
	return &inProcessAgent{addr: ln.Addr().String(), proxyAddr: proxyLn.Addr().String(), a: a}
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

// Dial connects to the one agent this transport was built for, or - when it
// was built without an address - to the agent the task itself names. The
// second form is what a multi-task fixture needs: each task has its own
// agent, and which one a session reaches is the whole question.
func (a agentTransport) Dial(ctx context.Context, t transport.Task) (net.Conn, error) {
	addr := a.addr
	if addr == "" {
		addr = t.Addr
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

// fakeProvider stands in for AWS.
type fakeProvider struct {
	region string
	task   transport.Task
	// tasks is what DiscoverAll answers: a service with more than one
	// RUNNING task, which is what `tetherd run` attaches to all of. When it
	// is empty both Discover and DiscoverAll fall back to the single task
	// above, so every fixture written before v0.3b still means what it did.
	//
	// mu guards tasks and discErr, because DiscoverAll is called from the
	// follower's goroutine as well as the run's: a test that changes the
	// task list mid-run (a deploy) must do it through setTasks.
	mu    sync.Mutex
	tasks []transport.Task
	vpc   []netip.Prefix
	svc   []netip.Prefix
	// discErr fails discovery - both Discover and DiscoverAll - which is
	// how a test gets "no attachable task" at startup, or a task list that
	// stops being readable while a run is live.
	discErr   error
	vpcErr    error
	agentAddr string

	// tr replaces the plain dial-to-agentAddr transport when a test needs
	// to control what Dial does (block until its context expires) or to
	// keep hold of the connection it returns (and break it mid-run).
	tr transport.Transport

	// vpcCalls and svcCalls count invocations, so a test can assert an AWS
	// round trip was (or, for a local_cidrs typo, was not) made.
	vpcCalls int
	svcCalls int

	// secrets, secretsErr and pidMode back SecretNames/PIDMode; secretsARN
	// records what definitionARN SecretNames was actually asked about, so a
	// test can pin that `tetherd env` passes the task's own DefinitionARN
	// through rather than some other value.
	secrets    map[string]bool
	secretsErr error
	secretsARN string
	pidMode    string
	pidModeErr error
	// pidModeARN records which definitionARN PIDMode was asked about, so a
	// test can pin that the task's own is passed through.
	pidModeARN string

	// identity and identityErr back Identity, which only `tetherd doctor`
	// calls.
	identity    string
	identityErr error
}

// errAlwaysFails stands in for an AWS permission failure (e.g. no
// ecs:DescribeTaskDefinition) in tests that must prove EnvRun fails closed
// rather than falling back to printing everything unmasked.
var errAlwaysFails = errors.New("simulated AWS failure")

func (f *fakeProvider) Region() string { return f.region }
func (f *fakeProvider) Discover(ctx context.Context, t ecsprov.Target) (transport.Task, error) {
	all, err := f.DiscoverAll(ctx, t)
	if err != nil {
		return transport.Task{}, err
	}
	return all[0], nil
}

// DiscoverAll answers like the real one: oldest task first, only the pinned
// task when Target.TaskID names one, and never an empty list with a nil
// error.
func (f *fakeProvider) DiscoverAll(_ context.Context, t ecsprov.Target) ([]transport.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.discErr != nil {
		return nil, f.discErr
	}
	all := f.tasks
	if len(all) == 0 {
		all = []transport.Task{f.task}
	}
	out := make([]transport.Task, 0, len(all))
	for _, tk := range all {
		if t.TaskID != "" && tk.ID != t.TaskID {
			continue
		}
		out = append(out, tk)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("task %s is not RUNNING in %s/%s", t.TaskID, t.Cluster, t.Service)
	}
	return out, nil
}

// setTasks and setDiscoverErr are how a test changes what discovery says
// while a run is live: the follower polls from its own goroutine.
func (f *fakeProvider) setTasks(tasks ...transport.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks = tasks
}

func (f *fakeProvider) setDiscoverErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discErr = err
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
	if f.tr != nil {
		return f.tr
	}
	return agentTransport{addr: f.agentAddr}
}
func (f *fakeProvider) SecretNames(_ context.Context, definitionARN string) (map[string]bool, error) {
	f.secretsARN = definitionARN
	return f.secrets, f.secretsErr
}
func (f *fakeProvider) PIDMode(_ context.Context, definitionARN string) (string, error) {
	f.pidModeARN = definitionARN
	return f.pidMode, f.pidModeErr
}
func (f *fakeProvider) Identity(context.Context) (string, error) {
	return f.identity, f.identityErr
}

func ssmOpts(cmd ...string) RunOptions {
	return RunOptions{
		Transport: "ssm", Cluster: "c", Service: "api", TargetEnv: "dev",
		User: "tester", NoNetwork: true, Command: cmd,
		// Steal is on by default (see DefaultLocalPort) and a session that
		// takes requests must carry a token, so a bare RunOptions with
		// neither would be refused before it ever reached the agent. The
		// tests that are about steal say so by clearing this and setting a
		// port and a token; every other test here is about something else.
		NoIncoming: true,
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
	ag.waitDetached(t)
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
	for _, want := range []string{"10.0.0.0/16", "10.9.0.0/16"} {
		if !got[want] {
			t.Errorf("%s missing from the captured set: %v", want, cap.spec.RemoteCIDRs)
		}
	}
	// 169.254.170.0/24 is deliberately absent: the credential endpoint is
	// served on loopback now, so capturing it machine-wide buys nothing and
	// takes the address away from anything local that owns it.
	if got["169.254.170.0/24"] {
		t.Errorf("the credential endpoint must not be captured by default: %v", cap.spec.RemoteCIDRs)
	}
}

// TestRunTruncatesTheRemoteSetInItsStatusLine: the network line printed
// every prefix comma-joined, and with `remote_services: [s3]` that is a
// whole managed prefix list plus whatever local_cidrs splitting added - one
// unreadable line in the place a developer looks first. doctor's remote
// CIDRs row had the same problem, and both now go through
// doctor.FormatPrefixes, so the two cannot drift apart again.
func TestRunTruncatesTheRemoteSetInItsStatusLine(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	// A stand-in for the S3 prefix list: 15 entries in ap-northeast-1.
	var svc []netip.Prefix
	for i := 0; i < 15; i++ {
		svc = append(svc, netip.MustParsePrefix(fmt.Sprintf("52.219.%d.0/24", i)))
	}
	p := &fakeProvider{
		region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		svc:       svc,
		agentAddr: ag.addr,
	}
	cap := newFakeCapturer()
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return cap }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.RemoteServices = []string{"s3"}
	opts.ExecPath = "/usr/bin/true"
	var out strings.Builder
	code, err := RunWithDeps(context.Background(), opts, &out, d)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}

	var line string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "✓ network") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no network status line at all: %s", out.String())
	}
	// pf still gets every prefix; only the line a human reads is shortened.
	if len(cap.spec.RemoteCIDRs) < len(svc) {
		t.Fatalf("truncating the line must not truncate the captured set: %d prefixes", len(cap.spec.RemoteCIDRs))
	}
	if !strings.Contains(line, fmt.Sprintf("%d prefixes", len(cap.spec.RemoteCIDRs))) {
		t.Errorf("the network line must say how many prefixes are captured, got %q", line)
	}
	if strings.Contains(line, svc[len(svc)-1].String()) {
		t.Errorf("the network line must not list the whole prefix list, got %q", line)
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
	// The probe has to travel the child's own path: through the loopback
	// port, over the session, to the endpoint - and the line has to say so
	// on the line that reports the probe, not merely somewhere in the log
	// (the ✓ endpoint line above already names the port, so a
	// whole-output Contains check here would pin nothing).
	if l := iamResultLine(t, out.String()); !strings.Contains(l, "127.0.0.1:") {
		t.Errorf("the iam line must name the loopback port the child is pointed at, got %q", l)
	}
}

// iamResultLine returns the ✓/⚠ iam line that reports what the credential
// probe found - the one carrying "(via …)". Asserting on this line rather
// than on the whole log is what makes "the probe went through the loopback
// proxy" testable: the ✓ endpoint line names the same port a few lines
// earlier, so a Contains check over out.String() passes even with the iam
// line reverted to "(via 169.254.170.2)".
func iamResultLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "iam") && strings.Contains(l, "(via ") {
			return l
		}
	}
	t.Fatalf("no iam line reporting the probe (a '(via …)' line) in:\n%s", out)
	return ""
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

// TestRunStripsLocalAWSCredentialsFromTheChild pins the one line that stops
// the child signing with the developer's own identity while the status line
// prints a green iam row: mergeOpts.StripLocal = env.LocalAWSCredentialVars.
// Mutating that line to nil left the whole suite green -
// TestTaskRoleEnvOverridesLocalSharedConfig covers the shared-config half of
// the same hardening (AWS_CONFIG_FILE pointed at an empty file), and the
// environment-variable half was unpinned. Every name in
// LocalAWSCredentialVars resolves before the container provider in the SDK
// chain, so one of them surviving is enough to make the task role never
// apply, silently.
//
// Every name in the list is set, not just two, so shortening the list is
// caught here as well.
//
// The child is a throwaway script at opts.ExecPath that dumps its own
// environment, for the reason spelled out on
// TestRunTaskRoleOverrideBeatsConfigEnvOverride: in transparent mode Run
// execs ExecPath and hands it the real command as trailing args, so a
// stand-in like /usr/bin/true cannot show what child.Env was. The
// credential endpoint fails on purpose (503) - the strip happens because the
// task role is *reachable*, before it is verified, so this is provable with
// no real STS call.
func TestRunStripsLocalAWSCredentialsFromTheChild(t *testing.T) {
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

	// The developer's own credentials, in this process's environment exactly
	// as they would be in a shell with a profile exported.
	for _, name := range env.LocalAWSCredentialVars {
		t.Setenv(name, "local-"+name)
	}

	dir := t.TempDir()
	dump := filepath.Join(dir, "env-dump")
	script := filepath.Join(dir, "fake-exec.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+dump+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := ssmOpts("true") // never run: the fake exec ignores its args
	opts.NoNetwork = false
	opts.ExecPath = script
	var out strings.Builder
	code, err := RunWithDeps(context.Background(), opts, &out, d)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}

	childEnv := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(readFile(t, dump), "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			childEnv[k] = v
		}
	}
	// A sanity check on the harness itself: if the dump were empty or the
	// script never ran, every assertion below would pass for the wrong
	// reason.
	if len(childEnv) == 0 || childEnv["AWS_CONTAINER_CREDENTIALS_FULL_URI"] == "" {
		t.Fatalf("the child's environment was not captured (%d vars): %s", len(childEnv), out.String())
	}
	for _, name := range env.LocalAWSCredentialVars {
		got, ok := childEnv[name]
		if !ok {
			continue
		}
		// AWS_CONTAINER_CREDENTIALS_FULL_URI is the one name in the list
		// tetherd sets itself: the developer's value has to go and be
		// replaced by the loopback port this run serves, not merely be
		// absent.
		if name == "AWS_CONTAINER_CREDENTIALS_FULL_URI" {
			if !strings.HasPrefix(got, "http://127.0.0.1:") {
				t.Errorf("%s = %q, want the loopback port tetherd serves", name, got)
			}
			continue
		}
		t.Errorf("%s reached the child as %q: the child would sign with the developer's own identity while the status line claims the task role", name, got)
	}
	if !strings.Contains(out.String(), "local AWS credentials") {
		t.Errorf("removing them silently is not enough; the run must say so: %s", out.String())
	}
}

// breakableTransport dials the in-process agent and keeps every connection
// it hands out, so a test can cut the session while the child is still
// running. Nothing outside Run can arrange that otherwise: the session is
// built and owned inside it.
// The connections are kept per task, because with more than one session
// only one task's is what a rolling deploy takes away at a time.
type breakableTransport struct {
	addr  string
	mu    sync.Mutex
	conns map[string][]net.Conn
}

func (b *breakableTransport) Dial(ctx context.Context, t transport.Task) (net.Conn, error) {
	addr := b.addr
	if addr == "" {
		// A multi-task fixture: each task has its own agent, and the task
		// says which one (see agentTransport.Dial).
		addr = t.Addr
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.conns == nil {
		b.conns = map[string][]net.Conn{}
	}
	b.conns[t.ID] = append(b.conns[t.ID], c)
	b.mu.Unlock()
	return c, nil
}

// breakSession closes the transport under every session, which is what the
// CLI sees when the session-manager-plugin dies or the task goes away.
func (b *breakableTransport) breakSession() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, cs := range b.conns {
		for _, c := range cs {
			c.Close()
		}
	}
}

// breakTask closes the transport under one task's session only: what a
// deploy does to one task while the rest of the service keeps serving.
func (b *breakableTransport) breakTask(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns[id] {
		c.Close()
	}
}

// writeChildScript writes a /bin/sh child and returns its path. Every script
// here ignores SIGINT, because that is the case both tests below are about:
// cancel() only asks.
func writeChildScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "child.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntrap '' INT\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	waitFor(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, path+" to appear")
}

// waitFor polls cond until it holds, and fails naming what never happened.
// Polling, not sleeping: the thing being waited for (a file the child wrote,
// a listener that shut down) happens in another process or goroutine, and a
// fixed sleep is either a flake or a wasted second.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// runInBackground starts Run and returns a channel carrying its result, so a
// test can assert on *when* it returns and not only on what it returns.
type runResult struct {
	code int
	err  error
}

func runInBackground(opts RunOptions, out io.Writer, d Deps) <-chan runResult {
	done := make(chan runResult, 1)
	go func() {
		code, err := RunWithDeps(context.Background(), opts, out, d)
		done <- runResult{code: code, err: err}
	}()
	return done
}

// TestRunWaitsForTheChildWhenTheSessionIsLost pins the `<-waitErr` on the
// session-lost arm. Deleting it left the suite green, and what it does is
// not cosmetic: Run's defers pull the pf rules, the helper connection and
// every /etc/resolver file down as it returns, so returning while the child
// is still alive leaves that child running with its network half
// dismantled.
//
// The child ignores SIGINT and then blocks until this test releases it, so
// "did Run wait" is decided by a file, not by a sleep race.
func TestRunWaitsForTheChildWhenTheSessionIsLost(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	tr := &breakableTransport{addr: ag.addr}
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, tr: tr}

	dir := t.TempDir()
	started, release, finished := filepath.Join(dir, "started"), filepath.Join(dir, "release"), filepath.Join(dir, "finished")
	script := writeChildScript(t, "echo up > "+started+"\n"+
		"while [ ! -f "+release+" ]; do sleep 0.05; done\n"+
		"echo done > "+finished+"\n")

	opts := ssmOpts("/bin/sh", script) // NoNetwork, so Command is the child
	var out strings.Builder
	done := runInBackground(opts, &out, depsFor(p))

	waitForFile(t, started)
	tr.breakSession()

	// The grace period is the assertion: Run has seen the session die and
	// must still be waiting, because the child has not finished.
	select {
	case r := <-done:
		t.Fatalf("Run returned (code=%d err=%v) while the child was still running; its defers have now torn down pf and /etc/resolver under it\n%s", r.code, r.err, out.String())
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(finished); err == nil {
		t.Fatal("the child finished on its own; this test proves nothing")
	}

	if err := os.WriteFile(release, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.code != 1 || r.err == nil {
			t.Errorf("a lost session must be reported: code=%d err=%v", r.code, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned after the child finished")
	}
	// Run returned after the child was gone, not before it.
	if _, err := os.Stat(finished); err != nil {
		t.Errorf("Run returned before the child had finished: %v", err)
	}
	if !strings.Contains(out.String(), "agent session lost") {
		t.Errorf("the run must say why it stopped: %s", out.String())
	}
}

// TestRunKillsAChildThatIgnoresSIGINT pins child.WaitDelay. Deleting it left
// the suite green too, and a zero WaitDelay means "wait indefinitely": a
// child that ignores SIGINT - an interactive shell, anything with its own
// handler - then wedges `tetherd run` forever while it is still holding the
// pf rules and the /etc/resolver files, which is the one state a developer
// cannot get out of without knowing about tetherd-helper.
//
// The delay is shortened for the test; five seconds is far too long to wait
// for and is what production keeps. The child gives up by itself after six
// seconds so that a regression costs a slow test rather than an orphaned
// process, and the elapsed-time assertion is well inside that.
func TestRunKillsAChildThatIgnoresSIGINT(t *testing.T) {
	restore := childWaitDelay
	childWaitDelay = 300 * time.Millisecond
	t.Cleanup(func() { childWaitDelay = restore })

	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	tr := &breakableTransport{addr: ag.addr}
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, tr: tr}

	started := filepath.Join(t.TempDir(), "started")
	script := writeChildScript(t, "echo up > "+started+"\n"+
		"i=0\nwhile [ $i -lt 120 ]; do sleep 0.05; i=$((i+1)); done\n")

	opts := ssmOpts("/bin/sh", script)
	var out strings.Builder
	done := runInBackground(opts, &out, depsFor(p))

	waitForFile(t, started)
	tr.breakSession()

	start := time.Now()
	select {
	case r := <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("Run took %s to give up on a child that ignores SIGINT, want about the %s wait delay", elapsed, childWaitDelay)
		}
		if r.code != 1 || r.err == nil {
			t.Errorf("a lost session must be reported: code=%d err=%v", r.code, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run never returned: a child that ignores SIGINT is holding tetherd run, and with it the pf rules and /etc/resolver")
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

// TestRunExitsTwoForEveryBadCIDRValue pins the exit code of one class of
// .tetherd.yml mistake - a CIDR value that is simply wrong - across both
// transports and both ways of being wrong.
//
// They used to disagree. Measured: `network.remote_cidrs: [10.0.0.0/99]`
// exited 2, a `network.local_cidrs` typo exited 1, and a local_cidrs that
// removed the whole remote set exited 1 - so a CI wrapper reading 2 as "fix
// the invocation" and 1 as "retry" looped forever on the second. Nothing
// about retrying any of them can change the answer, so all of them are 2.
//
// Each case is caught before the agent is dialed, which is why a bogus
// agent address is enough for the direct ones.
func TestRunExitsTwoForEveryBadCIDRValue(t *testing.T) {
	ssmCase := func(mod func(*RunOptions)) RunOptions {
		o := ssmOpts("true")
		o.NoNetwork = false
		o.ExecPath = "/usr/bin/true" // exists, so the pre-flight stat passes
		mod(&o)
		return o
	}
	directCase := func(mod func(*RunOptions)) RunOptions {
		o := RunOptions{
			Transport: "direct", AgentAddr: "127.0.0.1:1", TargetEnv: "dev", User: "tester", NoIncoming: true,
			RemoteCIDRs: []string{"10.0.0.0/16"}, ExecPath: "/usr/bin/true", Command: []string{"true"},
		}
		mod(&o)
		return o
	}
	cases := []struct {
		name string
		opts RunOptions
		want string // a token the error must name, so the operator knows which key
	}{
		// The reference behaviour the other rows are made to match: this
		// one always exited 2, because it is parsed before the switch on
		// opts.Transport.
		{"ssm remote_cidrs", ssmCase(func(o *RunOptions) { o.RemoteCIDRs = []string{"10.0.0.0/99"} }), "10.0.0.0/99"},
		{"ssm local_cidrs typo", ssmCase(func(o *RunOptions) { o.LocalCIDRs = []string{"not-a-cidr"} }), "network.local_cidrs"},
		{"ssm local_cidrs excludes everything", ssmCase(func(o *RunOptions) { o.LocalCIDRs = []string{"10.0.0.0/8"} }), "local_cidrs"},
		{"direct local_cidrs typo", directCase(func(o *RunOptions) { o.LocalCIDRs = []string{"not-a-cidr"} }), "network.local_cidrs"},
		{"direct local_cidrs excludes everything", directCase(func(o *RunOptions) { o.LocalCIDRs = []string{"10.0.0.0/8"} }), "local_cidrs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &fakeProvider{
				region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
				vpc: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
			}
			d := depsFor(p)
			d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
			d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }
			code, err := RunWithDeps(context.Background(), c.opts, io.Discard, d)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (a value that is wrong, not an operational failure): err=%v", code, err)
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to name %q", err, c.want)
			}
		})
	}

	// The other arm of the same mapping: an AWS call that failed is
	// operational and stays 1. Without this, "always return 2" would pass
	// every case above.
	t.Run("an AWS failure is still 1", func(t *testing.T) {
		p := &fakeProvider{
			region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
			vpcErr: errAlwaysFails,
		}
		d := depsFor(p)
		d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
		d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }
		opts := ssmCase(func(*RunOptions) {})
		code, err := RunWithDeps(context.Background(), opts, io.Discard, d)
		if code != 1 || err == nil {
			t.Fatalf("code = %d err = %v, want code 1: a failed AWS call is worth retrying", code, err)
		}
	})
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
		Transport: "direct", AgentAddr: ag.addr, TargetEnv: "dev", User: "tester", NoIncoming: true,
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
		Transport: "direct", AgentAddr: ag.addr, TargetEnv: "dev", User: "tester", NoIncoming: true,
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
//
// The exit code this produces is pinned by
// TestRunExitsTwoForEveryBadCIDRValue, alongside every other spelling of the
// same mistake.
func TestRunDirectRejectsLocalCIDRsExcludingEverything(t *testing.T) {
	opts := RunOptions{
		Transport: "direct", AgentAddr: "127.0.0.1:1", TargetEnv: "dev", User: "tester", NoIncoming: true,
		RemoteCIDRs: []string{"10.0.0.0/16"}, LocalCIDRs: []string{"10.0.0.0/8"},
		ExecPath: "/usr/bin/true", Command: []string{"true"},
	}
	code, err := RunWithDeps(context.Background(), opts, io.Discard, Deps{})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "network.local_cidrs") {
		t.Fatalf("code=%d err=%v, want a failure naming network.local_cidrs", code, err)
	}
}

// TestRunGivesTheChildTheTaskRoleWithoutCapturingTheEndpoint replaces the
// "the task advertises a role but 169.254.170.0/24 is not captured" warning
// this used to assert. Nothing has to be captured any more: the endpoint is
// served on loopback and the child is pointed at it by environment
// variable, so a remote set the operator chose by hand (--transport direct
// with one --remote-cidr) still gets the task role.
func TestRunGivesTheChildTheTaskRoleWithoutCapturingTheEndpoint(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil,
		endpointReturning(t, http.StatusServiceUnavailable, "no credentials for you"))
	cap := newFakeCapturer()
	d := Deps{
		DialHelper:  func(string) (HelperClient, error) { return &fakeHelperClient{}, nil },
		NewCapturer: func(HelperClient, func(string, ...any)) Capturer { return cap },
	}

	dir := t.TempDir()
	dump := filepath.Join(dir, "env-dump")
	script := filepath.Join(dir, "fake-exec.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+dump+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := RunOptions{
		Transport: "direct", AgentAddr: ag.addr, TargetEnv: "dev", User: "tester", NoIncoming: true,
		RemoteCIDRs: []string{"10.0.0.0/16"}, ExecPath: script, Command: []string{"true"},
	}
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if addrIn(cap.spec.RemoteCIDRs, "169.254.170.2") {
		t.Fatalf("the premise is wrong: %v already covers the endpoint", cap.spec.RemoteCIDRs)
	}
	childEnv := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(readFile(t, dump), "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			childEnv[k] = v
		}
	}
	if !strings.HasPrefix(childEnv["AWS_CONTAINER_CREDENTIALS_FULL_URI"], "http://127.0.0.1:") {
		t.Fatalf("the child must still get the task role: %v", childEnv["AWS_CONTAINER_CREDENTIALS_FULL_URI"])
	}
	// Transparent mode is the only place the explicit exclusion of the
	// relative URI is load-bearing: env.Options.DropAWSContainer (which
	// --no-network sets) happens to drop the same name, so every
	// --no-network test would pass with the exclusion deleted - measured.
	// The workload that matters most, `tetherd run -- aws s3 ls` in
	// transparent mode, is this one, and botocore reads the variable's
	// presence.
	if v, ok := childEnv["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]; ok {
		t.Fatalf("the relative URI reached the child as %q in transparent mode; botocore would fetch http://169.254.170.2", v)
	}
	if strings.Contains(out.String(), "not captured") {
		t.Errorf("nothing has to be captured for the task role any more: %s", out.String())
	}
}

// TestRunPointsTheChildAtTheLoopbackCredentialProxy: the task's environment
// names 169.254.170.2, which only exists inside the task. The child must be
// handed the loopback port tetherd serves instead, with the relative form
// cleared so nothing falls back to the address nothing routes.
//
// The endpoint answers 503 on purpose (as in
// TestRunVerifiesTheTaskRoleThroughTheAgent): serving usable credentials
// would send Run's own probe on to sts.<region>.amazonaws.com, and leaving
// the endpoint unserved would make the agent dial the real 169.254.170.2.
func TestRunPointsTheChildAtTheLoopbackCredentialProxy(t *testing.T) {
	dial := endpointReturning(t, http.StatusServiceUnavailable, "no credentials for you")
	ag := startAgentFor(t, map[string]string{
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/abc",
		"ECS_CONTAINER_METADATA_URI_V4":          "http://169.254.170.2/v4/task",
	}, nil, dial)
	p := &fakeProvider{region: "ap-northeast-1", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}
	// The child's exit status carries the assertion: a loopback FULL_URI,
	// no mention of the endpoint's address, and the relative form cleared.
	//
	// The relative form is checked for *presence*, not emptiness:
	// `[ -z "$X" ]` passes both for a variable that is gone and for one set
	// to "", and the difference is the whole bug - botocore's
	// ContainerProvider tests `ENV_VAR in self._environ` and then fetches
	// http://169.254.170.2 + "", so an empty variable takes `aws s3 ls`
	// to the address nothing routes while the developer's own credentials
	// have already been stripped.
	opts := ssmOpts("sh", "-c", `case "$AWS_CONTAINER_CREDENTIALS_FULL_URI" in http://127.0.0.1:*/v2/credentials/abc) ;; *) exit 11;; esac
		if [ "${AWS_CONTAINER_CREDENTIALS_RELATIVE_URI+set}" = set ]; then exit 12; fi
		case "$ECS_CONTAINER_METADATA_URI_V4" in *169.254*) exit 13;; esac
		case "$ECS_CONTAINER_METADATA_URI_V4" in http://127.0.0.1:*/v4/task) ;; *) exit 14;; esac
		exit 0`)
	var out strings.Builder
	code, err := RunWithDeps(context.Background(), opts, &out, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("the child rejected its environment: code=%d err=%v log=%s", code, err, out.String())
	}
	if l := iamResultLine(t, out.String()); !strings.Contains(l, "127.0.0.1:") {
		t.Errorf("the iam line must say where the child was pointed, got %q", l)
	}
}

// TestRunRemovesTheRelativeURIEvenFromTheDevelopersOwnEnvironment: the
// variable has to be gone from the child whatever its source. It is not in
// env.LocalAWSCredentialVars (that list is about the developer's own
// identity), so a copy exported in the shell would otherwise survive the
// task's copy being excluded - and botocore would read it.
func TestRunRemovesTheRelativeURIEvenFromTheDevelopersOwnEnvironment(t *testing.T) {
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "/v2/credentials/mine")
	ag := startAgentFor(t, map[string]string{
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/abc",
	}, nil, endpointReturning(t, http.StatusServiceUnavailable, "no credentials for you"))
	p := &fakeProvider{region: "ap-northeast-1", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}
	opts := ssmOpts("sh", "-c", `if [ "${AWS_CONTAINER_CREDENTIALS_RELATIVE_URI+set}" = set ]; then exit 12; fi
		exit 0`)
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, depsFor(p)); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
}

// TestRunProbesTheTaskRoleThroughTheLoopbackProxy pins which path the ✓ iam
// probe takes. It has to be the child's own - loopback, then the session -
// so that a broken listener is found by tetherd before the child's first
// SDK call, and so the "via 127.0.0.1:<port>" the line prints is not a
// claim about a path nothing tried.
//
// The session's dial fails on purpose, and the two paths fail differently: a
// probe through the proxy gets the proxy's own 502 and leaves the
// ErrorHandler's "through the agent" line behind, while a probe that dialed
// the session directly would report the dial error itself and log nothing
// from credproxy.go.
func TestRunProbesTheTaskRoleThroughTheLoopbackProxy(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil,
		func(context.Context, string) (net.Conn, error) {
			return nil, errors.New("the task has no route to that address")
		})
	p := &fakeProvider{region: "ap-northeast-1", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}
	var out strings.Builder
	code, err := RunWithDeps(context.Background(), ssmOpts("true"), &out, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("a failed probe must not fail the run: code=%d err=%v log=%s", code, err, out.String())
	}
	if !strings.Contains(out.String(), "through the agent") {
		t.Errorf("the probe must travel the loopback proxy (its ErrorHandler names the transport): %s", out.String())
	}
	if !strings.Contains(out.String(), "HTTP 502") {
		t.Errorf("and the probe must see the proxy's answer, not the raw dial error: %s", out.String())
	}
}

// TestRunServesTheTaskEndpointToTheChildOverLoopback is the other half: the
// value in the child's environment has to be a live endpoint, not just a
// well-formed URL. The child parks after dumping its environment so this
// test can use that exact URL while the run is still up, and what comes back
// has to be the task's own answer relayed through the session - a 503 from
// the endpoint, not a 502 from the proxy.
//
// A unit test cannot cover this: the listener, the session's DialTCP and the
// environment the child actually receives are wired together inside Run.
func TestRunServesTheTaskEndpointToTheChildOverLoopback(t *testing.T) {
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/credentials/") {
			http.Error(w, "no credentials for you", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"Cluster":"relayed","Path":%q}`, r.URL.Path)
	})}
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(cl)

	ag := startAgentFor(t, map[string]string{
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/abc",
		"ECS_CONTAINER_METADATA_URI_V4":          "http://169.254.170.2/v4/task",
	}, nil, func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", cl.Addr().String())
	})
	p := &fakeProvider{region: "ap-northeast-1", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}

	dir := t.TempDir()
	dump, release := filepath.Join(dir, "env-dump"), filepath.Join(dir, "release")
	script := filepath.Join(dir, "child.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+dump+".tmp\nmv "+dump+".tmp "+dump+
		"\nwhile [ ! -f "+release+" ]; do sleep 0.02; done\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	done := runInBackground(ssmOpts("/bin/sh", script), &out, depsFor(p))
	t.Cleanup(func() { os.WriteFile(release, []byte("go"), 0o644) })
	waitForFile(t, dump)
	childEnv := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(readFile(t, dump), "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			childEnv[k] = v
		}
	}

	full := childEnv["AWS_CONTAINER_CREDENTIALS_FULL_URI"]
	if !strings.HasPrefix(full, "http://127.0.0.1:") {
		t.Fatalf("AWS_CONTAINER_CREDENTIALS_FULL_URI = %q: %s", full, out.String())
	}
	// `env` prints an empty variable as "NAME=", so the key being absent
	// from this map is exactly the property botocore needs: not empty, gone.
	if v, ok := childEnv["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]; ok {
		t.Fatalf("the relative URI reached the child as %q; botocore branches on its presence and would fetch http://169.254.170.2", v)
	}
	// The credential path: the task's own 503 has to arrive, which proves
	// the request travelled endpoint-ward rather than failing at the proxy.
	resp, err := http.Get(full)
	if err != nil {
		t.Fatalf("the child's credential URL is not served: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "no credentials for you") {
		t.Fatalf("status=%d body=%q, want the task's own answer relayed", resp.StatusCode, body)
	}
	// The metadata path goes through the same port, and its path survives.
	resp, err = http.Get(childEnv["ECS_CONTAINER_METADATA_URI_V4"])
	if err != nil {
		t.Fatalf("the child's metadata URL is not served: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"Path":"/v4/task"`) {
		t.Fatalf("status=%d body=%q, want the metadata path relayed intact", resp.StatusCode, body)
	}

	if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.code != 0 {
		t.Fatalf("code=%d err=%v log=%s", r.code, r.err, out.String())
	}
	// And the port is gone once the run is over: it is the run's lifetime,
	// not the machine's.
	waitFor(t, func() bool {
		c, err := net.DialTimeout("tcp", strings.TrimPrefix(full[:strings.LastIndex(full, "/v2")], "http://"), 200*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		return false
	}, "the credential proxy to stop listening after the run")
}

// endpointReturning is a fake 169.254.170.2 that answers every request with
// the same status, for tests that only need the credential probe to fail
// fast and locally: unserved, the agent would dial the real link-local
// address and wait out its 10s dial timeout.
func endpointReturning(t *testing.T, status int, body string) func(context.Context, string) (net.Conn, error) {
	t.Helper()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, status)
	})}
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(cl)
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", cl.Addr().String())
	}
}

// credentialEndpoint is a fake 169.254.170.2 that serves usable-looking
// container credentials, for the tests that need Run's probe to get past
// FetchContainerCredentials and on to the STS leg.
func credentialEndpoint(t *testing.T) func(context.Context, string) (net.Conn, error) {
	t.Helper()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"AccessKeyId":"AKIAEXAMPLE","SecretAccessKey":"s3cret","Token":"tok","Expiration":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})}
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(cl)
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", cl.Addr().String())
	}
}

// TestRunSeparatesTheCredentialLegFromTheSTSLeg: the ✓ iam check has two
// halves and only one of them is about the child. The credential fetch is
// hermetic (loopback → the session → the task) and decides whether the child
// has an AWS identity at all; sts:GetCallerIdentity leaves the laptop's own
// network for sts.<region>.amazonaws.com and only names the role.
//
// Collapsing them - which is what this task's first round did - means a
// developer offline, behind a proxy, or on --no-network with no
// connectivity reads "the task role could not be verified; the child may
// have no AWS identity" about a child that can sign perfectly well.
func TestRunSeparatesTheCredentialLegFromTheSTSLeg(t *testing.T) {
	// The developer's own credentials are in the environment, so the
	// "removed so the task role applies" line is reached either way.
	t.Setenv("AWS_PROFILE", "mine")

	run := func(t *testing.T, identity func(context.Context, aws.Credentials, string) (string, error), dial func(context.Context, string) (net.Conn, error)) string {
		t.Helper()
		ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil, dial)
		p := &fakeProvider{region: "ap-northeast-1", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}
		d := depsFor(p)
		d.CallerIdentity = identity
		var out strings.Builder
		if code, err := RunWithDeps(context.Background(), ssmOpts("true"), &out, d); err != nil || code != 0 {
			t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
		}
		return out.String()
	}

	t.Run("both legs succeed", func(t *testing.T) {
		out := run(t, func(context.Context, aws.Credentials, string) (string, error) {
			return "arn:aws:sts::1:assumed-role/dev-task/abc", nil
		}, credentialEndpoint(t))
		l := iamResultLine(t, out)
		if !strings.Contains(l, "✓ iam") || !strings.Contains(l, "assumed-role/dev-task") {
			t.Errorf("want the ARN on a ✓ iam line, got %q", l)
		}
		// The successful line has to name the loopback port too: it is the
		// line a developer reads to know where their child is pointed.
		if !strings.Contains(l, "127.0.0.1:") {
			t.Errorf("the ✓ iam line must name the loopback port, got %q", l)
		}
		if !strings.Contains(out, "✓ env      local AWS credentials") {
			t.Errorf("the strip must be reported as good news: %s", out)
		}
	})

	t.Run("the credentials arrive but STS cannot be reached", func(t *testing.T) {
		out := run(t, func(context.Context, aws.Credentials, string) (string, error) {
			return "", errors.New("dial tcp: lookup sts.ap-northeast-1.amazonaws.com: no such host")
		}, credentialEndpoint(t))
		line := iamResultLine(t, out)
		if !strings.Contains(line, "could not confirm") || !strings.Contains(line, "no such host") {
			t.Errorf("the line must say STS is what failed, got %q", line)
		}
		if strings.Contains(line, "127.0.0.1") == false {
			t.Errorf("and still say where the credentials came from, got %q", line)
		}
		// The child can sign: the credentials reached tetherd over the same
		// path the child uses. Warning that it "may have no AWS identity"
		// would be a false alarm.
		if !strings.Contains(out, "✓ env      local AWS credentials") {
			t.Errorf("STS being unreachable must not be reported as the child having no identity: %s", out)
		}
	})

	t.Run("the credentials do not arrive", func(t *testing.T) {
		out := run(t, func(context.Context, aws.Credentials, string) (string, error) {
			t.Error("sts must not be asked about credentials that never arrived")
			return "", nil
		}, endpointReturning(t, http.StatusServiceUnavailable, "no credentials for you"))
		if l := iamResultLine(t, out); !strings.Contains(l, "HTTP 503") {
			t.Errorf("want the endpoint's own failure, got %q", l)
		}
		if !strings.Contains(out, "⚠ env      local AWS credentials") {
			t.Errorf("this is the case where the child may have no identity, and it must say so: %s", out)
		}
	})
}

// TestRunWarnsAboutATaskValueItCannotRewrite: a value that names
// 169.254.170.2 in a shape tetherd does not rewrite (https, a stray space,
// an application variable) reaches the child unroutable. Passing it through
// is right; saying nothing is not - until v0.3a it resolved anyway, because
// 169.254.170.0/24 was captured unconditionally.
func TestRunWarnsAboutATaskValueItCannotRewrite(t *testing.T) {
	taskEnv := map[string]string{
		"APP_METADATA_URL":              "https://169.254.170.2/v4/app",
		"ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2/v4/task",
	}
	ag := startAgentFor(t, taskEnv, nil, nil)
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

	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if !strings.Contains(out.String(), "APP_METADATA_URL") || !strings.Contains(out.String(), "pin_credential_route") {
		t.Errorf("the warning must name the variable and the way to make it work: %s", out.String())
	}
	// The rewritten one is not stuck and must not be named.
	if strings.Contains(out.String(), "ECS_CONTAINER_METADATA_URI_V4") {
		t.Errorf("a rewritten variable must not be warned about: %s", out.String())
	}

	// With the route pin on, the address *is* captured, so the value
	// resolves and there is nothing to warn about.
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }
	opts.PinCredentialRoute = true
	var pinned strings.Builder
	ag.waitDetached(t)
	if code, err := RunWithDeps(context.Background(), opts, &pinned, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, pinned.String())
	}
	if strings.Contains(pinned.String(), "APP_METADATA_URL") {
		t.Errorf("with the endpoint captured the value resolves; warning about it is noise: %s", pinned.String())
	}
}

// TestRunKeepsTheDevelopersIdentityWhenTheTaskHasNoRole: the hardening that
// hides ~/.aws and strips the developer's own AWS variables only makes sense
// when there is a task role to replace them with. A task with no role (or
// with metadata but no credentials) must leave the child its own identity -
// hiding both would leave it with no AWS identity at all.
func TestRunKeepsTheDevelopersIdentityWhenTheTaskHasNoRole(t *testing.T) {
	for _, c := range []struct {
		name    string
		taskEnv map[string]string
	}{
		{"no role at all", map[string]string{"PORT": "8080"}},
		{"metadata but no credentials", map[string]string{"ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2/v4/task"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ag := startAgentFor(t, c.taskEnv, nil, nil)
			p := &fakeProvider{region: "ap-northeast-1", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}
			t.Setenv("AWS_PROFILE", "mine")
			t.Setenv("AWS_CONFIG_FILE", "/Users/dev/.aws/config")
			opts := ssmOpts("sh", "-c", `[ "$AWS_PROFILE" = mine ] || exit 11
				[ "$AWS_CONFIG_FILE" = /Users/dev/.aws/config ] || exit 12`)
			var out strings.Builder
			code, err := RunWithDeps(context.Background(), opts, &out, depsFor(p))
			if err != nil || code != 0 {
				t.Fatalf("the child lost its own AWS identity: code=%d err=%v log=%s", code, err, out.String())
			}
			if strings.Contains(out.String(), "iam") {
				t.Errorf("there is no task role to report: %s", out.String())
			}
		})
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
	// routes is what RouteSet was asked to pin, routeCleared whether
	// RouteClear ran, and routeSetErr what RouteSet returns (a BusyError,
	// for the second `tetherd run` on a machine).
	routes       []netip.Addr
	routeCleared bool
	routeSetErr  error
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
func (f *fakeHelperClient) RouteSet(hosts []netip.Addr) error {
	if f.routeSetErr != nil {
		return f.routeSetErr
	}
	f.routes = append(f.routes, hosts...)
	return nil
}
func (f *fakeHelperClient) RouteClear() error {
	f.routeCleared = true
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
	// startErr is what Start returns: pf.apply is where a second `tetherd
	// run` on the machine finds out it is busy.
	startErr error
}

func newFakeCapturer() *fakeCapturer {
	return &fakeCapturer{accept: make(chan capture.Conn)}
}

func (f *fakeCapturer) Start(_ context.Context, spec capture.Spec) error {
	f.spec = spec
	f.started = true
	return f.startErr
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

// TestRunDoesNotPinTheRouteUnlessAsked: the child no longer dials
// 169.254.170.2 at all - it is pointed at a loopback port instead - so the
// machine-wide host route pf needed (see internal/helper/route.go) is an
// opt-in escape hatch for a tool inside the child's tree that hardcodes the
// address, not something every run installs. While it is pinned, every
// process on the Mac reaches the dev task's credentials.
func TestRunDoesNotPinTheRouteUnlessAsked(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/abc"}, nil,
		endpointReturning(t, http.StatusServiceUnavailable, "no credentials for you"))
	p := &fakeProvider{
		region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	hc := &fakeHelperClient{}
	cap := newFakeCapturer()
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return hc, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return cap }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = "/usr/bin/true"
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if len(hc.routes) != 0 {
		t.Fatalf("routes = %v: the machine-wide pin is opt-in now", hc.routes)
	}
	// And with no pin there is no reason to hold 169.254.170.0/24 in the
	// captured set either: that floor existed only to keep the pinned route
	// usable (v0.2b), and capturing the address costs every local ECS
	// endpoint emulator on the machine its own address for the session.
	if addrIn(cap.spec.RemoteCIDRs, "169.254.170.2") {
		t.Fatalf("the endpoint must not be captured by default: %v", cap.spec.RemoteCIDRs)
	}

	// With the opt-in set it is pinned - the escape hatch for a tool inside
	// the child's tree that hardcodes the address.
	hc2, cap2 := &fakeHelperClient{}, newFakeCapturer()
	d.DialHelper = func(string) (HelperClient, error) { return hc2, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return cap2 }
	opts.PinCredentialRoute = true
	ag.waitDetached(t)
	if code, err := RunWithDeps(context.Background(), opts, io.Discard, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(hc2.routes) != 1 || hc2.routes[0].String() != "169.254.170.2" {
		t.Fatalf("routes = %v, want the endpoint pinned when asked", hc2.routes)
	}
	if !hc2.routeCleared {
		t.Error("the route must be cleared when the run ends: it outlives the session otherwise")
	}
}

func TestRunDoesNotPinTheRouteWithoutCapture(t *testing.T) {
	// --no-network never touches the helper, so it pins no route even when
	// asked: there is no pf rule for the route to feed.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}
	hc := &fakeHelperClient{}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return hc, nil }
	opts := ssmOpts("true")
	opts.PinCredentialRoute = true
	if code, err := RunWithDeps(context.Background(), opts, io.Discard, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(hc.routes) != 0 {
		t.Fatalf("routes = %v, want none with --no-network", hc.routes)
	}
}

// TestRunCapturesTheEndpointWheneverItPinsTheRoute is the boundary the pin
// has to respect: the route is only correct while pf's `rdr pass on lo0`
// covers that address - pinning it otherwise sends the credential endpoint
// to lo0, where no rdr rule picks it up and nothing answers. Under
// --transport direct the operator chooses the remote set by hand and
// 169.254.170.0/24 is not in it, so asking for the pin has to add it: the
// floor and the pin are one decision, not two that can disagree.
func TestRunCapturesTheEndpointWheneverItPinsTheRoute(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	hc := &fakeHelperClient{}
	cap := newFakeCapturer()
	d := Deps{
		DialHelper:  func(string) (HelperClient, error) { return hc, nil },
		NewCapturer: func(HelperClient, func(string, ...any)) Capturer { return cap },
	}
	opts := RunOptions{
		Transport: "direct", AgentAddr: ag.addr, TargetEnv: "dev", User: "tester", NoIncoming: true,
		RemoteCIDRs: []string{"10.9.0.0/16"}, PinCredentialRoute: true,
		ExecPath: "/usr/bin/true", Command: []string{"true"},
	}
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if len(hc.routes) != 1 || hc.routes[0] != ecsprov.TaskRoleAddr {
		t.Fatalf("routes = %v, want the credential endpoint pinned when asked", hc.routes)
	}
	if !addrIn(cap.spec.RemoteCIDRs, "169.254.170.2") {
		t.Fatalf("a pinned route with no rdr rule behind it is a dead end: %v", cap.spec.RemoteCIDRs)
	}
	// Without the opt-in, direct captures exactly what the operator asked
	// for - so the assertion above is about the pin, not about direct
	// always adding the range.
	hc2, cap2 := &fakeHelperClient{}, newFakeCapturer()
	d.DialHelper = func(string) (HelperClient, error) { return hc2, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return cap2 }
	opts.PinCredentialRoute = false
	ag.waitDetached(t)
	if code, err := RunWithDeps(context.Background(), opts, io.Discard, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if addrIn(cap2.spec.RemoteCIDRs, "169.254.170.2") || len(hc2.routes) != 0 {
		t.Fatalf("captured %v, routes %v: neither belongs here", cap2.spec.RemoteCIDRs, hc2.routes)
	}
}

// TestRunExplainsABusyHelperFromRouteSet: with network.pin_credential_route
// set, route.set is the first call that claims the machine-wide session, so
// it is where a second `tetherd run` finds out. That must still be the
// explanation a developer can act on, not a bare wrapped error.
func TestRunExplainsABusyHelperFromRouteSet(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{
		region: "ap-northeast-1", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	hc := &fakeHelperClient{routeSetErr: &helper.BusyError{PID: 4242, Since: time.Now()}}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return hc, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = "/usr/bin/true"
	opts.PinCredentialRoute = true
	code, err := RunWithDeps(context.Background(), opts, io.Discard, d)
	if code != 1 || err == nil {
		t.Fatalf("code=%d err=%v, want a failure", code, err)
	}
	var busy *helper.BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want the BusyError to survive", err)
	}
	if !strings.Contains(err.Error(), "Stop the other") {
		t.Errorf("err = %q, want the same next step pf.apply gives", err)
	}
}

// TestRunExplainsABusyHelperFromPfApply is the same explanation from the
// call that discovers it on a default run: with the route pin opt-in,
// pf.apply is the first thing to claim the machine-wide session.
func TestRunExplainsABusyHelperFromPfApply(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{
		region: "ap-northeast-1", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	cap := newFakeCapturer()
	cap.startErr = &helper.BusyError{PID: 4242, Since: time.Now()}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return cap }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = "/usr/bin/true"
	code, err := RunWithDeps(context.Background(), opts, io.Discard, d)
	if code != 1 || err == nil {
		t.Fatalf("code=%d err=%v, want a failure", code, err)
	}
	var busy *helper.BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want the BusyError to survive", err)
	}
	if !strings.Contains(err.Error(), "Stop the other") {
		t.Errorf("err = %q, want the next step a developer can act on", err)
	}
}

// --- the steal receiver, over a real session -------------------------------
//
// The agent end of steal (its L7 proxy) is Task 4's; these tests stand in
// for it with the one thing it will do that matters here - open a stream
// toward this CLI, write the http header on it and speak HTTP/1.1 - so that
// what is exercised is the real session, the real accept loop, the real
// OnHTTP contract and the real receiver.

// stealAgent is a session.Handler that records the hello it was sent and
// keeps the Opener it was handed, which is how the agent pushes a stolen
// request at a CLI.
type stealAgent struct {
	addr string

	mu       sync.Mutex
	hello    proto.Hello
	open     session.Opener
	attached chan struct{}
	once     sync.Once
}

func startStealAgent(t *testing.T) *stealAgent {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &stealAgent{addr: ln.Addr().String(), attached: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go session.Serve(ctx, c, a, session.ServeOptions{})
		}
	}()
	return a
}

func (a *stealAgent) Hello(h proto.Hello, _ string, open session.Opener) (proto.Welcome, *proto.Error) {
	a.mu.Lock()
	a.hello, a.open = h, open
	a.mu.Unlock()
	a.once.Do(func() { close(a.attached) })
	return proto.Welcome{Version: proto.Version, TaskARN: "arn:test", Env: "dev", AppEnv: map[string]string{"A": "1"}}, nil
}

func (a *stealAgent) Dial(context.Context, string) (net.Conn, error) {
	return nil, errors.New("this agent does not dial")
}

func (a *stealAgent) Resolve(context.Context, string) ([]string, int, error) {
	return nil, 0, errors.New("this agent does not resolve")
}

func (a *stealAgent) Closed() {}

// waitAttached blocks until a CLI has completed the handshake and returns
// the hello it sent.
func (a *stealAgent) waitAttached(t *testing.T) proto.Hello {
	t.Helper()
	select {
	case <-a.attached:
	case <-time.After(10 * time.Second):
		t.Fatal("no CLI attached")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hello
}

// steal opens an http stream at the attached CLI and returns the response,
// or the proto error the CLI answered with instead.
func (a *stealAgent) steal(t *testing.T, req *http.Request) (*http.Response, *proto.Error) {
	t.Helper()
	a.mu.Lock()
	open := a.open
	a.mu.Unlock()
	if open == nil {
		t.Fatal("the agent has no opener: no session")
	}
	s, err := open.OpenStream()
	if err != nil {
		t.Fatalf("open a stream toward the CLI: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: req.Header.Get("X-Dev-User")}); err != nil {
		t.Fatal(err)
	}
	if err := req.Write(s); err != nil {
		t.Fatal(err)
	}
	s.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(s)
	// A CLI that is not taking requests answers with the proto error the
	// session layer sends, not with an HTTP response. The first byte tells
	// them apart: "{" is JSON Lines, "H" is HTTP/1.1.
	first, err := br.Peek(1)
	if err != nil {
		t.Fatalf("the CLI answered nothing: %v", err)
	}
	if first[0] == '{' {
		line, err := br.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var e proto.Error
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("unparseable refusal %q: %v", line, err)
		}
		return nil, &e
	}
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("reading the response off the stream: %v", err)
	}
	return resp, nil
}

// safeLog is Run's stderr for the tests below: Run writes to it from its own
// goroutine while the test reads it, which a strings.Builder does not allow.
type safeLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *safeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *safeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func devRequest(method, path, user, token string) *http.Request {
	r := httptest.NewRequest(method, "http://api.example.com"+path, nil)
	r.Header.Set("X-Dev-User", user)
	r.Header.Set("X-Dev-Token", token)
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	return r
}

func TestRunTakesAStolenRequestToTheDevelopersProcess(t *testing.T) {
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "LOCAL %s xff=%s", r.URL.Path, r.Header.Get("X-Forwarded-For"))
	}))
	ag := startStealAgent(t)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}

	opts := ssmOpts("sleep", "30")
	opts.NoIncoming = false
	opts.LocalPort = port
	opts.User = "shota"
	opts.Token = "tok-shota"
	var out safeLog
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		code, _ := RunWithDeps(ctx, opts, &out, depsFor(p))
		done <- code
	}()

	// What the agent matches against has to arrive in the hello, all of it:
	// the user name, the token, and both header names (config applies no
	// defaults, and the agent reads an empty name as "matches nothing").
	hello := ag.waitAttached(t)
	if hello.User != "shota" || hello.Token != "tok-shota" {
		t.Errorf("hello = user %q token %q, want both carried", hello.User, hello.Token)
	}
	if hello.Incoming != (proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}) {
		t.Errorf("hello.Incoming = %+v", hello.Incoming)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "✓ steal") }, "the steal line")
	if l := out.String(); !strings.Contains(l, "X-Dev-User: shota") || !strings.Contains(l, fmt.Sprintf("localhost:%d", port)) {
		t.Errorf("the steal line must say what is matched and where it goes: %s", l)
	}

	resp, perr := ag.steal(t, devRequest("GET", "/api/orders", "shota", "tok-shota"))
	if perr != nil {
		t.Fatalf("the CLI refused the stream: %+v", perr)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "LOCAL /api/orders") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "xff=203.0.113.5") {
		t.Errorf("the caller must reach the developer's process: %s", b)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "/api/orders  200") }, "the request log line")
	// The token must not be anywhere in what the developer's terminal saw.
	if strings.Contains(out.String(), "tok-shota") {
		t.Errorf("the token leaked into the log: %s", out.String())
	}
	cancel()
	<-done
}

// Nothing in the repository configures steal: the agent must still be told
// this session can take a request, on the default port and with the default
// header names, because --no-incoming is the only way to turn steal off.
func TestRunStealsOnTheDefaultPortWhenNothingConfiguresIt(t *testing.T) {
	ag := startStealAgent(t)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}

	opts := ssmOpts("sleep", "30")
	opts.NoIncoming = false
	opts.LocalPort = 0
	opts.User = "shota"
	opts.Token = "tok-shota"
	var out safeLog
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		code, _ := RunWithDeps(ctx, opts, &out, depsFor(p))
		done <- code
	}()

	if in := ag.waitAttached(t).Incoming; !in.Enabled {
		t.Fatalf("incoming = %+v, want it enabled with no configuration at all", in)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "✓ steal") }, "the steal line")
	if l := out.String(); !strings.Contains(l, fmt.Sprintf("localhost:%d", DefaultLocalPort)) {
		t.Errorf("the steal line must name the default port: %s", l)
	}
	cancel()
	<-done
}

func TestRunWithNoIncomingTakesNothing(t *testing.T) {
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the developer's process must not be reached at all: %s", r.URL.Path)
	}))
	ag := startStealAgent(t)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}

	opts := ssmOpts("sleep", "30")
	opts.NoIncoming = true
	opts.LocalPort = port
	opts.User = "shota"
	opts.Token = "tok-shota"
	var out safeLog
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		code, _ := RunWithDeps(ctx, opts, &out, depsFor(p))
		done <- code
	}()

	hello := ag.waitAttached(t)
	if hello.Incoming.Enabled {
		t.Errorf("--no-incoming must not advertise incoming requests: %+v", hello.Incoming)
	}
	if hello.Token != "" {
		t.Errorf("a session with nothing to match has no use for the token on the wire: %q", hello.Token)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "✓ env") }, "the session to come up")
	if strings.Contains(out.String(), "✓ steal") {
		t.Errorf("--no-incoming must not print a steal line: %s", out.String())
	}

	// The backstop: an agent that steals anyway must be told why, so its
	// proxy passes the request to the application instead of waiting on a
	// stream nobody reads.
	resp, perr := ag.steal(t, devRequest("GET", "/api/orders", "shota", "tok-shota"))
	if perr == nil {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("the stream must be refused, got an HTTP response: %s", b)
	}
	if perr.Code != proto.CodeNoIncoming {
		t.Errorf("refusal code = %q, want %q so the proxy knows to serve from the app", perr.Code, proto.CodeNoIncoming)
	}
	cancel()
	<-done
}

// A session that advertises incoming requests with no token is one the
// agent can never match: it would attach, print a green line, and leave
// every request with the application forever. That has to be a startup
// failure, before any AWS call and before the agent is dialled at all.
func TestRunRefusesToTakeRequestsWithNoToken(t *testing.T) {
	ag := startStealAgent(t)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}

	opts := ssmOpts("true")
	opts.NoIncoming = false
	opts.LocalPort = 3000
	opts.User = "shota"
	opts.Token = ""
	var out safeLog
	code, err := RunWithDeps(context.Background(), opts, &out, depsFor(p))
	if code != 2 || err == nil {
		t.Fatalf("code=%d err=%v, want exit 2 and a refusal", code, err)
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("err = %q, want it to name the token", err)
	}
	select {
	case <-ag.attached:
		t.Error("the agent was dialled anyway; a value the developer gave must be checked first")
	default:
	}
}

// `tetherd env` and `tetherd doctor` attach to read the task and then exit.
// A session either of them opened that advertised incoming requests would
// have the agent steal them into a process that is about to be gone, and
// would put the token on the wire for no reason - so both must attach with
// incoming off no matter what the steal settings in RunOptions say. They
// register no steal flags at all, which is why RunOptions can still carry
// a port and a token here.
func TestEnvAndDoctorAttachWithoutTakingRequests(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(t *testing.T, ag *stealAgent, p *fakeProvider)
	}{
		{"env", func(t *testing.T, ag *stealAgent, p *fakeProvider) {
			opts := EnvOptions{RunOptions: ssmOpts(), Format: "dotenv"}
			opts.NoIncoming = false
			opts.LocalPort = 3000
			opts.User = "shota"
			opts.Token = "tok-shota"
			var out, logs strings.Builder
			if code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p)); err != nil || code != 0 {
				t.Fatalf("code=%d err=%v logs=%s", code, err, logs.String())
			}
		}},
		{"doctor", func(t *testing.T, ag *stealAgent, p *fakeProvider) {
			opts := DoctorOptions{RunOptions: ssmOpts()}
			opts.NoIncoming = false
			opts.LocalPort = 3000
			opts.User = "shota"
			opts.Token = "tok-shota"
			var out strings.Builder
			DoctorRunWithDeps(context.Background(), opts, &out, depsFor(p))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ag := startStealAgent(t)
			p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a", DefinitionARN: "arn:def"}, agentAddr: ag.addr}
			c.run(t, ag, p)
			hello := ag.waitAttached(t)
			if hello.Incoming != (proto.Incoming{}) {
				t.Errorf("hello.Incoming = %+v, want nothing advertised", hello.Incoming)
			}
			if hello.Token != "" {
				t.Errorf("hello.Token = %q, want no token on the wire for a session that takes nothing", hello.Token)
			}
		})
	}
}

// --- the whole hop: the agent's real proxy in front of the CLI's real receiver

// pairedRun stands up everything a stolen request actually passes through:
// the application container, a real agent with its real reverse proxy in
// front of it, and a real `tetherd run` attached with a real StealServer.
// Requests go in at the agent's proxy address, which is where the ALB puts
// them.
//
// Each side is already pinned in isolation - internal/agent/proxy_test.go
// for the agent's half, steal_test.go for this one - and both read
// proto.NoListenerHeader, so the spelling cannot drift. What only this can
// check is the wire-level agreement: that what the CLI actually emits is
// what the agent actually keys on.
type pairedRun struct {
	proxyAddr string
	appHits   func() int
	out       *safeLog
}

func startPair(t *testing.T, localPort int, app http.HandlerFunc) *pairedRun {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		app(w, r)
	}))
	t.Cleanup(appSrv.Close)

	ag := startAgentForWithApp(t, map[string]string{"A": "1"}, appSrv.Listener.Addr().String())
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}

	opts := ssmOpts("sleep", "30")
	opts.NoIncoming = false
	opts.LocalPort = localPort
	opts.User = "shota"
	opts.Token = "tok-shota"
	out := &safeLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _ := RunWithDeps(ctx, opts, out, depsFor(p))
		done <- code
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { return strings.Contains(out.String(), "✓ steal") }, "the steal line")
	return &pairedRun{
		proxyAddr: ag.proxyAddr,
		appHits:   func() int { mu.Lock(); defer mu.Unlock(); return hits },
		out:       out,
	}
}

// do sends one request the way the ALB would and returns the response the
// public caller sees.
func (p *pairedRun) do(t *testing.T, method, path string, hdrs map[string]string) (*http.Response, string) {
	t.Helper()
	r, err := http.NewRequest(method, "http://"+p.proxyAddr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "api.example.com"
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	for k, v := range hdrs {
		r.Header.Set(k, v)
	}
	// No keep-alives: each request is its own connection, so one case
	// cannot inherit a pooled connection from the previous one.
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func devHeaders(user, token string) map[string]string {
	return map[string]string{"X-Dev-User": user, "X-Dev-Token": token}
}

// The brief's integration case, against the agent's real matching rather
// than a stand-in's: a request carrying this developer's name and token
// reaches their laptop, and everything else stays with the application.
func TestRunStealsThroughTheRealAgentProxy(t *testing.T) {
	local := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "LOCAL %s xff=%s host=%s", r.URL.Path, r.Header.Get("X-Forwarded-For"), r.Host)
	}))
	pair := startPair(t, local, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "APP %s", r.URL.Path)
	})

	_, body := pair.do(t, "GET", "/api/orders", devHeaders("shota", "tok-shota"))
	if !strings.Contains(body, "LOCAL /api/orders") {
		t.Errorf("a matching request must reach the laptop, got %q", body)
	} else if !strings.Contains(body, "xff=203.0.113.5") || !strings.Contains(body, "host=api.example.com") {
		t.Errorf("the caller and the host must survive both hops, got %q", body)
	}
	if _, body := pair.do(t, "GET", "/api/orders", nil); !strings.Contains(body, "APP /api/orders") {
		t.Errorf("an unmatched request must reach the app, got %q", body)
	}
	if _, body := pair.do(t, "GET", "/api/orders", devHeaders("shota", "wrong")); !strings.Contains(body, "APP /api/orders") {
		t.Errorf("a wrong token must reach the app, got %q", body)
	}
	if _, body := pair.do(t, "GET", "/api/orders", devHeaders("someone-else", "tok-shota")); !strings.Contains(body, "APP /api/orders") {
		t.Errorf("another developer's name must reach the app, got %q", body)
	}
	// The token must not be anywhere in what this developer's terminal saw.
	if strings.Contains(pair.out.String(), "tok-shota") {
		t.Errorf("the token leaked into the log: %s", pair.out.String())
	}
}

// docs/e2e-aws.md row 24, end to end, across the one thing neither side can
// check alone: that the 502 the CLI emits for a failed dial is the 502 the
// agent keys on, and that a 502 which is not that one is relayed untouched.
func TestRunRow24FallsBackToTheAppOnlyWhenNothingIsListening(t *testing.T) {
	t.Run("nothing listening on the laptop: the caller gets the app's answer", func(t *testing.T) {
		// Port 1 needs no listener of its own to be refused.
		pair := startPair(t, 1, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "APP %s body=%q", r.URL.Path, string(b))
		})
		resp, body := pair.do(t, "GET", "/api/orders", devHeaders("shota", "tok-shota"))

		// 1. the application's answer, body intact.
		if resp.StatusCode != 200 || !strings.Contains(body, `APP /api/orders body=""`) {
			t.Fatalf("status=%d body=%q, want the application's own answer", resp.StatusCode, body)
		}
		// 2. the hop's header must not reach the public caller.
		if got := resp.Header.Get("X-Tetherd-No-Listener"); got != "" {
			t.Errorf("X-Tetherd-No-Listener = %q on a response out of a public ALB", got)
		}
		// 3. exactly once - a fallback that ran the request twice would be
		//    the bug the narrowness exists to prevent.
		if n := pair.appHits(); n != 1 {
			t.Errorf("the application handled the request %d times, want exactly 1", n)
		}
		// 4. and the developer is told, on their own terminal, why.
		waitFor(t, func() bool {
			return strings.Contains(pair.out.String(), "nothing is listening on 127.0.0.1:1")
		}, "the CLI to name the port nothing is listening on")
	})

	t.Run("the laptop's own 502 is relayed, not replayed", func(t *testing.T) {
		local := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The developer's own application answering 502: it has the
			// request and may have acted on it.
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "LOCAL 502 from my own app")
		}))
		pair := startPair(t, local, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the application must not be reached at all: %s", r.URL.Path)
		})
		resp, body := pair.do(t, "POST", "/api/orders", devHeaders("shota", "tok-shota"))
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "LOCAL 502 from my own app") {
			t.Fatalf("status=%d body=%q, want the laptop's own 502 relayed", resp.StatusCode, body)
		}
		if n := pair.appHits(); n != 0 {
			t.Errorf("the application handled the request %d times, want 0: replaying a POST that already ran is worse than relaying the 502", n)
		}
	})
}

// --- one session per task (v0.3b) ------------------------------------------
//
// Until v0.3b `tetherd run` attached to one task while the ALB decided which
// task a request landed on, so on a service with two tasks roughly half the
// traffic a developer had asked to steal reached the deployed application
// instead - silently, with every status line still green. These tests are
// about that hole and about the two ways a rolling deploy used to close the
// run: a secondary session ending, and a task appearing that nobody attached
// to.

// refusingTransport fails every dial, standing in for a task whose SSM port
// forward cannot be opened at all (no connected exec agent, no
// session-manager-plugin).
type refusingTransport struct{}

func (refusingTransport) Dial(context.Context, transport.Task) (net.Conn, error) {
	return nil, errors.New("simulated forward failure")
}

// runWithCancel starts Run on its own goroutine under a context the test
// cancels on its way out, and returns the channel carrying its result plus
// that cancel.
//
// The cleanup waits for the goroutine, not for the result to be read: a test
// that already took the result must not deadlock in its own cleanup. Waiting
// at all is what makes it safe for these tests to read Run's log and to hand
// it a child script - a run still live when the test function returns would
// otherwise keep polling, keep logging into a buffer the test has finished
// with, and leave its child looping for the rest of the binary.
func runWithCancel(t *testing.T, opts RunOptions, out io.Writer, d Deps) (<-chan runResult, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan runResult, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		code, err := RunWithDeps(ctx, opts, out, d)
		done <- runResult{code: code, err: err}
	}()
	t.Cleanup(func() { cancel(); <-stopped })
	return done, cancel
}

// reachedWithin is waitFor without the t.Fatal: it reports whether cond
// came true, for a test that has stronger assertions to make afterwards and
// must not stop at this one.
func reachedWithin(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// shortenFollowInterval makes the task-list poll fast enough to test
// against. Only the tests that are about following a deploy call it: the one
// below is about the sessions opened at startup, and a fast poll would let a
// run that attached to a single task look correct one interval later.
func shortenFollowInterval(t *testing.T) {
	t.Helper()
	restore := followPollInterval
	followPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { followPollInterval = restore })
}

func TestRunAttachesToEveryTaskSoStealCannotMissOne(t *testing.T) {
	// desired_count = 2: the ALB picks which task a request lands on, so a
	// run attached to one of them silently misses half the traffic. This is
	// the defect v0.3b exists to fix, so the assertion is the developer's
	// own: a request arriving at *either* task reaches their process.
	//
	// The poll interval is left at its production value on purpose (see
	// shortenFollowInterval): what has to work here is the attach at
	// startup, not the follower catching up fifteen seconds later.
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "LOCAL %s", r.URL.Path)
	}))
	older := startStealAgent(t)
	newer := startStealAgent(t)
	p := &fakeProvider{
		region: "ap-northeast-1",
		tasks: []transport.Task{
			{ID: "older", SubnetID: "subnet-a", StartedAt: time.Unix(1000, 0), Addr: older.addr},
			{ID: "newer", SubnetID: "subnet-a", StartedAt: time.Unix(2000, 0), Addr: newer.addr},
		},
	}
	opts := ssmOpts("sleep", "30")
	opts.NoIncoming = false
	opts.LocalPort = port
	opts.User = "shota"
	opts.Token = "tok-shota"
	var out safeLog
	done, cancel := runWithCancel(t, opts, &out, depsFor(p))

	for _, tk := range []struct {
		id string
		ag *stealAgent
	}{{"older", older}, {"newer", newer}} {
		// waitAttached fails the test if this task never got a session,
		// which is exactly what attaching to tasks[0] alone would do.
		hello := tk.ag.waitAttached(t)
		if hello.User != "shota" || !hello.Incoming.Enabled || hello.Token != "tok-shota" {
			t.Errorf("task %s was told user=%q incoming=%+v (token carried: %v); every task has to be told what to steal, because the ALB chooses which one gets the request",
				tk.id, hello.User, hello.Incoming, hello.Token == "tok-shota")
		}
		resp, perr := tk.ag.steal(t, devRequest("GET", "/api/orders", "shota", "tok-shota"))
		if perr != nil {
			t.Fatalf("the CLI refused the stream from task %s: %+v", tk.id, perr)
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(b), "LOCAL /api/orders") {
			t.Fatalf("a request that arrived at task %s did not reach the developer's process: status=%d body=%s", tk.id, resp.StatusCode, b)
		}
	}

	waitFor(t, func() bool { return strings.Contains(out.String(), "▶ ") }, "the child to start")
	if l := out.String(); !strings.Contains(l, "c/api  2 tasks (older primary, newer)") {
		t.Errorf("the status line must say how many tasks are attached and which one is the primary:\n%s", l)
	}
	cancel()
	r := <-done
	if r.err != nil {
		t.Errorf("the run was ended by its context, so it must report no failure: code=%d err=%v\n%s", r.code, r.err, out.String())
	}
	if strings.Contains(out.String(), "agent session lost") {
		t.Errorf("no session died, so nothing may report one lost:\n%s", out.String())
	}
}

func TestRunSurvivesASecondaryTaskGoingAway(t *testing.T) {
	// A rolling deploy replaces tasks one at a time. Losing a secondary
	// must not end the run - before v0.3b any session loss did, so a deploy
	// killed the developer's session (and their child process with it)
	// halfway through.
	//
	// The child ignores SIGINT and blocks until this test releases it, so
	// whether the run ended is decided by files and by its result, not by a
	// sleep race.
	primary := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	secondary := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	tr := &breakableTransport{} // no addr: each task's own Addr is dialed
	p := &fakeProvider{
		region: "r",
		tasks: []transport.Task{
			{ID: "older", StartedAt: time.Unix(1000, 0), Addr: primary.addr},
			{ID: "newer", StartedAt: time.Unix(2000, 0), Addr: secondary.addr},
		},
		tr: tr,
	}

	dir := t.TempDir()
	started, release, finished := filepath.Join(dir, "started"), filepath.Join(dir, "release"), filepath.Join(dir, "finished")
	script := writeChildScript(t, "echo up > "+started+"\n"+
		"while [ ! -f "+release+" ]; do sleep 0.05; done\n"+
		"echo done > "+finished+"\n")

	var out safeLog
	done, _ := runWithCancel(t, ssmOpts("/bin/sh", script), &out, depsFor(p))

	waitFor(t, func() bool { return len(secondary.a.Sessions()) == 1 }, "the secondary task to be attached")
	waitForFile(t, started)

	tr.breakTask("newer")
	// Wait for the loss to have actually happened before asserting anything
	// about it: the agent unregisters when it notices its connection is
	// gone, so this is the point where the secondary is provably dead
	// rather than probably.
	waitFor(t, func() bool { return len(secondary.a.Sessions()) == 0 }, "the secondary's session to end")

	select {
	case r := <-done:
		t.Fatalf("the run ended when a secondary task went away (code=%d err=%v); a rolling deploy does this to every task in turn\n%s", r.code, r.err, out.String())
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(finished); err == nil {
		t.Fatal("the child finished on its own; this test proves nothing")
	}
	if err := os.WriteFile(release, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.code != 0 || r.err != nil {
			t.Errorf("the run must end with its child and nothing else: code=%d err=%v\n%s", r.code, r.err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never returned after the child finished")
	}
	if strings.Contains(out.String(), "agent session lost") {
		t.Errorf("losing one task of two is not losing the session:\n%s", out.String())
	}
}

func TestRunFailsWhenNoTaskCanBeAttached(t *testing.T) {
	// Unchanged behaviour: zero sessions is still fatal. Attaching to every
	// task must not turn "nothing answered" into a run that carries on with
	// no session at all.
	t.Run("discovery finds none", func(t *testing.T) {
		p := &fakeProvider{region: "r", discErr: &ecsprov.NotReadyError{
			Reasons: []string{"task t1: ECS Exec is disabled"},
		}}
		code, err := RunWithDeps(context.Background(), ssmOpts("true"), io.Discard, depsFor(p))
		if code != 1 || err == nil || !strings.Contains(err.Error(), "ECS Exec is disabled") {
			t.Fatalf("code=%d err=%v, want the discovery failure and exit 1", code, err)
		}
	})
	t.Run("every task refuses the session", func(t *testing.T) {
		p := &fakeProvider{
			region: "r",
			tasks: []transport.Task{
				{ID: "older", StartedAt: time.Unix(1000, 0)},
				{ID: "newer", StartedAt: time.Unix(2000, 0)},
			},
			tr: refusingTransport{},
		}
		var out strings.Builder
		code, err := RunWithDeps(context.Background(), ssmOpts("true"), &out, depsFor(p))
		if code != 1 || err == nil || !strings.Contains(err.Error(), "connect to agent") {
			t.Fatalf("code=%d err=%v, want the dial failure and exit 1\n%s", code, err, out.String())
		}
		// Both failures are named: a developer whose run died has to see
		// that it was not one task's problem.
		for _, id := range []string{"older", "newer"} {
			if !strings.Contains(out.String(), "task "+id+" could not be attached") {
				t.Errorf("task %s's failure is not in the log:\n%s", id, out.String())
			}
		}
		if strings.Contains(out.String(), "▶ ") {
			t.Errorf("the child must not run with no session:\n%s", out.String())
		}
	})
}

func TestRunFollowsADeployByAttachingToANewTask(t *testing.T) {
	// A deploy adds a task after the run started. The ALB will send it
	// requests whether or not tetherd noticed, so the run has to attach to
	// it while it is live - which is the follower, running inside Run.
	shortenFollowInterval(t)
	first := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	second := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	older := transport.Task{ID: "older", StartedAt: time.Unix(1000, 0), Addr: first.addr}
	newer := transport.Task{ID: "newer", StartedAt: time.Unix(2000, 0), Addr: second.addr}
	p := &fakeProvider{region: "r", tasks: []transport.Task{older}}

	var out safeLog
	done, cancel := runWithCancel(t, ssmOpts("sleep", "30"), &out, depsFor(p))
	waitFor(t, func() bool { return len(first.a.Sessions()) == 1 }, "the service's only task to be attached")

	// The one-task status line is what `tetherd env`, `tetherd doctor` and
	// the AWS e2e script read, and one task must not print the plural form.
	if l := out.String(); !strings.Contains(l, "c/api  task older  (started ") {
		t.Errorf("the one-task status line changed shape:\n%s", l)
	}
	if l := out.String(); strings.Contains(l, "tasks (") {
		t.Errorf("one task must not be reported as several:\n%s", l)
	}

	p.setTasks(older, newer)
	waitFor(t, func() bool { return len(second.a.Sessions()) == 1 }, "the task that appeared to be attached")
	waitFor(t, func() bool { return strings.Contains(out.String(), "task newer attached") }, "the line naming the task that appeared")

	cancel()
	r := <-done
	if r.err != nil {
		t.Errorf("the run was ended by its context, so it must report no failure: code=%d err=%v\n%s", r.code, r.err, out.String())
	}
}

func TestRunPinnedToOneTaskAttachesToThatTaskOnly(t *testing.T) {
	// --task ID is how a developer debugs one task of several - the point
	// of it is that the other tasks are left alone, so neither the attach
	// at startup nor the follower may reach them.
	shortenFollowInterval(t)
	pinned := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	other := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{region: "r", tasks: []transport.Task{
		{ID: "older", StartedAt: time.Unix(1000, 0), Addr: other.addr},
		{ID: "newer", StartedAt: time.Unix(2000, 0), Addr: pinned.addr},
	}}
	opts := ssmOpts("sleep", "30")
	opts.TaskID = "newer"
	var out safeLog
	done, cancel := runWithCancel(t, opts, &out, depsFor(p))

	waitFor(t, func() bool { return len(pinned.a.Sessions()) == 1 }, "the pinned task to be attached")
	// Several poll intervals of the follower not attaching the other task.
	if reachedWithin(300*time.Millisecond, func() bool { return len(other.a.Sessions()) > 0 }) {
		t.Errorf("--task newer attached to task older as well:\n%s", out.String())
	}
	if l := out.String(); !strings.Contains(l, "c/api  task newer  (started ") {
		t.Errorf("a pinned run is a one-task run and says so:\n%s", l)
	}
	cancel()
	if r := <-done; r.err != nil {
		t.Errorf("the run was ended by its context, so it must report no failure: code=%d err=%v\n%s", r.code, r.err, out.String())
	}
}

func TestRunKeepsItsSessionsWhenTheTaskListCannotBeRead(t *testing.T) {
	// A throttled ListTasks, an expired credential, a brief API outage: the
	// poll fails and the sessions already attached are untouched by it. The
	// failure mode this guards against is a poll that reports "no tasks"
	// instead of an error, which would detach from every task and end the
	// run - so the warning line is asserted too, because its absence is
	// what that mistake looks like from here.
	shortenFollowInterval(t)
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{region: "r", tasks: []transport.Task{
		{ID: "older", StartedAt: time.Unix(1000, 0), Addr: ag.addr},
	}}

	dir := t.TempDir()
	started, release, finished := filepath.Join(dir, "started"), filepath.Join(dir, "release"), filepath.Join(dir, "finished")
	script := writeChildScript(t, "echo up > "+started+"\n"+
		"while [ ! -f "+release+" ]; do sleep 0.05; done\n"+
		"echo done > "+finished+"\n")

	var out safeLog
	done, _ := runWithCancel(t, ssmOpts("/bin/sh", script), &out, depsFor(p))
	waitFor(t, func() bool { return len(ag.a.Sessions()) == 1 }, "the task to be attached")
	waitForFile(t, started)

	p.setDiscoverErr(errors.New("simulated throttling"))
	// Not waitFor: a poll that swallows its failure - the mistake this
	// test exists for - would stop here on a missing log line and never
	// reach the assertions below, which are the ones about the developer's
	// session surviving.
	if !reachedWithin(5*time.Second, func() bool {
		return strings.Contains(out.String(), "could not re-read the task list")
	}) {
		t.Errorf("a poll that failed must say so, so a developer whose steal goes stale learns why:\n%s", out.String())
	}

	select {
	case r := <-done:
		t.Fatalf("the run ended because the task list could not be read (code=%d err=%v); the sessions it holds are unaffected by that\n%s", r.code, r.err, out.String())
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(finished); err == nil {
		t.Fatal("the child finished on its own; this test proves nothing")
	}
	if err := os.WriteFile(release, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.code != 0 || r.err != nil {
			t.Errorf("the run must end with its child and nothing else: code=%d err=%v\n%s", r.code, r.err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never returned after the child finished")
	}
	if strings.Contains(out.String(), "agent session lost") {
		t.Errorf("a poll that failed is not a session that was lost:\n%s", out.String())
	}
}
