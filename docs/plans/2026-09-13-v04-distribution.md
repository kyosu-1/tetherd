# v0.4: 他人の Mac に配れる状態にする Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `brew install kyosu-1/tap/tetherd` と `sudo tetherd-helper install` の 2 行で、tetherd を触ったことのない Mac が `tetherd run` を使える状態になること。

**Architecture:** 配布の器を作る（GoReleaser → GitHub Release → Homebrew tap の cask → tetherd 自身が入れる LaunchDaemon）。**受け入れ側のコードは既にある** — `internal/helper/install.go` の `EnsureGroup` / `InstallExec` が root 初期化を持ち、`cmd/tetherd-helper/main.go` が起動時にそれを呼ぶ。v0.4 が足すのは「パッケージング、常駐の仕組み、ドキュメント、そして tetherd が入っていない Mac で本当に動くかの検証」。

**Tech Stack:** GoReleaser v2.18.1（Homebrew **cask**）、launchd、GitHub Actions。**この計画では初めて `go.mod` の変更を許可する**（Task 6 が `aws-sdk-go-v2/service/elasticloadbalancingv2` を足す。それ以外では増やさない）。

**Spec:** `docs/specs/2026-09-12-v1-macos-design.md`（§6「macOS ヘルパーの配布と保守」、§9 インフラ変更、§12 検証）

---

## 最初に読むこと: 計画の前提を実測して 3 つ覆した

この計画の初稿は `brew services` で helper を常駐させる前提で書いてあった。**実測の結果それは成り立たない。**以下は推測ではなく測った結果で、設計の土台なので変えないこと。

**1. GoReleaser の `brews:`（formula 生成）は使えない。**

```
$ goreleaser check   # v2.18.1, brews: を含む設定
• DEPRECATED:  brews  should not be used anymore
• error=configuration is valid, but uses deprecated properties
⨯ check failed     exit status 2
```

GoReleaser 公式の deprecation 表では **v2.10 で soft、v2.16 で hard deprecated**（現在 v2.18.1）。代替は `homebrew_casks:` と明記されている。`release` 自体は警告だけで通る（`deprecate.Notice` は `ctx.Deprecated = true` を立ててログを出すだけ）が、**`check` が exit 2 で落ちる**ので CI の門にできない。次のメジャーで消える経路に配布を載せない。

**2. Homebrew の cask には「サービス」が無い。`brew services` は cask を管理できない。**

cask の `service` スタンザは launchd とは無関係で、macOS の Services メニュー用ディレクトリへファイルを移すだけ:

```ruby
# $(brew --repository)/Library/Homebrew/cask/artifact/service.rb
class Service < Moved      # ← Moved アーティファクト
end
# $(brew --repository)/Library/Homebrew/cask/config.rb:30
servicedir: "~/Library/Services",
```

一方 `require_root true` / `keep_alive` / `run [...]` は **formula の `service do` DSL 専用**（`Library/Homebrew/service.rb:219` に `require_root` が実在する）。つまり `sudo brew services start tetherd` は **formula を選ばない限り存在しない**。

**3. 結論（Ruling）: tetherd が自分の LaunchDaemon を持つ。**

`homebrew_casks:` で配り、常駐は `sudo tetherd-helper install` が `/Library/LaunchDaemons/` に plist を書いて `launchctl bootstrap` する。**却下した代替は formula（`brews:`）を deprecation 警告つきで使い続けること** — UX は `sudo brew services start tetherd` の 1 行ぶん良いが、`goreleaser check` を門にできず、hard deprecated の経路に乗る。

自前で持つほうが結果的に良い点（これは副産物ではなく採用理由）:
- plist が自分のものなので `KeepAlive` / `ThrottleInterval` / ログの行き先 / `--exec-src` を自分で決められる
- `brew upgrade` 後に `sudo tetherd-helper install` を再実行すれば**両方のバイナリが入れ替わる**（後述）
- **間違っていた場合の代償**: ユーザが打つコマンドが `brew services` でない 1 つだけ。小さく、あとから formula に戻せる

**4. cask は quarantine を踏む。** GoReleaser の cask テンプレート自身が `postflight` に「removing the macOS quarantine attribute」の注記を持ち、公式の移行例も `post_install` で `xattr` を外している。**署名と notarization（Apple Developer アカウントが必要）は v0.4 の範囲外**で、v1 の課題として書き残す。v0.4 は `postflight` で quarantine 属性を外す。

**5. `LICENSE` ファイルがリポジトリに無い。** だから cask の `license:` は**書かない**（持っていないライセンスを宣言しないこと）。「LICENSE を追加するかは所有者の判断」として PR に書く。**MIT だと推測して書かない。**

**7. spec §8 は既にこの全部を指定していた。読んでから書くこと。** `docs/specs/2026-09-12-v1-macos-design.md:415-427` に導入手順・ラベル・`install` / `uninstall` の中身・`KeepAlive` の形まで書いてある。**この計画の初稿はそれを読まずに書いたので、ラベルを発明して production の文字列 2 本と矛盾させていた。** spec が binding authority。ただし spec の 3 点は今や古い:

| spec の記述 | 現状 | この計画の扱い |
|---|---|---|
| 「formula は 3 バイナリを prefix に置くだけ」 | **formula 生成は hard deprecated**（実測 1） | **cask にする。spec を修正する**（Task 2 Step 10） |
| tap は `kyosu-1/homebrew-tetherd` | batcha が使っている `kyosu-1/homebrew-tap` が既にある | **既存の tap を使う**（利用者の指示）。**spec を修正する** |
| **helper は常駐しない**（`Sockets` によるソケットアクティベーション、`launch_activate_socket()` を `purego` で呼ぶ、30 秒でアイドル終了） | 未実装。`helper.Server.ListenAndServe` は自分でソケットを作る | **v0.4 では常駐にする。下の Ruling S を読むこと** |

**Ruling S — ソケットアクティベーションは v0.4 では実装しない。** `RunAtLoad: true` + `KeepAlive: {SuccessfulExit: false}` の常駐デーモンにする。

- **理由 1:** 常駐なら helper 側のコードは **1 行も変わらない**（`ListenAndServe` がそのまま使える）。アクティベーションは `purego` という**新しい依存**（この計画は Task 6 の 1 モジュールしか許していない）と、fd を launchd から受け取る新しい経路と、アイドル終了のライフサイクルを要求する。
- **理由 2: 後から入れるのが安い。** plist を書くのは `sudo tetherd-helper install` 自身で、それは spec が定めた**アップグレード手順そのもの**。だから後で `Sockets` を足しても、利用者がすることは「文書済みのコマンドをもう一度打つ」だけ。**「先に入れないと高くつく」と最初は考えたが、それは間違いだった。**
- **代償:** それまでの利用者の Mac には root のデーモンが常駐する。spec が常駐を避けたのはこの攻撃面のため。**小さくない代償なので、README にも spec にも「v0.4 は常駐、アクティベーションは次」と書く。**
- **spec §8 を「v0.4 では常駐」と明記して修正すること**（Task 2 Step 10）。黙って乖離させない — それが v0.3b で 7 件直した欠陥そのもの。

**6. `goreleaser` はこのマシンに入っていない。入れないこと。** `go run github.com/goreleaser/goreleaser/v2@v2.18.1 ...` で動く。**実測済みで `go.mod` / `go.sum` は変化しない**（`go run pkg@version` はカレントモジュールを無視する）。バージョンは必ず固定する（`@latest` は再現しない）。

---

## Global Constraints

- **依存追加は Task 6 の 1 モジュールだけ。** `aws-sdk-go-v2/service/elasticloadbalancingv2` 以外で `go.mod` を増やさない。他のタスクでは `go mod tidy` が no-op で、`git diff main -- go.mod go.sum` が空。
- **テストは AWS 認証情報なし・ループバック以外のネットワークなし・root なし・`tetherd-helper` なしで通る。例外なし。** v0.4 は root で動くコードを足すので、ここが最も難しいタスクになる。**`install` のロジックは注入した root ディレクトリと注入した `run func(string, ...string) (string, error)` の上の純粋な関数にすること** — `EnsureGroup(run, name)` が既に使っている継ぎ目と同じ形。`launchctl` や `/Library` を直接叩く関数はテストできない。
- `gofmt` クリーン（`make lint` = `go vet ./...` + `gofmt -l cmd internal examples`）。
- agent は Linux で動く: `GOOS=linux go vet ./...` がクリーン、`make build` が通る。**`install` は darwin 専用コードなので、ビルドタグかランタイム判定で Linux のビルドを壊さないこと**（`main.go:42` に既に `runtime.GOOS != "darwin"` の判定がある）。
- `terraform plan` / `apply` / `destroy` は**実行しない**。Task 6 が developer policy を変えるので、適用は人間に依頼する。`fmt` と `validate` だけ実行してよい。
- **git worktree を作らない。** ブランチは main のチェックアウト上。
- 複数エージェントが同じツリーで動くので、**ステージはファイルパスを明示**。`git add <dir>` と `git stash` は禁止。**`git checkout` / `git pull` / `git merge` / `git push` をしないこと** — 実際に v0.3b の最後で、別のエージェントが報告を書いている最中にコントローラがブランチをマージして checkout し、同じチェックアウトで 2 つの git が走った。隔離ツリーは **`mktemp -d`**、展開後に `git show <sha>:<path> | shasum` で数ファイル検証してから信用する。
- コミットメッセージの末尾は正確に:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_015mTfR8PDnAkbdwehd3JgF9
  ```

## 既にあるもの / 無いもの（実測）

| | 状態 |
|---|---|
| `internal/helper/install.go` の `EnsureGroup` / `InstallExec` | **ある。** `GroupName = "tetherd"`、`ExecInstallDir = "/usr/local/libexec/tetherd"`、`InstallExec` は root:gid の 02755 で置く |
| `cmd/tetherd-helper/main.go` の起動時 root 初期化 | **ある。** `--socket` / `--exec-src` / `--install-dir` / `--resolver-dir` / `--version` を受け、`flag.Arg(0) == "version"` だけ部分的にサブコマンド扱い |
| helper のバージョン不一致メッセージ | **ある。** ただし `launchctl` の形で案内する文面は Task 2 で自前 plist のラベルに合わせ直す |
| `internal/version.Version` の ldflags 埋め込み | **ある。** `Makefile:3` が `-X github.com/kyosu-1/tetherd/internal/version.Version` で渡す |
| `examples/.tetherd.yml` | **ある**（README から指せる） |
| `.goreleaser.yml` / リリース workflow / plist / cask / README / LICENSE | **どれも無い** |

## 参考にする既存の慣例（`../batcha`）

同じ作者の別プロジェクトが GoReleaser + Homebrew を既に運用している。**合わせるところと、意図的に外すところを分ける。**

合わせる:
- tap は**新規作成しない**。`kyosu-1/homebrew-tap` が既にあり、batcha が使っている。tetherd はそこに 1 つ増えるだけで `brew install kyosu-1/tap/tetherd`。
- secret は `HOMEBREW_TAP_TOKEN`（設定内では `HOMEBREW_TAP_GITHUB_TOKEN` に渡す）。**batcha が同じ secret を使っているので、user/org スコープなら既に存在する可能性が高い。Task 1 で確認し、無ければ人間に依頼する。**
- workflow: タグ `v[0-9]+.[0-9]+.[0-9]+` の push、`permissions: contents: write`、`fetch-depth: 0`、`goreleaser-action@v6`、`version: "~> v2"`、`args: release --clean`、env 2 つ。

外す:
- batcha は `brews:`（formula）。**tetherd は `homebrew_casks:`。**上の実測 1・2 のとおり。
- batcha は `goos: [linux, darwin, windows]`。**tetherd は darwin だけ** — v1 は macOS 専用で、helper は pf と `/etc/resolver` を触る。`tetherd-agent` は Linux で動くがコンテナイメージとして別経路（`make push-images`）で配るので GoReleaser の対象外。
- batcha は単一バイナリ。**tetherd は 3 本**（`tetherd` / `tetherd-helper` / `tetherd-exec`）を 1 つのアーカイブに入れる。

## File Structure

| ファイル | 責務 |
|---|---|
| `.goreleaser.yml`（新規） | 3 バイナリ × darwin amd64/arm64、アーカイブ、`homebrew_casks:` |
| `.github/workflows/release.yml`（新規） | タグ push で GoReleaser を回す |
| `internal/helper/daemon.go`（新規） | plist の生成、root 所有の検証、バイナリ配置 — **すべて注入した root と `run` の上の純粋な関数** |
| `internal/helper/daemon_test.go`（新規） | 上記のテスト。root なしで通る |
| `cmd/tetherd-helper/main.go` | `install` / `uninstall` サブコマンド |
| `docs/install.md`（新規） | cask の全文、パスの判断、quarantine、アップグレード手順 |
| `docs/uninstall.md`（新規） | **初回インストールを検証するために必要**。グループ・setgid・LaunchDaemon・残留 pf ルールを消す手順 |
| `README.md`（新規） | 導入から `tetherd run` まで |
| `internal/cli/doctor.go` | `bounded` の直接受信アームの context エラー扱い（Task 8） |
| `internal/proto/messages.go` | agent の既定プロキシポートを両側から見える場所に出す（Task 6） |
| `internal/doctor/checks.go` / `internal/cli/doctor.go` | `CheckTargetGroup`（Task 6） |
| `deploy/dev-env/iam-developer.tf` | developer policy に権限を戻す（Task 6。**人間が apply**） |
| `internal/transport/ssm/ssm.go` / `internal/config/config.go` | 設定項目の嘘 3 件（Task 7） |
| `docs/e2e-aws.md` / `docs/specs/...` | 検証行と記録 |

---

### Task 1: GoReleaser の設定と リリース workflow

**Files:**
- Create: `.goreleaser.yml`, `.github/workflows/release.yml`
- Modify: `Makefile`（`release-check` / `release-dry-run` ターゲット）

**Interfaces:**
- Produces: タグを打つと darwin amd64/arm64 のアーカイブが GitHub Release に付き、`kyosu-1/homebrew-tap` に cask が push される状態。アーカイブには **3 本**入る。

**設計上の決定（変えないこと）:**

1. `goos` は `darwin` だけ。`tetherd-agent` は含めない。
2. `builds:` は 3 つ。`ldflags` は `Makefile:3` と同じ `-X github.com/kyosu-1/tetherd/internal/version.Version={{.Version}}`。`CGO_ENABLED=0`。
3. `before.hooks` に `go mod tidy` と `make lint` と `go test -race ./...`。batcha は `lint` を入れていないが、tetherd は CI で `gofmt` を見ているのでリリース時も同じ門をくぐらせる。
4. **`brews:` を書かない。** 上の実測 1。

- [ ] **Step 1: `.goreleaser.yml` の `builds` / `archives` / `checksum` / `changelog` を書く**

```yaml
version: 2
before:
  hooks:
    - go mod tidy
    - make lint
    - go test -race ./...

builds:
  - id: tetherd
    main: ./cmd/tetherd
    binary: tetherd
    env: [CGO_ENABLED=0]
    goos: [darwin]
    goarch: [amd64, arm64]
    ldflags:
      - -s -w -X github.com/kyosu-1/tetherd/internal/version.Version={{.Version}}
  - id: tetherd-helper
    main: ./cmd/tetherd-helper
    binary: tetherd-helper
    env: [CGO_ENABLED=0]
    goos: [darwin]
    goarch: [amd64, arm64]
    ldflags:
      - -s -w -X github.com/kyosu-1/tetherd/internal/version.Version={{.Version}}
  - id: tetherd-exec
    main: ./cmd/tetherd-exec
    binary: tetherd-exec
    env: [CGO_ENABLED=0]
    goos: [darwin]
    goarch: [amd64, arm64]
    ldflags:
      - -s -w -X github.com/kyosu-1/tetherd/internal/version.Version={{.Version}}

archives:
  - ids: [tetherd, tetherd-helper, tetherd-exec]

checksum:
  name_template: "checksums.txt"

changelog:
  sort: asc
  filters:
    exclude: ["^docs:", "^test:"]
```

**`homebrew_casks:` は Task 2 で足す** — cask の中身（binaries、caveats、uninstall、postflight）は LaunchDaemon の設計と一体なので、そちらで決める。

- [ ] **Step 2: `release.yml` を書く**

batcha の `release.yml` を踏襲する。**`fetch-depth: 0` を落とさないこと**（GoReleaser は changelog にタグ履歴が必要）。

- [ ] **Step 3: `Makefile` に dry-run を足す**

```make
# goreleaser はこのリポジトリの依存ではないので go run で固定版を使う
# (go run pkg@version は go.mod を変更しない)。
GORELEASER := go run github.com/goreleaser/goreleaser/v2@v2.18.1

.PHONY: release-check release-dry-run
release-check:
	$(GORELEASER) check

release-dry-run:
	$(GORELEASER) release --snapshot --clean --skip=publish
```

既存の `.PHONY` 行は `build test lint clean e2e-local push-images`。上のように別行で宣言してよい。

- [ ] **Step 4: 検証する**

Run: `make release-check` → **exit 0。警告行が 1 つも出ないこと。**（`brews:` を書くとここで exit 2 になる。それが `homebrew_casks` を選んだ理由。）
Run: `make release-dry-run`
Expected: `dist/` に darwin の amd64 / arm64 のアーカイブができる。

**`tar tzf` でアーカイブの中身が 3 本であることを目で確認すること。** `builds:` を 3 つ書いても `archives.ids` を間違えると 1 本だけのアーカイブが 3 つできる。それは cask 側で symlink が壊れる形で初めて分かるので、ここで見る。**確認したコマンドと出力を報告に貼ること。**

- [ ] **Step 5: secret の存在を確認する**

Run: `gh secret list --repo kyosu-1/tetherd`（と、可能なら user スコープ）
`HOMEBREW_TAP_TOKEN` があれば報告に書く。無ければ「人間が設定する必要がある」と報告に書く（**自分で作らない** — PAT の発行は人間の作業）。必要なのは `kyosu-1/homebrew-tap` に Contents: write を持つ fine-grained PAT。

- [ ] **Step 6: `dist/` を `.gitignore` に足す**（無ければ）。**コミットに `dist/` を入れないこと。**

- [ ] **Step 7: コミット**

```bash
git add .goreleaser.yml .github/workflows/release.yml Makefile .gitignore
git commit
```

---

### Task 2: LaunchDaemon — `tetherd-helper install` / `uninstall`

**Files:**
- Create: `internal/helper/daemon.go`, `internal/helper/daemon_test.go`
- Modify: `cmd/tetherd-helper/main.go`
- Modify: `.goreleaser.yml`（`homebrew_casks:`）
- Create: `docs/install.md`

**Interfaces:**
- Consumes: `helper.ExecInstallDir`, `helper.ExecName`, `helper.GroupName`, `helper.EnsureGroup`, `helper.InstallExec`（すべて既存）
- Produces: `sudo tetherd-helper install` が冪等に常駐化し、`sudo tetherd-helper uninstall` が元に戻す。`helper.DaemonLabel` が launchd ラベルの唯一の出どころ

**ここが v0.4 の核心。設計上の決定を先に全部書く。**

1. **ラベルは `dev.tetherd.helper`。決めるのではなく、既にあるものに合わせる。** spec §8 が `/Library/LaunchDaemons/dev.tetherd.helper.plist` を指定しており、**production の文字列 2 本が既にこのラベルを案内している**: `internal/doctor/checks.go:20` と `internal/helper/client.go:43` の `sudo launchctl kickstart -k system/dev.tetherd.helper`。テスト 2 本（`internal/doctor/doctor_test.go:191`、`internal/helper/client_test.go:115`）がそれを留めている。**別のラベルを発明すると doctor が出す案内が動かなくなる。**
2. **plist は root:wheel の 0644。** launchd は group/world 書き込み可能な plist を拒否する。ディレクトリは 0755。
3. **daemon が起動するのは Homebrew のパスではなく root 所有のコピー。** `install` は自分自身と `tetherd-exec` を `ExecInstallDir`（`/usr/local/libexec/tetherd`）にコピーし、plist はそのコピーを指す。理由は `internal/helper/install.go:13` のコメントが `tetherd-exec` について既に書いているのと同じ:「Homebrew の prefix はユーザが書ける」。**root の LaunchDaemon がユーザ書き込み可能なパスのバイナリを起動したら、そのユーザは root を取れる。**
4. **だから `install` は転送先のパス全体を検証する。** `ExecInstallDir` の各構成要素（`/`, `/usr`, `/usr/local`, `/usr/local/libexec`, `/usr/local/libexec/tetherd`）が **root 所有かつ group/world 書き込み不可**であることを確かめ、違えば**何も書かずに失敗する**。
   **なぜ必須か:** Intel Mac では Homebrew の prefix が `/usr/local` で、`install.go:13` のコメントの前提がそこで成り立つか**この計画では確かめられなかった**（Homebrew の書き込み可能ディレクトリ一覧が手元の Homebrew のソースに定数として見つからなかった）。このマシン（Apple Silicon、prefix は `/opt/homebrew`）では `/usr/local` と `/usr/local/libexec` は root:wheel 0755 で成り立っている。**コメントの前提を、コードで強制される不変条件に変える。** そうすれば Homebrew の内部仕様を当てにしなくてよい。
5. **`install` は upgrade でもある。** 冪等で、毎回両方のバイナリを上書きし、plist を書き直し、`bootout` → `bootstrap` し直す。`brew upgrade tetherd` の後にこれを打てば新しいバイナリが常駐する。**README と caveats はこれを「アップグレード手順」として書く。**
6. **`os.Executable()` の結果は `filepath.EvalSymlinks` に通す。** cask は `tetherd-helper` を Homebrew の `bin` から staged path への symlink として置く。symlink のままだと隣にある `tetherd-exec` が見つからない。**これは実機でしか分からない類の失敗なので e2e 行で見る。**
7. **`KeepAlive` は `SuccessfulExit: false` にする（spec §8 がこの形を指定している）。`RunAtLoad` は `true`**（Ruling S。アクティベーションを入れるまでは常駐）。 `main.go` は `EnsureGroup` / `InstallExec` の失敗で `log.Fatalf`（終了コード 1）する。素の `KeepAlive: true` だと、復旧しない失敗で再起動を繰り返してログが埋まり原因が埋もれる。`SuccessfulExit: false` なら異常終了時だけ上げ直すので**正常な停止（`bootout`）では上がってこず**、異常時は `ThrottleInterval` で間隔が空く。**`ThrottleInterval` は明示的に書く** — 既定値（10 秒）に頼らない。
8. **ログの行き先を plist で指定する。** `StandardOutPath` / `StandardErrorPath` を `/var/log/tetherd-helper.log` に。初回インストールで失敗したときに何も残らないのが最悪。

**テスト可能性の制約（Global Constraints の再掲。ここが一番効く）:**

`daemon.go` は次の形にする。**`/Library` や `launchctl` を直接触る関数を書かないこと。**

```go
// DaemonLabel はラベルと plist ファイル名の唯一の出どころ。
const DaemonLabel = "dev.tetherd.helper"

// Paths は install/uninstall が書き込む先。テストは tmpdir を渡す。
type Paths struct {
    Root       string // "/" 。テストは t.TempDir()
    LaunchDir  string // Root からの相対で /Library/LaunchDaemons
    InstallDir string // Root からの相対で ExecInstallDir
}

// Plist は label と引数から plist の中身を返す純粋な関数。
func Plist(label, helperPath, socket, execSrc, installDir, logPath string) []byte

// CheckOwnership は dir の各構成要素が root 所有かつ group/world 書き込み
// 不可であることを確かめる。stat は注入する。
func CheckOwnership(stat func(string) (os.FileInfo, error), dir string) error

// Install は冪等。run は launchctl の注入点（EnsureGroup と同じ形）。
func Install(p Paths, run func(string, ...string) (string, error), srcDir string) error
func Uninstall(p Paths, run func(string, ...string) (string, error)) error
```

- [ ] **Step 1: `Plist` のテストを書いて落とす → 実装 → 通す**

テストの要求（**コードは実装者が書く。この計画は要求だけを書く** — v0.3b で計画が書いたテストコードに 8 件の欠陥が出たため）:
- 返った XML が `plist` としてパースできること。**`plutil -lint` に通すのが最も確実**だが `plutil` は macOS 専用なので、パースは `encoding/xml` で行い、`plutil` があるときだけ追加で通す形にする（Linux の `go vet` とテストを壊さない）。
- `Label` が `DaemonLabel` と一致すること。**リテラルを 2 箇所に書かない。**
- `ProgramArguments` の 0 番目が `helperPath` で、`--socket` / `--exec-src` / `--install-dir` の値が引数どおりであること。
- `KeepAlive` が `SuccessfulExit: false` を含み、**素の `true` ではないこと**。
- `ThrottleInterval` が明示されていること。
- **mutation: `KeepAlive` を素の `true` にする / `Label` を別文字列にする / `--exec-src` の値を `--install-dir` の値と入れ替える。** 最後のものを捕まえられないテスト（部分文字列だけ見るもの）は不十分 — v0.3b で「2 つのフィールドが入れ替わっても通る部分文字列アサーション」が実際に見つかっている。

- [ ] **Step 2: `CheckOwnership` のテストを書いて落とす → 実装 → 通す**

テストの要求:
- 各構成要素が uid 0 所有・0755 なら成功。
- **途中の 1 つが group 書き込み可能（0775）なら失敗し、エラーがそのパスを名指しすること。**
- **途中の 1 つが root 以外の所有なら失敗し、そのパスを名指しすること。**
- 存在しない末端は失敗にしない（`install` が作るので）。ただし**存在する親は全部検査される**こと。
- **root なしで動くこと。** 実ファイルの uid を 0 にはできないので注入点を使う。`Sys().(*syscall.Stat_t)` の偽装が苦しいなら、**`CheckOwnership` は `func(string) (uid uint32, mode os.FileMode, err error)` を受ける形にしてよい** — テストできる継ぎ目を優先すること。シグネチャを変えたら報告に書く。
- **mutation: group 書き込みビットの検査を外す / 末端だけ検査して親を見ない / 所有者検査を外す。**

- [ ] **Step 3: `Install` / `Uninstall` のテストを書いて落とす → 実装 → 通す**

テストの要求:
- `t.TempDir()` を `Root` にして `Install` を呼ぶと、plist が `LaunchDir` に 0644 で、バイナリ 2 本が `InstallDir` に置かれること。**root なしなので所有者は検査せず、モードと配置を検査する**（所有者は `CheckOwnership` が注入した stat で別に検査済み）。
- **2 回呼んでも成功すること（冪等）**。2 回目で plist の内容が同じこと。
- 注入した `run` に渡った `launchctl` の呼び出し列が **`bootout` → `bootstrap` の順**であること。
- **`bootout` が「そんなものは無い」で失敗しても `Install` は成功すること**（初回インストールがこれ）。`bootout` の失敗を握りつぶす条件が広すぎないこと — **`bootstrap` の失敗は握りつぶさない。**
- `Uninstall` が **spec §8 の 4 つ**を行うこと: `launchctl bootout`、plist の削除、**グループの削除**、`/usr/local/libexec/tetherd` の削除。
  **初稿は「グループは消さない」と書いていたが、spec は消すと書いてある。spec が authority なので消す。** グループを消すと同じマシンの他の利用者の setgid バイナリが壊れうるが、これは 1 人の開発者の Mac に入るツールで、`uninstall` は「無かった状態に戻す」ためのもの。**消さないと初回インストールの検証ができない**（Task 4 の理由そのもの）。
- **`Uninstall` は途中で失敗しても残りを試すこと。** plist が無い・グループが無い・ディレクトリが無いのは**成功**として扱う（2 回実行できる必要がある）。**`bootout` の「無い」以外の失敗は報告する。**
- **mutation: `bootstrap` の失敗を握りつぶす / 順序を入れ替える / 2 回目の呼び出しで失敗する（冪等性の破壊） / plist を 0666 で書く。**

- [ ] **Step 4: `main.go` にサブコマンドを足す**

`install` / `uninstall` を `flag.Arg(0)` で受ける。既存の `version` の扱いと同じ場所。**既存の常駐動作（引数なし）を変えないこと** — launchd から起動されるのはそちら。`install` / `uninstall` も root を要求する（既存の `os.Geteuid() != 0` の判定より後ろに置く）。

`install` は `filepath.EvalSymlinks(os.Executable())` の親を `srcDir` として `Install` を呼ぶ。成功したら**何をどこに置いたかを 1 行ずつ出す**（初回インストールで唯一の手がかりになる）。

- [ ] **Step 5: 既にあるラベルのリテラルを `DaemonLabel` 1 箇所にまとめる**

**メッセージの文面は変えない。**`internal/doctor/checks.go:20` と `internal/helper/client.go:43` が既に `system/dev.tetherd.helper` を案内しており、これが正しい。やるのは**リテラルを定数に寄せること**だけ:

```
internal/doctor/checks.go:20  r.Next = "sudo tetherd-helper install  (then: sudo launchctl kickstart -k system/dev.tetherd.helper)"
internal/helper/client.go:43  ... "brew upgrade tetherd && sudo tetherd-helper install  (then: sudo launchctl kickstart -k system/dev.tetherd.helper)"
```

`internal/doctor` が `internal/helper` を import してよいかを確認してから寄せること（循環 import になるなら**寄せずに、両方が同じ文字列であることをテストで留める**ほうを選ぶ。判断と理由を報告に書く）。テストは `internal/doctor/doctor_test.go:191` と `internal/helper/client_test.go:115` が留めているので、**文面を変えたらそれらが落ちる。落ちたら文面を戻すこと**（テストが正しい）。

**v0.4 の本当の意味がここにある: `checks.go:20` は `sudo tetherd-helper install` を案内しているが、そのサブコマンドは存在しない。** doctor が「打て」と言うコマンドが無い状態が v0.3b まで続いていた。Task 2 はその案内を真にする。

- [ ] **Step 6: `.goreleaser.yml` に `homebrew_casks:` を足す**

```yaml
homebrew_casks:
  - repository:
      owner: kyosu-1
      name: homebrew-tap
      token: "{{ .Env.HOMEBREW_TAP_GITHUB_TOKEN }}"
    homepage: https://github.com/kyosu-1/tetherd
    description: mirrord-like local development environment for ECS Fargate
    # spec §8: 「formula は 3 バイナリを prefix に置くだけ」。cask でも
    # 同じ数を置く。tetherd-exec を PATH に出しても害は無く (setgid の
    # 実体は install が root 所有のディレクトリに置くコピーのほう)、
    # spec との乖離を増やさないほうを選ぶ。
    binaries:
      - tetherd
      - tetherd-helper
      - tetherd-exec
    dependencies:
      - formula: session-manager-plugin
    hooks:
      post:
        install: |
          system_command "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", staged_path]
    uninstall:
      launchctl: dev.tetherd.helper
      delete:
        - /Library/LaunchDaemons/dev.tetherd.helper.plist
        - /usr/local/libexec/tetherd
    caveats: |
      tetherd needs a root helper for pf, /etc/resolver and the tetherd group:

        sudo tetherd-helper install

      Run that again after every upgrade - it reinstalls both binaries into a
      root-owned directory and restarts the daemon.
```

**注意点:**
- `license:` は書かない（実測 5。LICENSE ファイルが無い）。
- `binary:`（単数）は deprecated。**`binaries:`（複数）を使う。**実測済み。
- `dependencies` の `session-manager-plugin` は **formula として実在する**（`brew info session-manager-plugin` で確認済み、1.2.835.0）。**必須依存にしてよい** — SSM が無ければ tetherd は何もできない。
- `uninstall.launchctl` と `uninstall.delete` は cask のスキーマに実在する（実測済み）。**これがあるので `brew uninstall` が daemon を落とす。**
- `uninstall.delete` のパスと plist のパスは `DaemonLabel` から導かれる。**設定ファイルなので Go の定数を参照できない。ずれたら `brew uninstall` が daemon を残す。** `docs/install.md` にこの重複を明記し、ラベルを変えるときは両方直すと書くこと。
- **`caveats` に書いたコマンドが実際に動くことを e2e で確かめること。** 案内が動かないのは v0.3b が 7 件直した「もう真実でないドキュメント」と同じ欠陥。

- [ ] **Step 7: `make release-check` が exit 0 で警告なしを確認し、`make release-dry-run` で生成された cask を全文読む**

`dist/` のどこに cask が出るかを確認して**全文を報告に貼る**。`binary "tetherd-exec"` が入っていないこと、`postflight`・`uninstall`・`caveats` が意図どおりであることを目で見る。

- [ ] **Step 8: `docs/install.md` を書く**

cask の全文、root 所有のディレクトリにコピーする理由（と `install.go:13` のコメントとの関係）、`KeepAlive` の判断とその根拠、quarantine の扱いと**署名・notarization が v1 の課題であること**、アップグレード手順（`brew upgrade` の後に `sudo tetherd-helper install`）、**ラベルが Go の定数と cask の両方に書かれていること**。

- [ ] **Step 9: `docs/specs/2026-09-12-v1-macos-design.md` §8 を修正する**

**黙って乖離させないこと。**`:415-427` の 3 点を直す（実測 7 の表）:
- `brew install kyosu-1/tetherd/tetherd` → `brew install kyosu-1/tap/tetherd`、tap 名も `kyosu-1/homebrew-tap`
- 「formula は 3 バイナリを prefix に置くだけ。`service` ブロックは使わない」→ **cask**。`service` を使わない理由は spec が既に正しく書いている（launchd の登録は helper 自身が行う）ので、そこは変えない
- 「**helper は常駐しない**」→ **v0.4 では常駐**（`RunAtLoad: true`）であることと、ソケットアクティベーションが次であることを明記。**30 秒のアイドル終了と `purego` の記述もそこに掛かるので、同じ場所に書く**

`§12` にも v0.4 の検証行への参照を足す。

- [ ] **Step 10: コミット**（`daemon.go`/`daemon_test.go`/`main.go` と `.goreleaser.yml`/`docs/install.md`/spec は**別コミット**に分ける。レビュアが片方だけ却下できるように）

---

### Task 3: README

**Files:** Create: `README.md`

**現状 README が 1 つも無い。** 他人が最初に読むもので、v0.4 の成果物としては cask と同じくらい重要。

**書くこと（順番もこのとおり）:**

1. **1 段落で「何ができるか」。** `tetherd run -- go run ./cmd/api` で、ローカルのプロセスが dev の ECS タスクの一部として振る舞う。env は本物、VPC 内向きの通信はタスクの ENI から出る、ALB に来た自分のリクエストが手元に届く。
2. **導入。** `brew install kyosu-1/tap/tetherd` と `sudo tetherd-helper install`。**sudo が要る理由**（pf、`/etc/resolver`、`tetherd` グループ、root 所有のディレクトリへの配置）と、**アップグレード時に同じコマンドを打つこと**を書く。
3. **インフラ側に必要なもの。** spec §9 の項目を要約し、詳細は spec に送る。**「タスク定義に agent サイドカーを足す」ことが前提**であることを隠さない。
4. **最初の実行。** `.tetherd.yml` を置いて `tetherd run -- <cmd>`。`examples/.tetherd.yml` を指す（実在を確認済み）。
5. **うまくいかないとき: `tetherd doctor`。** 13 行を検査して、失敗した行ごとに次に打つコマンドを出す。`✓` / `⚠` / `✗` / `?` の意味を 1 行ずつ。
6. **できないこと**を正直に。ファイルシステムの透過は無い、UDP は無い、HTTP 以外の steal は無い（Fargate の制約）、本番には繋がない（二重ガード）、**macOS だけ**、**バイナリは署名・notarize されていない**。
7. **信頼境界を 1 段落。** タスクロールの認証情報を配るループバック口は無認証で、同一マシンの全プロセスから到達可能。steal のトークンは公開 ALB とラップトップの間にある唯一の壁。

**書かないこと:** 設計の根拠（spec と `docs/design.md` にある）、実装の詳細、ロードマップの細目。

- [ ] **Step 1: 書く。Step 2: `docs/` への相対リンクが実在することを確認する。Step 3: コミット**

---

### Task 4: 初回インストールの検証手順（`docs/uninstall.md` と e2e 行）

**Files:** Create: `docs/uninstall.md`; Modify: `docs/e2e-aws.md`

**なぜこのタスクが要るか。** v0.4 の検証は今までと性質が違う。今までは「dev 環境に対して動くか」だったが、v0.4 は「**tetherd が入っていない Mac で、`brew install` から始めて動くか**」。ところが開発機には既にグループ `tetherd`、`/usr/local/libexec/tetherd/tetherd-exec`、場合により残留 pf ルールがある。**それを消せなければ初回インストールは検証できない。**

- [ ] **Step 1: `docs/uninstall.md` を書く**

この順で:
1. `sudo tetherd-helper uninstall` — **spec §8 の 4 つをこれ 1 つが行う**（`bootout`、plist、グループ、`/usr/local/libexec/tetherd`）。pf と `/etc/resolver` は helper 自身が終了時に片付ける。**以下の 3〜5 は、これが失敗したときの手作業の手順として書く**（消し残りがあると初回インストールの検証が嘘になるので、確認手順は残す）
2. 残留の確認: `sudo pfctl -a com.apple/900.tetherd -s rules`（空であること）、`ls /etc/resolver/`、`netstat -rn | grep 169.254.170`（`pin_credential_route` を使った場合）、`sudo launchctl print system/dev.tetherd.helper`（見つからないこと）
3. `sudo rm -rf /usr/local/libexec/tetherd`
4. `sudo dseditgroup -o delete tetherd`
5. `brew uninstall --cask tetherd`
6. 個人設定: `~/.tetherd/config.yml` は**トークンを持つ**。消すと rotate と同じ効果になるので、消すかは読者の判断

**各手順に「消し忘れると初回インストールの検証が嘘になる」理由を書く。** 例: グループが残っていると `EnsureGroup` が既存の gid を返すので、グループ作成の経路を通らない。

- [ ] **Step 2: e2e に行を足す**

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 32 | `docs/uninstall.md` を全部実行 → `brew install kyosu-1/tap/tetherd` | **3 本**が PATH に入る | アーカイブの中身が cask の `binaries` と一致（spec §8 の「3 バイナリを prefix に」） |
| 33 | `sudo tetherd-helper install` | plist が置かれ、グループが作られ、`/usr/local/libexec/tetherd/` に 2 本入り、daemon が上がる | **`EvalSymlinks` が効いていること** — Homebrew の symlink 経由で起動しても隣の `tetherd-exec` を見つけられる（設計上の決定 6）。**実機でしか分からない** |
| 34 | `sudo launchctl print system/dev.tetherd.helper` | 走っていて、`ProgramArguments` が root 所有のパスを指す | **Homebrew の prefix を指していないこと**（設計上の決定 3）。**`RunAtLoad` で常駐していること**（Ruling S。アクティベーションを入れたらこの行の期待値が変わる） |
| 35 | `tetherd doctor` | helper・グループ・setgid の 3 行が ✓ | 初回インストール直後が doctor から見て健全 |
| 36 | `tetherd run -- aws sts get-caller-identity` | タスクロールの ARN | **brew で入れたバイナリで**一通り動く。**helper が launchd 経由で動いている状態で**（前面起動の検証は v0.3b までに済んでいる） |
| 37 | `sudo tetherd-helper install` をもう一度 | 成功して daemon が上がり直す | 冪等性。**アップグレード手順がこれなので、動かないと README が嘘になる** |
| 38 | `brew uninstall --cask tetherd` | daemon が落ち、plist が消える | `uninstall.launchctl` / `delete` が効いている |

**36 行が dev 環境に触るので、`desired_count` と課金の状態を報告に書くこと。**

- [ ] **Step 3: コミット**

---

### Task 5: 実機検証（人間と一緒に）

**Files:** Modify: `docs/e2e-aws.md`（結果）, `docs/specs/2026-09-12-v1-macos-design.md` §12

タグを打たないと cask は tap に出ないので、**32〜38 行は「リリースを 1 回打つ」ことが前提**。順序:

1. Task 1〜4、6〜8 をマージ可能な状態にする
2. **人間が `HOMEBREW_TAP_TOKEN` を設定する**（Task 1 Step 5 の結果次第）
3. **人間がタグを打つ**（`v0.4.0`）→ workflow が走る → tap に cask が出る
4. `docs/uninstall.md` を実行して 32〜38 を通す
5. 結果を spec §12 に記録

**タグを打つのは人間。** リリースは外向きで不可逆（タグの削除は可能だが、tap に push された cask と GitHub Release は他人に見えている）。**エージェントはタグを打たない。**

**この順序の帰結: PR は 32〜38 が未検証の状態で出る。**それを PR に明記し、「タグを打ったあとに検証する行」として残す。v0.3a / v0.3b が実機検証済みで出せたのに対し、v0.4 はリリースという不可逆な一歩がその前に入るため。

---

### Task 6: 繰り越し — doctor のターゲットグループ検査（**この計画で唯一 `go.mod` を変える**）

**Files:**
- Modify: `go.mod` / `go.sum`（`aws-sdk-go-v2/service/elasticloadbalancingv2`）
- Modify: `internal/proto/messages.go`（agent の既定プロキシポートを公開）、`internal/agent/config.go`（参照。**リテラルを 2 箇所に置かない**）
- Modify: `internal/provider/ecs/`、`internal/cli/deps.go`（`DescribeTargetGroups`）
- Modify: `internal/doctor/checks.go`、`internal/cli/doctor.go`
- Modify: `deploy/dev-env/iam-developer.tf`（**人間が apply**）

**v0.3b の Task 9 がここで止まった。理由と、そのとき決めたことを引き継ぐ:**

`protocol_version` を報告する API は `DescribeTargetGroups` だけ（`ecs:DescribeServices` はターゲットグループの ARN とポートを返すが protocol version は返さず、agent 側にも分からない）。v0.3b は「依存追加なし」の制約と両立しなかったので、**判定も権限追加も入れなかった** — 実装の無い権限を配らないため。

**判定の規則（v0.3b で 1 度間違えて訂正したもの。変えないこと）:**

| 状況 | 判定 | 理由 |
|---|---|---|
| `protocol_version` が `HTTP1` でない | **`Fail`（✗）** | spec §5.1 が gRPC / HTTP2 を対象外にしており、steal が成立しない |
| ポートが agent の既定と違う | **`Warn`（⚠）** | `TETHERD_PROXY` で動かせるので、違うポートを向けた配置は**正しく設定されている**。**`✗` にしてはいけない** |
| 権限が無い | **`Unknown`（`?`）** | 変更前のポリシーを持つ開発者は正常に動いている |

**v0.4 でできるようになること:** タスク定義の agent コンテナの env から `TETHERD_PROXY` を読めば、ポートの判定を `⚠`（質問）から本当の検査にできる。`awsProvider` は `SecretNames` / `PIDMode` で既にタスク定義を読んでいるので同じ呼び出し形。**`TETHERD_PROXY` が設定されていないときは既定値と比べること。**

**制約 2 つ:** `?` は**どの行でも exit code を動かさない**。**`?` の詳細は `"not checked:"` で始まる**（不変条件をテストが留めている。破るとスイートが落ちる）。

- [ ] **Step 1〜5:** `CheckTargetGroup` は**純粋な判定関数**（集めた入力を受ける。AWS を呼ばない）。既存の `Check*` と同じ形・同じテーブルテスト。**mutation: 非 `HTTP1` を通す / 違うポートを `Fail` にする / 権限不在を `Fail` にする / `?` の詳細を `"not checked:"` で始めない。**
- [ ] **Step 6: Terraform と apply の依頼**

`iam-developer.tf` に `elasticloadbalancing:DescribeTargetGroups` を戻す。**`resources = ["*"]` が唯一の選択肢** — ELB の `Describe*` はリソース ARN を取らない（Service Authorization Reference に `DescribeTargetGroups` のリソース型が無い）ので、ARN を書くと全ての呼び出しが拒否される。v0.3b が revert したときのコメントがそのまま使える。

`terraform fmt -check` と `validate` を通し、**`apply` は人間に依頼する**。plan に出るのは `aws_iam_policy.developer` の in-place 更新 1 件だけであることを報告に書く。

---

### Task 7: 繰り越し — 設定項目の嘘を残り 3 件片付ける

**Files:** `internal/transport/ssm/ssm.go`, `internal/agent/config.go`, `internal/config/config.go`, `internal/provider/ecs/discover.go`, `docs/config.md`

**3 件とも v0.3b で見つけて記録したもの。今回は直す。各項目を別コミットにする** — レビュアが 1 件だけ却下できるように。mutation も項目ごとに。

**7a. `TETHERD_CONTROL` を本当に効かせる。** `internal/agent/config.go` が `TETHERD_CONTROL` を読む一方、`internal/transport/ssm/ssm.go:44` が `controlPort = "9900"` を固定で SSM フォワードの `portNumber` に渡すので、**設定を変えると agent は 9901 で待ち、CLI は 9900 に転送し、誰も応答しない**。v0.3b は説明文を直すだけにした。

**1 つの定数に統一して両側から参照する。** `internal/proto` が両側から見える（Task 6 が既にそこへ既定ポートの定数を置く）。**却下した代替は「CLI が agent に問い合わせる」** — 制御ポートを知るために制御ポートに繋ぐ循環になる。統一しても**デプロイが `TETHERD_CONTROL` を変えたら CLI は追随できない**ので、そのことを 1 文書く（真に追随させるには `.tetherd.yml` に書かせる必要があり、それは v1 の判断）。

**7b. `target.container` を拒否する。** v0.3b は「意図的に配線しない」と決めてドキュメントにそう書いたが、今は**受け取って黙って無視する**。`KnownFields(true)` があるのでキーを消すと既存の `.tetherd.yml` が parse error になる。**書かれていたら起動時に `TETHERD_APP_CONTAINER` を名指しして拒否する**（usage error）。黙って無視するより、名指しして落ちるほうが親切。

**7c. `Target.AgentContainer` を `.tetherd.yml` に出す。** 既定 `"tetherd-agent"` を持つのに設定経路が無い。**出す** — サイドカーの名前を変えたいチームは実在しうるし、フィールドは既にある。`target.agent_container` として通し、`ecsTarget` が渡す。`docs/config.md` に足す。

---

### Task 8: `internal/cli` を `-count=5` で緑にする（**production の欠陥を 1 件含む**）

**Files:** Modify: `internal/cli/doctor.go`, `internal/cli/doctor_test.go`, `internal/cli/run_test.go`

**出どころ:** v0.3b の最後の flake 修正エージェントが、`0ed4a6e` を出したあとに測って報告した内容。**pristine な `e88a20b` を 20 ラウンド × `-race -count=5` で回して 7 ラウンドが赤**。`0ed4a6e` はそのうち 1 件（steal 行の `"502"`）だけを直した。**残りはまだある。**

**8a（production の欠陥、これが本題）: `bounded` の直接受信アームが context エラーを濾さない。**

報告された形: `abandonBounded` は `isContextError(r.err)` で濾すのに、`bounded` の `case r := <-ch` のアームは濾さない。だから **`context.DeadlineExceeded` がそのまま「検査の答え」として返り、その行が間違った助言を出す**。`TestDoctorBoundsAWedgedGroupLookup` が **20 ラウンド中 4 回**赤くなるのはこれを指している。

**これは doctor の中核的な性質を壊す:** doctor の 4 状態は「`?` は検査できなかった行」であり、context エラーはまさにそれ。`✓` や `✗` として出たら、**測っていないものを測ったと言う**ことになる。v0.3b が 9 タスクかけて取り除いた欠陥と同じ形。

- [ ] **Step 1: まず現象を再現して記録する。** `-race -count=5 ./internal/cli/ -run TestDoctorBoundsAWedgedGroupLookup` を 20 ラウンド回し、**赤くなった回数と、そのときの mark と detail を報告に貼る**。再現できなければ**そう報告して先に進むこと**（直せない欠陥を直したと書かない）。
- [ ] **Step 2: `bounded` の両方のアームが同じ扱いをするように直す。** context エラーは `?` の経路へ。**`abandonBounded` と `bounded` が同じ判定関数（`isContextError`）を通ることを、テストで留める。**
- [ ] **Step 3: 不変条件のテストを足す。** 「`?` ⇔ 詳細が `"not checked:"` で始まる」は既にテストがある。ここに足すのは **「どの行も context エラーを `✓` や `✗` として報告しない」**。mutation: 直したアームの濾しを外す → 落ちること。
- [ ] **Step 4: 残りの flake を潰す。** 報告された 3 件:
  - `TestRunFailsWhenEverySessionDiesDuringTheAttach`（2 回）— **`started 497010h10m ago` を印字**。ゼロ値の `time.Time` が経過時間に入っている。`startedAgo` / `shortAge` にゼロ値のガードが無い。**これは flake というより表示の欠陥で、ユーザにも見える**（タスクの起動時刻が取れなかったときに 497010 時間前と出る）。ガードを入れ、ゼロ値のテストを足す。
  - `TestAttachUsesTheDefaultBudgetWhenNothingInjectsOne`
  - `TestFollowerDoesNotAnnounceAPromotionToATaskItIsAlsoDropping`
  後ろ 2 つは**原因を測ってから直すこと**。`time.Sleep` や実クロックに依存しているなら、v0.3b が使った注入点（`boundedAfter` のような seam）に寄せる。
- [ ] **Step 5: 検証。** `-race -count=5 ./internal/cli/` を **20 ラウンド**回して全部緑。**ラウンド数と結果を報告に貼る**。1 つでも赤ければ、それを報告に書いて残す（緑だと言わない）。
- [ ] **Step 6: 各項目を別コミット**（8a は production を触るので単独で）

---

## 完了条件

1. `go test -race ./...` / `make lint` / `GOOS=linux go vet ./...` / `make build` が通る。`go mod tidy` は Task 6 の 1 モジュール追加を反映した状態で no-op
2. **`-race -count=5 ./internal/cli/` が 20 ラウンド緑**（Task 8）
3. **`make release-check` が exit 0、deprecation 警告ゼロ**
4. `make release-dry-run` が 3 本のバイナリを含む darwin のアーカイブを作り、生成された cask に `binaries`（2 本）・`postflight`・`uninstall`・`caveats` がある
5. `sudo tetherd-helper install` が冪等で、plist が root 所有のパスを指し、`CheckOwnership` が書き込み可能な経路を拒否する
6. `README.md` があり、導入・インフラ前提・`doctor`・できないこと・信頼境界を含む
7. `docs/uninstall.md` があり、初回インストールを検証できる状態に戻せる
8. `doctor` がターゲットグループを判定し、ポートの違いを `✗` ではなく `⚠` で出す。**どの行も context エラーを `✓`/`✗` として報告しない**
9. `TETHERD_CONTROL` のリテラルが 1 つになり、`target.container` が黙って無視されず、`target.agent_container` が効く
10. **実機 32〜38 はタグを打った後**（Task 5）。PR にはその時点の状態を正直に書く

## 次の計画に回すもの: dev-env のタスク定義が毎回置き換わる

**`aws_ecs_task_definition.api` は、何を apply しようとしても常に `forces
replacement` になる。** v0.4 の IAM 権限 1 件を当てるだけの plan が
`1 to add, 2 to change, 1 to destroy` になり、`-target=aws_iam_policy.developer`
で回避した。**実害は「本物の変更がノイズに埋もれること」**で、実際にこの時
コントローラの plan 予測が外れた。

原因は実測済み（provider の更新ではない。lock file は `e9e83dc` 以降 `6.64.0`
で固定、`ecs.tf` は以下のどのフィールドも書いていない）。**AWS が
`DescribeTaskDefinition` で埋めて返す値**を refresh が state に取り込み、
`jsonencode` した設定側には無いので毎回差分になる:

| AWS が足すもの | 場所 |
|---|---|
| `hostPort`（`containerPort` と同値） | 両コンテナの `portMappings` |
| `cpu: 0` | 両コンテナ直下 |
| `mountPoints: []` / `volumesFrom: []` / `systemControls: []` | 両コンテナ直下 |
| `drop: []` | agent の `linuxParameters.capabilities` |

意味的には全て既定値で、置き換えても内容は同一（awsvpc では `hostPort` は
`containerPort` と一致必須なので省略すれば AWS が同じ値を入れる）。

**選択肢は 2 つあり、このリポジトリでは前者を選ぶべき:**

1. **上表の既定値を `ecs.tf` に明示して JSON を往復させる。** plan が信用でき
   るようになる。欠点は provider が将来さらに正規化を増やすと再発すること。
2. `lifecycle { ignore_changes = [container_definitions] }`。1 行で消える。
   **ただしこのリポジトリでは後者の代償が特に大きい** — タスク定義の
   `container_definitions` は tetherd を導入するときに利用者が編集する場所その
   もので（agent サイドカー、`TETHERD_ENV`、`SYS_PTRACE`、ポート）、無視すると
   それらの変更が黙って効かなくなる。「タスク定義を Terraform 外で回す」構成
   では定番の手だが、ここはそうではない。

**「直った」と言うには実機で plan が空になることを確認する必要があり、それは
apply を伴う**ので、この計画には含めない。

## 次の計画（v1.0、この計画には含めない）

**agent / sampleapp イメージを公開レジストリ（GHCR）に publish する**（workflow 1 本とパッケージ権限が要る外向きのスコープ。v0.4 では Task 1 の決定 1 どおり `.goreleaser.yml` に含めず、代わりに README / `docs/design.md` / spec から「`ghcr.io/kyosu-1/tetherd-agent` を pull できる」という記述を取り除いた。**現状、利用者は `make push-images ECR_REGISTRY=...` で自分のレジストリに push する**）。**署名と notarization**（Apple Developer アカウントが必要。quarantine の `xattr` 回避をやめられる）。複数人が同じタスクに同時接続する実機検証（steal の照合は実装済みだが 2 人以上で測っていない）。現実的なアプリでのレイテンシ計測。`TETHERD_CONTROL` をデプロイ側から CLI に伝える経路。

持ち越し: `follow.go` の**永続的な** attach 失敗が何もログに出さない点、`sessionset.go` の昇格通知が `Primary()` の liveness 検査を通らない点、`discover.go:90` の `reasons` 上書き、`internal/dnsproxy` の一部テストがポート競合の緩和策の外にある点、リポジトリ直下の `.tetherd.yml` がテストスイートの隠れた入力になりうる点、`opts.Timeout` を長く取ったテストは本当のハングを Go の 10 分 panic まで待つ点、`LICENSE` ファイルが無い点（所有者の判断）。
