# tetherd v0.2b — 設定ファイル・DNS・`env` / `doctor` 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `.tetherd.yml` を置けば `tetherd run -- go run ./cmd/api` だけで済み、VPC 内のドメイン（Cloud Map、プライベートホストゾーン）が手元から解け、`tetherd env` と `tetherd doctor` が使える状態にする（spec §3.4、§4.1、§4.2、§6.5、§6.7、design.md §16 の v0.2 の後半）。

**Architecture:** 最初に `Run` の依存（AWS クライアント、helper、capturer）をインターフェース越しにして、ssm 分岐をプロセス内テストで触れるようにする（v0.2a で実機まで行かないと分からなかったバグが 3 件あった領域）。その土台の上に、設定ファイル、リモート集合の残り（prefix list と `local_cidrs`）、DNS（agent の `resolve` ストリーム + CLI の 127.0.0.1:53530 リゾルバ + helper の `/etc/resolver`）、`env`、`doctor` を載せる。steal と全タスク接続は v0.3。

**Tech Stack:** Go 1.27、`gopkg.in/yaml.v3`（新規）、`github.com/miekg/dns`（新規）、既存の aws-sdk-go-v2 / yamux / cobra / x/sys。

**Spec:** `docs/specs/2026-09-12-v1-macos-design.md`。特に §3.4（DNS）、§3.6（helper の 5 操作）、§4.1（リモート集合）、§4.2（VPC 外のサービス）、§6.5（コマンド）、§6.7（設定ファイル）、§7（`resolve` ストリーム）。

## Global Constraints

- モジュール `github.com/kyosu-1/tetherd`、Go 1.27。新規依存は `gopkg.in/yaml.v3` と `github.com/miekg/dns` のみ
- helper のプロトコルに操作を足さない。DNS は既存の `resolver.set` / `resolver.clear` を使う（spec §3.6）
- 設定の優先順位は **フラグ > `~/.tetherd/config.yml` > `.tetherd.yml` > 既定値**。`.tetherd.yml` はリポジトリにコミットされる共有設定、`~/.tetherd/config.yml` は個人設定（spec §6.7）
- リモート集合は「VPC CIDR（自動）+ `169.254.170.0/24` + `remote_cidrs` + `remote_services` の prefix list」から `local_cidrs` を引いたもの（spec §4.1）
- `remote_domains` が空なら `/etc/resolver` に触らない。作るファイルは `# managed by tetherd` ヘッダー付き（spec §3.4）
- `tetherd env` の既定は secrets をマスク。どれが secret かはタスク定義の `secrets` ブロックの名前で判定（spec §6.5）
- agent は AWS API を呼ばない
- コミットメッセージ `<type>: <summary>`。TDD（RED → GREEN を report に）
- **実機の検証環境が動いている**: `terraform apply` / `plan` / `destroy` は実行しない（`fmt` / `validate` は可）。AWS を呼ぶ検証はコントローラが行う

## ファイル構成

```
internal/cli/deps.go            Run の差し替え可能な依存（awsProvider / HelperClient / Capturer）と既定実装
internal/cli/run.go             deps 経由に組み替え、設定・prefix list・DNS・local_cidrs を反映
internal/cli/config.go          フラグと設定ファイルの合成（applyConfig）
internal/cli/env.go             tetherd env
internal/cli/doctor.go          tetherd doctor の事実収集
internal/doctor/doctor.go       検査結果の型と描画
internal/doctor/checks.go       検査ごとの判定（純粋関数）
internal/cli/root.go            env / doctor のコマンド登録、--config フラグ
internal/config/config.go       .tetherd.yml と ~/.tetherd/config.yml の読み込みと合成
internal/config/personal.go     個人設定の生成（user / token）
internal/provider/ecs/prefix.go ServiceCIDRs（managed prefix list）
internal/provider/ecs/secrets.go SecretNames / PIDMode（DescribeTaskDefinition）
internal/agent/resolve.go       resolve ストリームのハンドラ（タスクの resolv.conf で解決）
internal/session/*              resolve ストリームの client/server 配線
internal/dnsproxy/dnsproxy.go   127.0.0.1:53530 のリゾルバ。問い合わせを resolve ストリームに転送
docs/*                          spec の該当節、design.md、e2e-aws.md
```

---

### Task 1: `Run` の依存を差し替え可能にする（テスト土台）

**Files:**
- Create: `internal/cli/deps.go`
- Modify: `internal/cli/run.go`（既定依存を使う形に組み替え。振る舞いは変えない）
- Modify: `internal/agent/agent.go`（`SetDialer` を足す。テストが agent の dial 先を差し替えられるように）
- Test: `internal/cli/run_e2e_test.go`（新規。プロセス内 agent を相手に `Run` を通す）

**Interfaces:**
- Produces:
  ```go
  // deps.go
  type awsProvider interface {
      Region() string
      Discover(ctx context.Context, t ecsprov.Target) (transport.Task, error)
      VPCCIDRs(ctx context.Context, subnetID string) ([]netip.Prefix, error)
      Transport(logf func(string, ...any)) transport.Transport
  }
  type HelperClient interface {
      PfApply(spec helper.PfSpec) error
      PfClear() error
      NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error)
      ResolverSet(domains []string, port int) error
      ResolverClear() error
      Close() error
  }
  type Capturer interface {
      capture.Capturer
      RedirectPort() int
  }
  type Deps struct {
      NewAWSProvider func(ctx context.Context, opts RunOptions) (awsProvider, error)
      DialHelper     func(socket string) (HelperClient, error)
      NewCapturer    func(h HelperClient, logf func(string, ...any)) Capturer
  }
  func (d Deps) withDefaults() Deps   // 未設定のフィールドを本物で埋める
  func Run(ctx context.Context, opts RunOptions, stderr io.Writer) (int, error)                   // 既存シグネチャのまま。RunWithDeps(ctx, opts, stderr, Deps{}) を呼ぶ
  func RunWithDeps(ctx context.Context, opts RunOptions, stderr io.Writer, d Deps) (int, error)   // 中身は今の Run
  ```
  `*helper.Client` は `HelperClient` を満たす。`*pfrdr.Capturer` は `Capturer` を満たす。
- Produces（agent）:
  ```go
  // SetDialer replaces what the agent dials on a dial stream (tests).
  func (a *Agent) SetDialer(dial func(ctx context.Context, addr string) (net.Conn, error))
  ```

- [ ] **Step 1: 失敗するテストを書く**

`internal/cli/run_e2e_test.go`:

```go
package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
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
	region   string
	task     transport.Task
	vpc      []netip.Prefix
	discErr  error
	vpcErr   error
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
	code, err := RunWithDeps(context.Background(), ssmOpts("sh", "-c", "echo host=$DB_HOST"), &out, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
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
		vpc:    []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	var applied helper.PfSpec
	cap := &fakeCapturer{}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{applied: &applied}, nil }
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
	for _, p := range applied.RemoteCIDRs {
		got[p.String()] = true
	}
	for _, want := range []string{"10.0.0.0/16", "169.254.170.0/24", "10.9.0.0/16"} {
		if !got[want] {
			t.Errorf("%s missing from the captured set: %v", want, applied.RemoteCIDRs)
		}
	}
}

// The task role has to be claimed only when its endpoint is captured.
func TestRunVerifiesTheTaskRoleThroughTheAgent(t *testing.T) {
	creds := `{"AccessKeyId":"AKIA","SecretAccessKey":"sk","Token":"tok","Expiration":"2030-01-01T00:00:00Z"}`
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(creds))
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
		vpc:    []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return &fakeCapturer{} }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = "/usr/bin/true"
	var out strings.Builder
	// STS is not reachable from the test, so the probe fails after the
	// credentials are fetched; what matters is that tetherd got that far
	// through the agent and said so honestly.
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
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return &fakeCapturer{} }

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
	applied *helper.PfSpec
	domains []string
}

func (f *fakeHelperClient) PfApply(spec helper.PfSpec) error {
	if f.applied != nil {
		*f.applied = spec
	}
	return nil
}
func (f *fakeHelperClient) PfClear() error { return nil }
func (f *fakeHelperClient) NatLook(string, netip.AddrPort, netip.AddrPort) (netip.AddrPort, error) {
	return netip.MustParseAddrPort("10.0.0.1:5432"), nil
}
func (f *fakeHelperClient) ResolverSet(domains []string, _ int) error { f.domains = domains; return nil }
func (f *fakeHelperClient) ResolverClear() error                      { return nil }
func (f *fakeHelperClient) Close() error                              { return nil }

type fakeCapturer struct {
	started bool
	closed  bool
	accept  chan capture.Conn
}

func (f *fakeCapturer) Start(context.Context, capture.Spec) error { f.started = true; return nil }
func (f *fakeCapturer) Accept() (capture.Conn, error) {
	if f.accept == nil {
		f.accept = make(chan capture.Conn)
	}
	cc, ok := <-f.accept
	if !ok {
		return capture.Conn{}, io.EOF
	}
	return cc, nil
}
func (f *fakeCapturer) Close() error      { f.closed = true; return nil }
func (f *fakeCapturer) RedirectPort() int { return 15300 }
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/cli/`
Expected: FAIL（`undefined: RunWithDeps`, `undefined: Deps`, `agent.Agent` に `SetDialer` が無い）

- [ ] **Step 3: 実装**

`internal/agent/agent.go` に追加（`handler.Dial` がこれを使うようにする。既定は今の `net.Dialer`）:

```go
// Agent に追加
	dial func(ctx context.Context, addr string) (net.Conn, error)

// SetDialer replaces what a dial stream connects to. Production leaves it
// unset and the agent dials the address itself; tests point it at a stub so
// the CLI side can be exercised without a VPC.
func (a *Agent) SetDialer(dial func(ctx context.Context, addr string) (net.Conn, error)) {
	a.dial = dial
}

// handler.Dial を置き換え
func (h *handler) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if h.a.dial != nil {
		return h.a.dial(ctx, addr)
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}
```

`internal/cli/deps.go`（新規）:

```go
package cli

import (
	"context"
	"net"
	"net/netip"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/capture/pfrdr"
	"github.com/kyosu-1/tetherd/internal/helper"
	ecsprov "github.com/kyosu-1/tetherd/internal/provider/ecs"
	"github.com/kyosu-1/tetherd/internal/transport"
	ssmtr "github.com/kyosu-1/tetherd/internal/transport/ssm"
)

// awsProvider is everything Run needs from AWS for one session. Three of the
// four bugs v0.2a only found on real hardware lived in this branch, which was
// unreachable from a test because the SDK clients were built inline; going
// through an interface makes discovery, the remote set and the transport
// substitutable.
type awsProvider interface {
	Region() string
	Discover(ctx context.Context, t ecsprov.Target) (transport.Task, error)
	VPCCIDRs(ctx context.Context, subnetID string) ([]netip.Prefix, error)
	Transport(logf func(string, ...any)) transport.Transport
}

// HelperClient is the part of the privileged helper the CLI uses.
// *helper.Client satisfies it.
type HelperClient interface {
	PfApply(spec helper.PfSpec) error
	PfClear() error
	NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error)
	ResolverSet(domains []string, port int) error
	ResolverClear() error
	Close() error
}

// Capturer is capture.Capturer plus the transparent port Run reports.
type Capturer interface {
	capture.Capturer
	RedirectPort() int
}

// Deps are Run's replaceable collaborators. The zero value is production:
// the real AWS SDK, the real privileged helper, the real pf capturer.
type Deps struct {
	NewAWSProvider func(ctx context.Context, opts RunOptions) (awsProvider, error)
	DialHelper     func(socket string) (HelperClient, error)
	NewCapturer    func(h HelperClient, logf func(string, ...any)) Capturer
}

func (d Deps) withDefaults() Deps {
	if d.NewAWSProvider == nil {
		d.NewAWSProvider = newSDKProvider
	}
	if d.DialHelper == nil {
		d.DialHelper = func(socket string) (HelperClient, error) { return helper.Dial(socket) }
	}
	if d.NewCapturer == nil {
		d.NewCapturer = func(h HelperClient, logf func(string, ...any)) Capturer {
			c := pfrdr.New(h)
			c.Logf = logf
			return c
		}
	}
	return d
}

// sdkProvider is the production awsProvider.
type sdkProvider struct {
	cfg     aws.Config
	profile string
}

func newSDKProvider(ctx context.Context, opts RunOptions) (awsProvider, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if opts.Profile != "" {
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(opts.Profile))
	}
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, err
	}
	return &sdkProvider{cfg: cfg, profile: opts.Profile}, nil
}

func (p *sdkProvider) Region() string { return p.cfg.Region }

func (p *sdkProvider) Discover(ctx context.Context, t ecsprov.Target) (transport.Task, error) {
	return ecsprov.Discover(ctx, awsecs.NewFromConfig(p.cfg), t)
}

func (p *sdkProvider) VPCCIDRs(ctx context.Context, subnetID string) ([]netip.Prefix, error) {
	return ecsprov.VPCCIDRs(ctx, awsec2.NewFromConfig(p.cfg), subnetID)
}

func (p *sdkProvider) Transport(logf func(string, ...any)) transport.Transport {
	return &ssmtr.Transport{API: awsssm.NewFromConfig(p.cfg), Region: p.cfg.Region, Profile: p.profile, Logf: logf}
}

var _ = net.Dial // keep net imported for the interface signatures above
```

（最後の `var _` は不要なら消す。`net` を使わないなら import しない。）

`internal/cli/run.go` の変更:

- `func Run(ctx context.Context, opts RunOptions, stderr io.Writer) (int, error)` を
  ```go
  // Run connects to the agent, installs capture, runs the command and cleans
  // up. It returns the child's exit code.
  func Run(ctx context.Context, opts RunOptions, stderr io.Writer) (int, error) {
  	return RunWithDeps(ctx, opts, stderr, Deps{})
  }

  // RunWithDeps is Run with substitutable collaborators (see Deps).
  func RunWithDeps(ctx context.Context, opts RunOptions, stderr io.Writer, d Deps) (int, error) {
  	d = d.withDefaults()
  	…今の Run の中身…
  }
  ```
- ssm 分岐の中の `awsconfig.LoadDefaultConfig` / `ecsprov.Discover` / `ecsprov.VPCCIDRs` / `ssmtr.Transport` の直接呼び出しを `d.NewAWSProvider` で得た `provider` 経由に置き換える:
  ```go
  	case "ssm":
  		if opts.Cluster == "" || opts.Service == "" {
  			return 2, errors.New("--transport ssm needs --cluster and --service")
  		}
  		prov, err := d.NewAWSProvider(ctx, opts)
  		if err != nil {
  			return 1, fmt.Errorf("aws config: %w", err)
  		}
  		region = prov.Region()
  		task, err = prov.Discover(ctx, ecsprov.Target{Cluster: opts.Cluster, Service: opts.Service, TaskID: opts.TaskID})
  		…（ログはそのまま）…
  		if !opts.NoNetwork {
  			vpc, err := prov.VPCCIDRs(ctx, task.SubnetID)
  			…
  		}
  		tr = prov.Transport(logf)
  ```
- `helper.Dial(opts.HelperSocket)` を `d.DialHelper(opts.HelperSocket)` に、`hc` の型を `HelperClient` に
- `pfrdr.New(hc)` + `cap.Logf = logf` を `d.NewCapturer(hc, logf)` に、`cap` の型を `Capturer` に
- それ以外の振る舞い（順序、ログ文面、ゲート）は一切変えない。`awsconfig` / `awsec2` / `awsecs` / `awsssm` / `pfrdr` の import は run.go から外れる（deps.go に移る）

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./internal/cli/ ./internal/agent/ && make lint && make test && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS。既存のテスト（`TestExitCodeMapping` など）も変更なしで通ること

- [ ] **Step 5: Commit**

```bash
git add internal/cli internal/agent
git commit -m "refactor(cli): make Run's AWS, helper and capturer collaborators substitutable"
```

---

### Task 2: 設定ファイル（`internal/config`）

**Files:**
- Create: `internal/config/config.go`, `internal/config/personal.go`
- Test: `internal/config/config_test.go`
- Modify: `internal/cli/root.go`（`--config` フラグと、フラグ未指定の値を設定で埋める処理）
- Test: `internal/cli/root_test.go` に追記（`internal/cli/run_test.go` にあってもよい）

**Interfaces:**
- Consumes: `RunOptions`（Task 1 のまま）
- Produces:
  ```go
  // config.go
  type AWS struct {
      Profile string `yaml:"profile"`
      Region  string `yaml:"region"`
  }
  type Target struct {
      Cluster   string `yaml:"cluster"`
      Service   string `yaml:"service"`
      Container string `yaml:"container"`
      Env       string `yaml:"env"`
  }
  type Env struct {
      Override map[string]string `yaml:"override"`
      Exclude  []string          `yaml:"exclude"`
  }
  type Network struct {
      RemoteCIDRs    []string `yaml:"remote_cidrs"`
      LocalCIDRs     []string `yaml:"local_cidrs"`
      RemoteDomains  []string `yaml:"remote_domains"`
      RemoteServices []string `yaml:"remote_services"`
  }
  type Incoming struct {
      LocalPort int   `yaml:"local_port"`
      Match     Match `yaml:"match"`
  }
  type Match struct {
      Header      string `yaml:"header"`
      TokenHeader string `yaml:"token_header"`
  }
  type Shared struct {   // .tetherd.yml
      Version  int      `yaml:"version"`
      AWS      AWS      `yaml:"aws"`
      Target   Target   `yaml:"target"`
      Env      Env      `yaml:"env"`
      Network  Network  `yaml:"network"`
      Incoming Incoming `yaml:"incoming"`
  }
  type Personal struct { // ~/.tetherd/config.yml
      User  string `yaml:"user"`
      Token string `yaml:"token"`
      AWS   AWS    `yaml:"aws"`
  }
  type Config struct {
      Shared     Shared
      Personal   Personal
      SharedPath string // 読めたファイルのパス（空なら無かった）
  }
  func Find(start string) (string, bool)                       // start から上に .tetherd.yml を探す（リポジトリ直下想定、/ まで）
  func Load(sharedPath, personalPath string) (Config, error)    // 無いファイルは既定値。version != 1 はエラー
  func DefaultPersonalPath() (string, error)                    // ~/.tetherd/config.yml
  // personal.go
  func EnsurePersonal(path, user string) (Personal, bool, error) // 無ければ user と 32 バイトの token で作る。bool は作ったか
  ```
  `Load` は未知のキーをエラーにする（`yaml.Decoder.KnownFields(true)`）。タイプミスを黙って無視しない。

- [ ] **Step 1: 失敗するテストを書く**

`internal/config/config_test.go`:

```go
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
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go get gopkg.in/yaml.v3@latest && go test ./internal/config/`
Expected: FAIL（`undefined: Load`）

- [ ] **Step 3: 実装**

`internal/config/config.go`:

```go
// Package config reads the two configuration files: .tetherd.yml in the
// repository (shared, committed) and ~/.tetherd/config.yml (personal). The
// precedence Run applies is flags > personal > shared > defaults (spec §6.7).
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SharedName is the file looked up from the working directory upwards.
const SharedName = ".tetherd.yml"

// Version is the only supported `version:` value.
const Version = 1

// AWS is the shared aws block; the personal file may override the profile.
type AWS struct {
	Profile string `yaml:"profile"`
	Region  string `yaml:"region"`
}

// Target names the service to attach to.
type Target struct {
	Cluster   string `yaml:"cluster"`
	Service   string `yaml:"service"`
	Container string `yaml:"container"`
	Env       string `yaml:"env"`
}

// Env tunes what reaches the child.
type Env struct {
	Override map[string]string `yaml:"override"`
	Exclude  []string          `yaml:"exclude"`
}

// Network is the routing configuration (spec §4.1, §3.4).
type Network struct {
	RemoteCIDRs    []string `yaml:"remote_cidrs"`
	LocalCIDRs     []string `yaml:"local_cidrs"`
	RemoteDomains  []string `yaml:"remote_domains"`
	RemoteServices []string `yaml:"remote_services"`
}

// Match is the steal condition (used from v0.3; parsed now so a repository
// can carry the setting before the feature lands).
type Match struct {
	Header      string `yaml:"header"`
	TokenHeader string `yaml:"token_header"`
}

// Incoming is the steal configuration (v0.3).
type Incoming struct {
	LocalPort int   `yaml:"local_port"`
	Match     Match `yaml:"match"`
}

// Shared is .tetherd.yml.
type Shared struct {
	Version  int      `yaml:"version"`
	AWS      AWS      `yaml:"aws"`
	Target   Target   `yaml:"target"`
	Env      Env      `yaml:"env"`
	Network  Network  `yaml:"network"`
	Incoming Incoming `yaml:"incoming"`
}

// Personal is ~/.tetherd/config.yml.
type Personal struct {
	User  string `yaml:"user"`
	Token string `yaml:"token"`
	AWS   AWS    `yaml:"aws"`
}

// Config is both files, with the shared file's path for error messages.
type Config struct {
	Shared     Shared
	Personal   Personal
	SharedPath string
}

// Find walks up from start looking for .tetherd.yml.
func Find(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(dir, SharedName)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// DefaultPersonalPath is ~/.tetherd/config.yml.
func DefaultPersonalPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tetherd", "config.yml"), nil
}

// Load reads both files. A missing file is not an error (defaults are used);
// an unknown key is, so a typo cannot be silently ignored.
func Load(sharedPath, personalPath string) (Config, error) {
	var cfg Config
	if sharedPath != "" {
		if err := decodeFile(sharedPath, &cfg.Shared); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return Config{}, err
			}
		} else {
			cfg.SharedPath = sharedPath
			if cfg.Shared.Version != Version {
				return Config{}, fmt.Errorf("%s: unsupported version %d (this tetherd understands version %d)", sharedPath, cfg.Shared.Version, Version)
			}
		}
	}
	if personalPath != "" {
		if err := decodeFile(personalPath, &cfg.Personal); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
	}
	return cfg, nil
}

func decodeFile(path string, into any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // an empty file is an empty config
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
```

`internal/config/personal.go`:

```go
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// EnsurePersonal returns the personal configuration, creating it with the
// given user and a fresh steal token when the file does not exist. The token
// is what the agent matches on X-Dev-Token from v0.3, so the file is 0600 and
// is never rewritten once it exists.
func EnsurePersonal(path, user string) (Personal, bool, error) {
	var p Personal
	err := decodeFile(path, &p)
	switch {
	case err == nil:
		return p, false, nil
	case !errors.Is(err, os.ErrNotExist):
		return Personal{}, false, err
	}
	token, err := newToken()
	if err != nil {
		return Personal{}, false, err
	}
	p = Personal{User: user, Token: token}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Personal{}, false, err
	}
	body, err := yaml.Marshal(p)
	if err != nil {
		return Personal{}, false, err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return Personal{}, false, fmt.Errorf("write %s: %w", path, err)
	}
	return p, true, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
```

`internal/cli/root.go` の変更（`run` の `RunE` の中、`runFn` を呼ぶ前）:

```go
	// Fill in what the flags did not set: personal file, then the shared
	// file, then the defaults already on the flags (spec §6.7).
	if err := applyConfig(cmd, &opts); err != nil {
		return err
	}
```

`internal/cli/config.go`（新規、`applyConfig` の実装。`cmd.Flags().Changed(name)` で「フラグで明示されたか」を見る）:

```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kyosu-1/tetherd/internal/config"
)

// configPath is set by --config; empty means "look upwards for .tetherd.yml".
var configPath string

// applyConfig fills RunOptions fields the flags did not set, from
// ~/.tetherd/config.yml and then .tetherd.yml. Flags always win, so a
// value the user typed is never overwritten.
func applyConfig(cmd *cobra.Command, opts *RunOptions) error {
	shared := configPath
	if shared == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if p, ok := config.Find(wd); ok {
			shared = p
		}
	}
	personal, err := config.DefaultPersonalPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(shared, personal)
	if err != nil {
		return err
	}
	if opts.User == "" {
		opts.User = cfg.Personal.User
	}
	if opts.User == "" {
		opts.User = os.Getenv("USER")
	}
	// The personal file may pin a different profile than the repository's.
	set := func(flag string, dst *string, values ...string) {
		if cmd.Flags().Changed(flag) {
			return
		}
		for _, v := range values {
			if v != "" {
				*dst = v
				return
			}
		}
	}
	set("profile", &opts.Profile, cfg.Personal.AWS.Profile, cfg.Shared.AWS.Profile)
	set("region", &opts.Region, cfg.Personal.AWS.Region, cfg.Shared.AWS.Region)
	set("cluster", &opts.Cluster, cfg.Shared.Target.Cluster)
	set("service", &opts.Service, cfg.Shared.Target.Service)
	set("env", &opts.TargetEnv, cfg.Shared.Target.Env)
	if !cmd.Flags().Changed("remote-cidr") {
		opts.RemoteCIDRs = append(opts.RemoteCIDRs, cfg.Shared.Network.RemoteCIDRs...)
	}
	opts.LocalCIDRs = cfg.Shared.Network.LocalCIDRs
	opts.RemoteServices = cfg.Shared.Network.RemoteServices
	opts.RemoteDomains = cfg.Shared.Network.RemoteDomains
	opts.EnvOverride = cfg.Shared.Env.Override
	opts.EnvExclude = cfg.Shared.Env.Exclude
	if cfg.SharedPath != "" {
		opts.ConfigPath = cfg.SharedPath
	}
	_ = fmt.Sprint // keep fmt if unused after edits
	return nil
}
```

`RunOptions` に足すフィールド（Task 3・5 で使う。ここで宣言してしまう）:

```go
	LocalCIDRs     []string
	RemoteServices []string
	RemoteDomains  []string
	EnvOverride    map[string]string
	EnvExclude     []string
	ConfigPath     string // 読んだ .tetherd.yml。ステータス行に出す
```

`root.go` に `--config` フラグを足す: `cmd.Flags().StringVar(&configPath, "config", "", "path to .tetherd.yml (default: the nearest one above the working directory)")`

`Run` の中で、設定から来た値を使う:
- `env.Options` に `Override: opts.EnvOverride`（ただし Task 13 で入れた `taskRoleEnv` の値が勝つ必要があるので、**`taskRoleEnv` の結果を後から上書きで足す**: `override := maps.Clone(opts.EnvOverride)` してから `for k, v := range taskRoleEnv(...) { override[k] = v }`）と `Exclude: opts.EnvExclude`
- ステータス行の先頭に、設定を読んだ場合だけ `logf("config     %s", opts.ConfigPath)` を出す

- [ ] **Step 4: CLI 側のテストを足す**

`internal/cli/config_test.go`:

```go
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
```

- [ ] **Step 5: テストが通ることを確認**

Run: `go test -race -count=1 ./internal/config/ ./internal/cli/ && make lint && make test && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/config internal/cli
git commit -m "feat(config): read .tetherd.yml and the personal config"
```

---

### Task 3: リモート集合の残り — `remote_services` と `local_cidrs`

**Files:**
- Create: `internal/provider/ecs/prefix.go`
- Modify: `internal/cli/deps.go`（`awsProvider` に `ServiceCIDRs` を足す）、`internal/cli/run.go`（集合の組み立て）
- Test: `internal/provider/ecs/prefix_test.go`, `internal/cli/run_test.go` に追記

**Interfaces:**
- Produces:
  ```go
  // prefix.go
  type PrefixListAPI interface {
      DescribeManagedPrefixLists(ctx context.Context, in *ec2.DescribeManagedPrefixListsInput, opts ...func(*ec2.Options)) (*ec2.DescribeManagedPrefixListsOutput, error)
      GetManagedPrefixListEntries(ctx context.Context, in *ec2.GetManagedPrefixListEntriesInput, opts ...func(*ec2.Options)) (*ec2.GetManagedPrefixListEntriesOutput, error)
  }
  // ServiceCIDRs resolves service names ("s3", "dynamodb") to the IPv4
  // prefixes of com.amazonaws.<region>.<service>.
  func ServiceCIDRs(ctx context.Context, api PrefixListAPI, region string, services []string) ([]netip.Prefix, error)
  ```
- Produces（cli）:
  ```go
  // Subtract removes every prefix contained in one of the excluded ranges.
  func Subtract(all []netip.Prefix, exclude []netip.Prefix) []netip.Prefix
  ```
  `awsProvider` に `ServiceCIDRs(ctx context.Context, services []string) ([]netip.Prefix, error)` を追加（`sdkProvider` は `ecsprov.ServiceCIDRs` に委譲、fake は返り値を持つ）

- [ ] **Step 1: 失敗するテストを書く**

`internal/provider/ecs/prefix_test.go`:

```go
package ecs

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type fakePrefix struct {
	described []string
	listID    string
}

func (f *fakePrefix) DescribeManagedPrefixLists(_ context.Context, in *awsec2.DescribeManagedPrefixListsInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeManagedPrefixListsOutput, error) {
	for _, filter := range in.Filters {
		f.described = append(f.described, filter.Values...)
	}
	return &awsec2.DescribeManagedPrefixListsOutput{PrefixLists: []types.ManagedPrefixList{
		{PrefixListId: aws.String("pl-1"), PrefixListName: aws.String("com.amazonaws.ap-northeast-1.s3")},
	}}, nil
}

func (f *fakePrefix) GetManagedPrefixListEntries(_ context.Context, in *awsec2.GetManagedPrefixListEntriesInput, _ ...func(*awsec2.Options)) (*awsec2.GetManagedPrefixListEntriesOutput, error) {
	f.listID = aws.ToString(in.PrefixListId)
	return &awsec2.GetManagedPrefixListEntriesOutput{Entries: []types.PrefixListEntry{
		{Cidr: aws.String("52.219.0.0/20")},
		{Cidr: aws.String("3.5.152.0/21")},
		{Cidr: aws.String("2600:1f00::/40")}, // IPv6 is skipped in v1
	}}, nil
}

func TestServiceCIDRs(t *testing.T) {
	f := &fakePrefix{}
	got, err := ServiceCIDRs(context.Background(), f, "ap-northeast-1", []string{"s3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "52.219.0.0/20" || got[1].String() != "3.5.152.0/21" {
		t.Fatalf("got %v", got)
	}
	if len(f.described) != 1 || f.described[0] != "com.amazonaws.ap-northeast-1.s3" {
		t.Fatalf("filtered on %v", f.described)
	}
	if f.listID != "pl-1" {
		t.Fatalf("entries fetched for %q", f.listID)
	}
}

func TestServiceCIDRsRejectsUnknownService(t *testing.T) {
	if _, err := ServiceCIDRs(context.Background(), &fakePrefix{}, "ap-northeast-1", []string{"rds"}); err == nil || !strings.Contains(err.Error(), "rds") {
		t.Fatalf("only the services AWS publishes a prefix list for are allowed: %v", err)
	}
}

func TestServiceCIDRsEmpty(t *testing.T) {
	got, err := ServiceCIDRs(context.Background(), &fakePrefix{}, "ap-northeast-1", nil)
	if err != nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}
```

`internal/cli/run_test.go` に追記:

```go
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
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/provider/ecs/ ./internal/cli/`
Expected: FAIL（`undefined: ServiceCIDRs`, `undefined: Subtract`）

- [ ] **Step 3: 実装**

`internal/provider/ecs/prefix.go`:

```go
package ecs

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// PrefixListAPI is the subset of the EC2 client ServiceCIDRs needs.
type PrefixListAPI interface {
	DescribeManagedPrefixLists(ctx context.Context, in *awsec2.DescribeManagedPrefixListsInput, opts ...func(*awsec2.Options)) (*awsec2.DescribeManagedPrefixListsOutput, error)
	GetManagedPrefixListEntries(ctx context.Context, in *awsec2.GetManagedPrefixListEntriesInput, opts ...func(*awsec2.Options)) (*awsec2.GetManagedPrefixListEntriesOutput, error)
}

// gatewayServices are the services AWS publishes a managed prefix list for.
// They are the ones reached over a gateway endpoint, where the traffic keeps
// public addresses and a network condition in a policy can only be satisfied
// by leaving through the task's ENI (spec §4.2).
var gatewayServices = map[string]bool{"s3": true, "dynamodb": true}

// ServiceCIDRs resolves service names to the IPv4 prefixes of
// com.amazonaws.<region>.<service>.
func ServiceCIDRs(ctx context.Context, api PrefixListAPI, region string, services []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range services {
		if !gatewayServices[s] {
			return nil, fmt.Errorf("network.remote_services: %q has no managed prefix list; only s3 and dynamodb do (interface endpoints are reached through network.remote_domains instead)", s)
		}
		name := fmt.Sprintf("com.amazonaws.%s.%s", region, s)
		desc, err := api.DescribeManagedPrefixLists(ctx, &awsec2.DescribeManagedPrefixListsInput{
			Filters: []types.Filter{{Name: aws.String("prefix-list-name"), Values: []string{name}}},
		})
		if err != nil {
			return nil, fmt.Errorf("DescribeManagedPrefixLists %s: %w", name, err)
		}
		if len(desc.PrefixLists) == 0 {
			return nil, fmt.Errorf("no managed prefix list named %s in this region", name)
		}
		id := aws.ToString(desc.PrefixLists[0].PrefixListId)
		entries, err := api.GetManagedPrefixListEntries(ctx, &awsec2.GetManagedPrefixListEntriesInput{PrefixListId: aws.String(id)})
		if err != nil {
			return nil, fmt.Errorf("GetManagedPrefixListEntries %s: %w", id, err)
		}
		for _, e := range entries.Entries {
			p, err := netip.ParsePrefix(aws.ToString(e.Cidr))
			if err != nil || !p.Addr().Is4() {
				continue
			}
			out = append(out, p.Masked())
		}
	}
	return out, nil
}
```

`internal/cli/run.go` に追加:

```go
// Subtract removes every prefix that a local_cidrs range covers, so a part
// of the VPC range that the laptop must reach directly (an overlapping home
// network, a service pinned to the machine) stays off the captured set.
func Subtract(all []netip.Prefix, exclude []netip.Prefix) []netip.Prefix {
	if len(exclude) == 0 {
		return all
	}
	out := make([]netip.Prefix, 0, len(all))
	for _, p := range all {
		covered := false
		for _, e := range exclude {
			if e.Overlaps(p) && e.Bits() <= p.Bits() {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out
}
```

`ssm` 分岐の集合組み立ては関数に切り出す。`tetherd doctor`（Task 7）が「run と同じ集合」を表示する必要があり、共有しないと意味が無いため:

```go
// remoteSet is everything that goes to the task: the VPC, the credential
// endpoint, the configured extras and any gateway-endpoint service ranges,
// minus the ranges the laptop must keep for itself (spec §4.1).
func remoteSet(ctx context.Context, opts RunOptions, prov awsProvider, task transport.Task, logf func(string, ...any)) ([]netip.Prefix, error) {
	extra, err := ParseRemoteCIDRs(opts.RemoteCIDRs)
	if err != nil {
		return nil, err
	}
	vpc, err := prov.VPCCIDRs(ctx, task.SubnetID)
	if err != nil {
		return nil, err
	}
	cidrs := append(append(vpc, ecsprov.TaskRoleCIDR), extra...)
	if len(opts.RemoteServices) > 0 {
		svc, err := prov.ServiceCIDRs(ctx, opts.RemoteServices)
		if err != nil {
			return nil, err
		}
		cidrs = append(cidrs, svc...)
		logf("           remote_services %s → %d prefixes", strings.Join(opts.RemoteServices, ", "), len(svc))
	}
	local, err := ParseRemoteCIDRs(opts.LocalCIDRs)
	if err != nil {
		return nil, fmt.Errorf("network.local_cidrs: %w", err)
	}
	return Subtract(cidrs, local), nil
}

// ecsTarget is the discovery target the flags and the config describe.
func ecsTarget(opts RunOptions) ecsprov.Target {
	return ecsprov.Target{Cluster: opts.Cluster, Service: opts.Service, TaskID: opts.TaskID}
}
```

`Run` の `ssm` 分岐はこれを呼ぶだけにする:

```go
		if !opts.NoNetwork {
			cidrs, err = remoteSet(ctx, opts, prov, task, logf)
			if err != nil {
				return 1, err
			}
		}
```

（`remoteSet` が返すエラーはどれも設定・API 由来なので exit 1。`local_cidrs` のパース失敗を 2（usage）にしていた区別は、設定ファイル由来でも起きるようになったのでやめる。）

`awsProvider` と `sdkProvider` に `ServiceCIDRs` を足す:

```go
	ServiceCIDRs(ctx context.Context, services []string) ([]netip.Prefix, error)

func (p *sdkProvider) ServiceCIDRs(ctx context.Context, services []string) ([]netip.Prefix, error) {
	return ecsprov.ServiceCIDRs(ctx, awsec2.NewFromConfig(p.cfg), p.cfg.Region, services)
}
```

Task 1 のテストの `fakeProvider` にも `ServiceCIDRs` を足す（`svc []netip.Prefix` フィールドを返すだけ）。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./... && make lint && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal
git commit -m "feat(provider/ecs): resolve remote_services prefix lists; subtract local_cidrs"
```

---

### Task 4: agent の `resolve` ストリーム

**Files:**
- Create: `internal/agent/resolve.go`
- Modify: `internal/proto/messages.go`（`ResolveHeader` / `ResolveReply`）、`internal/session/server.go`（`resolve` ストリームの受け口）、`internal/session/client.go`（`Resolve` メソッド）、`internal/agent/agent.go`（`Handler.Resolve` の実装）
- Test: `internal/agent/resolve_test.go`, `internal/session/session_test.go` に追記

**Interfaces:**
- Produces:
  ```go
  // proto
  type ResolveHeader struct {
      Name  string `json:"name"`
      QType string `json:"qtype"`  // "A" only in v1
  }
  type ResolveReply struct {
      OK    bool     `json:"ok"`
      Addrs []string `json:"addrs,omitempty"`
      TTL   int      `json:"ttl,omitempty"`
      Error string   `json:"error,omitempty"`
  }
  // session: Handler gains
  Resolve(ctx context.Context, name string) (addrs []string, ttl int, err error)
  // session: Client gains
  func (c *Client) Resolve(ctx context.Context, name string) ([]string, int, error)
  // agent
  func (h *handler) Resolve(ctx context.Context, name string) ([]string, int, error)  // net.Resolver（タスクの resolv.conf）で A を引く
  ```
  `session.Handler` にメソッドを足すと既存の実装（`internal/agent` の `handler`、テストの `fakeHandler`）を直す必要がある。テストの fake にも足す。
  agent 側の TTL は固定 `30`（Go の `net.Resolver` は TTL を返さない。spec §7 の `ttl` は目安値でよい）。

- [ ] **Step 1: 失敗するテストを書く**

`internal/agent/resolve_test.go`:

```go
package agent

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestResolveUsesTheInjectedResolver(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	a.SetResolver(func(ctx context.Context, name string) ([]net.IPAddr, error) {
		if name != "api.myapp.internal" {
			t.Errorf("name = %q", name)
		}
		return []net.IPAddr{{IP: net.ParseIP("10.0.11.229")}, {IP: net.ParseIP("10.0.12.7")}}, nil
	})
	h := &handler{a: a}
	addrs, ttl, err := h.Resolve(context.Background(), "api.myapp.internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 2 || addrs[0] != "10.0.11.229" || addrs[1] != "10.0.12.7" {
		t.Fatalf("addrs = %v", addrs)
	}
	if ttl <= 0 {
		t.Fatalf("ttl = %d", ttl)
	}
}

func TestResolveSkipsIPv6AndReportsNotFound(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	a.SetResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("fd00::1")}}, nil
	})
	h := &handler{a: a}
	if _, _, err := h.Resolve(context.Background(), "v6.only"); err == nil || !strings.Contains(err.Error(), "no IPv4") {
		t.Fatalf("err = %v", err)
	}
}
```

`internal/session/session_test.go` に追記（`fakeHandler` に `Resolve` を足すのも同じ差分で）:

```go
func TestResolveThroughSession(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), addrs: []string{"10.0.0.7"}, ttl: 30}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	addrs, ttl, err := c.Resolve(context.Background(), "api.myapp.internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0] != "10.0.0.7" || ttl != 30 {
		t.Fatalf("addrs=%v ttl=%d", addrs, ttl)
	}
	if h.resolved != "api.myapp.internal" {
		t.Fatalf("the agent saw %q", h.resolved)
	}
}

func TestResolveFailurePropagates(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), resolveErr: errors.New("NXDOMAIN")}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.Resolve(context.Background(), "nope.internal"); err == nil || !strings.Contains(err.Error(), "NXDOMAIN") {
		t.Fatalf("err = %v", err)
	}
}
```

（`fakeHandler` に `addrs []string`, `ttl int`, `resolveErr error`, `resolved string` を足し、`Resolve` を実装する。`errors` と `strings` の import を足す。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/agent/ ./internal/session/`
Expected: FAIL（`SetResolver` / `Resolve` が無い）

- [ ] **Step 3: 実装**

`internal/proto/messages.go` に追加:

```go
// ResolveHeader is the first line of a resolve stream.
type ResolveHeader struct {
	Name  string `json:"name"`
	QType string `json:"qtype"`
}

// ResolveReply is the agent's answer on a resolve stream.
type ResolveReply struct {
	OK    bool     `json:"ok"`
	Addrs []string `json:"addrs,omitempty"`
	TTL   int      `json:"ttl,omitempty"`
	Error string   `json:"error,omitempty"`
}
```

`internal/session/server.go`:

- `Handler` に `Resolve(ctx context.Context, name string) (addrs []string, ttl int, err error)` を足す
- `serveStream` の `switch typ` に `case proto.TypeResolve:` を足す:
  ```go
  	case proto.TypeResolve:
  		var hd proto.ResolveHeader
  		if err := proto.Unmarshal(raw, &hd); err != nil {
  			enc.Encode(proto.TypeResolve, proto.ResolveReply{Error: err.Error()})
  			return
  		}
  		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
  		addrs, ttl, err := h.Resolve(rctx, hd.Name)
  		cancel()
  		if err != nil {
  			enc.Encode(proto.TypeResolve, proto.ResolveReply{Error: err.Error()})
  			return
  		}
  		enc.Encode(proto.TypeResolve, proto.ResolveReply{OK: true, Addrs: addrs, TTL: ttl})
  ```
  （`resolve` はリプライを返したらストリームを閉じる。`defer s.Close()` は既にある。）

`internal/session/client.go` に追加:

```go
// Resolve asks the agent to resolve name with the task's resolver.
func (c *Client) Resolve(ctx context.Context, name string) ([]string, int, error) {
	s, err := c.mux.OpenStream()
	if err != nil {
		return nil, 0, fmt.Errorf("session: open stream: %w", err)
	}
	defer s.Close()
	if err := proto.NewEncoder(s).Encode(proto.TypeResolve, proto.ResolveHeader{Name: name, QType: "A"}); err != nil {
		return nil, 0, err
	}
	if dl, ok := ctx.Deadline(); ok {
		s.SetReadDeadline(dl)
	} else {
		s.SetReadDeadline(time.Now().Add(10 * time.Second))
	}
	_, rawReply, err := proto.ReadHeader(s)
	if err != nil {
		return nil, 0, fmt.Errorf("session: resolve %s: %w", name, err)
	}
	var reply proto.ResolveReply
	if err := proto.Unmarshal(rawReply, &reply); err != nil {
		return nil, 0, err
	}
	if !reply.OK {
		return nil, 0, fmt.Errorf("resolve %s via agent: %s", name, reply.Error)
	}
	return reply.Addrs, reply.TTL, nil
}
```

`internal/agent/resolve.go`:

```go
package agent

import (
	"context"
	"fmt"
	"net"
)

// resolveTTL is what the CLI's resolver reports to its callers. Go's
// net.Resolver does not surface the record's TTL, and the value only decides
// how long a child caches an answer for a name that lives in the VPC, so a
// short fixed value is enough (spec §7).
const resolveTTL = 30

// SetResolver replaces the lookup a resolve stream performs. Production
// leaves it unset and the agent uses the task's own resolver, which is the
// VPC resolver; tests substitute a stub.
func (a *Agent) SetResolver(lookup func(ctx context.Context, name string) ([]net.IPAddr, error)) {
	a.lookup = lookup
}

// Resolve implements session.Handler: look the name up with the task's
// resolv.conf, which is what makes Cloud Map names and private hosted zones
// resolvable from the laptop (spec §3.4).
func (h *handler) Resolve(ctx context.Context, name string) ([]string, int, error) {
	lookup := h.a.lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	ips, err := lookup(ctx, name)
	if err != nil {
		return nil, 0, err
	}
	var out []string
	for _, ip := range ips {
		if v4 := ip.IP.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	if len(out) == 0 {
		return nil, 0, fmt.Errorf("%s has no IPv4 address", name)
	}
	return out, resolveTTL, nil
}
```

`internal/agent/agent.go` の `Agent` に `lookup func(ctx context.Context, name string) ([]net.IPAddr, error)` を足す。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./internal/proto/ ./internal/session/ ./internal/agent/ && make lint && make test && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS（`internal/cli` 側に `session.Handler` の fake があればそれも直す）

- [ ] **Step 5: Commit**

```bash
git add internal/proto internal/session internal/agent
git commit -m "feat(agent): resolve names with the task's resolver over a resolve stream"
```

---

### Task 5: CLI の DNS リゾルバと `/etc/resolver` の配線

**Files:**
- Create: `internal/dnsproxy/dnsproxy.go`
- Test: `internal/dnsproxy/dnsproxy_test.go`
- Modify: `internal/cli/run.go`（`remote_domains` があればリゾルバを起動し `resolver.set`）、`internal/cli/run_e2e_test.go` に追記

**Interfaces:**
- Consumes: `session.Client.Resolve`（Task 4）、`HelperClient.ResolverSet/ResolverClear`（Task 1）
- Produces:
  ```go
  type Resolver func(ctx context.Context, name string) (addrs []string, ttl int, err error)
  type Server struct {
      Resolve Resolver
      Logf    func(string, ...any)
  }
  // DefaultPort is the port spec §3.4 names; the helper writes it into
  // /etc/resolver/<domain>.
  const DefaultPort = 53530
  // Start starts a UDP and a TCP DNS server on 127.0.0.1:port and serves
  // until ctx is done. port 0 picks a free port; Addr reports what was
  // bound. StartPreferring(ctx, DefaultPort) falls back to a free port when
  // DefaultPort is taken (a leftover process from a killed session).
  func (s *Server) Start(ctx context.Context, port int) (addr netip.AddrPort, err error)
  func (s *Server) StartPreferring(ctx context.Context, port int) (netip.AddrPort, error)
  func (s *Server) Close() error
  ```
  A レコードだけ答える。それ以外の qtype は `NOTIMP`、解決失敗は `SERVFAIL`、IPv4 が無い名前は `NXDOMAIN`。

- [ ] **Step 1: 失敗するテストを書く**

`internal/dnsproxy/dnsproxy_test.go`:

```go
package dnsproxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func start(t *testing.T, r Resolver) netip.AddrPort {
	t.Helper()
	s := &Server{Resolve: r, Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Close() })
	addr, err := s.Start(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

func query(t *testing.T, addr netip.AddrPort, name string, qtype uint16, proto string) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	c := &dns.Client{Net: proto, Timeout: 5 * time.Second}
	resp, _, err := c.Exchange(m, addr.String())
	if err != nil {
		t.Fatalf("%s query: %v", proto, err)
	}
	return resp
}

func TestAnswersAOverUDPAndTCP(t *testing.T) {
	addr := start(t, func(_ context.Context, name string) ([]string, int, error) {
		if name != "api.myapp.internal" {
			return nil, 0, errors.New("unexpected " + name)
		}
		return []string{"10.0.11.229"}, 30, nil
	})
	for _, proto := range []string{"udp", "tcp"} {
		resp := query(t, addr, "api.myapp.internal", dns.TypeA, proto)
		if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
			t.Fatalf("%s: rcode=%d answer=%v", proto, resp.Rcode, resp.Answer)
		}
		a, ok := resp.Answer[0].(*dns.A)
		if !ok || a.A.String() != "10.0.11.229" || a.Hdr.Ttl != 30 {
			t.Fatalf("%s: answer = %v", proto, resp.Answer[0])
		}
	}
}

func TestNonAQueriesAndFailures(t *testing.T) {
	addr := start(t, func(_ context.Context, name string) ([]string, int, error) {
		if name == "broken.internal" {
			return nil, 0, errors.New("agent said no")
		}
		return []string{"10.0.0.1"}, 30, nil
	})
	if resp := query(t, addr, "api.myapp.internal", dns.TypeAAAA, "udp"); resp.Rcode != dns.RcodeNotImplemented {
		t.Errorf("AAAA rcode = %d, want NOTIMP", resp.Rcode)
	}
	if resp := query(t, addr, "broken.internal", dns.TypeA, "udp"); resp.Rcode != dns.RcodeServerFailure {
		t.Errorf("failure rcode = %d, want SERVFAIL", resp.Rcode)
	}
}

func TestStartPreferringFallsBackWhenThePortIsTaken(t *testing.T) {
	// A leftover resolver from a killed session must not stop the next run.
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	taken := pc.LocalAddr().(*net.UDPAddr).Port

	s := &Server{Resolve: func(context.Context, string) ([]string, int, error) { return []string{"10.0.0.1"}, 30, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Close() })
	addr, err := s.StartPreferring(ctx, taken)
	if err != nil {
		t.Fatal(err)
	}
	if int(addr.Port()) == taken {
		t.Fatalf("bound the taken port %d", taken)
	}
	if resp := query(t, addr, "api.myapp.internal", dns.TypeA, "udp"); resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d", resp.Rcode)
	}
}

func TestStartUsesLoopbackOnly(t *testing.T) {
	addr := start(t, func(context.Context, string) ([]string, int, error) { return []string{"10.0.0.1"}, 30, nil })
	if !addr.Addr().IsLoopback() {
		t.Fatalf("the resolver must bind loopback, got %s", addr)
	}
	if _, err := net.DialTimeout("udp", addr.String(), time.Second); err != nil {
		t.Fatal(err)
	}
}
```

（import に `net/netip` を足す。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go get github.com/miekg/dns@latest && go test ./internal/dnsproxy/`
Expected: FAIL（`undefined: Server`）

- [ ] **Step 3: 実装**

`internal/dnsproxy/dnsproxy.go`:

```go
// Package dnsproxy answers DNS queries for the domains that only the VPC can
// resolve. macOS sends a child's lookups through mDNSResponder, which pf
// cannot scope to a process, so the names are routed per-domain by
// /etc/resolver files pointing here, and this server forwards each question
// to the agent (spec §3.4).
package dnsproxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/miekg/dns"
)

// Resolver answers one name, normally session.Client.Resolve.
type Resolver func(ctx context.Context, name string) (addrs []string, ttl int, err error)

// Server is the loopback resolver. Only A records are answered: the captured
// set is IPv4-only in v1, so handing a child an AAAA would send it somewhere
// tetherd cannot carry.
type Server struct {
	Resolve Resolver
	Logf    func(string, ...any)

	mu   sync.Mutex
	udp  *dns.Server
	tcp  *dns.Server
	addr netip.AddrPort
}

// DefaultPort is the port the helper writes into /etc/resolver/<domain>
// (spec §3.4). It is fixed so a stale resolver file still points somewhere
// predictable, and so doctor can say what to look for.
const DefaultPort = 53530

// StartPreferring binds port if it is free and any free port otherwise: a
// session killed hard can leave a resolver holding DefaultPort, and the
// helper is told whichever port was actually bound, so the fallback is
// invisible to the child.
func (s *Server) StartPreferring(ctx context.Context, port int) (netip.AddrPort, error) {
	addr, err := s.Start(ctx, port)
	if err == nil || port == 0 {
		return addr, err
	}
	return s.Start(ctx, 0)
}

// Start binds 127.0.0.1:port for UDP and TCP (port 0 picks a free one) and
// serves until ctx is done.
func (s *Server) Start(ctx context.Context, port int) (netip.AddrPort, error) {
	pc, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return netip.AddrPort{}, err
	}
	bound := pc.LocalAddr().(*net.UDPAddr).Port
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", bound))
	if err != nil {
		pc.Close()
		return netip.AddrPort{}, err
	}
	h := dns.HandlerFunc(s.handle)
	s.mu.Lock()
	s.udp = &dns.Server{PacketConn: pc, Handler: h}
	s.tcp = &dns.Server{Listener: ln, Handler: h}
	s.addr = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(bound))
	udp, tcp := s.udp, s.tcp
	s.mu.Unlock()

	go func() {
		if err := udp.ActivateAndServe(); err != nil {
			s.logf("dns udp: %v", err)
		}
	}()
	go func() {
		if err := tcp.ActivateAndServe(); err != nil {
			s.logf("dns tcp: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s.addr, nil
}

// Close stops both listeners. It is safe to call more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	udp, tcp := s.udp, s.tcp
	s.udp, s.tcp = nil, nil
	s.mu.Unlock()
	if udp != nil {
		udp.Shutdown()
	}
	if tcp != nil {
		tcp.Shutdown()
	}
	return nil
}

func (s *Server) handle(w dns.ResponseWriter, req *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(req)
	if len(req.Question) != 1 {
		resp.Rcode = dns.RcodeFormatError
		w.WriteMsg(resp)
		return
	}
	q := req.Question[0]
	if q.Qtype != dns.TypeA || q.Qclass != dns.ClassINET {
		// Anything but an A record is out of scope: tetherd only carries IPv4.
		resp.Rcode = dns.RcodeNotImplemented
		w.WriteMsg(resp)
		return
	}
	name := q.Name
	addrs, ttl, err := s.Resolve(context.Background(), trimDot(name))
	if err != nil {
		s.logf("dns %s: %v", trimDot(name), err)
		resp.Rcode = dns.RcodeServerFailure
		w.WriteMsg(resp)
		return
	}
	if len(addrs) == 0 {
		resp.Rcode = dns.RcodeNameError
		w.WriteMsg(resp)
		return
	}
	if ttl <= 0 {
		ttl = 30
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			continue
		}
		resp.Answer = append(resp.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: uint32(ttl)},
			A:   ip.To4(),
		})
	}
	if len(resp.Answer) == 0 {
		resp.Rcode = dns.RcodeNameError
	}
	w.WriteMsg(resp)
}

func trimDot(name string) string {
	if len(name) > 1 && name[len(name)-1] == '.' {
		return name[:len(name)-1]
	}
	return name
}

// Addr reports the bound address (zero before Start).
func (s *Server) Addr() netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
```

`internal/cli/run.go` の透過モードのブロックに、`✓ network` の行の後、IAM 確認の前に足す:

```go
		// DNS: names that only the VPC resolver knows (Cloud Map, private
		// hosted zones). macOS routes them per-domain through
		// /etc/resolver files that point at this loopback resolver, which
		// forwards each question to the agent (spec §3.4).
		dnsStatus := "local"
		if len(opts.RemoteDomains) > 0 {
			dsrv := &dnsproxy.Server{Resolve: sess.Resolve, Logf: logf}
			daddr, err := dsrv.StartPreferring(ctx, dnsproxy.DefaultPort)
			if err != nil {
				return 1, fmt.Errorf("start the DNS resolver: %w", err)
			}
			defer dsrv.Close()
			if err := hc.ResolverSet(opts.RemoteDomains, int(daddr.Port())); err != nil {
				return 1, fmt.Errorf("point %s at the agent: %w", strings.Join(opts.RemoteDomains, ", "), err)
			}
			defer hc.ResolverClear()
			dnsStatus = fmt.Sprintf("local (+ %s via the VPC resolver on 127.0.0.1:%d)", strings.Join(opts.RemoteDomains, ", "), daddr.Port())
		}
		logf("✓ network  transparent (pf rdr, gid tetherd) · remote: %s · DNS: %s", joinPrefixes(cidrs), dnsStatus)
```

（既存の `✓ network` の行はこの 1 行に置き換える。`--no-network` のときは `remote_domains` があっても何もしない。）

`internal/cli/run_e2e_test.go` に追記:

```go
func TestRunPointsRemoteDomainsAtTheAgent(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{
		region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"},
		vpc:    []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: ag.addr,
	}
	hcFake := &fakeHelperClient{}
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return hcFake, nil }
	d.NewCapturer = func(HelperClient, func(string, ...any)) Capturer { return &fakeCapturer{} }

	opts := ssmOpts("true")
	opts.NoNetwork = false
	opts.ExecPath = "/usr/bin/true"
	opts.RemoteDomains = []string{"myapp.internal"}
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, d); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if len(hcFake.domains) != 1 || hcFake.domains[0] != "myapp.internal" {
		t.Fatalf("resolver.set was called with %v", hcFake.domains)
	}
	if !strings.Contains(out.String(), "myapp.internal via the VPC resolver") {
		t.Errorf("the status line must say what is routed: %s", out.String())
	}
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./... && make lint && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/dnsproxy internal/cli
git commit -m "feat(dns): resolve VPC-only domains through the agent"
```

---

### Task 6: `tetherd env`

**Files:**
- Create: `internal/cli/env.go`, `internal/cli/env_test.go`
- Modify: `internal/transport/transport.go`（`Task.DefinitionARN`）、`internal/provider/ecs/discover.go`（`DefinitionARN` を埋める）、`internal/provider/ecs/secrets.go`（新規）、`internal/cli/run.go`（`awsProvider` に `SecretNames`）、`internal/cli/root.go`（`newEnvCommand` を登録）
- Test: `internal/provider/ecs/secrets_test.go`, `internal/cli/env_test.go`

**Interfaces:**
- Consumes: Task 1 の `Deps` / `awsProvider`、`run.go` の `resolveTaskEnv` / `checkTargetEnv`、Task 2 の `applyConfig`
- Produces:
  ```go
  // provider/ecs
  type TaskDefAPI interface {
      DescribeTaskDefinition(ctx context.Context, in *awsecs.DescribeTaskDefinitionInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTaskDefinitionOutput, error)
  }
  // SecretNames returns the env var names the task definition sources from
  // Secrets Manager or SSM, across all containers.
  func SecretNames(ctx context.Context, api TaskDefAPI, definitionARN string) (map[string]bool, error)
  func PIDMode(ctx context.Context, api TaskDefAPI, definitionARN string) (string, error)

  // cli
  type EnvOptions struct {
      RunOptions        // 同じ探索・接続のフラグを共有する
      Format string     // "dotenv" | "json" | "shell"
      Reveal bool
  }
  func EnvRun(ctx context.Context, opts EnvOptions, stdout, stderr io.Writer) (int, error)
  func EnvRunWithDeps(ctx context.Context, opts EnvOptions, stdout, stderr io.Writer, d Deps) (int, error)
  func FormatEnv(w io.Writer, vars map[string]string, secrets map[string]bool, format string, reveal bool) error
  ```
  `awsProvider` に足すメソッド:
  ```go
  SecretNames(ctx context.Context, definitionARN string) (map[string]bool, error)
  PIDMode(ctx context.Context, definitionARN string) (string, error)   // Task 7 が使う
  ```
  マスクは `***`（secret の値を長さも含めて漏らさない）。`--reveal` で実値。

- [ ] **Step 1: 失敗するテストを書く**

`internal/provider/ecs/secrets_test.go`:

```go
package ecs

import (
	"context"
	"testing"

	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/aws"
)

type fakeTaskDef struct {
	in  string
	out *types.TaskDefinition
	err error
}

func (f *fakeTaskDef) DescribeTaskDefinition(_ context.Context, in *awsecs.DescribeTaskDefinitionInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeTaskDefinitionOutput, error) {
	f.in = aws.ToString(in.TaskDefinition)
	if f.err != nil {
		return nil, f.err
	}
	return &awsecs.DescribeTaskDefinitionOutput{TaskDefinition: f.out}, nil
}

func def() *types.TaskDefinition {
	return &types.TaskDefinition{
		PidMode: types.PidModeTask,
		ContainerDefinitions: []types.ContainerDefinition{
			{
				Name: aws.String("app"),
				Secrets: []types.Secret{
					{Name: aws.String("DATABASE_PASSWORD")},
					{Name: aws.String("API_KEY")},
				},
			},
			{Name: aws.String("tetherd-agent")},
		},
	}
}

func TestSecretNames(t *testing.T) {
	api := &fakeTaskDef{out: def()}
	got, err := SecretNames(context.Background(), api, "arn:aws:ecs:...:task-definition/api:7")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["DATABASE_PASSWORD"] || !got["API_KEY"] {
		t.Fatalf("got %v", got)
	}
	if api.in != "arn:aws:ecs:...:task-definition/api:7" {
		t.Errorf("asked for %q", api.in)
	}
}

func TestPIDMode(t *testing.T) {
	got, err := PIDMode(context.Background(), &fakeTaskDef{out: def()}, "arn")
	if err != nil {
		t.Fatal(err)
	}
	if got != "task" {
		t.Fatalf("pidMode = %q", got)
	}
}
```

`internal/cli/env_test.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kyosu-1/tetherd/internal/transport"
)

func TestFormatEnvMasksSecretsByDefault(t *testing.T) {
	vars := map[string]string{"PORT": "8080", "DATABASE_PASSWORD": "hunter2"}
	secrets := map[string]bool{"DATABASE_PASSWORD": true}

	var b strings.Builder
	if err := FormatEnv(&b, vars, secrets, "dotenv", false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "hunter2") {
		t.Fatalf("the secret leaked: %s", out)
	}
	if !strings.Contains(out, "DATABASE_PASSWORD=***") || !strings.Contains(out, "PORT=8080") {
		t.Fatalf("dotenv = %q", out)
	}
	// Sorted, so the output is stable enough to diff between runs.
	if strings.Index(out, "DATABASE_PASSWORD") > strings.Index(out, "PORT") {
		t.Errorf("keys must be sorted: %q", out)
	}

	b.Reset()
	if err := FormatEnv(&b, vars, secrets, "dotenv", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "DATABASE_PASSWORD=hunter2") {
		t.Fatalf("--reveal must print the value: %q", b.String())
	}
}

func TestFormatEnvQuotesAndFormats(t *testing.T) {
	vars := map[string]string{"MSG": "a b'c", "N": "1"}

	var b strings.Builder
	if err := FormatEnv(&b, vars, nil, "shell", true); err != nil {
		t.Fatal(err)
	}
	// Safe to paste into a shell: the quote inside the value must survive.
	if !strings.Contains(b.String(), `export MSG='a b'"'"'c'`) {
		t.Fatalf("shell = %q", b.String())
	}

	b.Reset()
	if err := FormatEnv(&b, vars, map[string]bool{"MSG": true}, "json", false); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("json = %q: %v", b.String(), err)
	}
	if got["MSG"] != "***" || got["N"] != "1" {
		t.Fatalf("json = %v", got)
	}

	if err := FormatEnv(&b, vars, nil, "yaml", false); err == nil {
		t.Error("an unknown format must be rejected")
	}
}

func TestEnvRunPrintsTheTaskEnvironment(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "8080", "API_KEY": "s3cret"}, nil, nil)
	p := &fakeProvider{
		region:    "ap-northeast-1",
		task:      transport.Task{ID: "t1", SubnetID: "subnet-a", DefinitionARN: "arn:def"},
		agentAddr: ag.addr,
		secrets:   map[string]bool{"API_KEY": true},
	}
	opts := EnvOptions{RunOptions: ssmOpts(), Format: "dotenv"}
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v logs=%s", code, err, logs.String())
	}
	if !strings.Contains(out.String(), "PORT=8080") || !strings.Contains(out.String(), "API_KEY=***") {
		t.Fatalf("stdout = %q", out.String())
	}
	if strings.Contains(out.String(), "s3cret") {
		t.Fatal("the secret leaked to stdout")
	}
	if p.secretsARN != "arn:def" {
		t.Errorf("SecretNames was asked about %q", p.secretsARN)
	}
	// The status chatter belongs on stderr so `eval "$(tetherd env)"` works.
	if strings.Contains(out.String(), "tetherd ") {
		t.Errorf("stdout must carry only the variables: %q", out.String())
	}
}

func TestEnvRunRefusesAnEnvironmentMismatch(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: ag.addr}
	opts := EnvOptions{RunOptions: ssmOpts(), Format: "dotenv"}
	opts.TargetEnv = "prod" // the in-process agent reports "dev"
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if err == nil || code == 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing may be printed on a refusal: %q", out.String())
	}
}
```

（`startAgentFor` は Task 1 のヘルパ（agent は `TETHERD_ENV=dev` を名乗る）。`fakeProvider` に `secrets map[string]bool` / `secretsARN string` / `pidMode string` を足し、`SecretNames` は `secretsARN` に引数を記録して `secrets` を返す。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/provider/ecs/ ./internal/cli/`
Expected: FAIL（`undefined: SecretNames` / `FormatEnv` / `EnvRunWithDeps` / `DefinitionARN`）

- [ ] **Step 3: 実装**

`internal/transport/transport.go` の `Task` に足す:

```go
	// DefinitionARN is the task definition revision the task runs, used to
	// read the secrets list and pidMode.
	DefinitionARN string
```

`internal/provider/ecs/discover.go`: `DescribeTasks` の結果から `DefinitionARN: aws.ToString(task.TaskDefinitionArn)` を `transport.Task` に埋める。

`internal/provider/ecs/secrets.go`:

```go
package ecs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
)

// TaskDefAPI is the slice of the ECS API that reads a task definition.
type TaskDefAPI interface {
	DescribeTaskDefinition(ctx context.Context, in *awsecs.DescribeTaskDefinitionInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTaskDefinitionOutput, error)
}

// SecretNames returns the names of the variables the task definition sources
// from Secrets Manager or SSM Parameter Store. `tetherd env` masks these by
// default: the values are in the child's environment either way, but printing
// them to a terminal puts them in scrollback and shell history (spec §6.5).
func SecretNames(ctx context.Context, api TaskDefAPI, definitionARN string) (map[string]bool, error) {
	td, err := describe(ctx, api, definitionARN)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range td.ContainerDefinitions {
		for _, s := range c.Secrets {
			if n := aws.ToString(s.Name); n != "" {
				out[n] = true
			}
		}
	}
	return out, nil
}

// PIDMode reports the task definition's pidMode, which must be "task" for the
// agent to read the application container's environment (spec §5.3).
func PIDMode(ctx context.Context, api TaskDefAPI, definitionARN string) (string, error) {
	td, err := describe(ctx, api, definitionARN)
	if err != nil {
		return "", err
	}
	return string(td.PidMode), nil
}

func describe(ctx context.Context, api TaskDefAPI, definitionARN string) (*types.TaskDefinition, error) {
	if definitionARN == "" {
		return nil, fmt.Errorf("no task definition to read")
	}
	out, err := api.DescribeTaskDefinition(ctx, &awsecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(definitionARN),
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeTaskDefinition %s: %w", definitionARN, err)
	}
	if out.TaskDefinition == nil {
		return nil, fmt.Errorf("DescribeTaskDefinition %s returned nothing", definitionARN)
	}
	return out.TaskDefinition, nil
}
```

（`types` は `github.com/aws/aws-sdk-go-v2/service/ecs/types`。）

`internal/cli/env.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// maskedValue is what `tetherd env` prints instead of a secret. It is a fixed
// string so the output does not leak the value's length either.
const maskedValue = "***"

// EnvOptions is `tetherd env`: the same discovery and transport flags as run,
// plus how to print.
type EnvOptions struct {
	RunOptions
	Format string
	Reveal bool
}

// EnvRun prints the task's environment. Variables come from the agent, the
// secret names from the task definition.
func EnvRun(ctx context.Context, opts EnvOptions, stdout, stderr io.Writer) (int, error) {
	return EnvRunWithDeps(ctx, opts, stdout, stderr, Deps{})
}

// EnvRunWithDeps is EnvRun with its AWS and transport dependencies injected.
func EnvRunWithDeps(ctx context.Context, opts EnvOptions, stdout, stderr io.Writer, d Deps) (int, error) {
	d = d.withDefaults()
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, "tetherd  "+format+"\n", args...) }

	prov, task, err := discoverTask(ctx, opts.RunOptions, d, logf)
	if err != nil {
		return 1, err
	}
	sess, err := dialAgent(ctx, opts.RunOptions, d, prov, task)
	if err != nil {
		return 1, err
	}
	defer sess.Close()
	if err := checkTargetEnv(sess.Welcome(), opts.RunOptions); err != nil {
		return 1, err
	}
	taskEnv, container, err := resolveTaskEnv(sess.Welcome(), opts.RunOptions)
	if err != nil {
		return 1, err
	}

	secrets := map[string]bool{}
	if !opts.Reveal {
		// Without the names, every value would have to be masked to stay
		// safe; say so rather than print something misleading.
		secrets, err = prov.SecretNames(ctx, task.DefinitionARN)
		if err != nil {
			return 1, fmt.Errorf("read the task definition to find which variables are secrets: %w\n        (use --reveal to print every value, or grant ecs:DescribeTaskDefinition)", err)
		}
	}
	logf("✓ env      %d variables from container %s (%d masked)", len(taskEnv), container, countMasked(taskEnv, secrets))
	return 0, FormatEnv(stdout, taskEnv, secrets, opts.Format, opts.Reveal)
}

func countMasked(vars map[string]string, secrets map[string]bool) int {
	n := 0
	for k := range vars {
		if secrets[k] {
			n++
		}
	}
	return n
}

// FormatEnv writes vars in the requested format, masking the names in
// secrets unless reveal is set. Keys are sorted so two runs diff cleanly.
func FormatEnv(w io.Writer, vars map[string]string, secrets map[string]bool, format string, reveal bool) error {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	value := func(k string) string {
		if !reveal && secrets[k] {
			return maskedValue
		}
		return vars[k]
	}

	switch format {
	case "dotenv", "":
		for _, k := range keys {
			if _, err := fmt.Fprintf(w, "%s=%s\n", k, value(k)); err != nil {
				return err
			}
		}
		return nil
	case "shell":
		for _, k := range keys {
			if _, err := fmt.Fprintf(w, "export %s=%s\n", k, shellQuote(value(k))); err != nil {
				return err
			}
		}
		return nil
	case "json":
		out := make(map[string]string, len(keys))
		for _, k := range keys {
			out[k] = value(k)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	default:
		return fmt.Errorf("unknown --format %q; use dotenv, shell or json", format)
	}
}

// shellQuote single-quotes a value for a POSIX shell, closing and reopening
// the quote around each embedded quote.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}
```

`internal/cli/run.go` からは、Task 1 で切った接続部分を 2 つの関数として取り出して `Run` と `EnvRun` の両方から呼ぶ:

```go
// discoverTask resolves the provider and the task to attach to, and logs the
// target line.
func discoverTask(ctx context.Context, opts RunOptions, d Deps, logf func(string, ...any)) (awsProvider, transport.Task, error)

// dialAgent opens the transport and completes the control handshake.
func dialAgent(ctx context.Context, opts RunOptions, d Deps, prov awsProvider, task transport.Task) (*session.Client, error)
```

（`Run` の既存の 1〜3 の処理をこの 2 つに移し、`Run` はそれを呼ぶ。`session.Client.Welcome()` は既にある。`awsProvider` に `SecretNames` / `PIDMode` を足し、`sdkProvider` は `ecsprov.SecretNames(ctx, p.ecs, arn)` / `ecsprov.PIDMode(...)` に委譲する。`--transport direct` では `DefinitionARN` が空なので、`SecretNames` は「no task definition to read」で失敗する → メッセージに `--reveal` の案内が出るので十分。）

`internal/cli/root.go`:

```go
	root.AddCommand(newRunCommand(), newEnvCommand())
```

```go
func newEnvCommand() *cobra.Command {
	var opts EnvOptions
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Print the dev task's environment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := applyConfig(cmd, &opts.RunOptions); err != nil {
				return err
			}
			if opts.User == "" {
				opts.User = os.Getenv("USER")
			}
			code, err := envFn(opts)
			if err != nil {
				if code == 0 {
					code = 1
				}
				return &exitError{code: code, err: err}
			}
			return nil
		},
	}
	f := cmd.Flags()
	addTargetFlags(f, &opts.RunOptions) // run と共通の探索・接続フラグ
	f.StringVar(&opts.Format, "format", "dotenv", "output format: dotenv | shell | json")
	f.BoolVar(&opts.Reveal, "reveal", false, "print secret values instead of ***")
	return cmd
}
```

（`newRunCommand` のフラグ定義のうち、`--transport` / `--profile` / `--region` / `--cluster` / `--service` / `--task` / `--env` / `--agent-addr` / `--config` / `--user` を `addTargetFlags(f *pflag.FlagSet, opts *RunOptions)` に切り出し、`run` と `env` の両方で呼ぶ。`run` だけのフラグ（`--remote-cidr` / `--helper-socket` / `--exec-path` / `--no-network` / `--no-env`）は `newRunCommand` に残す。`envFn` は `runFn` と同じ形のテスト差し替え変数。）

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./... && make lint && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS

手で 1 回確認（実機のタスクに対して。`--reveal` は付けない）:

```bash
./bin/tetherd env --profile personal --region ap-northeast-1 --cluster tetherd-dev --service api | head
```

- [ ] **Step 5: Commit**

```bash
git add internal/transport internal/provider/ecs internal/cli
git commit -m "feat(cli): add tetherd env with secrets masked by default"
```

---

### Task 7: `tetherd doctor`

**Files:**
- Create: `internal/doctor/doctor.go`, `internal/doctor/checks.go`, `internal/cli/doctor.go`
- Test: `internal/doctor/doctor_test.go`, `internal/doctor/checks_test.go`, `internal/cli/doctor_test.go`
- Modify: `internal/cli/root.go`（`newDoctorCommand` を登録）

**Interfaces:**
- Consumes: `helper.Dial` / `helper.GroupGID` / `helper.ExecInstallDir` / `helper.ExecName`、Task 1 の `Deps` と `awsProvider`、Task 3 の `Subtract`、`LocalOverlaps`、Task 6 の `PIDMode` / `discoverTask` / `dialAgent`、Task 4 の `session.Client.Resolve`
- Produces:
  ```go
  package doctor

  type Status int
  const (
      OK Status = iota
      Warn
      Fail
  )
  type Result struct {
      Name   string // 左カラムの見出し（固定幅で揃える）
      Status Status
      Detail string // 分かったこと（1 行）
      Next   string // 次に何をするか。OK のときは空
  }
  func Render(w io.Writer, results []Result) int   // 戻り値は Fail の数

  // 判定は純粋関数に切り出す。事実の取得は CLI 側、判定はここ。
  func CheckHelper(protocol string, dialErr error) Result
  func CheckExecSetgid(path string, mode fs.FileMode, fileGID int, groupGID int, groupFound bool, statErr error) Result
  func CheckPlugin(path string, lookErr error) Result
  func CheckIdentity(arn string, err error) Result
  func CheckTask(task transport.Task, err error) Result
  func CheckPIDMode(mode string, err error) Result
  func CheckOverlap(overlaps []string) Result
  func CheckRemoteCIDRs(cidrs []netip.Prefix) Result
  func CheckDomains(domains []string, resolved map[string]error) Result
  ```
  終了コードは Fail が 1 つでもあれば 1、Warn だけなら 0。

- [ ] **Step 1: 失敗するテストを書く**

`internal/doctor/doctor_test.go`:

```go
package doctor

import (
	"strings"
	"testing"
)

func TestRenderShowsTheNextStepForFailuresOnly(t *testing.T) {
	var b strings.Builder
	failed := Render(&b, []Result{
		{Name: "helper", Status: OK, Detail: "protocol 1", Next: "never shown"},
		{Name: "setgid tetherd-exec", Status: Fail, Detail: "mode 0755", Next: "sudo tetherd-helper install"},
		{Name: "remote_cidrs", Status: Warn, Detail: "0.0.0.0/0 is routed", Next: "narrow it"},
	})
	if failed != 1 {
		t.Fatalf("failed = %d", failed)
	}
	out := b.String()
	if !strings.Contains(out, "✓ helper") || strings.Contains(out, "never shown") {
		t.Errorf("an OK row must not print a next step: %q", out)
	}
	if !strings.Contains(out, "✗ setgid tetherd-exec") || !strings.Contains(out, "sudo tetherd-helper install") {
		t.Errorf("a failure must print what to do: %q", out)
	}
	if !strings.Contains(out, "! remote_cidrs") || !strings.Contains(out, "narrow it") {
		t.Errorf("a warning must print what to do: %q", out)
	}
}

func TestRenderCountsNoFailures(t *testing.T) {
	var b strings.Builder
	if failed := Render(&b, []Result{{Name: "a", Status: OK}, {Name: "b", Status: Warn, Next: "x"}}); failed != 0 {
		t.Fatalf("failed = %d", failed)
	}
}
```

`internal/doctor/checks_test.go`:

```go
package doctor

import (
	"errors"
	"io/fs"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/transport"
)

func TestCheckHelper(t *testing.T) {
	if r := CheckHelper("1", nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	r := CheckHelper("", errors.New("tetherd-helper is not running (/var/run/tetherd.sock): dial unix: connect: no such file"))
	if r.Status != Fail || !strings.Contains(r.Next, "tetherd-helper install") {
		t.Errorf("got %+v", r)
	}
}

func TestCheckExecSetgid(t *testing.T) {
	const g = 309
	if r := CheckExecSetgid("/usr/local/libexec/tetherd/tetherd-exec", 0o755|fs.ModeSetgid, g, g, true, nil); r.Status != OK {
		t.Errorf("a 2755 root:tetherd binary is fine: %+v", r)
	}
	// The whole capture depends on the setgid bit: without it the child runs
	// with the developer's gid and pf never matches it.
	r := CheckExecSetgid("/x", 0o755, g, g, true, nil)
	if r.Status != Fail || !strings.Contains(r.Detail, "setgid") {
		t.Errorf("got %+v", r)
	}
	if r := CheckExecSetgid("/x", 0o755|fs.ModeSetgid, 20, g, true, nil); r.Status != Fail {
		t.Errorf("the wrong group must fail: %+v", r)
	}
	if r := CheckExecSetgid("/x", 0, 0, 0, false, nil); r.Status != Fail || !strings.Contains(r.Detail, "group") {
		t.Errorf("a missing group must fail: %+v", r)
	}
	if r := CheckExecSetgid("/x", 0, 0, g, true, fs.ErrNotExist); r.Status != Fail {
		t.Errorf("a missing binary must fail: %+v", r)
	}
}

func TestCheckPluginAndIdentity(t *testing.T) {
	if r := CheckPlugin("/opt/homebrew/bin/session-manager-plugin", nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	r := CheckPlugin("", exec.ErrNotFound)
	if r.Status != Fail || !strings.Contains(r.Next, "session-manager-plugin") {
		t.Errorf("got %+v", r)
	}
	if r := CheckIdentity("arn:aws:sts::1:assumed-role/dev/me", nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	if r := CheckIdentity("", errors.New("no valid credential sources")); r.Status != Fail || !strings.Contains(r.Next, "aws") {
		t.Errorf("got %+v", r)
	}
}

func TestCheckTaskAndPIDMode(t *testing.T) {
	task := transport.Task{ID: "abc", StartedAt: time.Now().Add(-time.Hour)}
	if r := CheckTask(task, nil); r.Status != OK || !strings.Contains(r.Detail, "abc") {
		t.Errorf("got %+v", r)
	}
	r := CheckTask(transport.Task{}, errors.New("no attachable task:\n        task abc: enableExecuteCommand is false"))
	if r.Status != Fail || !strings.Contains(r.Detail, "enableExecuteCommand") {
		t.Errorf("the reason must survive: %+v", r)
	}
	if r := CheckPIDMode("task", nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	// Without pidMode: task the agent cannot see the app container's procs,
	// so env injection silently reads the wrong process.
	if r := CheckPIDMode("", nil); r.Status != Fail || !strings.Contains(r.Next, "pidMode") {
		t.Errorf("got %+v", r)
	}
}

func TestCheckOverlapAndRemoteCIDRs(t *testing.T) {
	if r := CheckOverlap(nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	r := CheckOverlap([]string{"en0 10.0.3.14/24 overlaps 10.0.0.0/16"})
	if r.Status != Warn || !strings.Contains(r.Detail, "en0") {
		t.Errorf("got %+v", r)
	}
	if r := CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	// Routing the default route through the task turns the dev ENI into the
	// laptop's internet gateway (spec §4.2).
	if r := CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}); r.Status != Warn {
		t.Errorf("got %+v", r)
	}
}

func TestCheckDomains(t *testing.T) {
	if r := CheckDomains(nil, nil); r.Status != OK || !strings.Contains(r.Detail, "none") {
		t.Errorf("got %+v", r)
	}
	ok := CheckDomains([]string{"myapp.internal"}, map[string]error{"myapp.internal": nil})
	if ok.Status != OK {
		t.Errorf("got %+v", ok)
	}
	bad := CheckDomains([]string{"myapp.internal"}, map[string]error{"myapp.internal": errors.New("NXDOMAIN")})
	if bad.Status != Fail || !strings.Contains(bad.Detail, "myapp.internal") {
		t.Errorf("got %+v", bad)
	}
}
```

`internal/cli/doctor_test.go`:

```go
package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/kyosu-1/tetherd/internal/transport"
)

func TestDoctorRunReportsEveryCheckAndExitsOnFailure(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	p := &fakeProvider{
		region:    "ap-northeast-1",
		task:      transport.Task{ID: "t1", SubnetID: "subnet-a", DefinitionARN: "arn:def"},
		agentAddr: ag.addr,
		pidMode:   "task",
		identity:  "arn:aws:sts::1:assumed-role/dev/me",
	}
	d := depsFor(p)
	// No helper on a test machine: the check must fail loudly, not panic.
	d.DialHelper = func(string) (HelperClient, error) { return nil, errNoHelperForTest }

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), DoctorOptions{RunOptions: ssmOpts()}, &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("a failing check must exit 1, got %d", code)
	}
	for _, want := range []string{"helper", "task", "pidMode", "AWS identity", "remote CIDRs"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q row:\n%s", want, out.String())
		}
	}
}
```

（`errNoHelperForTest` はテスト内の `errors.New(...)`。`fakeProvider` に `pidMode` / `identity` を足す。`awsProvider` に `Identity(ctx) (string, error)` を追加。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/doctor/ ./internal/cli/`
Expected: FAIL（`no required module provides package .../internal/doctor` と `undefined: DoctorRunWithDeps`）

- [ ] **Step 3: 実装**

`internal/doctor/doctor.go`:

```go
// Package doctor checks that a laptop and a dev service are set up for
// tetherd, and says what to do about whatever is not (spec §6.5). Every
// decision here is a pure function of facts the caller gathered, so the
// judgements are unit-tested without a helper, an ECS service or root.
package doctor

import (
	"fmt"
	"io"
)

// Status is how a check came out.
type Status int

const (
	// OK means nothing to do.
	OK Status = iota
	// Warn means tetherd works but something deserves attention.
	Warn
	// Fail means tetherd will not work until it is fixed.
	Fail
)

func (s Status) mark() string {
	switch s {
	case OK:
		return "✓"
	case Warn:
		return "!"
	default:
		return "✗"
	}
}

// Result is one check's outcome.
type Result struct {
	Name   string
	Status Status
	Detail string
	Next   string
}

// Render prints the results and returns how many failed. Next steps print
// only where there is something to do, so a healthy machine is quiet.
func Render(w io.Writer, results []Result) int {
	width := 0
	for _, r := range results {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	failed := 0
	for _, r := range results {
		if r.Status == Fail {
			failed++
		}
		fmt.Fprintf(w, "%s %-*s  %s\n", r.Status.mark(), width, r.Name, r.Detail)
		if r.Status != OK && r.Next != "" {
			fmt.Fprintf(w, "  %-*s  → %s\n", width, "", r.Next)
		}
	}
	return failed
}
```

`internal/doctor/checks.go`:

```go
package doctor

import (
	"fmt"
	"io/fs"
	"net/netip"
	"strings"

	"github.com/kyosu-1/tetherd/internal/transport"
)

// CheckHelper reports whether the root helper answered and speaks the
// protocol this CLI expects.
func CheckHelper(protocol string, dialErr error) Result {
	r := Result{Name: "helper"}
	if dialErr != nil {
		r.Status = Fail
		r.Detail = dialErr.Error()
		r.Next = "sudo tetherd-helper install  (then: sudo launchctl kickstart -k system/dev.tetherd.helper)"
		return r
	}
	r.Detail = "answered, protocol " + protocol
	return r
}

// CheckExecSetgid reports whether tetherd-exec can put a child in the
// tetherd group. Without the setgid bit the child keeps the developer's gid
// and the pf rules never match it (spec §3.2).
func CheckExecSetgid(path string, mode fs.FileMode, fileGID, groupGID int, groupFound bool, statErr error) Result {
	r := Result{Name: "setgid tetherd-exec", Next: "sudo tetherd-helper install"}
	if !groupFound {
		r.Status = Fail
		r.Detail = "the tetherd group does not exist"
		return r
	}
	if statErr != nil {
		r.Status = Fail
		r.Detail = fmt.Sprintf("%s: %v", path, statErr)
		return r
	}
	if mode&fs.ModeSetgid == 0 {
		r.Status = Fail
		r.Detail = fmt.Sprintf("%s is mode %04o, not setgid", path, mode.Perm())
		return r
	}
	if fileGID != groupGID {
		r.Status = Fail
		r.Detail = fmt.Sprintf("%s is setgid to gid %d, not the tetherd group (gid %d)", path, fileGID, groupGID)
		return r
	}
	r.Detail = fmt.Sprintf("%s is setgid to the tetherd group (gid %d)", path, groupGID)
	r.Next = ""
	return r
}

// CheckPlugin reports whether the SSM session-manager-plugin is installed;
// the ssm transport runs it as a subprocess (spec §6.1).
func CheckPlugin(path string, lookErr error) Result {
	r := Result{Name: "session-manager-plugin"}
	if lookErr != nil {
		r.Status = Fail
		r.Detail = "not on PATH"
		r.Next = "brew install --cask session-manager-plugin"
		return r
	}
	r.Detail = path
	return r
}

// CheckIdentity reports whether AWS credentials resolve.
func CheckIdentity(arn string, err error) Result {
	r := Result{Name: "AWS identity"}
	if err != nil {
		r.Status = Fail
		r.Detail = err.Error()
		r.Next = "authenticate for the profile in .tetherd.yml (for example: aws sso login --profile <name>)"
		return r
	}
	r.Detail = arn
	return r
}

// CheckTask reports whether a task is attachable: ECS Exec enabled, the
// agent container present and its ExecuteCommandAgent running. Discover
// already explains every rejection, so the reasons are passed through.
func CheckTask(task transport.Task, err error) Result {
	r := Result{Name: "attachable task"}
	if err != nil {
		r.Status = Fail
		r.Detail = err.Error()
		r.Next = "enable ECS Exec on the service and deploy the tetherd-agent sidecar (see docs/dev-env.md)"
		return r
	}
	r.Detail = fmt.Sprintf("%s (started %s)", task.ID, task.StartedAt.Format("2006-01-02 15:04"))
	return r
}

// CheckPIDMode reports whether the task definition shares a pid namespace,
// which the agent needs to read the application container's environment
// (spec §5.3).
func CheckPIDMode(mode string, err error) Result {
	r := Result{Name: "pidMode"}
	if err != nil {
		r.Status = Fail
		r.Detail = err.Error()
		r.Next = "grant ecs:DescribeTaskDefinition, or run with --no-env"
		return r
	}
	if mode != "task" {
		r.Status = Fail
		r.Detail = fmt.Sprintf("the task definition sets pidMode %q", mode)
		r.Next = `set "pidMode": "task" on the task definition, or run with --no-env`
		return r
	}
	r.Detail = "task"
	return r
}

// CheckOverlap reports local interfaces whose addresses fall inside the
// captured set: traffic to those addresses would go to the VPC instead of
// the LAN (spec §11).
func CheckOverlap(overlaps []string) Result {
	r := Result{Name: "local addresses"}
	if len(overlaps) == 0 {
		r.Detail = "no interface overlaps the captured set"
		return r
	}
	r.Status = Warn
	r.Detail = strings.Join(overlaps, "; ")
	r.Next = "add the overlapping range to local_cidrs in .tetherd.yml"
	return r
}

// CheckRemoteCIDRs warns about a captured set wide enough to send the
// laptop's whole internet path through the dev task.
func CheckRemoteCIDRs(cidrs []netip.Prefix) Result {
	r := Result{Name: "remote CIDRs"}
	var wide []string
	for _, p := range cidrs {
		if p.Bits() <= 8 && !p.Addr().IsPrivate() {
			wide = append(wide, p.String())
		}
	}
	if len(wide) > 0 {
		r.Status = Warn
		r.Detail = strings.Join(wide, ", ") + " is captured: every connection goes through the dev task"
		r.Next = "list only the ranges you need in remote_cidrs / remote_services"
		return r
	}
	r.Detail = joinPrefixes(cidrs)
	return r
}

// CheckDomains reports whether every configured remote domain resolves
// through the agent.
func CheckDomains(domains []string, resolved map[string]error) Result {
	r := Result{Name: "remote domains"}
	if len(domains) == 0 {
		r.Detail = "none configured"
		return r
	}
	var bad []string
	for _, d := range domains {
		if err := resolved[d]; err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", d, err))
		}
	}
	if len(bad) > 0 {
		r.Status = Fail
		r.Detail = strings.Join(bad, "; ")
		r.Next = "check the name exists in the VPC (Cloud Map or a private hosted zone) and that remote_domains matches it"
		return r
	}
	r.Detail = strings.Join(domains, ", ") + " resolve through the agent"
	return r
}

func joinPrefixes(cidrs []netip.Prefix) string {
	if len(cidrs) == 0 {
		return "none"
	}
	out := make([]string, 0, len(cidrs))
	for _, p := range cidrs {
		out = append(out, p.String())
	}
	return strings.Join(out, ", ")
}
```

`internal/cli/doctor.go`:

```go
package cli

import (
	"context"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/kyosu-1/tetherd/internal/doctor"
	"github.com/kyosu-1/tetherd/internal/helper"
)

// DoctorOptions is `tetherd doctor`: the same target flags as run.
type DoctorOptions struct {
	RunOptions
}

// DoctorRun checks the local setup and the dev service.
func DoctorRun(ctx context.Context, opts DoctorOptions, stdout io.Writer) (int, error) {
	return DoctorRunWithDeps(ctx, opts, stdout, Deps{})
}

// DoctorRunWithDeps is DoctorRun with its dependencies injected.
//
// Each check gathers its facts and hands them to a pure judgement in
// internal/doctor. A check that cannot run because an earlier one failed
// still prints a row saying so — a doctor that stops at the first problem
// makes the developer run it once per problem.
func DoctorRunWithDeps(ctx context.Context, opts DoctorOptions, stdout io.Writer, d Deps) (int, error) {
	d = d.withDefaults()
	var results []doctor.Result

	// 1. The root helper.
	hc, herr := d.DialHelper(opts.HelperSocket)
	if herr == nil {
		defer hc.Close()
	}
	results = append(results, doctor.CheckHelper(helper.ProtocolVersion, herr))

	// 2. The tetherd group and the setgid shim.
	gid, groupFound, gerr := helper.GroupGID(runCommand, helper.GroupName)
	if gerr != nil {
		groupFound = false
	}
	execPath := opts.ExecPath
	if execPath == "" {
		execPath = filepath.Join(helper.ExecInstallDir, helper.ExecName)
	}
	mode, fileGID, serr := statGID(execPath)
	results = append(results, doctor.CheckExecSetgid(execPath, mode, fileGID, gid, groupFound, serr))

	// 3. The SSM plugin (only the ssm transport needs it).
	if opts.Transport == "ssm" {
		path, lerr := exec.LookPath("session-manager-plugin")
		results = append(results, doctor.CheckPlugin(path, lerr))
	}

	// 4. AWS: credentials, then the task, then the task definition.
	prov, perr := d.NewAWSProvider(ctx, opts.RunOptions)
	if perr != nil {
		results = append(results, doctor.CheckIdentity("", perr))
		return doctor.Render(stdout, results), nil
	}
	arn, ierr := prov.Identity(ctx)
	results = append(results, doctor.CheckIdentity(arn, ierr))

	task, terr := prov.Discover(ctx, ecsTarget(opts.RunOptions))
	results = append(results, doctor.CheckTask(task, terr))
	if terr == nil {
		mode, merr := prov.PIDMode(ctx, task.DefinitionARN)
		results = append(results, doctor.CheckPIDMode(mode, merr))
	}

	// 5. The captured set: what it contains, and what it collides with.
	var cidrs []netip.Prefix
	if terr == nil {
		cidrs, _ = remoteSet(ctx, opts.RunOptions, prov, task, func(string, ...any) {})
	}
	results = append(results, doctor.CheckRemoteCIDRs(cidrs))
	if addrs, err := net.InterfaceAddrs(); err == nil {
		results = append(results, doctor.CheckOverlap(LocalOverlaps(cidrs, addrs)))
	}

	// 6. The remote domains, asked of the agent itself.
	resolved := map[string]error{}
	if len(opts.RemoteDomains) > 0 && terr == nil {
		if sess, err := dialAgent(ctx, opts.RunOptions, d, prov, task); err == nil {
			defer sess.Close()
			for _, name := range opts.RemoteDomains {
				_, _, rerr := sess.Resolve(ctx, name)
				resolved[name] = rerr
			}
		} else {
			for _, name := range opts.RemoteDomains {
				resolved[name] = err
			}
		}
	}
	results = append(results, doctor.CheckDomains(opts.RemoteDomains, resolved))

	return doctor.Render(stdout, results), nil
}

// statGID reports a file's mode and owning group.
func statGID(path string) (fs.FileMode, int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.Mode(), 0, nil
	}
	return fi.Mode(), int(st.Gid), nil
}

// runCommand is what helper.GroupGID shells out with (dscl on macOS).
func runCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
```

（`remoteSet` と `ecsTarget` は Task 3 で、`discoverTask` / `dialAgent` は Task 6 で `Run` から切り出した関数をそのまま使う。`awsProvider` に `Identity(ctx) (string, error)` を足し、`sdkProvider` は `sts.NewFromConfig(p.cfg).GetCallerIdentity` で実装する。`github.com/aws/aws-sdk-go-v2/service/sts` は既に go.mod の直接依存にある。）

`internal/cli/root.go`:

```go
	root.AddCommand(newRunCommand(), newEnvCommand(), newDoctorCommand())
```

```go
func newDoctorCommand() *cobra.Command {
	var opts DoctorOptions
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine and the dev service are set up for tetherd",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := applyConfig(cmd, &opts.RunOptions); err != nil {
				return err
			}
			code, err := doctorFn(opts)
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			if code != 0 {
				// The rows already said what is wrong; exit non-zero without
				// adding a message.
				return &exitError{code: 1, child: true}
			}
			return nil
		},
	}
	addTargetFlags(cmd.Flags(), &opts.RunOptions)
	cmd.Flags().StringVar(&opts.HelperSocket, "helper-socket", helper.DefaultSocket, "tetherd-helper socket")
	cmd.Flags().StringVar(&opts.ExecPath, "exec-path", helper.ExecInstallDir+"/"+helper.ExecName, "path of the setgid tetherd-exec")
	return cmd
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./... && make lint && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS

実機で 1 回:

```bash
./bin/tetherd doctor --profile personal --region ap-northeast-1 --cluster tetherd-dev --service api
```

helper が動いていれば全行 `✓`（`remote domains` は `none configured`）。

- [ ] **Step 5: Commit**

```bash
git add internal/doctor internal/cli go.mod go.sum
git commit -m "feat(cli): add tetherd doctor"
```

---

### Task 8: ドキュメントと e2e チェックリスト

**Files:**
- Create: `docs/config.md`
- Modify: `docs/e2e-aws.md`（6 行目を差し替え、`env` / `doctor` / `remote_services` / `remote_domains` の行を追加）、`docs/specs/2026-09-12-v1-macos-design.md`（§6.5 の doctor 項目に「ターゲットグループの HTTP1 検査は v0.3」を明記、§12 に v0.2b の検証枠を追加）、`examples/sampleapp` に `.tetherd.yml` の例（`examples/.tetherd.yml`）

**Interfaces:**
- Consumes: Task 2〜7 の実装（フラグ名・設定キー・出力の形）
- Produces: なし（ドキュメントのみ）

- [ ] **Step 1: `docs/config.md` を書く**

内容（実装に合わせて正確に書く。推測で書かない — 各キーは `internal/config/config.go` の struct タグと 1 対 1 で対応させる）:

1. 2 つのファイル（`.tetherd.yml` = リポジトリにコミット、`~/.tetherd/config.yml` = 個人・gitignore）とその優先順位（フラグ > 個人 > 共有 > 既定）
2. `.tetherd.yml` の全キーを、spec §6.7 のサンプルをそのまま載せた上でキーごとに 1 行説明
3. `~/.tetherd/config.yml` は初回 `tetherd run` が 0600 で生成し、`token` は `tetherd token rotate`（v0.3）で入れ替える
4. リモート集合の組み立て方を 1 段落: VPC CIDR + `169.254.170.0/24` + `remote_cidrs` + `remote_services` の prefix list − `local_cidrs`
5. `remote_domains` を書いたときに何が起きるか（`/etc/resolver/<domain>` が `127.0.0.1:<port>` を指し、その問い合わせが agent の resolver に行く。`tetherd run` の間だけ。落ちても `resolver.clear` で消える）
6. `env.override` / `env.exclude` と、tetherd が常に除外するもの（`internal/env` の 3 つのリストを名前で列挙し、なぜ除外するかを 1 行ずつ）

- [ ] **Step 2: `docs/e2e-aws.md` を更新**

- 冒頭の `RUN=` を `.tetherd.yml` 前提に変える（`examples/.tetherd.yml` をリポジトリ直下にコピーして `RUN="./bin/tetherd run"` で動くことを 1 行目の検証にする）
- 6 行目（`api.myapp.internal` が解けない）を、解けることの検証に差し替える:

  | 6 | `$RUN -- curl -s http://api.myapp.internal:8081/` | `sampleapp on … from 10.0.x.x` | `remote_domains: [myapp.internal]` で Cloud Map の名前が解け、その IP が捕捉範囲に入って agent 経由で届く。`/etc/resolver/myapp.internal` が run 中だけ存在し、終了後に消えること（`ls /etc/resolver`）も見る |

- 行を追加:

  | 11 | `./bin/tetherd env \| head` と `./bin/tetherd env --format json \| jq -r '.DB_PASSWORD'` | 変数一覧が出て、`DB_PASSWORD` は `***` | secrets はタスク定義の `secrets` ブロックから判定してマスクされる（`--reveal` で実値） |
  | 12 | `./bin/tetherd doctor` | 全行 `✓`（`remote domains` は設定していれば `myapp.internal … resolve through the agent`）、exit 0 | doctor が helper・setgid・plugin・認証・タスク・pidMode・捕捉範囲・ドメインを見ている |
  | 13 | `.tetherd.yml` の `service` を存在しない名前にして `./bin/tetherd doctor` | `✗ attachable task` に理由が出て exit 1、他の行は出続ける | 1 つ失敗しても残りの検査が走る |
  | 14 | `remote_services: [s3]` を足して `$RUN -- sh -c 'aws s3 ls && curl -s -o /dev/null -w "%{http_code}" https://example.com'` | `aws s3 ls` が成功し `example.com` も 200 | prefix list 由来の CIDR が捕捉範囲に入り、それ以外はラップトップから直接出る |
  | 15 | `local_cidrs` に自宅 LAN の range を足して `./bin/tetherd doctor` | `local addresses` が `✓`（足す前は `!` で en0 が出る） | `local_cidrs` が捕捉範囲から引かれている |

  （14 の `remote_services: [s3]` は S3 の prefix list が数百 prefix あるので、`✓ network` 行の prefix 数が増えることも見る。）

- [ ] **Step 3: spec を更新**

- §6.5 の doctor 項目の末尾に足す: 「ターゲットグループが HTTP1 かの検査は v0.3（`elasticloadbalancing:DescribeTargetGroups` を developer policy に足す必要があるため）」
- §12 に `### v0.2b（設定ファイル・DNS・env・doctor）` の枠を作り、Task 8 の e2e 実施後に結果を埋める旨を 1 行書いておく（結果自体は実行後に書く）

- [ ] **Step 4: `examples/.tetherd.yml` を書く**

`deploy/dev-env` に向けた、そのまま動く例:

```yaml
# tetherd の検証環境（deploy/dev-env）に向けた設定例。
# リポジトリ直下に .tetherd.yml としてコピーすると `tetherd run -- <cmd>` だけで動く。
version: 1
aws:
  profile: personal
  region: ap-northeast-1
target:
  cluster: tetherd-dev
  service: api
  container: app
  env: dev
network:
  remote_domains: [myapp.internal]   # Cloud Map の名前を agent 側で解決する
env:
  exclude: []
```

- [ ] **Step 5: e2e を実施して結果を記録**

`docs/e2e-aws.md` の 15 行を上から実行し、spec §12 の v0.2b 枠に結果を書く（各行「何が確認できたか」を 1 行、実測値つき）。失敗した行は原因と修正コミットまで書く。**`terraform apply` / `plan` / `destroy` は実行しない**（検証環境は既に動いている）。

- [ ] **Step 6: Commit**

```bash
git add docs examples
git commit -m "docs: document the config file and extend the AWS e2e checklist"
```

---

## 完了条件

1. `go test -race ./...` が通り、`make lint` / `GOOS=linux go vet ./...` / `make build` も通る
2. リポジトリ直下に `.tetherd.yml` を置けば `tetherd run -- <cmd>` がフラグ無しで動く（e2e 1 行目）
3. `remote_domains` に書いたドメインが子プロセスから解け、その IP に agent 経由で届く（e2e 6 行目）
4. `tetherd env` が変数を出し、タスク定義の secrets をマスクする（e2e 11 行目）
5. `tetherd doctor` が 8 項目を検査し、失敗しても残りを続け、Fail があれば exit 1（e2e 12・13 行目）
6. `remote_services` / `local_cidrs` が捕捉範囲に反映される（e2e 14・15 行目）
7. spec §12 に v0.2b の検証結果が記録されている
8. `Run` が `Deps` 経由で依存を受け取り、`internal/cli` が実プロセスの agent 相手にユニットテストできる（Task 1）

## 次の計画（v0.3、この計画には含めない）

- 逆方向（`--no-incoming` を外す）: ALB ヘッダマッチ + agent の `http` ストリーム + `local_port`
- `tetherd status` / `tetherd token rotate`
- 配布: Homebrew tap、`sudo tetherd-helper install`、署名と notarization、README
- doctor のターゲットグループ HTTP1 検査（developer policy に `elasticloadbalancing:DescribeTargetGroups` を足す）
- 持ち越し: `taskRoleReachable` の呼び出しを 1 変数に、`AWSContainerVars` に `AWS_CONTAINER_CREDENTIALS_FULL_URI` / `AWS_CONTAINER_AUTHORIZATION_TOKEN{,_FILE}` を追加、`ListTasks` のページング、`freePort` の TOCTOU、plugin の stdout を捨てている点
