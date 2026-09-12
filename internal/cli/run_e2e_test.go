package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

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
	// discErr and vpcErr are for later tasks; nothing sets them yet.
	discErr   error
	vpcErr    error
	agentAddr string
}

func (f *fakeProvider) Region() string { return f.region }
func (f *fakeProvider) Discover(context.Context, ecsprov.Target) (transport.Task, error) {
	return f.task, f.discErr
}
func (f *fakeProvider) VPCCIDRs(context.Context, string) ([]netip.Prefix, error) {
	return f.vpc, f.vpcErr
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

// --- fakes for the helper and the capturer ---

type fakeHelperClient struct {
	domains []string
}

func (f *fakeHelperClient) PfApply(helper.PfSpec) error { return nil }
func (f *fakeHelperClient) PfClear() error              { return nil }
func (f *fakeHelperClient) NatLook(string, netip.AddrPort, netip.AddrPort) (netip.AddrPort, error) {
	return netip.MustParseAddrPort("10.0.0.1:5432"), nil
}
func (f *fakeHelperClient) ResolverSet(domains []string, _ int) error {
	f.domains = domains
	return nil
}
func (f *fakeHelperClient) ResolverClear() error { return nil }
func (f *fakeHelperClient) Close() error         { return nil }

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
