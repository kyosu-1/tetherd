# v0.3b: 全タスク接続・deploy 追従・status Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `tetherd run` を RUNNING な全タスクに接続し、deploy でタスクが入れ替わっても追従させ、`status` と `token rotate` を足して、doctor が steal の要件を検査できるようにする。

**Architecture:** `ecsprov.Discover` を `DiscoverAll`（全 eligible タスク）に開き、CLI 側に `SessionSet` を置く。`SessionSet` は N 本のセッションを持ち、そのうち 1 本を **primary**（`DialTCP` / `Resolve` / `Welcome` の供給元）に指名する。steal は全セッションから受ける。`ListTasks` のポーリングで増えたタスクに繋ぎ、消えたタスクを落とし、primary が失われたら生存セッションから昇格させる。

**Tech Stack:** Go 1.25、`aws-sdk-go-v2`、`hashicorp/yamux`、`spf13/cobra`、`miekg/dns`。**依存追加なし。**

**Spec:** `docs/specs/2026-09-12-v1-macos-design.md`（§6.2 タスク選択、§6.3 起動シーケンス、§6.5 コマンド、§8 プロトコル、§12 検証）

## Global Constraints

- 依存追加なし。`go.mod` / `go.sum` は 1 行も変わらない。`go mod tidy` は no-op。
- **テストは AWS 認証情報なし・ループバック以外のネットワークなし・root なし・`tetherd-helper` なしで通る。** 例外なし。
- `gofmt` クリーン（`make lint` が `go vet ./...` と `gofmt -l cmd internal examples` を実行）。
- agent は Linux で動く: `GOOS=linux go vet ./...` がクリーン、`make build` が通る。
- `terraform plan` / `apply` / `destroy` は**実行しない**。`fmt` と `validate` のみ。適用は人間に依頼する。
- プロトコルは**追加のみ**。`proto.Version` は `"1"` のまま上げない（v0.3a で `"2"` に上げたのは `internal/helper/wire.go` の helper プロトコルで、agent のものとは別物 — 確認済み）。Task 5 が `Welcome` にフィールドを 1 つ足すが、それが追加のみの規約に収まる形で行う。
- コミットメッセージの末尾は正確に:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_015mTfR8PDnAkbdwehd3JgF9
  ```
- **git worktree を作らない。** このブランチ（`feat/v03b-session-management`）は main のチェックアウト上にある。
- 複数エージェントが同じツリーで動くので、**ステージはファイルパスを明示**する。`git add <dir>` は禁止。`git stash` も禁止（マシン内で共有）。

## File Structure

| ファイル | 責務 |
|---|---|
| `internal/provider/ecs/discover.go` | `DiscoverAll` を公開し、`Discover` をその先頭要素を返す薄い包みにする |
| `internal/cli/sessionset.go`（新規） | N 本のセッションの保持、primary の指名と昇格、steal の受け口の共有、全体の後片付け |
| `internal/cli/sessionset_test.go`（新規） | 複数 in-process agent に対する接続・昇格・脱落の検査 |
| `internal/cli/follow.go`（新規） | `ListTasks` ポーリングで `SessionSet` に増減を反映する |
| `internal/cli/follow_test.go`（新規） | 増減の反映、ポーリング失敗時の挙動 |
| `internal/cli/run.go` | `SessionSet` を使うよう配線。`sess.X` → `set.X` |
| `internal/cli/status.go`（新規） | `tetherd status` |
| `internal/cli/token.go`（新規） | `tetherd token rotate` |
| `internal/doctor/doctor.go` | `Unknown` ステータスの追加と警告記号の統一 |
| `internal/doctor/checks.go` | `CheckSteal`、`CheckCredentialEndpoint`、`CheckTargetGroup` |
| `internal/cli/doctor.go` | 新しい行の配線 |
| `internal/agent/agent.go` | `ListenAndServe` の削除（dead code） |
| `internal/agent/config.go` | `Config.AppAddr` の既定を `ConfigFromEnv` の外でも効かせる |
| `internal/session/client.go` | `duplicate_user` のリトライ（`Dial` の呼び出し側から使える形で） |
| `deploy/dev-env/iam.tf` 相当 | developer policy に `elasticloadbalancing:DescribeTargetGroups` |
| `docs/config.md` / `docs/design.md` / `docs/specs/...` / `docs/e2e-aws.md` | 更新 |

---

### Task 1: `DiscoverAll` — eligible な全タスクを返す

**Files:**
- Modify: `internal/provider/ecs/discover.go:47-145`
- Test: `internal/provider/ecs/discover_test.go` に追記

**Interfaces:**
- Consumes: 既存の `ECSAPI`、`Target`、`transport.Task`
- Produces:
  ```go
  // DiscoverAll returns every eligible RUNNING task, oldest first.
  func DiscoverAll(ctx context.Context, api ECSAPI, t Target) ([]transport.Task, error)
  // Discover returns the oldest one. Unchanged signature.
  func Discover(ctx context.Context, api ECSAPI, t Target) (transport.Task, error)
  ```

**現状**: `Discover` は `eligible []transport.Task` を組んで `StartedAt` でソートし `eligible[0]` を返す。**ロジックはそのまま使える。**

- [ ] **Step 1: 失敗するテストを書く**

`internal/provider/ecs/discover_test.go` に追記。既存のテストが使っている fake API の組み立て方に合わせること（`fakeECS` の形を必ず先に読む）。

```go
func TestDiscoverAllReturnsEveryEligibleTaskOldestFirst(t *testing.T) {
	// Two eligible tasks: the steal path needs both, because the ALB
	// picks which one a request lands on and a session attached to only
	// one of them silently misses half the traffic.
	api := &fakeECS{
		arns: []string{"arn:aws:ecs:r:1:task/c/newer", "arn:aws:ecs:r:1:task/c/older"},
		tasks: []types.Task{
			runningTask("newer", time.Unix(2000, 0)),
			runningTask("older", time.Unix(1000, 0)),
		},
	}
	all, err := DiscoverAll(context.Background(), api, Target{Cluster: "c", Service: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d tasks, want 2", len(all))
	}
	if all[0].ID != "older" || all[1].ID != "newer" {
		t.Fatalf("order = %s, %s; want older first (primary is the oldest task)", all[0].ID, all[1].ID)
	}
}

func TestDiscoverStillReturnsTheOldestOnly(t *testing.T) {
	api := &fakeECS{
		arns: []string{"arn:aws:ecs:r:1:task/c/newer", "arn:aws:ecs:r:1:task/c/older"},
		tasks: []types.Task{
			runningTask("newer", time.Unix(2000, 0)),
			runningTask("older", time.Unix(1000, 0)),
		},
	}
	task, err := Discover(context.Background(), api, Target{Cluster: "c", Service: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "older" {
		t.Fatalf("Discover returned %s, want the oldest", task.ID)
	}
}

func TestDiscoverAllSkipsTheIneligibleAndSaysWhy(t *testing.T) {
	// One task RUNNING with the agent, one without ECS Exec. The eligible
	// one must come back and the reason for the other must not be lost -
	// a developer whose deploy is half-rolled needs to know which task is
	// not attachable and why.
	api := &fakeECS{
		arns: []string{"arn:aws:ecs:r:1:task/c/good", "arn:aws:ecs:r:1:task/c/noexec"},
		tasks: []types.Task{
			runningTask("good", time.Unix(1000, 0)),
			noExecTask("noexec", time.Unix(1100, 0)),
		},
	}
	all, err := DiscoverAll(context.Background(), api, Target{Cluster: "c", Service: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != "good" {
		t.Fatalf("got %v, want only the eligible task", all)
	}
}

func TestDiscoverAllWithNoEligibleTaskReportsEveryReason(t *testing.T) {
	api := &fakeECS{
		arns:  []string{"arn:aws:ecs:r:1:task/c/noexec"},
		tasks: []types.Task{noExecTask("noexec", time.Unix(1100, 0))},
	}
	_, err := DiscoverAll(context.Background(), api, Target{Cluster: "c", Service: "s"})
	var nr *NotReadyError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want a *NotReadyError", err)
	}
	if len(nr.Reasons) != 1 || !strings.Contains(nr.Reasons[0], "ECS Exec is disabled") {
		t.Fatalf("reasons = %v, want the ECS Exec explanation", nr.Reasons)
	}
}
```

`runningTask` と `noExecTask` が既存のヘルパに無ければ、既存テストが `types.Task` をどう組んでいるかに合わせて作る。**必要な条件は 4 つ**: `LastStatus: "RUNNING"`、`EnableExecuteCommand: true`、`Containers` に `tetherd-agent` があること、その `ManagedAgents` の `ExecuteCommandAgent` が `RUNNING`。`noExecTask` は `EnableExecuteCommand: false` にするだけ。

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/provider/ecs/ -run DiscoverAll -v`
Expected: FAIL — `undefined: DiscoverAll`

- [ ] **Step 3: `Discover` を `DiscoverAll` に開く**

既存の `Discover` の本体をそのまま `DiscoverAll` にリネームし、末尾の `return eligible[0], nil` を `return eligible, nil` に変える。`Discover` は薄い包みにする:

```go
// Discover returns the oldest eligible RUNNING task - the primary, which
// is where dial, resolve and the task environment come from. Steal needs
// every task (the ALB chooses), so the session layer uses DiscoverAll.
func Discover(ctx context.Context, api ECSAPI, t Target) (transport.Task, error) {
	all, err := DiscoverAll(ctx, api, t)
	if err != nil {
		return transport.Task{}, err
	}
	return all[0], nil
}
```

`DiscoverAll` のドキュメントコメントに、**戻り値が空のスライスで返ることは無い**（0 件なら `NotReadyError` か「no RUNNING tasks」エラー）ことを書く。`Discover` が `all[0]` を無検査で読めるのはそれが根拠。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./internal/provider/ecs/`
Expected: PASS（既存テストも全部）

- [ ] **Step 5: mutation で検査する**

順序は load-bearing（primary = 最古）なので、次を順に壊して**必ずテストが落ちること**を確認し、戻す:
1. `sort.Slice` の比較を `After` に反転 → `TestDiscoverAllReturnsEveryEligibleTaskOldestFirst` と `TestDiscoverStillReturnsTheOldestOnly` が落ちること
2. `sort.Slice` の行を削除 → 落ちること（落ちなければテストが弱い。2 件のタスクで順序が偶然合う可能性があるので、fake の入力順を「新しい方が先」にしてあることを確認する）
3. `Discover` を `all[len(all)-1]` に変更 → 落ちること

各 mutation が**実際にコンパイルして適用された**ことを確認してから結論を出すこと。

- [ ] **Step 6: コミット**

```bash
git add internal/provider/ecs/discover.go internal/provider/ecs/discover_test.go
git commit
```
メッセージ本文に「`Discover` の挙動は変えていない」「順序が primary の定義そのもの」を書く。

---

### Task 2: `SessionSet` — N 本のセッションと primary の昇格

**Files:**
- Create: `internal/cli/sessionset.go`, `internal/cli/sessionset_test.go`

**Interfaces:**
- Consumes: Task 1 の `ecsprov.DiscoverAll`、既存の `dialAgent`（`internal/cli/run.go:545`）、`session.Client`
- Produces:
  ```go
  // SessionSet holds one session per attachable task. Exactly one is the
  // primary: dial, resolve and the task environment come from it, because
  // they are properties of the service, not of a task. Steal arrives from
  // any of them, because the ALB chooses.
  type SessionSet struct {
      Logf func(string, ...any)
  }

  // Add attaches sess for task and returns whether it became the primary.
  func (s *SessionSet) Add(task transport.Task, sess *session.Client) bool
  // Remove closes and forgets the session for taskID, promoting a new
  // primary if it was the primary. Returns false if no such session.
  func (s *SessionSet) Remove(taskID string) bool
  // Has reports whether taskID is attached.
  func (s *SessionSet) Has(taskID string) bool
  // TaskIDs returns the attached task ids, primary first.
  func (s *SessionSet) TaskIDs() []string
  // Primary returns the session dial/resolve/env use, or nil if empty.
  func (s *SessionSet) Primary() *session.Client
  // DialTCP and Resolve forward to the current primary, so a promotion is
  // invisible to the forwarder and the DNS server holding these as funcs.
  func (s *SessionSet) DialTCP(ctx context.Context, addr string) (net.Conn, error)
  func (s *SessionSet) Resolve(ctx context.Context, name string) ([]string, int, error)
  // Reap closes sessions whose Done channel has fired, promoting as needed,
  // and returns the task ids it dropped.
  func (s *SessionSet) Reap() []string
  // Len is the number of live sessions.
  func (s *SessionSet) Len() int
  // Close closes every session.
  func (s *SessionSet) Close()
  ```

**設計上の決定 3 つ。実装者はこれを変えないこと。**

1. **primary は最古のタスク。** `DiscoverAll` の順序がそれを与える。`Add` は `task.StartedAt` を見て、既存 primary より古ければ自分が primary になる。
2. **`DialTCP` / `Resolve` はメソッドであってフィールドではない。** `proxy.Forwarder` と `dnsproxy.Server` はこれを関数値として受け取って保持するので、昇格しても差し替えが要らない形にする必要がある。`set.DialTCP` をメソッド値として渡せば、内部で毎回 primary を引き直す。
3. **primary が落ちたら昇格する。** deploy はいずれ全タスクを入れ替えるので、昇格しないと deploy 中に `tetherd run` が死ぬ — それがこのタスクが存在する理由。ただし **env は再取得しない**（子プロセスには起動時に注入済みで、後から変えられない）。昇格は `DialTCP` と `Resolve` の供給元が変わるだけ。

- [ ] **Step 1: 失敗するテストを書く**

`internal/cli/sessionset_test.go`。`startAgentFor`（`run_e2e_test.go:44`）を使って複数の in-process agent を立てる。**`startAgentFor` は `*inProcessAgent` を返し、`.addr` と `.a`（`*agent.Agent`）を持つ**ことを先に確認すること。

```go
package cli

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// dialInto attaches to ag as user and returns the session. It is the test's
// stand-in for run.go's dialAgent, which needs a provider and options this
// test has no use for.
func dialInto(t *testing.T, ag *inProcessAgent, user string) *session.Client {
	t.Helper()
	conn, err := net.Dial("tcp", ag.addr)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.Dial(context.Background(), conn, proto.Hello{Version: proto.Version, User: user}, session.Options{})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func task(id string, started int64) transport.Task {
	return transport.Task{ID: id, StartedAt: time.Unix(started, 0)}
}

func TestSessionSetMakesTheOldestTaskThePrimary(t *testing.T) {
	// The primary is where dial, resolve and env come from. Oldest wins so
	// that a deploy adding a newer task does not move it.
	older := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	newer := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}

	if primary := set.Add(task("newer", 2000), dialInto(t, newer, "tester")); !primary {
		t.Fatal("the first session added must be the primary")
	}
	if primary := set.Add(task("older", 1000), dialInto(t, older, "tester")); !primary {
		t.Fatal("an older task must take the primary")
	}
	if got := set.TaskIDs(); len(got) != 2 || got[0] != "older" {
		t.Fatalf("TaskIDs = %v, want the primary first and it to be older", got)
	}
}

func TestSessionSetKeepsThePrimaryWhenANewerTaskArrives(t *testing.T) {
	older := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	newer := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, older, "tester"))
	if primary := set.Add(task("newer", 2000), dialInto(t, newer, "tester")); primary {
		t.Fatal("a newer task must not take the primary from an older one")
	}
}

func TestSessionSetPromotesWhenThePrimaryIsRemoved(t *testing.T) {
	// This is the case deploy following exists for: the primary's task goes
	// away mid-run and the run has to keep working.
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, a1, "tester"))
	set.Add(task("newer", 2000), dialInto(t, a2, "tester"))

	if !set.Remove("older") {
		t.Fatal("Remove returned false for an attached task")
	}
	if set.Len() != 1 {
		t.Fatalf("Len = %d, want 1", set.Len())
	}
	if got := set.TaskIDs(); got[0] != "newer" {
		t.Fatalf("primary = %s, want the surviving task promoted", got[0])
	}
	// The promotion must be visible through the method the forwarder holds.
	if _, err := set.DialTCP(context.Background(), "127.0.0.1:1"); err == nil {
		t.Log("dial to a closed port failed as expected")
	}
}

func TestSessionSetDialTCPFollowsThePromotion(t *testing.T) {
	// The forwarder and the DNS server hold set.DialTCP / set.Resolve as
	// function values for the life of the run. If those captured the
	// primary instead of looking it up, a promotion would leave them
	// talking to a closed session - so prove the lookup is live by serving
	// a real listener only from the second agent.
	dead := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("second"))
			c.Close()
		}
	}()
	alive := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)

	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, dead, "tester"))
	set.Add(task("newer", 2000), dialInto(t, alive, "tester"))
	dialTCP := set.DialTCP // captured once, exactly as run.go does

	set.Remove("older")

	c, err := dialTCP(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial through the promoted primary: %v", err)
	}
	defer c.Close()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "second" {
		t.Fatalf("read %q, want the listener behind the promoted session", buf)
	}
}

func TestSessionSetReapDropsSessionsThatDied(t *testing.T) {
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	s1 := dialInto(t, a1, "tester")
	set.Add(task("older", 1000), s1)
	set.Add(task("newer", 2000), dialInto(t, a2, "tester"))

	s1.Close() // the agent's task went away
	waitFor(t, func() bool { return len(set.Reap()) == 0 && set.Len() == 1 }, "the dead session to be reaped")
	if got := set.TaskIDs(); len(got) != 1 || got[0] != "newer" {
		t.Fatalf("TaskIDs = %v, want only the survivor, promoted", got)
	}
}

func TestSessionSetCloseClosesEveryone(t *testing.T) {
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, a1, "tester"))
	set.Add(task("newer", 2000), dialInto(t, a2, "tester"))
	set.Close()
	if set.Len() != 0 {
		t.Fatalf("Len = %d after Close, want 0", set.Len())
	}
	if set.Primary() != nil {
		t.Fatal("Primary must be nil after Close")
	}
	// Both agents must see the detach, or a second run is refused.
	waitFor(t, func() bool { return len(a1.a.Sessions()) == 0 && len(a2.a.Sessions()) == 0 }, "both agents to see the detach")
}

func TestSessionSetPrimaryOfAnEmptySetIsNil(t *testing.T) {
	set := &SessionSet{Logf: func(string, ...any) {}}
	if set.Primary() != nil {
		t.Fatal("Primary of an empty set must be nil, not a zero Client")
	}
	if _, err := set.DialTCP(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatal("DialTCP with no primary must error, not panic or hang")
	}
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/cli/ -run SessionSet -v`
Expected: FAIL — `undefined: SessionSet`

- [ ] **Step 3: `SessionSet` を実装する**

```go
// internal/cli/sessionset.go
package cli

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"

	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// errNoPrimary is what dial and resolve answer when every session is gone.
// Run treats that as the session being lost; a single caller retrying into
// an empty set must get an error rather than a nil dereference.
var errNoPrimary = errors.New("no agent session is attached")

type attached struct {
	task transport.Task
	sess *session.Client
}

// SessionSet holds one session per attachable task.
//
// Exactly one session is the primary. dial, resolve and the task
// environment come from it, because those are properties of the service
// rather than of one task, and answering them from an arbitrary task would
// make a developer's DNS results depend on which task the poller happened
// to attach last. Steal arrives from any session, because the ALB chooses
// which task a request lands on - which is the whole reason this type
// exists rather than a single *session.Client.
//
// The primary is the oldest task, so a deploy adding a newer task does not
// move it. When the primary's own task goes away the oldest survivor is
// promoted: without that, a deploy would kill the run, and following a
// deploy is what this is for. Promotion does not re-read the environment -
// the child process already has it and cannot be told again.
type SessionSet struct {
	Logf func(string, ...any)

	mu      sync.Mutex
	entries []attached // sorted by task.StartedAt, oldest first
}

func (s *SessionSet) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Add attaches sess for task and reports whether it is now the primary.
func (s *SessionSet) Add(task transport.Task, sess *session.Client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, attached{task: task, sess: sess})
	s.sortLocked()
	return s.entries[0].task.ID == task.ID
}

func (s *SessionSet) sortLocked() {
	sort.SliceStable(s.entries, func(i, j int) bool {
		return s.entries[i].task.StartedAt.Before(s.entries[j].task.StartedAt)
	})
}

// Remove closes and forgets taskID's session, promoting a new primary if it
// was the primary. It reports whether such a session existed.
func (s *SessionSet) Remove(taskID string) bool {
	s.mu.Lock()
	var removed *attached
	kept := s.entries[:0]
	wasPrimary := len(s.entries) > 0 && s.entries[0].task.ID == taskID
	for _, e := range s.entries {
		if e.task.ID == taskID {
			e := e
			removed = &e
			continue
		}
		kept = append(kept, e)
	}
	s.entries = kept
	promoted := ""
	if wasPrimary && len(s.entries) > 0 {
		promoted = s.entries[0].task.ID
	}
	s.mu.Unlock()

	if removed == nil {
		return false
	}
	removed.sess.Close()
	if promoted != "" {
		s.logf("↻ session   task %s went away; dial and DNS now go through task %s", short(taskID), short(promoted))
	}
	return true
}

// Has reports whether taskID is attached.
func (s *SessionSet) Has(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.task.ID == taskID {
			return true
		}
	}
	return false
}

// TaskIDs returns the attached task ids, primary first.
func (s *SessionSet) TaskIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		ids = append(ids, e.task.ID)
	}
	return ids
}

// Primary is the session dial, resolve and env come from, or nil when the
// set is empty.
func (s *SessionSet) Primary() *session.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return nil
	}
	return s.entries[0].sess
}

// DialTCP forwards to the current primary. It is a method, not a field, so
// that the forwarder holding it as a function value keeps working across a
// promotion - it looks the primary up on every call.
func (s *SessionSet) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	p := s.Primary()
	if p == nil {
		return nil, errNoPrimary
	}
	return p.DialTCP(ctx, addr)
}

// Resolve forwards to the current primary, for the same reason as DialTCP.
func (s *SessionSet) Resolve(ctx context.Context, name string) ([]string, int, error) {
	p := s.Primary()
	if p == nil {
		return nil, 0, errNoPrimary
	}
	return p.Resolve(ctx, name)
}

// Reap drops sessions whose Done channel has fired and returns their task
// ids. A secondary dying is normal - the task was replaced - so it is
// reaped and logged rather than ending the run.
func (s *SessionSet) Reap() []string {
	s.mu.Lock()
	var dead []string
	for _, e := range s.entries {
		select {
		case <-e.sess.Done():
			dead = append(dead, e.task.ID)
		default:
		}
	}
	s.mu.Unlock()
	for _, id := range dead {
		s.Remove(id)
	}
	return dead
}

// Len is the number of live sessions.
func (s *SessionSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Close closes every session and empties the set.
func (s *SessionSet) Close() {
	s.mu.Lock()
	entries := s.entries
	s.entries = nil
	s.mu.Unlock()
	for _, e := range entries {
		e.sess.Close()
	}
}
```

**`short`** は `internal/cli` に既にある（`run.go` が使っている）。無ければ既存の呼び出しを探して合わせること。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=5 ./internal/cli/ -run SessionSet`
Expected: PASS。`-count=5` は `Reap` の並行性のため。

- [ ] **Step 5: mutation で検査する**

1. `sortLocked` の `Before` を `After` に → primary の指名 2 件が落ちること
2. `DialTCP` を `p := s.entries[0].sess` の直接参照（ロック無し）に変更 → `-race` で落ちること
3. `DialTCP` をフィールドに変えて `Add` 時に primary を焼き込む形に → `TestSessionSetDialTCPFollowsThePromotion` が落ちること（**これが落ちなければそのテストは無意味**。落ちることを必ず確認する）
4. `Remove` の昇格ログを削除 → 落ちないはず。落ちないなら、ログを検査するテストが無いということ。**ログは `run` のステータス行として開発者が読むので、1 件アサーションを足す**
5. `Primary` の空チェックを削除 → `TestSessionSetPrimaryOfAnEmptySetIsNil` が panic で落ちること

- [ ] **Step 6: コミット**

```bash
git add internal/cli/sessionset.go internal/cli/sessionset_test.go
git commit
```

---

### Task 3: deploy 追従 — `ListTasks` ポーリングで増減を反映

**Files:**
- Create: `internal/cli/follow.go`, `internal/cli/follow_test.go`

**Interfaces:**
- Consumes: Task 1 の `DiscoverAll`、Task 2 の `SessionSet`
- Produces:
  ```go
  // Follower keeps a SessionSet in step with the service's RUNNING tasks.
  type Follower struct {
      Set      *SessionSet
      Interval time.Duration            // default followInterval
      List     func(context.Context) ([]transport.Task, error)
      Attach   func(context.Context, transport.Task) (*session.Client, error)
      Logf     func(string, ...any)
  }
  // Run polls until ctx is done. It never returns an error: a failed poll
  // is transient and the existing sessions keep working.
  func (f *Follower) Run(ctx context.Context)
  const followInterval = 15 * time.Second
  ```

**設計上の決定**: ポーリングの失敗で run を落とさない。ECS の API は一時的に落ちるし、既存のセッションはそれと無関係に動いている。ただし**同じ理由を毎回ログに出すと子プロセスの出力を埋める**ので、同じエラーの連続は 1 回だけ出す。

- [ ] **Step 1: 失敗するテストを書く**

```go
func TestFollowerAttachesToATaskThatAppears(t *testing.T) {
	// A deploy adds a task; steal has to reach it, because the ALB will
	// start sending it requests whether or not tetherd noticed.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	var mu sync.Mutex
	tasks := []transport.Task{}
	f := &Follower{
		Set:      set,
		Interval: 10 * time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]transport.Task(nil), tasks...), nil
		},
		Attach: func(_ context.Context, tk transport.Task) (*session.Client, error) {
			return dialInto(t, ag, "tester-"+tk.ID), nil
		},
		Logf: func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)

	mu.Lock()
	tasks = append(tasks, task("t1", 1000))
	mu.Unlock()
	waitFor(t, func() bool { return set.Has("t1") }, "the follower to attach to the new task")
}

func TestFollowerDropsATaskThatDisappears(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("gone", 1000), dialInto(t, ag, "tester"))
	f := &Follower{
		Set:      set,
		Interval: 10 * time.Millisecond,
		List:     func(context.Context) ([]transport.Task, error) { return nil, nil },
		Attach:   func(context.Context, transport.Task) (*session.Client, error) { t.Fatal("must not attach"); return nil, nil },
		Logf:     func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	waitFor(t, func() bool { return !set.Has("gone") && set.Len() == 0 }, "the follower to drop the vanished task")
}

func TestFollowerKeepsGoingWhenAPollFails(t *testing.T) {
	// ECS APIs fail transiently. The sessions already attached are
	// unaffected, so a failed poll must not end the run or drop anyone.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("keep", 1000), dialInto(t, ag, "tester"))
	var calls atomic.Int64
	f := &Follower{
		Set:      set,
		Interval: 10 * time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			calls.Add(1)
			return nil, errors.New("ListTasks: throttled")
		},
		Attach: func(context.Context, transport.Task) (*session.Client, error) { return nil, nil },
		Logf:   func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	waitFor(t, func() bool { return calls.Load() >= 3 }, "the follower to keep polling after a failure")
	if !set.Has("keep") {
		t.Fatal("a failed poll must not drop an attached session")
	}
}

func TestFollowerLogsARepeatedPollFailureOnce(t *testing.T) {
	// The child process's own output shares this terminal. A throttled
	// ECS API must not print the same line every 15 seconds forever.
	set := &SessionSet{Logf: func(string, ...any) {}}
	var mu sync.Mutex
	var lines []string
	f := &Follower{
		Set:      set,
		Interval: 5 * time.Millisecond,
		List:     func(context.Context) ([]transport.Task, error) { return nil, errors.New("ListTasks: throttled") },
		Attach:   func(context.Context, transport.Task) (*session.Client, error) { return nil, nil },
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	time.Sleep(60 * time.Millisecond) // at least ten polls
	cancel()
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "throttled") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the same poll failure was logged %d times, want once (lines: %v)", n, lines)
	}
}

func TestFollowerAttachFailureIsRetriedNextPoll(t *testing.T) {
	// A task can appear in ListTasks before its ExecuteCommandAgent is
	// ready, so the first attach legitimately fails. Retrying is what
	// makes a rolling deploy end with every task attached.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	var attempts atomic.Int64
	f := &Follower{
		Set:      set,
		Interval: 10 * time.Millisecond,
		List:     func(context.Context) ([]transport.Task, error) { return []transport.Task{task("t1", 1000)}, nil },
		Attach: func(context.Context, transport.Task) (*session.Client, error) {
			if attempts.Add(1) == 1 {
				return nil, errors.New("ExecuteCommandAgent is not RUNNING yet")
			}
			return dialInto(t, ag, "tester"), nil
		},
		Logf: func(string, ...any) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Run(ctx)
	waitFor(t, func() bool { return set.Has("t1") }, "the follower to retry the attach")
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/cli/ -run Follower -v`
Expected: FAIL — `undefined: Follower`

- [ ] **Step 3: `Follower` を実装する**

```go
// internal/cli/follow.go
package cli

import (
	"context"
	"time"

	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// followInterval is how often the service's task list is re-read. A rolling
// ECS deploy takes minutes, so this only has to be fast enough that a new
// task is attached well before the ALB has sent it much traffic.
const followInterval = 15 * time.Second

// Follower keeps a SessionSet in step with the service's RUNNING tasks:
// it attaches to tasks that appear and drops ones that go away.
//
// Without it, `tetherd run` holds the sessions it opened at startup, so a
// deploy replacing every task leaves the run attached to nothing while the
// ALB routes to tasks nobody is listening on - requests would silently go
// to the application instead of the developer's laptop.
type Follower struct {
	Set      *SessionSet
	Interval time.Duration
	// List returns the tasks that should be attached right now.
	List func(context.Context) ([]transport.Task, error)
	// Attach opens one session. A failure is normal and transient: a task
	// can be listed before its ExecuteCommandAgent is RUNNING.
	Attach func(context.Context, transport.Task) (*session.Client, error)
	Logf   func(string, ...any)
}

func (f *Follower) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}

// Run polls until ctx is done. It returns no error on purpose: every
// failure here is transient and the sessions already attached are
// unaffected by it.
func (f *Follower) Run(ctx context.Context) {
	interval := f.Interval
	if interval <= 0 {
		interval = followInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f.Set.Reap()
		want, err := f.List(ctx)
		if err != nil {
			// Log a given failure once. The child process shares this
			// terminal, and a throttled API would otherwise print the
			// same line for the length of the session.
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				f.logf("⚠ session   could not re-read the task list (%v); keeping the %d session(s) already attached", err, f.Set.Len())
			}
			continue
		}
		lastErr = ""
		live := map[string]bool{}
		for _, tk := range want {
			live[tk.ID] = true
			if f.Set.Has(tk.ID) {
				continue
			}
			sess, err := f.Attach(ctx, tk)
			if err != nil {
				// Not logged once-only: each task is its own story, and
				// this resolves itself within a poll or two.
				continue
			}
			if f.Set.Add(tk, sess) {
				f.logf("↻ session   task %s attached and is now the primary", short(tk.ID))
			} else {
				f.logf("↻ session   task %s attached (%d total)", short(tk.ID), f.Set.Len())
			}
		}
		for _, id := range f.Set.TaskIDs() {
			if !live[id] {
				f.Set.Remove(id)
			}
		}
	}
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=5 ./internal/cli/ -run Follower`
Expected: PASS

- [ ] **Step 5: mutation で検査する**

1. `f.Set.Reap()` を削除 → 落ちるテストが無ければ、`Reap` が呼ばれていることを検査するテストを足す
2. 消えたタスクを落とすループを削除 → `TestFollowerDropsATaskThatDisappears` が落ちること
3. `err != nil` で `return` する（run を落とす）→ `TestFollowerKeepsGoingWhenAPollFails` が落ちること
4. `lastErr` の抑制を外す → `TestFollowerLogsARepeatedPollFailureOnce` が落ちること
5. `f.Set.Has(tk.ID)` の早期 continue を削除 → 同じタスクに毎回繋ぎ直す。**これを落とすテストが無い**ので足すこと（`Attach` の呼び出し回数を数える）

- [ ] **Step 6: コミット**

---

### Task 4: `run` を `SessionSet` と `Follower` に配線する

**Files:**
- Modify: `internal/cli/run.go`（`discoverTask` の呼び出し、`sess` を使っている全箇所、後片付け、ステータス行）
- Test: `internal/cli/run_e2e_test.go` に追記

**Interfaces:**
- Consumes: Task 1〜3 のすべて
- Produces:
  ```go
  // internal/cli/deps.go — awsProvider インターフェースに追加。
  // sdkProvider と directProvider の両方に実装する。
  //   DiscoverAll(ctx context.Context, t ecsprov.Target) ([]transport.Task, error)

  // internal/cli/run.go — discoverTask の複数形。Task 5 の `status` も
  // これを使う（自前でタスク探索を複製しないこと）。
  func discoverTasks(ctx context.Context, opts RunOptions, d Deps, logf func(string, ...any)) (awsProvider, []transport.Task, error)
  ```
  `discoverTasks` は `ssm` で `prov.DiscoverAll` を呼び、`direct` では 1 件のスライスを返す。
  `discoverTask`（単数）は**残す** — `env` と `doctor` が使っている。

**やること**

1. `discoverTask` の隣に `discoverTasks`（複数形）を足す。`ssm` の場合は `prov.DiscoverAll` を呼び、`direct` の場合は 1 件のスライスを返す。`awsProvider` インターフェースに `DiscoverAll(ctx, t) ([]transport.Task, error)` を足し、`sdkProvider` と `directProvider` の両方に実装する。**`Discover` は残す**（`env` と `doctor` が使っている）。
2. `RunWithDeps` で全タスクに `dialAgent` し、`SessionSet` に入れる。**1 本も繋がらなければ従来どおり失敗**。
3. `sess.DialTCP` → `set.DialTCP`、`sess.Resolve` → `set.Resolve`、`sess.Welcome()` → `set.Primary().Welcome()`。
4. `<-sess.Done()` の待ちを「**全セッションが死んだら**」に変える。secondary の死は `Follower` が回収する。実装は `set.Len() == 0` を検知するチャネルか、primary の `Done()` を監視して `Reap` 後に `Len()` を確認する形。**どちらにせよ、secondary 1 本の死で run が終わってはいけない。**
5. `Follower` を goroutine で起動し、`ctx` の cancel で止める。
6. ステータス行を複数タスク対応にする。今は `tetherd-dev/api  task fbc1abc4…  (started 14m ago)` の 1 行。2 タスク以上なら `tetherd-dev/api  2 tasks (fbc1abc4… primary, a17e1234…)` の形にする（spec §6.3 と design.md のサンプルがこの形）。
7. `--task` が指定されていれば 1 タスクだけに繋ぐ（既存の `Target.TaskID` がそれをやる）。

- [ ] **Step 1: 失敗するテストを書く**

`run_e2e_test.go` に追記。既存の `startAgentFor` を 2 つ使う。

**`fakeProvider` は今 `task transport.Task`（単数）を持っている**（`run_e2e_test.go:123-131`）。複数タスクを返せるよう `tasks []transport.Task` を足し、`DiscoverAll` を実装し、既存の `Discover` は `tasks` が空なら従来の `task` を返す形にして**既存テストを壊さない**こと。既存テストは全部 `task:` で組んでいるので、そこを一括で書き換えるのは差分を無駄に広げる。

```go
func TestRunAttachesToEveryTaskSoStealCannotMissOne(t *testing.T) {
	// desired_count = 2: the ALB picks which task a request lands on, so a
	// run attached to one of them silently misses half the traffic. This
	// is the defect v0.3b exists to fix.
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	p := &fakeProvider{
		region: "ap-northeast-1",
		tasks: []transport.Task{
			{ID: "older", SubnetID: "subnet-a", StartedAt: time.Unix(1000, 0), Addr: a1.addr},
			{ID: "newer", SubnetID: "subnet-a", StartedAt: time.Unix(2000, 0), Addr: a2.addr},
		},
		vpc: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
	}
	opts := RunOptions{
		Transport: "ssm", Cluster: "c", Service: "s", TargetEnv: "dev", User: "tester",
		Token: "tok", NoNetwork: true,
		ExecPath: "/usr/bin/true", Command: []string{"true"},
	}
	var out strings.Builder
	if code, err := RunWithDeps(context.Background(), opts, &out, depsFor(p)); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v log=%s", code, err, out.String())
	}
	if !strings.Contains(out.String(), "2 tasks") {
		t.Fatalf("the status line must say how many tasks are attached:\n%s", out.String())
	}
	// Both agents must have seen this user attach.
	if len(a1.a.Sessions()) != 0 || len(a2.a.Sessions()) != 0 {
		t.Log("sessions are torn down by the time Run returns, as expected")
	}
}

func TestRunSurvivesASecondaryTaskGoingAway(t *testing.T) {
	// A rolling deploy replaces tasks one at a time. Losing a secondary
	// must not end the run - before v0.3b any session loss did.
	...
}

func TestRunFailsWhenNoTaskCanBeAttached(t *testing.T) {
	// Unchanged behaviour: zero sessions is still fatal.
	...
}
```

**この 3 件の 2 つ目と 3 つ目は、実装者が既存のテストの形（`runInBackground`、`waitFor`、`safeLog`）に合わせて書くこと。** 特に 2 つ目は、agent を 2 つ立てて片方の listener を閉じ、`run` が生き続けて子プロセスが動いたままであることを確認する。

- [ ] **Step 2〜4: 実装と確認**

Run: `go test -race -count=1 ./internal/cli/`

- [ ] **Step 5: mutation で検査する**

1. secondary の死で run を終える（`<-set.Primary().Done()` ではなく最初に死んだものを待つ）→ 2 つ目のテストが落ちること
2. 1 タスクにしか繋がない（`tasks[:1]`）→ 1 つ目のテストが落ちること
3. `Follower` の起動を削除 → 落ちるテストが無ければ、`run` の中で `Follower` が動いていることを検査するテストを足す
4. ステータス行の `2 tasks` を消す → 落ちること

- [ ] **Step 6: コミット**

---

### Task 5: `tetherd status`

**Files:**
- Create: `internal/cli/status.go`, `internal/cli/status_test.go`
- Modify: `internal/cli/root.go`（サブコマンド登録）

**設計上の決定**: `status` は全タスクに `Incoming` 無効・トークン無しで接続し、各タスクの接続者を表示して切る。新しい制御オペレーションは**作らない**が、**`Welcome` にフィールドを 1 つ足す**。

実測（計画の自己レビューで確認した）: `proto.Welcome.Others` は **`[]string`**（ユーザー名だけ）で、`internal/agent/registry.go:56` の `SessionInfo{User, From, Since}` は**ワイヤに乗っていない**。agent 側に `Sessions()` があってデータは存在するので、乗せるだけで済む。

「誰が繋いでいるか」だけでは `status` の答えたい問いに足りない — 実際の問いは「同僚はまだ繋いでいるのか」「いつから繋いでいるのか」なので、`From` と `Since` が要る。プロトコルの互換規約は「**ストリーム型とフィールドは追加のみ**」なので、追加は規約内。

```go
// internal/proto/messages.go に追加
// SessionInfo describes one attached session in Welcome.Sessions.
type SessionInfo struct {
    User  string    `json:"user"`
    From  string    `json:"from"`
    Since time.Time `json:"since"`
}

// Welcome gains, alongside the existing Others []string:
//   Sessions []SessionInfo `json:"sessions,omitempty"`
```

**`Others` は残す**。古い CLI はそれを読み続けるし、`proto.Version` は上げない。`status` は `Sessions` があればそれを使い、無ければ `Others` にフォールバックして「（詳細は古い agent では取れません）」と言う — v0.3a の agent が動いている環境にこの CLI を繋ぐ経路が実在する（spec §8 がその検証を要求している）。

`internal/agent` 側は `others()` の隣に `sessionInfos()` を足して `Welcome.Sessions` を埋める。**`Sessions()` のソートが契約**（`registry_test.go:152` がそれを留めている）なので、同じ順序を使うこと。

出力の形:

```
tetherd  tetherd-dev/api  2 tasks
  task fbc1abc4…  (started 2h12m ago)
    shota   from 203.0.113.5   attached 14m ago   → localhost:8080
    taro    from 198.51.100.9  attached 3m ago    → localhost:3000
  task a17e1234…  (started 6m ago)
    (nobody attached)
```

- [ ] **Step 1: 失敗するテストを書く**

```go
func TestStatusListsWhoIsAttachedToEachTask(t *testing.T) {
	// Steal is shared: two developers on one dev service. "my requests
	// are not arriving" is answered by seeing who else is attached and to
	// which task, so this is the diagnostic for the most likely support
	// question.
	...
}

func TestStatusSaysSoWhenNobodyIsAttached(t *testing.T) {
	// An empty list must read as "nobody attached", not as an empty
	// section a reader mistakes for an error.
	...
}

func TestStatusNeverSendsATokenOrTakesRequests(t *testing.T) {
	// status attaches to read, never to receive: it must send Incoming
	// disabled and no token, or it would register as a steal target and
	// quietly take another developer's requests into a process that
	// exits immediately.
	...
}

func TestStatusReportsATaskItCannotReach(t *testing.T) {
	// One unreachable task must not hide the others.
	...
}
```

3 つ目は**セキュリティ上の要点**なので、agent 側で受け取った `proto.Hello` を検査する形で書くこと（`startAgentFor` の agent から `Sessions()` を見るか、`fakeEnvReader` のように hello を捕まえる仕掛けを足す）。

- [ ] **Step 2〜6**: 実装、確認、mutation（`Incoming.Enabled = true` にする mutation が 3 つ目のテストで落ちること、トークンを載せる mutation も落ちること）、コミット

---

### Task 6: `tetherd token rotate`

**Files:**
- Create: `internal/cli/token.go`, `internal/cli/token_test.go`
- Modify: `internal/cli/root.go`、`internal/config/config.go`（書き込みのヘルパが無ければ足す）

**設計上の決定**: `~/.tetherd/config.yml` を 0600 で書き直す。**`config.EnsurePersonal` が既に flock で直列化してトークンを生成している**ので、その仕組みを再利用する（必ず先に読むこと）。回転後は「今動いているセッションは古いトークンのまま」なので、その旨を出力に書く。

- [ ] **Step 1: 失敗するテストを書く**

```go
func TestTokenRotateReplacesTheTokenAndKeepsEverythingElse(t *testing.T) {
	// The personal file also carries `user` and an aws profile. Rotating
	// the token must not drop them - losing `user` would silently change
	// which requests the agent matches.
	...
}

func TestTokenRotateWritesZeroSixHundred(t *testing.T) {
	// The token is the only thing between a public ALB and this laptop.
	...
}

func TestTokenRotateProducesADifferentTokenEveryTime(t *testing.T) {
	...
}

func TestTokenRotateSaysRunningSessionsKeepTheOldToken(t *testing.T) {
	// A developer who rotates because they think the token leaked needs
	// to know the leak is not closed until every running session is
	// restarted.
	...
}
```

- [ ] **Step 2〜6**: 実装、確認、mutation（0600 を 0644 にする、既存フィールドを落とす、同じトークンを返す — 全部落ちること）、コミット

---

### Task 7: doctor — `Unknown` ステータスと steal の行

**Files:**
- Modify: `internal/doctor/doctor.go:15-35`、`internal/doctor/checks.go`、`internal/cli/doctor.go`
- Test: `internal/doctor/checks_test.go`、`internal/doctor/doctor_test.go`

**設計上の決定**: 今 `!` が 2 つの意味を持っている — 「動くが注意」（`Warn`）と「依存する行が失敗したので検査できなかった」（`Detail` に `not checked:` を付けた `Warn`）。**これを分ける。**

```go
const (
	OK Status = iota
	Warn          // works, but deserves attention   → ⚠
	Fail          // will not work until fixed        → ✗
	Unknown       // could not be checked             → ?
)
```

`⚠` に統一する理由は `run` が既に `⚠` を使っていること。`?` を足す理由は「検査できなかった」が設定への警告ではないこと — レビューの指摘そのもの（同じ `LocalOverlaps` の所見が 2 つの記号と 2 つの名詞で出ていた）。

新しい行 2 つ:

- `CheckSteal(inc proto.Incoming, localPort int, listening bool) Result` — steal が有効なのに `localhost:<port>` で誰も listen していなければ `Warn`。**これが無いと、`run` が exit 2 するマシンで doctor が全部緑を出す。**
- `CheckCredentialEndpoint(addr string, arn string, err error) Result` — `run` の `✓ iam` と同じ経路を doctor でも通す。今 doctor の `AWS identity` 行は**開発者自身の ARN**を出していて、`run` の `iam` 行はタスクロールを出す。同じ言葉で違うものを報告しているので、行を分けて両方出す。

- [ ] **Step 1〜6**: 既存の `checks_test.go` の形（純粋な判定関数にテーブルテスト）に合わせて書く。**golden な doctor レポートを検査しているテストがある**（`doctor_test.go`）ので、記号を変えるとそれが落ちる。落ちたら golden を更新し、**更新が意図通りであることを diff で確認**してからコミットする。mutation: `Unknown` を `Warn` に戻す、`⚠` を `!` に戻す、`CheckSteal` を常に `OK` にする — 全部落ちること。

---

### Task 8: 持ち越しの回収

**Files:**
- Modify: `internal/session/client.go`（`duplicate_user` のリトライ）、`internal/cli/run.go`（`--no-network` と `remote_domains`）、`internal/agent/agent.go`（`ListenAndServe` 削除）、`internal/agent/config.go`（`AppAddr` の既定）

**4 件。それぞれ独立。**

**8a. `duplicate_user` の即再実行。** agent の登録解除は接続断に気づいた自分の goroutine で走るので、CLI が終了した瞬間はまだ前のセッションを保持している（実測 60/60）。**CLI 側で最大 2 秒リトライする**。agent の「1 ユーザー 1 セッション」の不変条件は変えない — 死んだセッションを置き換える判定は agent 側では安全に作れないため。

```go
// In dialAgent (internal/cli/run.go), wrap session.Dial:
//
// A hello refused with duplicate_user is usually this developer's own
// previous run: the agent unregisters on the goroutine that notices the
// disconnect, so for a moment after `tetherd run` exits the old session is
// still registered. Measured: at the instant RunWithDeps returns, the agent
// still held it in 60 of 60 runs. Retrying briefly turns a confusing
// refusal into a two-second pause; a genuine second session still fails,
// just later.
const duplicateUserRetry = 2 * time.Second
```

テスト: agent に 1 セッション繋いだ状態で 2 本目を張り、途中で 1 本目を閉じたら 2 本目が成功すること。閉じなければ `duplicateUserRetry` 後に失敗すること（テストではその定数を短くできるよう `Options` か `Deps` に出す）。

**8b. `--no-network` が `remote_domains` を黙って無視する。** `run` が `--no-network` のとき DNS リゾルバも `/etc/resolver` も張らないので、`.tetherd.yml` に `remote_domains` を書いた開発者は「設定したのに効かない」状態になる。**1 行の警告**を出す: `⚠ network  --no-network ignores remote_domains (myapp.internal); those names resolve on this laptop`。テストは警告文の有無と、`remote_domains` が空なら出ないこと。

**8c. `Agent.ListenAndServe` は dead code。** 削除する。**削除前に `grep -rn 'ListenAndServe' --include='*.go' .` で参照が無いことを確認**し、テストからも使われていなければ消す。使われていれば消さずに報告する。

**8d. `Config.AppAddr` の既定が `ConfigFromEnv` の外で効かない。** `agent.New` で `Config` を直に組む経路（テストと将来の埋め込み）では `AppAddr` が空のまま `Proxy` に渡る。`Config` を受け取る側で既定を当てるか、`New` で正規化する。**`ConfigFromEnv` の既定値 `127.0.0.1:8081` を 2 箇所に書かない**こと — 定数を 1 つにする。

- [ ] 各項目ごとにテストを先に書き、mutation で確認し、**項目ごとに別コミット**にする。レビュアが 1 件だけ却下できるようにするため。

---

### Task 9: doctor のターゲットグループ検査、Terraform、ドキュメント、e2e

**Files:**
- Modify: `deploy/dev-env/` の developer policy（`elasticloadbalancing:DescribeTargetGroups` を足す。**どのファイルにあるか grep で確認すること** — v0.3a では sg-app が `network.tf` ではなく `rds.tf` にあって計画が外れた）
- Modify: `internal/doctor/checks.go`（`CheckTargetGroup`）、`internal/cli/doctor.go`
- Modify: `docs/config.md`、`docs/design.md`、`docs/specs/2026-09-12-v1-macos-design.md`、`docs/e2e-aws.md`

**`CheckTargetGroup(protocolVersion string, port int32, err error) Result`**: `protocol_version` が `HTTP1` でなければ `Fail`（spec §5.1 が gRPC / HTTP2 のターゲットグループを対象外としている）。ポートが agent の 8080 でなければ `Fail`。権限が無ければ `Unknown`（Task 7 の新ステータス）で「developer policy に `elasticloadbalancing:DescribeTargetGroups` が要る」と言う。

**ドキュメント**:
- `docs/config.md`: `incoming` の節に「全タスクに接続する」ことと `status` / `token rotate` を追記
- `docs/design.md` §16 ロードマップ: v0.3b を ✓ に
- spec §6.3 の起動シーケンス: 「全タスクへ接続」が実装と一致したことを反映（**今は仕様が先に書いてあって実装が追いついていない状態**）
- spec §12: v0.3b の検証枠を作る
- `docs/e2e-aws.md`: 27〜31 行を足す

**e2e の新しい行**（番号は既存の最後の次から。**必ずファイルを読んで確認すること** — v0.3a で私が行数を記憶で言って外した）:

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 27 | `desired_count` を 2 にして `$RUN -- <自分のサーバ>`、一致するヘッダーで 20 回 `curl` | 20 回すべてラップトップに届く | どのタスクに落ちても steal できる（v0.3b の本体） |
| 28 | `$RUN` 実行中に `aws ecs update-service --force-new-deployment` | 新しいタスクに `↻ session` が出て繋がり、古いタスクが落ちても run は生き続ける | deploy 追従 |
| 29 | `./bin/tetherd status` | 各タスクに誰が繋いでいるかが出る | 共有時の診断 |
| 30 | `./bin/tetherd token rotate` 後に古いトークンで `curl` | タスクの応答（ラップトップには来ない） | 回転が効く |
| 31 | ターゲットグループを HTTP2 にして `./bin/tetherd doctor` | `✗` で `protocol_version` を名指しする | steal の要件検査 |

**`terraform apply` は実行しない。** developer policy の変更をまとめて人間に提示し、依頼する。27 と 28 は `desired_count` の変更が必要なので、それも同じ依頼に含める（**検証後に 1 に戻すことも依頼に含めること** — 課金が倍になるため）。

- [ ] 実装 → `terraform fmt -check` と `validate` → ドキュメント → **人間に apply を依頼** → e2e → spec §12 に結果を記録 → コミット

---

## 完了条件

1. `go test -race ./...` / `make lint` / `GOOS=linux go vet ./...` / `make build` / `go mod tidy` が no-op
2. `desired_count = 2` で、一致するヘッダーを持つリクエストが**どのタスクに落ちても**ラップトップに届く（e2e 27）
3. deploy 中にタスクが入れ替わっても `run` が生き続け、新しいタスクに繋ぐ（e2e 28）
4. secondary セッションの喪失で `run` が終わらない。全セッションの喪失では従来どおり終わる
5. `tetherd status` が各タスクの接続者を出し、**トークンを送らず、steal の対象にならない**
6. `tetherd token rotate` が 0600 で書き、既存フィールドを保ち、実行中セッションが古いトークンのままであることを伝える
7. doctor が「検査できなかった」を `?` で、「注意」を `⚠` で出し、steal と認証情報エンドポイントの行を持つ
8. `duplicate_user` で即再実行が失敗しない
9. spec §12 に v0.3b の検証結果が記録されている

## 次の計画（v0.4、この計画には含めない）

Homebrew tap、LaunchDaemon（helper を `sudo` 前面起動から常駐へ）、GoReleaser、README。**他人の Mac に配れる状態**にする。

持ち越し: 書き込み中断の猶予が「クライアントが送り切らないボディを読んでブロックした writer」を解放できない点、`internal/dnsproxy` の一部テストがポート競合の緩和策の外にある点、リポジトリ直下の `.tetherd.yml` がテストスイートの隠れた入力になりうる点（cwd の設定発見に依存）。
