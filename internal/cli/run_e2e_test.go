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
	"github.com/kyosu-1/tetherd/internal/env"
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
	if len(childEnv) == 0 || childEnv["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] == "" {
		t.Fatalf("the child's environment was not captured (%d vars): %s", len(childEnv), out.String())
	}
	for _, name := range env.LocalAWSCredentialVars {
		if got, ok := childEnv[name]; ok {
			t.Errorf("%s reached the child as %q: the child would sign with the developer's own identity while the status line claims the task role", name, got)
		}
	}
	if !strings.Contains(out.String(), "local AWS credentials") {
		t.Errorf("removing them silently is not enough; the run must say so: %s", out.String())
	}
}

// breakableTransport dials the in-process agent and keeps every connection
// it hands out, so a test can cut the session while the child is still
// running. Nothing outside Run can arrange that otherwise: the session is
// built and owned inside it.
type breakableTransport struct {
	addr  string
	mu    sync.Mutex
	conns []net.Conn
}

func (b *breakableTransport) Dial(ctx context.Context, _ transport.Task) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", b.addr)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.conns = append(b.conns, c)
	b.mu.Unlock()
	return c, nil
}

// breakSession closes the transport under the session, which is what the
// CLI sees when the session-manager-plugin dies or the task goes away.
func (b *breakableTransport) breakSession() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
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
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
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
			Transport: "direct", AgentAddr: "127.0.0.1:1", TargetEnv: "dev", User: "tester",
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
//
// The exit code this produces is pinned by
// TestRunExitsTwoForEveryBadCIDRValue, alongside every other spelling of the
// same mistake.
func TestRunDirectRejectsLocalCIDRsExcludingEverything(t *testing.T) {
	opts := RunOptions{
		Transport: "direct", AgentAddr: "127.0.0.1:1", TargetEnv: "dev", User: "tester",
		RemoteCIDRs: []string{"10.0.0.0/16"}, LocalCIDRs: []string{"10.0.0.0/8"},
		ExecPath: "/usr/bin/true", Command: []string{"true"},
	}
	code, err := RunWithDeps(context.Background(), opts, io.Discard, Deps{})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "network.local_cidrs") {
		t.Fatalf("code=%d err=%v, want a failure naming network.local_cidrs", code, err)
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

// TestRunPinsTheCredentialEndpointRoute: connect()'s route lookup runs
// before pf's output rules, so without a host route for 169.254.170.2 the
// kernel answers EHOSTUNREACH from the reject route a failed ARP left
// behind and pf never sees the packet (internal/helper/route.go).
func TestRunPinsTheCredentialEndpointRoute(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{
		region: "ap-northeast-1", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	hc := &fakeHelperClient{}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return hc, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = "/usr/bin/true"
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if len(hc.routes) != 1 || hc.routes[0].String() != "169.254.170.2" {
		t.Fatalf("route.set was called with %v, want the credential endpoint", hc.routes)
	}
	if !hc.routeCleared {
		t.Error("the route must be cleared when the run ends")
	}
}

func TestRunDoesNotPinTheRouteWithoutCapture(t *testing.T) {
	// --no-network never touches the helper, so it pins no route either.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}
	hc := &fakeHelperClient{}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return hc, nil }
	if code, err := RunWithDeps(context.Background(), ssmOpts("true"), io.Discard, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(hc.routes) != 0 {
		t.Fatalf("routes = %v, want none with --no-network", hc.routes)
	}
}

// TestRunDoesNotPinARouteTheCaptureWillNotRedirect is the boundary the pin
// has to respect: the route is only correct while pf's `rdr pass on lo0`
// covers that address. Under --transport direct the operator chooses the
// remote set by hand, and 169.254.170.0/24 is not in it unless they say so -
// pinning anyway would send the credential endpoint to lo0 where no rdr rule
// picks it up, turning an immediate "no route to host" into a hang.
func TestRunDoesNotPinARouteTheCaptureWillNotRedirect(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	hc := &fakeHelperClient{}
	cap := newFakeCapturer()
	d := Deps{
		DialHelper:  func(string) (HelperClient, error) { return hc, nil },
		NewCapturer: func(HelperClient, func(string, ...any)) Capturer { return cap },
	}
	opts := RunOptions{
		Transport: "direct", AgentAddr: ag.addr, TargetEnv: "dev", User: "tester",
		RemoteCIDRs: []string{"10.9.0.0/16"},
		ExecPath:    "/usr/bin/true", Command: []string{"true"},
	}
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if addrIn(cap.spec.RemoteCIDRs, "169.254.170.2") {
		t.Fatalf("the premise is wrong: %v already covers the endpoint", cap.spec.RemoteCIDRs)
	}
	if len(hc.routes) != 0 {
		t.Fatalf("routes = %v, want none: no rdr rule covers 169.254.170.2 here", hc.routes)
	}
}

// TestRunPinsTheRouteWhenDirectCapturesTheEndpoint is the other half: pass
// the range explicitly and the pin comes back. Without this, a pin that
// never happened at all would satisfy the test above.
func TestRunPinsTheRouteWhenDirectCapturesTheEndpoint(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	hc := &fakeHelperClient{}
	d := Deps{
		DialHelper:  func(string) (HelperClient, error) { return hc, nil },
		NewCapturer: func(HelperClient, func(string, ...any)) Capturer { return newFakeCapturer() },
	}
	opts := RunOptions{
		Transport: "direct", AgentAddr: ag.addr, TargetEnv: "dev", User: "tester",
		RemoteCIDRs: []string{"10.9.0.0/16", ecsprov.TaskRoleCIDR.String()},
		ExecPath:    "/usr/bin/true", Command: []string{"true"},
	}
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if len(hc.routes) != 1 || hc.routes[0] != ecsprov.TaskRoleAddr {
		t.Fatalf("routes = %v, want the credential endpoint pinned", hc.routes)
	}
}

// TestRunExplainsABusyHelperFromRouteSet: route.set is now the first call
// that claims the machine-wide session, so it is where a second `tetherd
// run` finds out. That must still be the explanation a developer can act on,
// not a bare wrapped error.
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
