# tetherd v0.3a — 認証エンドポイントのルート固定と steal 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `169.254.170.2` の捕捉が間欠的に失敗する穴を閉じ、ALB に届いたリクエストのうちヘッダーとトークンが一致したものだけがラップトップの `localhost:<local_port>` に落ちてくる状態にする（spec §5.1、§5.2、§3.6、§7）。

**Architecture:** 2 つの独立した仕事を 1 つのマイルストーンにまとめる。前半は helper に `route.set` / `route.clear` を足して、セッション中だけ `169.254.170.2` の host route を lo0 に向ける（`connect()` のルート探索が pf より先に走るため、拒否ルートが生きていると pf がパケットを一度も見ない）。後半が steal で、yamux が対称であることを使って **agent 側からストリームを開く**経路を初めて作る。agent は常時 HTTP/1.1 リバースプロキシとして `:8080` に立ち、一致しないリクエストは app の `:8081` へ素通しする。

**Tech Stack:** Go 1.27、標準ライブラリの `net/http` + `httputil.ReverseProxy`、既存の yamux / cobra / aws-sdk-go-v2。**新規依存なし**。

**Spec:** `docs/specs/2026-09-12-v1-macos-design.md`。特に §3.6（helper の操作）、§5.1（L7 プロキシ）、§5.2（steal）、§5.4（セッション管理）、§6.3（run の流れ）、§7（制御プロトコル）、§11（安全策）。

## Global Constraints

- モジュール `github.com/kyosu-1/tetherd`、Go 1.27。**新規依存を入れない**（`go mod tidy` が no-op のまま）
- helper に足す操作は `route.set` / `route.clear` の 2 つだけ。コマンドを起動する操作は増やさない（spec §11）。`ProtocolVersion` を `"1"` → `"2"` に上げ、CLI と helper のバージョン不一致を `helper.Dial` が拒否する（既存の `VersionError` 経路）
- steal の一致条件は `X-Dev-User == user` かつ `X-Dev-Token == token`。**比較は定数時間**（`crypto/subtle.ConstantTimeCompare`）。ヘッダー名は `incoming.match.header` / `token_header` で変更可（spec §5.2）
- ヘルスチェック（ヘッダーが無い）は常に app へ。WebSocket の upgrade は app にそのまま通し、steal しない（spec §5.1）
- `X-Forwarded-*` / `traceparent` / `X-Amzn-Trace-Id` は素通し（spec §5.1）
- フォールバック: ラップトップへの dial が失敗したらそのリクエストを app へ。ボディは **1 MiB** までバッファしてリプレイ可能にし、超えるものは dial 失敗時に 502（spec §5.2）
- リクエストログは **CLI 側のハンドラ**が出す。agent からログは送らない（spec §5.2）
- 誰も繋いでいなければ完全な素通し。セッション断で即時に素通しへ復帰（spec §11）
- agent は AWS API を呼ばない
- コミットメッセージ `<type>: <summary>`。進行管理の語（"fix round" など）を件名に入れない。TDD
- **実機の検証環境が動いている**。`terraform plan` / `fmt` / `validate` は可。`terraform apply` はコントローラが人間に依頼する（Task 6）
- テストは AWS 認証・ネットワーク（ループバック以外）・root・helper なしで通ること

## ファイル構成

```
internal/helper/wire.go          route.set / route.clear の追加、ProtocolVersion を 2 に
internal/helper/route.go         host route の追加と削除（新規。route(8) を exec）
internal/helper/server.go        2 操作の受け口と、接続断時の後片付けに route.clear を追加
internal/helper/client.go        RouteSet / RouteClear
internal/cli/deps.go             HelperClient に RouteSet / RouteClear
internal/cli/run.go              セッション中だけ credential endpoint の host route を張る
internal/session/server.go       Handler.Hello に Opener を渡す（agent 側からストリームを開けるように）
internal/session/client.go       受信ストリームの accept ループと Options.OnHTTP
internal/agent/registry.go       セッションごとに token / incoming / opener を持つ（agent.go から分離）
internal/agent/steal.go          一致判定（純粋関数、定数時間比較）
internal/agent/proxy.go          :8080 の L7 リバースプロキシと app フォールバック
internal/agent/agent.go          プロキシの起動と登録の配線
cmd/tetherd-agent/main.go        :8080 の listen 開始
internal/cli/steal.go            http ストリームを http.Serve し localhost:<local_port> へ
internal/cli/root.go             --no-incoming / --local-port / --as
deploy/dev-env/{alb,ecs}.tf      ターゲットグループを agent の 8080 へ、sg と portMappings
docs/*                           spec §12 の枠、e2e チェックリスト、config.md の incoming
```

---

### Task 1: helper の `route.set` / `route.clear` と認証エンドポイントのルート固定

**Files:**
- Create: `internal/helper/route.go`, `internal/helper/route_test.go`
- Modify: `internal/helper/wire.go`（2 操作と `ProtocolVersion = "2"`）、`internal/helper/server.go`（受け口と後片付け）、`internal/helper/client.go`（`RouteSet` / `RouteClear`）、`internal/cli/deps.go`（`HelperClient`）、`internal/cli/run.go`（セッション中だけ張る）
- Test: `internal/helper/route_test.go`, `internal/helper/helper_test.go` に追記, `internal/cli/run_e2e_test.go` に追記

**Interfaces:**
- Produces:
  ```go
  // helper/wire.go
  const ProtocolVersion = "2"
  const (
      OpRouteSet   = "route.set"
      OpRouteClear = "route.clear"
  )
  // request gains:
  //   Hosts []string `json:"hosts,omitempty"`  // route.set: 単一アドレス（/32）のみ

  // helper/route.go
  type Router struct {
      Run func(name string, args ...string) ([]byte, error) // 既定は exec.Command(...).CombinedOutput
      set []netip.Addr                                      // 張ったもの。clear で消す
  }
  func (r *Router) Set(hosts []netip.Addr) error   // route add -host <h> -interface lo0
  func (r *Router) Clear() error                   // 張ったものだけ route delete
  func (r *Router) Active() []netip.Addr

  // helper/wire.go — Platform gains two methods (this is how every other
  // root-only operation is abstracted; DarwinPlatform is the real one and
  // fakePlatform in helper_test.go is the test double)
  //   RouteSet(hosts []netip.Addr) error
  //   RouteClear() error

  // helper/platform_darwin.go — DarwinPlatform gets a `route Router` field
  // and delegates to it.

  // helper/client.go
  func (c *Client) RouteSet(hosts []netip.Addr) error
  func (c *Client) RouteClear() error

  // cli/deps.go — HelperClient に追加
  RouteSet(hosts []netip.Addr) error
  RouteClear() error
  ```
- Consumes: 既存の `Server` の 1 セッション制と接続断時 `cleanup()`、`ecsprov.TaskRoleAddr`（`169.254.170.2`）

**なぜこれが要るか**（実装者向け。ここを誤解すると直し方を間違える）:
`connect()` のルート探索は pf の `pass out route-to lo0` より**先**に走る。macOS は `169.254.170.2` への ARP を LAN に投げて失敗し、en0 上に拒否ルートを残す（`netstat -rn` の `!`、`route get` の `LLINFO` と負の `expire`）。それが生きている間はカーネルが `EHOSTUNREACH` を即返し、**pf はパケットを一度も見ない**。実機で、同じ子プロセス・同じ gid なのに `curl` は 200 を得て AWS CLI 同梱の python は `Errno 65` で落ちた。lo0 への host route を張れば探索が必ず成功し、`rdr pass on lo0` が拾う。

- [ ] **Step 1: 失敗するテストを書く**

`internal/helper/route_test.go`:

```go
package helper

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

type fakeRun struct {
	calls [][]string
	err   map[string]error
}

func (f *fakeRun) run(name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	if e, ok := f.err[strings.Join(call, " ")]; ok {
		return []byte("boom"), e
	}
	return nil, nil
}

func TestRouterSetAddsAHostRouteToLoopback(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run}
	if err := r.Set([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	want := "route -n add -host 169.254.170.2 -interface lo0"
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != want {
		t.Fatalf("calls = %v, want %q", f.calls, want)
	}
	if got := r.Active(); len(got) != 1 || got[0].String() != "169.254.170.2" {
		t.Fatalf("active = %v", got)
	}
}

func TestRouterClearRemovesOnlyWhatItAdded(t *testing.T) {
	f := &fakeRun{}
	r := &Router{Run: f.run}
	if err := r.Set([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := r.Clear(); err != nil {
		t.Fatal(err)
	}
	want := "route -n delete -host 169.254.170.2"
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != want {
		t.Fatalf("calls = %v, want %q", f.calls, want)
	}
	if len(r.Active()) != 0 {
		t.Fatalf("active after clear = %v", r.Active())
	}
	// 二度目の Clear は何もしない。helper の接続断後片付けは毎回呼ばれる。
	f.calls = nil
	if err := r.Clear(); err != nil || len(f.calls) != 0 {
		t.Fatalf("second clear ran %v (err %v)", f.calls, err)
	}
}

func TestRouterSetRollsBackOnPartialFailure(t *testing.T) {
	// 2 つ目が失敗したら 1 つ目も消す。残すと、そのアドレスへの通信が
	// セッション終了後も lo0 に吸い込まれ続ける。
	f := &fakeRun{err: map[string]error{
		"route -n add -host 169.254.170.3 -interface lo0": errors.New("exit 1"),
	}}
	r := &Router{Run: f.run}
	err := r.Set([]netip.Addr{
		netip.MustParseAddr("169.254.170.2"),
		netip.MustParseAddr("169.254.170.3"),
	})
	if err == nil {
		t.Fatal("a failed add must be reported")
	}
	if !strings.Contains(err.Error(), "169.254.170.3") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("the error must name the address and what route said: %v", err)
	}
	last := strings.Join(f.calls[len(f.calls)-1], " ")
	if last != "route -n delete -host 169.254.170.2" {
		t.Errorf("the successful add must be rolled back, last call = %q", last)
	}
	if len(r.Active()) != 0 {
		t.Errorf("active = %v, want empty after a rolled-back Set", r.Active())
	}
}

func TestRouterSetRejectsAnythingButALinkLocalHost(t *testing.T) {
	// 任意のアドレスを lo0 に吸い込める操作にはしない。root の helper が
	// 受ける操作なので、範囲は「タスクロールのエンドポイント 1 個」に絞る。
	r := &Router{Run: (&fakeRun{}).run}
	for _, bad := range []string{"10.0.0.1", "0.0.0.0", "8.8.8.8", "fd00::1"} {
		if err := r.Set([]netip.Addr{netip.MustParseAddr(bad)}); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}
```

`internal/helper/helper_test.go` に追記。**既存のテストの形に合わせること**: `startServer(t, allow func(Peer) bool) (sock string, fp *fakePlatform)` が `Server{Platform: fp, ...}` を一時ソケットで起動して `fakePlatform` を返す。`fakePlatform` に `RouteSet` / `RouteClear` を足し（`routes []netip.Addr` と `routeCleared int` を記録、`snapshot()` と同じ形のアクセサを用意）、往復はそれで確認する:

```go
func TestRouteSetAndClearOverTheSocket(t *testing.T) {
	sock, fp := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	if got := fp.routeSnapshot(); len(got) != 1 || got[0].String() != "169.254.170.2" {
		t.Fatalf("platform saw %v", got)
	}
	if err := c.RouteClear(); err != nil {
		t.Fatal(err)
	}
	if got := fp.routeSnapshot(); len(got) != 0 {
		t.Fatalf("after clear the platform still has %v", got)
	}
	c.Close()
}

func TestDisconnectClearsTheRouteToo(t *testing.T) {
	// The CLI dying is the case this matters for: a host route left pointing
	// at lo0 would swallow that address with nothing listening.
	sock, fp := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	waitFor(t, func() bool { return len(fp.routeSnapshot()) == 0 }, "the route to be cleared on disconnect")
}

func TestRouteSetRefusesASecondSession(t *testing.T) {
	// Same one-session rule as pf.apply: two CLIs cannot both pin routes.
	sock, _ := startServer(t, func(Peer) bool { return true })
	first, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	second, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	err = second.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")})
	var busy *BusyError
	if err == nil || !errors.As(err, &busy) {
		t.Fatalf("err = %v, want a BusyError", err)
	}
}
```

（`errors` の import を足す。`BusyError` は既存。`waitFor` は既存のヘルパ。）

`internal/cli/run_e2e_test.go` に追記:

```go
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
	// --no-network は helper に一切触らない。ルートも張らない。
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
```

（`fakeHelperClient` に `routes []netip.Addr` / `routeCleared bool` と `RouteSet` / `RouteClear` を足す。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/helper/ ./internal/cli/`
Expected: FAIL（`undefined: Router` / `RouteSet`）

- [ ] **Step 3: 実装**

`internal/helper/wire.go`:

```go
// ProtocolVersion must match between CLI and helper. Bumped to 2 in v0.3a
// with route.set / route.clear: a v0.2 helper does not know them, and a CLI
// that silently skipped pinning the credential endpoint's route would leave
// the task role working only intermittently.
const ProtocolVersion = "2"

const (
	OpRouteSet   = "route.set"
	OpRouteClear = "route.clear"
)
```

`request` 構造体に `Hosts []string \`json:"hosts,omitempty"\`` を足す。

`internal/helper/route.go`:

```go
// Package-level note: connect()'s route lookup runs before pf's output
// rules, so pf cannot rescue a destination the kernel considers
// unreachable. macOS ARPs for 169.254.170.2 on the LAN, gets nothing, and
// leaves a reject host route behind; while that entry is live, connect()
// returns EHOSTUNREACH and the rdr rule never sees a packet. Pinning the
// address to lo0 for the session's duration makes the lookup succeed, and
// `rdr pass on lo0` then does the work.
package helper

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// Router adds and removes host routes. Only the ECS task-role credential
// endpoint is allowed: this is an operation a root daemon accepts, so the
// range it can redirect is deliberately one address, not "any".
type Router struct {
	Run func(name string, args ...string) ([]byte, error)

	set []netip.Addr
}

// allowedHosts is what Set will pin. 169.254.170.2 is the ECS task-role
// credential and task-metadata endpoint (spec §4.1).
var allowedHosts = map[string]bool{"169.254.170.2": true}

func (r *Router) run(args ...string) ([]byte, error) {
	run := r.Run
	if run == nil {
		run = func(name string, a ...string) ([]byte, error) {
			return exec.Command(name, a...).CombinedOutput()
		}
	}
	return run("route", args...)
}

// Set pins each host to lo0. A partial failure is rolled back: a host route
// left behind would keep swallowing that address into lo0 after the session
// ended, with nothing listening.
func (r *Router) Set(hosts []netip.Addr) error {
	for _, h := range hosts {
		if !allowedHosts[h.String()] {
			return fmt.Errorf("route.set: %s is not a host tetherd pins (only 169.254.170.2)", h)
		}
	}
	for _, h := range hosts {
		// -n keeps route(8) from resolving names, which would make this
		// depend on DNS while DNS is being rearranged.
		if out, err := r.run("-n", "add", "-host", h.String(), "-interface", "lo0"); err != nil {
			rollback := r.Clear()
			return fmt.Errorf("route.set %s: %w: %s (rolled back: %v)", h, err, strings.TrimSpace(string(out)), rollback)
		}
		r.set = append(r.set, h)
	}
	return nil
}

// Clear removes the routes this Router added, and nothing else. Safe to call
// more than once: the helper's disconnect cleanup calls it unconditionally.
func (r *Router) Clear() error {
	var firstErr error
	for _, h := range r.set {
		if out, err := r.run("-n", "delete", "-host", h.String()); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("route.clear %s: %w: %s", h, err, strings.TrimSpace(string(out)))
		}
	}
	r.set = nil
	return firstErr
}

// Active reports what is currently pinned.
func (r *Router) Active() []netip.Addr { return append([]netip.Addr(nil), r.set...) }
```

`internal/helper/wire.go` の `Platform` インターフェースに `RouteSet(hosts []netip.Addr) error` と `RouteClear() error` を足す。

`internal/helper/platform_darwin.go` の `DarwinPlatform` に `route Router` フィールドを足し、2 メソッドを `p.route.Set` / `p.route.Clear` に委譲する（`Router.Run` は nil のままで、既定の `exec.Command("route", ...)` が使われる）。`route.go` にはビルドタグを付けない — `resolver.go` と同じく、実行するのは darwin だけだがコード自体は移植性がある。

`internal/helper/server.go`: `OpRouteSet` / `OpRouteClear` を `pf.apply` / `resolver.set` と同じ形で受ける（1 セッション制の検査も同じ）。接続断の `cleanup()` は **resolver → route → pf** の順で消す（resolver が生きている間はルートも要る、という依存の逆順）。

`internal/helper/client.go`:

```go
// RouteSet pins hosts to lo0 for the life of this connection.
func (c *Client) RouteSet(hosts []netip.Addr) error {
	ss := make([]string, 0, len(hosts))
	for _, h := range hosts {
		ss = append(ss, h.String())
	}
	_, err := c.call(request{Op: OpRouteSet, Hosts: ss})
	return err
}

// RouteClear removes them.
func (c *Client) RouteClear() error {
	_, err := c.call(request{Op: OpRouteClear})
	return err
}
```

`internal/cli/run.go`: 捕捉を張るブロックの中、pf を当てる**前**に足す（ルートが先にあれば、pf が当たった瞬間から取りこぼしが無い）:

```go
		// The credential endpoint needs a route before pf can help: see
		// internal/helper/route.go for why.
		if err := hc.RouteSet([]netip.Addr{ecsprov.TaskRoleAddr}); err != nil {
			return 1, fmt.Errorf("pin the route to %s: %w", ecsprov.TaskRoleAddr, err)
		}
		defer hc.RouteClear()
```

`internal/cli/deps.go` の `HelperClient` に 2 メソッドを足す。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./... && make lint && GOOS=linux go vet ./... && make build && go mod tidy && git diff --exit-code go.mod go.sum`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/helper internal/cli
git commit -m "feat(helper): pin the credential endpoint's route to lo0 for the session"
```

---

### Task 2: 双方向ストリーム — agent 側から開く経路

**Files:**
- Modify: `internal/session/server.go`（`Opener` を `Handler.Hello` に渡す）、`internal/session/client.go`（受信ストリームの accept ループ、`Options.OnHTTP`）、`internal/agent/agent.go`（`Hello` のシグネチャ追従）
- Test: `internal/session/session_test.go` に追記

**Interfaces:**
- Produces:
  ```go
  // session
  // Opener opens a new stream toward the peer. The agent uses it to push an
  // http stream to a CLI; nothing else opens streams from that side.
  type Opener interface {
      OpenStream() (net.Conn, error)
  }

  // Handler.Hello gains a third parameter:
  Hello(hello proto.Hello, remote string, open Opener) (proto.Welcome, *proto.Error)

  // Options gains:
  //   OnHTTP func(stream net.Conn)
  // Called on its own goroutine for each inbound stream whose header is
  // proto.TypeHTTP. nil means the CLI does not accept steal (--no-incoming):
  // the stream is answered with proto.TypeError and closed.
  ```
- Consumes: 既存の `Serve` / `Dial`、`proto.TypeHTTP`、`proto.ReadHeader`

**この設計の理由**: yamux は対称なので、どちらの側からでも `OpenStream` できる。v0.2b までは CLI だけが開いていたので、CLI 側に accept ループが無い。steal は agent が開く（ALB からリクエストが来たときに初めて開く）ので、ここを作るのがこのタスク。`Handler.Hello` にパラメータを足すのは実装が 2 つ（`agent.handler` とテストの `fakeHandler`）だけなので、`Resolve` を足したときと同じコスト。

- [ ] **Step 1: 失敗するテストを書く**

`internal/session/session_test.go` に追記:

```go
func TestAgentCanOpenAnHTTPStreamToTheCLI(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})

	got := make(chan string, 1)
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{
		OnHTTP: func(s net.Conn) {
			defer s.Close()
			b := make([]byte, 5)
			io.ReadFull(s, b)
			got <- string(b)
			s.Write([]byte("PONG!"))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// The handler received an Opener at hello time; use it the way the L7
	// proxy will.
	open := h.opener()
	if open == nil {
		t.Fatal("Hello was not given an Opener")
	}
	s, err := open.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("PING!")); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != "PING!" {
			t.Fatalf("the CLI read %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the CLI never saw the stream")
	}
	b := make([]byte, 5)
	if _, err := io.ReadFull(s, b); err != nil || string(b) != "PONG!" {
		t.Fatalf("agent read %q err %v: the stream must carry both directions", b, err)
	}
}

func TestAnHTTPStreamIsRefusedWhenTheCLIDoesNotAcceptSteal(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{})}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s, err := h.opener().OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	proto.NewEncoder(s).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"})
	typ, raw, err := proto.ReadHeader(s)
	if err != nil {
		t.Fatal(err)
	}
	if typ != proto.TypeError {
		t.Fatalf("type = %q, want an error reply", typ)
	}
	var e proto.Error
	proto.Unmarshal(raw, &e)
	if !strings.Contains(e.Message, "incoming") {
		t.Errorf("the refusal must say the CLI is not accepting incoming requests: %q", e.Message)
	}
}

func TestAnUnknownInboundStreamTypeIsRefusedAndDoesNotKillTheSession(t *testing.T) {
	cc, sc := pair(t)
	h := &fakeHandler{closed: make(chan struct{}), addrs: []string{"10.0.0.7"}, ttl: 30}
	go Serve(context.Background(), sc, h, ServeOptions{})
	c, err := Dial(context.Background(), cc, proto.Hello{Version: proto.Version, User: "shota"}, Options{
		OnHTTP: func(s net.Conn) { s.Close() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s, err := h.opener().OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	proto.NewEncoder(s).Encode("mirror", map[string]any{})
	typ, _, err := proto.ReadHeader(s)
	if err != nil {
		t.Fatal(err)
	}
	if typ != proto.TypeError {
		t.Fatalf("type = %q", typ)
	}
	s.Close()
	// The session must still work: a future agent opening a stream type this
	// CLI does not know must not take the run down.
	if _, _, err := c.Resolve(context.Background(), "api.myapp.internal"); err != nil {
		t.Fatalf("the session died after an unknown stream: %v", err)
	}
}
```

（`fakeHandler` に `mu sync.Mutex` / `open Opener` と `opener()` アクセサを足し、`Hello` で受け取った `Opener` を保存する。`proto.HTTPHeader` は Task 4 で定義するが、このタスクで先に `internal/proto/messages.go` に足す — 型が無いとテストが書けない。`import "sync"` を足す。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/session/`
Expected: FAIL（`undefined: Opener` / `Options.OnHTTP` / `proto.HTTPHeader`）

- [ ] **Step 3: 実装**

`internal/proto/messages.go` に足す:

```go
// HTTPHeader is the first line of an http stream, agent -> CLI. The user is
// informational: the agent only ever opens the stream toward that user's
// session, and the CLI has exactly one. It is carried so a stream seen in a
// packet capture or a log says who it was for.
type HTTPHeader struct {
	User string `json:"user"`
}
```

`internal/session/server.go`:

```go
// Opener opens a new stream toward the peer. Serve hands one to the handler
// at hello time so the agent can push an http stream to this CLI when a
// request for that user arrives; nothing else opens streams from the agent
// side.
type Opener interface {
	OpenStream() (net.Conn, error)
}
```

`Handler` の `Hello` を `Hello(hello proto.Hello, remote string, open Opener) (proto.Welcome, *proto.Error)` にし、`Serve` は `mux` を渡す（`*yamux.Session` は `OpenStream() (*yamux.Stream, error)` なので、`net.Conn` を返す小さなアダプタで包む）:

```go
// muxOpener adapts yamux's concrete return type to Opener.
type muxOpener struct{ mux *yamux.Session }

func (m muxOpener) OpenStream() (net.Conn, error) { return m.mux.OpenStream() }
```

`internal/session/client.go`: `Options` に `OnHTTP func(stream net.Conn)` を足し、`Dial` が welcome を受け取った後に accept ループを起動する:

```go
	go c.acceptLoop(opts.OnHTTP)
```

```go
// acceptLoop serves streams the agent opens. Each one is handled on its own
// goroutine: a slow steal must not block the next request, and the session's
// control loop must not be blocked at all.
func (c *Client) acceptLoop(onHTTP func(net.Conn)) {
	for {
		s, err := c.mux.AcceptStream()
		if err != nil {
			return // the session is going away; readLoop reports why
		}
		go c.serveInbound(s, onHTTP)
	}
}

func (c *Client) serveInbound(s net.Conn, onHTTP func(net.Conn)) {
	typ, _, err := proto.ReadHeader(s)
	if err != nil {
		s.Close()
		return
	}
	switch {
	case typ == proto.TypeHTTP && onHTTP != nil:
		onHTTP(s) // owns s, including closing it
	case typ == proto.TypeHTTP:
		proto.NewEncoder(s).Encode(proto.TypeError, proto.Error{
			Code:    proto.CodeBadHello,
			Message: "this session is not accepting incoming requests (--no-incoming)",
		})
		s.Close()
	default:
		// Additive by design: a newer agent may open a stream type this CLI
		// does not know. Refuse that stream and keep the session.
		proto.NewEncoder(s).Encode(proto.TypeError, proto.Error{
			Code:    proto.CodeBadHello,
			Message: "unknown stream type " + typ,
		})
		s.Close()
	}
}
```

`internal/agent/agent.go` の `handler.Hello` をシグネチャに合わせ、受け取った `Opener` を登録時に保存する（保存先は Task 3）。このタスクでは引数を受け取って捨てるだけでよい（`_ session.Opener`）が、コメントで Task 3 が使うと書く。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./... && go test -race -count=5 ./internal/session/ && make lint && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/proto internal/session internal/agent
git commit -m "feat(session): let the agent open streams toward the CLI"
```

---

### Task 3: セッションレジストリと一致判定

**Files:**
- Create: `internal/agent/registry.go`, `internal/agent/steal.go`, `internal/agent/steal_test.go`
- Modify: `internal/agent/agent.go`（レジストリを分離、`Hello` で token / incoming / opener を保存）
- Test: `internal/agent/registry_test.go`, `internal/agent/steal_test.go`

**Interfaces:**
- Consumes: Task 2 の `session.Opener`、`proto.Hello{User, Token, Incoming}`
- Produces:
  ```go
  // registry.go
  // sessions becomes map[string]*Session (it held SessionInfo before): the
  // proxy needs the opener and the token, and Task 4's tests register a
  // *Session directly.
  type Session struct {
      User     string
      From     string
      Since    time.Time
      Incoming proto.Incoming
      token    string        // 比較のみ。ログにも Sessions() にも出さない
      open     session.Opener
  }
  // SessionInfo は Sessions() が返す公開ビュー（token と open を含まない）
  func (a *Agent) register(h proto.Hello, from string, open session.Opener) *proto.Error
  func (a *Agent) unregister(user string)
  func (a *Agent) Sessions() []SessionInfo
  // Match returns the session a request belongs to, or nil.
  func (a *Agent) Match(header func(string) string) *Session

  // steal.go
  // MatchSession picks the session whose user and token both match the
  // request's headers. Returns nil when no session claims the request.
  func MatchSession(sessions []*Session, header func(string) string) *Session
  ```

- [ ] **Step 1: 失敗するテストを書く**

`internal/agent/steal_test.go`:

```go
package agent

import (
	"testing"

	"github.com/kyosu-1/tetherd/internal/proto"
)

func sess(user, token string, enabled bool) *Session {
	return &Session{
		User:     user,
		token:    token,
		Incoming: proto.Incoming{Enabled: enabled, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"},
	}
}

func hdr(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestMatchSessionNeedsBothUserAndToken(t *testing.T) {
	s := sess("shota", "tok-shota", true)
	all := []*Session{s}

	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok-shota"})); got != s {
		t.Errorf("both matching must steal: %v", got)
	}
	// The token is the whole security property on a public ALB: knowing the
	// user name must not be enough (spec §11).
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "shota"})); got != nil {
		t.Errorf("a missing token must not steal: %v", got)
	}
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "wrong"})); got != nil {
		t.Errorf("a wrong token must not steal: %v", got)
	}
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-Token": "tok-shota"})); got != nil {
		t.Errorf("a token without the user must not steal: %v", got)
	}
	// A health check carries neither header.
	if got := MatchSession(all, hdr(nil)); got != nil {
		t.Errorf("a request with no headers must go to the app: %v", got)
	}
}

func TestMatchSessionPicksTheRightUserAmongSeveral(t *testing.T) {
	a, b := sess("shota", "tok-a", true), sess("taro", "tok-b", true)
	all := []*Session{a, b}
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "taro", "X-Dev-Token": "tok-b"})); got != b {
		t.Errorf("got %v, want taro's session", got)
	}
	// taro's name with shota's token belongs to nobody.
	if got := MatchSession(all, hdr(map[string]string{"X-Dev-User": "taro", "X-Dev-Token": "tok-a"})); got != nil {
		t.Errorf("a token from another session must not steal: %v", got)
	}
}

func TestMatchSessionRespectsIncomingDisabledAndCustomHeaders(t *testing.T) {
	off := sess("shota", "tok", false)
	if got := MatchSession([]*Session{off}, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok"})); got != nil {
		t.Errorf("--no-incoming means this session takes nothing: %v", got)
	}
	custom := sess("shota", "tok", true)
	custom.Incoming.Header = "X-Who"
	custom.Incoming.TokenHeader = "X-Secret"
	if got := MatchSession([]*Session{custom}, hdr(map[string]string{"X-Who": "shota", "X-Secret": "tok"})); got != custom {
		t.Errorf("custom header names must be honoured: %v", got)
	}
	if got := MatchSession([]*Session{custom}, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok"})); got != nil {
		t.Errorf("the default names must not also work: %v", got)
	}
}

func TestMatchSessionRejectsAnEmptyToken(t *testing.T) {
	// A session that somehow attached without a token must not be matchable
	// by a request that also sends no token - that would make every
	// header-only request steal.
	s := sess("shota", "", true)
	if got := MatchSession([]*Session{s}, hdr(map[string]string{"X-Dev-User": "shota"})); got != nil {
		t.Errorf("an empty token must never match: %v", got)
	}
	if got := MatchSession([]*Session{s}, hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": ""})); got != nil {
		t.Errorf("an empty token must never match: %v", got)
	}
}
```

`internal/agent/registry_test.go`:

```go
package agent

import (
	"net"
	"strings"
	"testing"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
)

type nopOpener struct{}

func (nopOpener) OpenStream() (net.Conn, error) { return nil, nil }

func TestRegisterKeepsTheTokenOutOfTheVisibleView(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	h := proto.Hello{Version: proto.Version, User: "shota", Token: "s3cret-token",
		Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
	if e := a.register(h, "127.0.0.1:1", nopOpener{}); e != nil {
		t.Fatal(e)
	}
	infos := a.Sessions()
	if len(infos) != 1 || infos[0].User != "shota" {
		t.Fatalf("sessions = %+v", infos)
	}
	// SessionInfo is what `tetherd status` will print and what the agent
	// logs; a token must not be reachable through it.
	if strings.Contains(fmtSessions(infos), "s3cret-token") {
		t.Fatal("the token must not appear in the visible session view")
	}
	if got := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "s3cret-token"})); got == nil {
		t.Fatal("the registered session must be matchable")
	}
	a.unregister("shota")
	if got := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "s3cret-token"})); got != nil {
		t.Fatal("a detached session must stop taking requests")
	}
}

func TestRegisterStoresTheOpenerForTheProxy(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	var open session.Opener = nopOpener{}
	h := proto.Hello{Version: proto.Version, User: "shota", Token: "t",
		Incoming: proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}}
	if e := a.register(h, "127.0.0.1:1", open); e != nil {
		t.Fatal(e)
	}
	s := a.Match(hdr(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "t"}))
	if s == nil || s.open == nil {
		t.Fatal("the proxy needs the opener to reach this CLI")
	}
}

// fmtSessions renders the view the way a log line or `status` would.
func fmtSessions(in []SessionInfo) string {
	var b strings.Builder
	for _, s := range in {
		b.WriteString(s.User + " " + s.From + " " + s.Since.String() + " ")
	}
	return b.String()
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/agent/`
Expected: FAIL（`undefined: Session` / `MatchSession` / `a.Match`）

- [ ] **Step 3: 実装**

`internal/agent/steal.go`:

```go
package agent

import "crypto/subtle"

// MatchSession picks the session a request belongs to: the one whose user
// name and token both match the request's headers. Returns nil when no
// session claims it, which is every health check and every request from a
// user who is not attached - those go to the application.
//
// The token comparison is constant time. The user name's is not, and does
// not need to be: user names are not secret (they travel in a header a
// browser extension sets), while the token is the only thing standing
// between a public ALB and a developer's laptop (spec §5.2, §11).
func MatchSession(sessions []*Session, header func(string) string) *Session {
	for _, s := range sessions {
		if !s.Incoming.Enabled || s.token == "" {
			continue
		}
		if header(s.Incoming.Header) != s.User {
			continue
		}
		got := header(s.Incoming.TokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1 {
			return s
		}
	}
	return nil
}
```

`internal/agent/registry.go`: `agent.go` から `SessionInfo` / `sessions` / `register` / `unregister` / `Sessions` を移し、`Session`（token と open を持つ内部型）と `SessionInfo`（公開ビュー）に分ける。`register` は `proto.Hello` を取り、重複ユーザーの拒否は今のまま。`Match` はロックを取って `MatchSession` に渡す:

```go
// Match returns the session a request belongs to, or nil for the
// application. Held under the lock: a session can detach while a request is
// being routed.
func (a *Agent) Match(header func(string) string) *Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	all := make([]*Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		all = append(all, s)
	}
	return MatchSession(all, header)
}
```

`handler.Hello` は Task 2 で受け取った `Opener` をそのまま `register` に渡す。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./... && make lint && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "feat(agent): match a request to a session by user and token"
```

---

### Task 4: agent の L7 リバースプロキシ（`:8080`）とフォールバック

**Files:**
- Create: `internal/agent/proxy.go`, `internal/agent/proxy_test.go`
- Modify: `internal/agent/config.go`（`Proxy` と `AppAddr`）、`internal/agent/agent.go`（プロキシの起動）、`cmd/tetherd-agent/main.go`（`:8080` の listen）
- Test: `internal/agent/proxy_test.go`

**Interfaces:**
- Consumes: Task 3 の `Agent.Match` と `Session.open`、Task 2 の `proto.HTTPHeader`
- Produces:
  ```go
  // agent/config.go に追加
  //   Proxy   string // TETHERD_PROXY、既定 0.0.0.0:8080。ALB が来る口
  //   AppAddr string // TETHERD_APP_ADDR、既定 127.0.0.1:8081。素通し先

  // agent/proxy.go
  // MaxReplayBody is how much of a request body the proxy buffers so it can
  // be replayed to the application if the laptop turns out to be gone.
  const MaxReplayBody = 1 << 20 // 1 MiB
  type Proxy struct {
      Agent   *Agent
      AppAddr string
      Logf    func(string, ...any)
  }
  func (p *Proxy) Handler() http.Handler
  func (a *Agent) ServeProxy(ctx context.Context, ln net.Listener) error
  ```

**振る舞いの定義**（spec §5.1、§5.2。実装者はこの順で判定する）:
1. `Upgrade` ヘッダーがある（WebSocket など）→ **常に app**。steal しない
2. `Agent.Match` が nil → **app**（ヘルスチェックと、繋いでいない人のリクエストが全部ここ）
3. 一致した → そのセッションのストリームへ。`httputil.ReverseProxy` の `Transport.DialContext` が `open.OpenStream()` してヘッダー行を書く
4. ストリームを開けない / 開いた直後に落ちた → **app にフォールバック**。そのためにボディを `MaxReplayBody` までバッファしてから 3 に入る。超えるサイズのボディは巻き戻せないので、dial 失敗時に 502

- [ ] **Step 1: 失敗するテストを書く**

`internal/agent/proxy_test.go`:

```go
package agent

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// appStub stands in for the application container on :8081.
func appStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s path=%s body=%q upgrade=%q", body, r.URL.Path, string(b), r.Header.Get("Upgrade"))
	}))
	t.Cleanup(s.Close)
	return s
}

// laptop is a session whose OpenStream hands back one end of a pipe; the
// test speaks HTTP/1.1 on the other end, the way the CLI will.
type laptop struct {
	mu      sync.Mutex
	streams []net.Conn
	fail    bool
	handler http.Handler
}

func (l *laptop) OpenStream() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail {
		return nil, fmt.Errorf("the laptop is gone")
	}
	mine, theirs := net.Pipe()
	l.streams = append(l.streams, mine)
	go func() {
		// Read the stream header the proxy writes, then serve HTTP on it.
		typ, raw, err := proto.ReadHeader(mine)
		if err != nil || typ != proto.TypeHTTP {
			mine.Close()
			return
		}
		var h proto.HTTPHeader
		proto.Unmarshal(raw, &h)
		srv := &http.Server{Handler: l.handler}
		srv.Serve(&oneConnListener{c: mine})
	}()
	return theirs, nil
}

type oneConnListener struct {
	c    net.Conn
	done bool
}

// Pointer receiver, and done really is set: a value receiver would hand the
// same connection back on every Accept and http.Server would serve it in a
// loop.
func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.done {
		return nil, io.EOF
	}
	l.done = true
	return l.c, nil
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

func proxyFor(t *testing.T, app string, sessions ...*Session) http.Handler {
	t.Helper()
	a := New(Config{Env: "dev"}, nil)
	for _, s := range sessions {
		a.mu.Lock()
		a.sessions[s.User] = s
		a.mu.Unlock()
	}
	p := &Proxy{Agent: a, AppAddr: app, Logf: t.Logf}
	return p.Handler()
}

func do(t *testing.T, h http.Handler, method, path string, body string, hdrs map[string]string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdrs {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func TestProxyPassesEverythingToTheAppWithNoSession(t *testing.T) {
	app := appStub(t, "APP")
	h := proxyFor(t, app.Listener.Addr().String())
	resp := do(t, h, "GET", "/health", "", nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "APP path=/health") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
}

func TestProxyStealsAMatchingRequestAndPassesTheRest(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "LAPTOP path=%s body=%q xff=%q", r.URL.Path, string(b), r.Header.Get("X-Forwarded-For"))
	})}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.Listener.Addr().String(), s)

	match := map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok", "X-Forwarded-For": "203.0.113.5"}
	resp := do(t, h, "POST", "/api/orders", "hello", match)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `LAPTOP path=/api/orders body="hello"`) {
		t.Fatalf("a matching request must reach the laptop: status=%d body=%s", resp.StatusCode, b)
	}
	// X-Forwarded-* and tracing headers pass through untouched (spec §5.1).
	if !strings.Contains(string(b), `xff="203.0.113.5"`) {
		t.Errorf("X-Forwarded-For must survive: %s", b)
	}

	// A health check on the same proxy still goes to the app.
	resp = do(t, h, "GET", "/health", "", nil)
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "APP path=/health") {
		t.Fatalf("health checks must not be stolen: %s", b)
	}
	// So does the same path with the wrong token.
	resp = do(t, h, "GET", "/api/orders", "", map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "nope"})
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "APP path=/api/orders") {
		t.Fatalf("a wrong token must fall through to the app: %s", b)
	}
}

func TestProxyNeverStealsAnUpgrade(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an upgrade must never reach the laptop")
	})}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.Listener.Addr().String(), s)
	resp := do(t, h, "GET", "/ws", "", map[string]string{
		"X-Dev-User": "shota", "X-Dev-Token": "tok", "Upgrade": "websocket", "Connection": "Upgrade",
	})
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `APP path=/ws`) || !strings.Contains(string(b), `upgrade="websocket"`) {
		t.Fatalf("a websocket upgrade must go to the app untouched: %s", b)
	}
}

func TestProxyFallsBackToTheAppWhenTheLaptopIsGone(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{fail: true}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.Listener.Addr().String(), s)
	resp := do(t, h, "POST", "/api/orders", "important", map[string]string{
		"X-Dev-User": "shota", "X-Dev-Token": "tok",
	})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("a vanished laptop must not fail the request: status=%d body=%s", resp.StatusCode, b)
	}
	// The body must arrive intact: it was already read off the wire before
	// the dial failed, so it has to be replayed.
	if !strings.Contains(string(b), `APP path=/api/orders body="important"`) {
		t.Fatalf("the body must be replayed to the app: %s", b)
	}
}

func TestProxyRefusesToFallBackWithAnUnreplayableBody(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{fail: true}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.Listener.Addr().String(), s)
	big := strings.Repeat("x", MaxReplayBody+1)
	resp := do(t, h, "POST", "/upload", big, map[string]string{
		"X-Dev-User": "shota", "X-Dev-Token": "tok",
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: a body too large to buffer cannot be replayed", resp.StatusCode)
	}
}

func TestProxyServesConcurrentRequestsOnOneSession(t *testing.T) {
	app := appStub(t, "APP")
	l := &laptop{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, "LAPTOP %s", r.URL.Path)
	})}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.Listener.Addr().String(), s)

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	start := time.Now()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := do(t, h, "GET", fmt.Sprintf("/p%d", i), "", map[string]string{
				"X-Dev-User": "shota", "X-Dev-Token": "tok",
			})
			b, _ := io.ReadAll(resp.Body)
			want := fmt.Sprintf("LAPTOP /p%d", i)
			if !strings.Contains(string(b), want) {
				errs <- fmt.Sprintf("got %q, want %q", b, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	// One stream per request, served concurrently: eight 50ms handlers must
	// not take 400ms.
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Errorf("requests were serialised: %s", el)
	}
}

func TestProxyReadsTheStreamHeaderBeforeTheRequest(t *testing.T) {
	// The CLI reads {"type":"http"} first and only then hands the stream to
	// http.Serve; if the proxy wrote the request first the CLI would read a
	// request line as a header and close.
	app := appStub(t, "APP")
	l := &laptop{}
	s := sess("shota", "tok", true)
	s.open = l
	h := proxyFor(t, app.Listener.Addr().String(), s)

	done := make(chan string, 1)
	l.handler = nil // no HTTP server; read the raw bytes instead
	go func() {
		resp := do(t, h, "GET", "/x", "", map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok"})
		io.ReadAll(resp.Body)
	}()
	waitFor(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.streams) > 0
	}, "the proxy never opened a stream")
	l.mu.Lock()
	c := l.streams[0]
	l.mu.Unlock()
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, `"type":"http"`) {
		t.Fatalf("first line = %q, want the http stream header", line)
	}
	req, err := http.ReadRequest(br)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Path != "/x" {
		t.Fatalf("path = %q", req.URL.Path)
	}
	done <- "ok"
}
```

（`waitFor` は Task 1 で足したヘルパと同じもの。`internal/agent` 側にも小さく用意する。`sess` は Task 3 の `steal_test.go` のヘルパ。)

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/agent/`
Expected: FAIL（`undefined: Proxy` / `MaxReplayBody`）

- [ ] **Step 3: 実装**

`internal/agent/proxy.go`:

```go
// The agent is always an HTTP/1.1 reverse proxy on the ALB's port, whether
// anyone is attached or not: the ALB reuses its connections with keep-alive,
// so routing has to be decided per request, and "pass through" is simply
// every request going to the application (spec §5.1).
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// MaxReplayBody is how much of a request body the proxy keeps so it can be
// replayed to the application when the laptop turns out to be gone. Beyond
// it a request cannot be rewound, and a dial failure has to be a 502
// (spec §5.2).
const MaxReplayBody = 1 << 20

// Proxy routes each request either to the application or to the laptop of
// the user whose name and token it carries.
type Proxy struct {
	Agent   *Agent
	AppAddr string
	Logf    func(string, ...any)
}

// Handler is the http.Handler the ALB talks to.
func (p *Proxy) Handler() http.Handler {
	app := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = p.AppAddr
			// Deliberately no SetXForwarded: the ALB already set
			// X-Forwarded-For and friends, and they pass through
			// unchanged (spec §5.1).
		},
		ErrorLog: nil,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An upgrade never gets stolen: the steal path is HTTP/1.1
		// request/response over a yamux stream, and a hijacked connection
		// is not that (spec §5.1).
		if r.Header.Get("Upgrade") != "" {
			app.ServeHTTP(w, r)
			return
		}
		s := p.Agent.Match(r.Header.Get)
		if s == nil {
			app.ServeHTTP(w, r)
			return
		}
		p.steal(w, r, s, app)
	})
}

// steal forwards one request to a laptop, falling back to the application if
// the stream cannot be opened.
func (p *Proxy) steal(w http.ResponseWriter, r *http.Request, s *Session, app http.Handler) {
	// Buffer the body first: once ReverseProxy has read it, falling back
	// would send the application an empty request.
	replay, tooBig, err := bufferBody(r)
	if err != nil {
		http.Error(w, "read the request body", http.StatusBadGateway)
		return
	}

	var dialErr error
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			// The authority is not used to route - the stream already goes
			// to exactly one laptop - but it has to be something valid.
			pr.Out.URL.Host = "tetherd"
		},
		Transport: &streamTransport{open: s.open, user: s.User, onDialErr: func(e error) { dialErr = e }},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			// Only a failure to reach the laptop is recoverable. Once the
			// laptop has the request, its error is its own answer.
			if dialErr == nil {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			if tooBig {
				p.logf("steal %s %s: the laptop is gone and the body is too large to replay: 502", r.Method, r.URL.Path)
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			p.logf("steal %s %s: the laptop is gone; passing this request to the app", r.Method, r.URL.Path)
			r.Body = io.NopCloser(bytes.NewReader(replay))
			r.ContentLength = int64(len(replay))
			app.ServeHTTP(w, r)
		},
	}
	rp.ServeHTTP(w, r)
}

// bufferBody reads up to MaxReplayBody+1 bytes so the request can be
// replayed to the application. tooBig reports that it did not fit, in which
// case r.Body is left streaming and a dial failure cannot be recovered.
func bufferBody(r *http.Request) (replay []byte, tooBig bool, err error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, false, nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, MaxReplayBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(buf) > MaxReplayBody {
		// Put what was read back in front of the rest and stream it.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
		return nil, true, nil
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	return buf, false, nil
}

// streamTransport sends one request over one freshly opened yamux stream.
type streamTransport struct {
	open      interface{ OpenStream() (net.Conn, error) }
	user      string
	onDialErr func(error)
}

func (t *streamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if t.open == nil {
				err := errors.New("this session cannot accept requests")
				t.onDialErr(err)
				return nil, err
			}
			c, err := t.open.OpenStream()
			if err != nil {
				t.onDialErr(err)
				return nil, err
			}
			// The CLI reads this line before handing the stream to
			// http.Serve, so it must be written before the request.
			if err := proto.NewEncoder(c).Encode(proto.TypeHTTP, proto.HTTPHeader{User: t.user}); err != nil {
				c.Close()
				t.onDialErr(err)
				return nil, err
			}
			return c, nil
		},
		// One stream per request: a yamux stream is cheap and reusing one
		// would serialise a laptop's requests behind each other.
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 0,
	}
	defer tr.CloseIdleConnections()
	return tr.RoundTrip(r)
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// ServeProxy serves the ALB's port until ctx is done.
func (a *Agent) ServeProxy(ctx context.Context, ln net.Listener) error {
	p := &Proxy{Agent: a, AppAddr: a.cfg.AppAddr, Logf: a.logf}
	srv := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 20 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("agent: proxy: %w", err)
	}
	return nil
}
```

（上の実装は `net/url` を使わないので import しない。`bytes` / `context` / `errors` / `fmt` / `io` / `net` / `net/http` / `net/http/httputil` / `time` と `internal/proto` だけ。）

`internal/agent/config.go` に `Proxy` と `AppAddr` を足す（`TETHERD_PROXY` 既定 `0.0.0.0:8080`、`TETHERD_APP_ADDR` 既定 `127.0.0.1:8081`）。

`cmd/tetherd-agent/main.go`: 制御リスナーと並行してプロキシを listen する。どちらかが落ちたらプロセスを終える（`essential: true` なので ECS が再起動する）。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./... && go test -race -count=5 ./internal/agent/ && make lint && GOOS=linux go vet ./... && make build`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent cmd/tetherd-agent
git commit -m "feat(agent): reverse-proxy the ALB port and steal matching requests"
```

---

### Task 5: CLI 側の受け口 — ストリームを `http.Serve` して `localhost:<local_port>` へ

**Files:**
- Create: `internal/cli/steal.go`, `internal/cli/steal_test.go`
- Modify: `internal/cli/run.go`（`Options.OnHTTP` を渡す、`✓ steal` 行、`hello` に token と incoming を載せる）、`internal/cli/root.go`（`--no-incoming` / `--local-port` / `--as`）、`internal/cli/config.go`（`incoming` を `RunOptions` に反映）
- Test: `internal/cli/steal_test.go`, `internal/cli/run_e2e_test.go` に追記

**Interfaces:**
- Consumes: Task 2 の `session.Options.OnHTTP`、Task 4 が書く `proto.HTTPHeader`、Task 3 の一致判定（agent 側）
- Produces:
  ```go
  // cli/steal.go
  // StealServer serves the http streams the agent opens, proxying each one
  // to the developer's own process.
  type StealServer struct {
      LocalPort int
      Logf      func(string, ...any)
  }
  // Serve takes ownership of one stream: it reads HTTP/1.1 from it and
  // proxies to 127.0.0.1:LocalPort until the stream ends.
  func (s *StealServer) Serve(stream net.Conn)

  // RunOptions に追加
  //   NoIncoming       bool
  //   LocalPort        int
  //   As               string // X-Dev-User に載せる名前（--as）。opts.User に入る
  //   Token            string // 個人設定から。hello で渡す
  //   MatchHeader      string // 既定 "X-Dev-User"
  //   MatchTokenHeader string // 既定 "X-Dev-Token"
  ```

**なぜ `http.Serve` なのか**: agent は 1 リクエスト = 1 ストリームで送るが、ストリームの中身は普通の HTTP/1.1。`net/http` にそのまま読ませれば、チャンク転送・ヘッダー・`Expect: 100-continue` の扱いを自分で書かずに済む。ストリーム 1 本を `net.Listener` に見せかけて `http.Serve` するのが spec §5.2 の指定。

- [ ] **Step 1: 失敗するテストを書く**

`internal/cli/steal_test.go`:

```go
package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// localApp is the developer's own process on 127.0.0.1:<port>.
func localApp(t *testing.T, h http.Handler) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// agentSide writes one request onto a stream the way the agent's proxy does
// and returns the response it reads back.
func agentSide(t *testing.T, s *StealServer, req *http.Request) *http.Response {
	t.Helper()
	mine, theirs := net.Pipe()
	go s.Serve(theirs)
	if err := proto.NewEncoder(mine).Encode(proto.TypeHTTP, proto.HTTPHeader{User: "shota"}); err != nil {
		t.Fatal(err)
	}
	if err := req.Write(mine); err != nil {
		t.Fatal(err)
	}
	mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufioReader(mine), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestStealServerProxiesToTheLocalPort(t *testing.T) {
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "LOCAL %s %s body=%q xff=%q", r.Method, r.URL.Path, string(b), r.Header.Get("X-Forwarded-For"))
	}))
	var logs strings.Builder
	s := &StealServer{LocalPort: port, Logf: func(f string, a ...any) { fmt.Fprintf(&logs, f+"\n", a...) }}

	req := httptest.NewRequest("POST", "http://tetherd/api/orders", strings.NewReader("hi"))
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	resp := agentSide(t, s, req)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `LOCAL POST /api/orders body="hi"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `xff="203.0.113.5"`) {
		t.Errorf("X-Forwarded-For must reach the developer's process: %s", b)
	}
	// The request log is the CLI's job, not the agent's (spec §5.2).
	if !strings.Contains(logs.String(), "POST") || !strings.Contains(logs.String(), "/api/orders") ||
		!strings.Contains(logs.String(), "200") || !strings.Contains(logs.String(), "203.0.113.5") {
		t.Errorf("the log line must carry method, path, status and the caller: %q", logs.String())
	}
}

func TestStealServerReportsALocalAppThatIsNotListening(t *testing.T) {
	// The developer has not started their server yet. The agent must get a
	// response, not a hang, and the log must say what to do.
	var logs strings.Builder
	s := &StealServer{LocalPort: 1, Logf: func(f string, a ...any) { fmt.Fprintf(&logs, f+"\n", a...) }}
	resp := agentSide(t, s, httptest.NewRequest("GET", "http://tetherd/", nil))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "127.0.0.1:1") {
		t.Errorf("the log must name the port nothing is listening on: %q", logs.String())
	}
}

func TestStealServerHandlesSeveralRequestsOnSeparateStreams(t *testing.T) {
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "LOCAL %s", r.URL.Path)
	}))
	s := &StealServer{LocalPort: port}
	for _, p := range []string{"/a", "/b", "/c"} {
		resp := agentSide(t, s, httptest.NewRequest("GET", "http://tetherd"+p, nil))
		b, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(b), "LOCAL "+p) {
			t.Fatalf("%s: %s", p, b)
		}
	}
}

func TestStealServerIgnoresAStreamWithTheWrongHeader(t *testing.T) {
	s := &StealServer{LocalPort: 1}
	mine, theirs := net.Pipe()
	go s.Serve(theirs)
	// A header that is not an http stream: Serve must close and not try to
	// parse a request.
	proto.NewEncoder(mine).Encode(proto.TypeDial, proto.DialHeader{Addr: "10.0.0.1:5432"})
	mine.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(mine); err != nil && !strings.Contains(err.Error(), "closed") &&
		!strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "reset") {
		t.Fatalf("the stream must be closed, got %v", err)
	}
}
```

（`bufioReader` は `bufio.NewReader` を呼ぶだけの小さなヘルパ。テストファイル内に置く。`import "bufio"` を足す。）

`internal/cli/run_e2e_test.go` に追記（プロセス内 agent 相手の結合。agent の proxy と CLI の受け口を両方通す）:

```go
func TestRunStealsThroughTheRealAgentProxy(t *testing.T) {
	// The developer's process.
	port := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "LOCAL %s", r.URL.Path)
	}))
	// The application container the agent passes everything else to.
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "APP %s", r.URL.Path)
	}))
	defer app.Close()

	ag := startAgentForWithApp(t, map[string]string{"A": "1"}, app.Listener.Addr().String())
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}

	opts := ssmOpts("sleep", "5")
	opts.LocalPort = port
	opts.Token = "tok-shota"
	opts.User = "shota"
	var out strings.Builder
	done := make(chan int, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		code, _ := RunWithDeps(ctx, opts, &out, depsFor(p))
		done <- code
	}()
	waitFor(t, func() bool { return strings.Contains(out.String(), "✓ steal") }, "the steal line never appeared")

	// A matching request must reach the developer's process; everything else
	// the application.
	get := func(hdrs map[string]string) string {
		r, _ := http.NewRequest("GET", "http://"+ag.proxyAddr+"/api/orders", nil)
		for k, v := range hdrs {
			r.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := get(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "tok-shota"}); !strings.Contains(got, "LOCAL /api/orders") {
		t.Errorf("a matching request must reach the laptop, got %q", got)
	}
	if got := get(nil); !strings.Contains(got, "APP /api/orders") {
		t.Errorf("an unmatched request must reach the app, got %q", got)
	}
	if got := get(map[string]string{"X-Dev-User": "shota", "X-Dev-Token": "wrong"}); !strings.Contains(got, "APP /api/orders") {
		t.Errorf("a wrong token must reach the app, got %q", got)
	}
	cancel()
	<-done
}

func TestRunWithNoIncomingTakesNothing(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "APP %s", r.URL.Path)
	}))
	defer app.Close()
	ag := startAgentForWithApp(t, map[string]string{"A": "1"}, app.Listener.Addr().String())
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1"}, agentAddr: ag.addr}

	opts := ssmOpts("sleep", "5")
	opts.NoIncoming = true
	opts.Token = "tok-shota"
	opts.User = "shota"
	var out strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunWithDeps(ctx, opts, &out, depsFor(p))
	waitFor(t, func() bool { return strings.Contains(out.String(), "✓ env") }, "the session never came up")

	r, _ := http.NewRequest("GET", "http://"+ag.proxyAddr+"/api/orders", nil)
	r.Header.Set("X-Dev-User", "shota")
	r.Header.Set("X-Dev-Token", "tok-shota")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "APP") {
		t.Fatalf("--no-incoming must leave every request with the app, got %q", b)
	}
	if strings.Contains(out.String(), "✓ steal") {
		t.Errorf("--no-incoming must not print a steal line: %s", out.String())
	}
}
```

（`startAgentForWithApp` は Task 1 の `startAgentFor` を拡張したもの: agent の制御リスナーに加えて `127.0.0.1:0` でプロキシも listen し、`proxyAddr` を返す。`ag.env` 相当の既存フィールドはそのまま。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/cli/`
Expected: FAIL（`undefined: StealServer` / `RunOptions.LocalPort`）

- [ ] **Step 3: 実装**

`internal/cli/steal.go`:

```go
// The steal receiver. The agent opens one yamux stream per stolen request
// and speaks plain HTTP/1.1 on it; the CLI hands each stream to net/http so
// chunked bodies, Expect: 100-continue and header parsing are the standard
// library's problem, and proxies the result to the developer's own process
// (spec §5.2).
package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// StealServer serves the http streams the agent opens.
type StealServer struct {
	LocalPort int
	Logf      func(string, ...any)
}

// Serve takes ownership of one stream, including closing it.
func (s *StealServer) Serve(stream net.Conn) {
	defer stream.Close()
	typ, _, err := proto.ReadHeader(stream)
	if err != nil {
		return
	}
	if typ != proto.TypeHTTP {
		// Only the agent opens streams toward the CLI, and only http ones.
		// Anything else is a bug or a newer agent; close rather than guess.
		return
	}
	addr := fmt.Sprintf("127.0.0.1:%d", s.LocalPort)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = addr
			// The ALB's X-Forwarded-* and the tracing headers are already on
			// the request and pass through untouched.
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			s.logf("← %s %s  502  nothing is listening on %s (%v)", r.Method, r.URL.Path, addr, e)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	srv := &http.Server{
		Handler:           s.logging(rp),
		ReadHeaderTimeout: 20 * time.Second,
	}
	// One stream, one connection. Serve returns when the agent closes it.
	srv.Serve(&singleConn{c: stream})
}

// logging prints one line per stolen request: the CLI is where the developer
// is looking, and the agent deliberately sends no logs (spec §5.2).
func (s *StealServer) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logf("← %-6s %s  %d  %s  (from %s)", r.Method, r.URL.Path, rec.status,
			time.Since(started).Round(time.Millisecond), firstForwardedFor(r))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

// firstForwardedFor is the original caller: the ALB appends, so the first
// entry is the client.
func firstForwardedFor(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return "unknown"
	}
	if i := strings.IndexByte(xff, ','); i >= 0 {
		return strings.TrimSpace(xff[:i])
	}
	return strings.TrimSpace(xff)
}

// singleConn presents one already-accepted connection as a Listener, so
// net/http can serve a yamux stream.
type singleConn struct {
	c    net.Conn
	done bool
}

func (l *singleConn) Accept() (net.Conn, error) {
	if l.done {
		return nil, io.EOF
	}
	l.done = true
	return l.c, nil
}
func (l *singleConn) Close() error   { return nil }
func (l *singleConn) Addr() net.Addr { return l.c.LocalAddr() }

func (s *StealServer) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
```

`internal/cli/run.go`:
- `hello` に token と incoming を載せる:
  ```go
  	hello := proto.Hello{Version: proto.Version, User: opts.User, Token: opts.Token}
  	if !opts.NoIncoming && opts.LocalPort > 0 && opts.Token != "" {
  		hello.Incoming = proto.Incoming{Enabled: true, Header: opts.MatchHeader, TokenHeader: opts.MatchTokenHeader}
  	}
  ```
- `session.Options` に `OnHTTP` を渡す:
  ```go
  	var steal *StealServer
  	if hello.Incoming.Enabled {
  		steal = &StealServer{LocalPort: opts.LocalPort, Logf: logf}
  	}
  	sess, err := session.Dial(ctx, conn, hello, session.Options{
  		OnHTTP: func(s net.Conn) {
  			if steal != nil {
  				steal.Serve(s)
  				return
  			}
  			s.Close()
  		},
  	})
  ```
- ステータス行:
  ```go
  	if hello.Incoming.Enabled {
  		logf("✓ steal    %s: %s (+ %s) → localhost:%d", opts.MatchHeader, opts.User, opts.MatchTokenHeader, opts.LocalPort)
  	}
  ```
  `--no-incoming` や token 無しのときは行を出さない（出ていないこと自体が「取らない」の表示）。

`RunOptions` に `NoIncoming bool` / `LocalPort int` / `As string` / `Token string` / `MatchHeader string` / `MatchTokenHeader string` を足す。`applyConfig` が `incoming.local_port` と `incoming.match.*` と個人設定の `token` を埋め、`--as` が指定されればそれを `opts.User` に入れる（`hello.user` と `X-Dev-User` は同じ値）。既定のヘッダー名は `X-Dev-User` / `X-Dev-Token`。

`internal/cli/root.go`: `addTargetFlags` ではなく `newRunCommand` 側に `--no-incoming` / `--local-port` / `--as` を足す（`env` と `doctor` には無い）。`changed()` を通す。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./... && go test -race -count=5 ./internal/cli/ && make lint && GOOS=linux go vet ./... && make build && go mod tidy && git diff --exit-code go.mod go.sum`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli
git commit -m "feat(cli): serve stolen requests against the developer's own process"
```

---

### Task 6: dev-env の配線、ドキュメント、実機 e2e

**Files:**
- Modify: `deploy/dev-env/alb.tf`（ターゲットグループを agent の 8080 へ）、`deploy/dev-env/ecs.tf`（agent に `portMappings` 8080、app の 8081 はそのまま）、`deploy/dev-env/network.tf`（sg-app のインバウンドを 8080 に）、`docs/e2e-aws.md`（steal の行）、`docs/config.md`（`incoming` を「読まれる」に）、`docs/specs/2026-09-12-v1-macos-design.md`（§12 に v0.3a の枠）
- Test: 実機（コントローラが人間に `terraform apply` を依頼する）

**Interfaces:** なし（インフラとドキュメント）

- [ ] **Step 1: Terraform を書く**

`alb.tf` のターゲットグループ:

```hcl
# The agent is always on the data path: it reverse-proxies to the app on
# 8081 and steals only requests whose user and token match an attached
# session. With nobody attached this is a pass-through (spec §5.1).
resource "aws_lb_target_group" "api" {
  name        = "tetherd-dev-api"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.this.id

  health_check {
    path                = "/"
    matcher             = "200"
    interval            = 15
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }
}
```

（既存の属性はそのまま残し、`port` だけ 8081 → 8080 に変える。`protocol_version` を明示していなければ既定の HTTP1 のままであることを確認する — spec §5.1 は gRPC / HTTP2 のターゲットグループを対象外としている。）

`ecs.tf`:
- `tetherd-agent` コンテナに `portMappings = [{ containerPort = 8080, protocol = "tcp" }]` と env `TETHERD_APP_ADDR = "127.0.0.1:8081"` を足す
- `load_balancer` ブロックの `container_name` を `"app"` → `"tetherd-agent"`、`container_port` を 8080 に

`network.tf`: sg-app のインバウンド 8081 ← sg-alb を 8080 に変える。

- [ ] **Step 2: `terraform fmt` と `validate`**

Run: `cd deploy/dev-env && terraform fmt -check && terraform validate`
Expected: 両方 PASS。**`plan` も `apply` もこのステップでは実行しない**（`plan` は認証情報が要るうえ、apply は人間が行う）

- [ ] **Step 3: ドキュメント**

`docs/config.md`:
- `incoming` の節を「v0.3 で読まれる」に更新し、`local_port`（既定 8080）と `match.header` / `match.token_header`（既定 `X-Dev-User` / `X-Dev-Token`）、`--no-incoming` / `--local-port` / `--as` の関係を書く
- トークンの説明を steal の文脈に繋げる（個人設定の `token` が `hello` で agent に渡り、一致条件の片方になる。公開 ALB でもユーザー名だけでは届かない）
- ブラウザから使うには拡張（ModHeader 等）で dev ドメインだけに 2 ヘッダーを付けるプロファイルを配る、を 1 段落

`docs/e2e-aws.md` に steal の行を足す:

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 20 | `$RUN -- <自分のサーバ>` を起動した状態で `curl -H 'X-Dev-User: abe' -H "X-Dev-Token: $(grep token ~/.tetherd/config.yml \| awk '{print $2}')" http://<alb>/` | ラップトップのプロセスの応答。CLI に `← GET / 200 <ms> (from <自分の IP>)` が出る | ALB → agent → yamux → ラップトップが通る |
| 21 | ヘッダー無しで `curl http://<alb>/` | `sampleapp on ip-10-0-…`（タスクの応答） | 一致しないリクエストは素通し |
| 22 | トークンを 1 文字変えて `curl` | タスクの応答。ラップトップには来ない | 公開 ALB でユーザー名だけでは届かない |
| 23 | `$RUN` を Ctrl-C した直後に `curl -H 'X-Dev-User: abe' -H 'X-Dev-Token: …' http://<alb>/` | タスクの応答（502 ではない） | セッション断で即時に素通しへ復帰 |
| 24 | ラップトップのサーバだけ落として `curl`（`$RUN` は生かす） | タスクの応答。CLI に `502 nothing is listening on 127.0.0.1:8080` | dial 失敗はそのリクエストだけ app にフォールバック |
| 25 | `$RUN --no-incoming -- sleep 60` 中に一致するヘッダーで `curl` | タスクの応答。CLI に `✓ steal` 行が無い | `--no-incoming` は何も取らない |
| 26 | ALB のヘルスチェックが 2 分間 healthy のまま | ターゲットが healthy | ヘルスチェックは常に app に届く |

`docs/specs/2026-09-12-v1-macos-design.md` §12 に `### v0.3a（ルート固定と steal）` の枠を作り、上の行を「未実施」で置く。

- [ ] **Step 4: 人間に `terraform apply` を依頼し、e2e を実施**

コントローラの仕事: `terraform plan` の内容（ターゲットグループのポート、sg、agent の portMappings、`load_balancer` の向き先の 4 点）を要約して人間に提示し、`apply` を依頼する。apply 後に agent イメージを再ビルド・再デプロイ（`make push-images ECR_REGISTRY=… && aws ecs update-service --force-new-deployment`）してから 20〜26 行を実施し、結果を spec §12 に記録する。

- [ ] **Step 5: Commit**

```bash
git add deploy/dev-env docs
git commit -m "feat(dev-env): put the agent on the ALB's path, and document steal"
```

---

## 完了条件

1. `go test -race ./...` / `make lint`（vet + gofmt）/ `GOOS=linux go vet ./...` / `make build` / `go mod tidy` が no-op
2. `169.254.170.2` の host route がセッション中だけ張られ、終了時（異常終了を含む）に消える。`tetherd run -- aws sts get-caller-identity` が繰り返し成功する（間欠的に失敗しない）
3. 一致するヘッダーとトークンを持つリクエストだけがラップトップに届き、それ以外は app に素通しする（e2e 20〜22）
4. セッションが無い / 切れた / `--no-incoming` のとき、agent は完全な素通しに戻る（e2e 23、25）
5. ラップトップのプロセスが落ちていれば、そのリクエストだけ app にフォールバックする。1 MiB を超えるボディは 502（e2e 24 と Task 4 のテスト）
6. ヘルスチェックと WebSocket upgrade は常に app（e2e 26 と Task 4 のテスト）
7. リクエストログが CLI 側に出る（method / path / status / 所要時間 / 呼び出し元）
8. spec §12 に v0.3a の検証結果が記録されている

## 次の計画（v0.3b、この計画には含めない）

- RUNNING な全タスクへの接続と deploy 追従（`ListTasks` のポーリング。steal はどのタスクに落ちるか分からないので、`desired_count = 2` での取りこぼしゼロが検証項目）
- `tetherd status`（誰がどのタスクに繋いでいるか）と `tetherd token rotate`
- doctor にターゲットグループの HTTP1 検査（developer policy に `elasticloadbalancing:DescribeTargetGroups` を足す → Terraform の再適用）と steal 要件の検査
- 持ち越し: `run` が DNS の経路を主張する前に一度引いて確かめる、`⚠` と `!` の統一、`--no-network` が `remote_domains` を黙って無視する点、`needsDotenvQuotes` がバックスラッシュ単独に反応しない点
