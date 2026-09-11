# tetherd v1 (macOS) — 設計仕様

2026-09-12。[docs/design.md](../design.md)（全体設計）を v1 として具体化したもの。design.md と重複する部分は参照で済ませ、ここには **v1 で決めたこと・design.md から変えたこと** を書く。

---

## 0. 決定事項の要約

| 項目 | 決定 | design.md からの変更 |
|---|---|---|
| 対象 OS | macOS 14 以降（arm64 / amd64） | Linux から着手 → macOS から着手 |
| 成果物 | チームに `brew install` で配れる v1 | — |
| 捕まえる層 | pf `rdr` + `DIOCNATLOOK`（sshuttle 方式）。終端はカーネルの TCP | utun + gVisor netstack → 不採用（v2 で Linux netns と共に検討） |
| プロセスのスコープ | primary gid = `tetherd`。setgid ラッパー `tetherd-exec` で起動 | 補助グループ → primary gid（pf は effective gid しか見ない）。helper が spawn → CLI の子のまま |
| ルーティング | 宛先 IP。VPC CIDR + `169.254.170.0/24` + 追加 CIDR + prefix list だけリモート | 「全部リモート」「ホスト名で local 指定」→ CIDR ベース。§14 未決の第 1 項を決着 |
| steal の一致 | `X-Dev-User` + `X-Dev-Token`（ユーザー固定トークン） | ヘッダー 1 つ → 2 つ |
| 複数タスク | RUNNING な全タスクに接続 | 1 タスク → 全タスク |
| トランスポート | `ssm:StartSession` を SDK で呼び `session-manager-plugin` を子プロセスで起動 | AWS CLI 依存を削除 |
| agent イメージ | `ghcr.io/kyosu-1/tetherd-agent` | 未決 → GHCR |
| 検証環境 | `deploy/dev-env/`（Terraform） | 新規 |
| v1 から外す | mirror、env-rewrite、`remote_localhost`、`init`、UDP、IPv6、Linux、Cloud Run、公証 | ロードマップ再編 |

---

## 1. スコープ

### 入る

- `tetherd run` / `env` / `status` / `doctor` / `token rotate`
- 透過 outgoing（TCP）。VPC 内、タスクロールの認証情報エンドポイント、指定した AWS サービス（S3 / DynamoDB）を dev タスクの ENI から出す
- `/etc/resolver` による per-domain のリモート名前解決
- agent が `/proc/<pid>/environ` から読んだ app の env（secrets 解決済み）の注入
- steal（HTTP/1.1）。ALB → agent → ラップトップ。フォールバック付き
- RUNNING な全タスクへの同時接続と、deploy 中の追従
- 特権ヘルパー（LaunchDaemon）、setgid ラッパー、Homebrew tap、GoReleaser
- Terraform の検証環境、サンプルアプリ、AWS 不要のローカル e2e

### 外す（v2 以降）

| 機能 | 理由 | 戻すとき |
|---|---|---|
| mirror | 共有 DB への二重書き込みの扱いを決めてから | GET 限定などの方針と共に |
| env-rewrite モード | helper を入れられない端末向けの逃げ道。v1 は helper 必須 | 需要が出たら |
| `remote_localhost` | lo0 宛の group マッチが rdr の評価順序上まっすぐ書けない | netstack 導入時 |
| `init` | 設定は README の雛形と `deploy/dev-env` の output で足りる | — |
| UDP / IPv6 | rdr 方式ではフロー単位の元宛先が取れない | netstack 導入時 |
| WebSocket / gRPC の steal | agent は upgrade を app にそのまま通す | — |
| Linux / Cloud Run | design.md §13 のとおり | Capturer / Transport の実装追加 |
| Developer ID 署名・公証 | brew 経由なら quarantine が付かない | Developer ID 取得後 |
| 同一マシンでの複数セッション | gid `tetherd` を共有すると pf が区別できない | セッションごとの gid |

---

## 2. 全体構成

部品は 4 つ + 検証環境。

| 部品 | 場所 | 権限 | 役割 |
|---|---|---|---|
| `tetherd` | ラップトップ | ユーザー | CLI。SSM セッション、yamux、透過プロキシ、DNS リゾルバ、steal の受け口、env 注入、子プロセス管理 |
| `tetherd-helper` | ラップトップ、LaunchDaemon | root | pf アンカーのルール投入/削除、`/etc/resolver/` の管理、`DIOCNATLOOK`。**この 5 操作以外は無い** |
| `tetherd-exec` | ラップトップ、`/usr/local/libexec/tetherd/` | setgid `tetherd` | `setregid(tetherd, tetherd)` → `exec`。十数行 |
| `tetherd-agent` | ECS タスクのサイドカー | root + `SYS_PTRACE` | `:8080` L7 プロキシ、`:9900` 制御（lo のみ）、environ 読み取り、dial / resolve |
| `deploy/dev-env/` | Terraform | — | 検証用の最小 dev 環境 |

通信の流れ（outgoing）:

```
子プロセス connect(10.0.3.21:5432)
  → pf: group tetherd かつ宛先 ∈ <tetherd_remote> → route-to lo0
  → pf: rdr on lo0 → 127.0.0.1:<redirect_port>
  → CLI accept → helper.NatLook → 元の宛先 10.0.3.21:5432
  → yamux stream {"type":"dial","addr":"10.0.3.21:5432"} → agent
  → agent がタスクの netns から dial → RDS
```

design.md の 4 層で言うと、②「終端する層」がカーネル TCP になり、①は「(元の宛先, 接続) を渡す」インターフェースに整理される。③④は design.md のまま。

---

## 3. 捕まえる層（macOS）

### 3.1 gid によるスコープ

- helper が起動時にグループ `tetherd` が無ければ作る（`dscl`、gid は 300 番台の空き）。§8 の初期化の一部
- `tetherd-exec` は root 所有・グループ `tetherd`・mode `2755` で `/usr/local/libexec/tetherd/` に置く（brew prefix はユーザー書き込み可なので、setgid バイナリは置かない）
- `tetherd-exec` は `setregid(egid, egid)` で **real と effective の両方**を `tetherd` にしてから `exec` する。effective だけだと bash が起動時に「rgid ≠ egid」を検知して egid を戻し、`#!/bin/bash` のスクリプトや `sh -c` 経由の子孫がスコープから抜ける（zsh / dash / Go / Node / Python は戻さない）。元のグループは補助グループに残るのでファイルアクセスは変わらない
- setgid していない状態（egid == rgid）で起動されたら「`tetherd-exec` is not setgid; run `tetherd doctor`」で終了
- gid `tetherd` で得られる権限は「pf に捕まる」ことだけ。ファイルや他プロセスへの権限は増えない

### 3.2 pf ルール

helper がアンカー `com.tetherd` にロードする。ディスクには書かない。

```
table <tetherd_remote> { 10.0.0.0/16, 169.254.170.0/24, ... }
rdr pass on lo0 inet proto tcp from any to <tetherd_remote> -> 127.0.0.1 port <redirect_port>
pass out route-to lo0 inet proto tcp from any to <tetherd_remote> group tetherd keep state
```

- `group tetherd` のプロセスが `<tetherd_remote>` 宛に出した TCP だけが lo0 に回り、rdr で CLI の透過ポートに落ちる。他プロセスの同じ宛先への通信は物理 IF から出る
- macOS の既定 `/etc/pf.conf` には `set skip on lo0` は**無い**（design.md の記述は OpenBSD の既定との混同。手元の macOS 26 で確認済み）
- メインルールセットへのアンカー参照（`rdr-anchor "com.tetherd"` / `anchor "com.tetherd"`）は `DIOCCHANGERULE` ioctl でメモリ上に挿入する（sshuttle の `pf_add_anchor_rule` と同じ）。`pfctl -sr` → 加工 → `pfctl -f -` の dump/reload 方式は Apple のシステムサービスが動的に挿すアンカーと競合しうるので取らない。ioctl の構造体定義が手間なら、dump/reload を暫定の代替にしてよいが、v1 リリースまでに ioctl に寄せる
- アンカー内のルールは `pfctl -a com.tetherd -f -` で stdin からロード
- `pf.apply` で `pfctl -E`（参照カウント。stderr の `Token : N` を保持）、`pf.clear` と終了時に `pfctl -X N`。セッションの外では pf を有効化した状態すら残さない。`/etc/pf.conf` のコメントにある作法そのもの
- helper 起動時に `com.tetherd` アンカーが残っていれば空にする（前回の異常終了対策）

### 3.3 元の宛先の復元

CLI が `127.0.0.1:<redirect_port>` で accept したら、helper に `natlook{proto, src, dst}` を送り、helper が `/dev/pf` に `DIOCNATLOOK`（direction `PF_OUT`）を発行して rdr 前の宛先を返す。UNIX ソケット 1 往復（< 1 ms）で、SSM の RTT（数十 ms）に対して無視できる。`/dev/pf` の fd を CLI に渡す最適化は取らない（helper の操作を 5 つに閉じるため）。

### 3.4 DNS

- `network.remote_domains` が空なら `/etc/resolver` に触らない
- 空でなければ helper が `/etc/resolver/<domain>` を作る。内容は `nameserver 127.0.0.1` と `port 53530`。終了時に消す。tetherd 管理のファイルはヘッダー行 `# managed by tetherd` で識別し、それ以外は触らない
- CLI の `127.0.0.1:53530` のリゾルバは、問い合わせを `resolve` ストリームで agent に転送する。agent はタスクの resolv.conf（VPC リゾルバ）で解決する
- `getaddrinfo` 経由（Go 既定、Node `dns.lookup`、JVM）は `/etc/resolver` を尊重する。Node の `dns.resolve*`（c-ares）や `dig` は尊重しない。README に明記
- RDS / ElastiCache / 内部 ALB のエンドポイント名はパブリック DNS でプライベート IP に解け、その IP が VPC CIDR に入るので、典型構成では `remote_domains` 無しで動く

### 3.5 スコープから漏れるもの（既知）

- システムデーモン経由の通信（`mDNSResponder` の DNS、`nsurlsessiond` のバックグラウンド転送、`trustd` の OCSP）。DNS は 3.4 で扱う。他は開発用途では実害が無い
- 子孫が自分で `setgid` して抜ける場合（稀）
- Docker / VM 内のプロセス（VM プロセスの通信は VM のもの）。コンテナ内から使いたい場合は v2 の Linux 方式

### 3.6 helper プロトコル

- `/var/run/tetherd.sock`、mode `0666`、JSON Lines、リクエストに `id`
- 接続時に peer credential（`LOCAL_PEERCRED`）を取り、uid が **`admin` グループのメンバー**であることを要求（`resolver.set` はマシン全体の名前解決に影響するため）
- 1 接続 = 1 セッション。`pf.apply` は接続ごとに 1 回。接続が切れたら（CLI の異常終了含む）helper がそのセッションの pf ルールと resolver ファイルを消す
- 同時セッションは 1 つ。2 つ目の `pf.apply` は `busy{pid, command, since}` で拒否
- 最初に `version` を交換。プロトコルバージョン不一致なら CLI が `brew upgrade tetherd && sudo brew services restart tetherd` を案内

| 操作 | 引数 | 内容 |
|---|---|---|
| `pf.apply` | `remote_cidrs[]`, `redirect_port` | テーブルとルールをアンカーにロード |
| `pf.clear` | — | アンカーを空にする |
| `resolver.set` | `domains[]`, `port` | `/etc/resolver/<domain>` を作成 |
| `resolver.clear` | — | tetherd 管理のファイルを削除 |
| `natlook` | `proto`, `src`, `dst` | 元の宛先を返す |

---

## 4. ルーティングと IAM

### 4.1 リモートに回す集合

`<tetherd_remote>` の中身。**宛先 IP で決める。既定は「VPC の中だけリモート」**。

1. 対象タスクの VPC の CIDR（セカンダリ含む）。`DescribeTasks` → attachments の subnetId → `DescribeSubnets` → `DescribeVpcs` の `CidrBlockAssociationSet`（state = associated）
2. `169.254.170.0/24`。タスクロールの認証情報とタスクメタデータ v4。これで AWS SDK がタスクロールとして動く
3. `network.remote_cidrs`。ピアリング先 VPC、Transit Gateway 越しのオンプレなど
4. `network.remote_services` に書いた AWS サービスの managed prefix list（`com.amazonaws.<region>.s3` / `.dynamodb`）。`DescribeManagedPrefixLists` → `GetManagedPrefixListEntries`

から `network.local_cidrs` を除く。

それ以外（インターネット、localhost、LAN）は子プロセスからそのまま出る。`go run` のモジュール取得、`npm install`、外部 API はラップトップの回線。dev タスクの ENI を踏み台にインターネットへ出る経路は既定で無い。`remote_cidrs: [0.0.0.0/0]` を書けば可能だが `doctor` が警告する。

起動時にラップトップの IF アドレスとリモート集合の重なりを検査し、重なっていれば警告して `local_cidrs` を案内する。

### 4.2 VPC 外の AWS サービスとタスクロール

S3 / DynamoDB / SQS / Secrets Manager / Bedrock など VPC 外のサービスは**既定のままで動く**。SDK は `169.254.170.2`（トンネル経由）から一時クレデンシャルを取り、以降は SigV4 署名でパブリックエンドポイントに直接送る。署名が正しければ送信元 IP がラップトップでも受け付けられる。

動かないのは IAM / バケット / SCP / エンドポイントポリシーに**ネットワーク条件**（`aws:SourceVpc` / `aws:SourceVpce` / `aws:SourceIp`）がある場合。対処:

| エンドポイント | 対処 |
|---|---|
| Interface endpoint（Secrets Manager、SQS、STS 等。private DNS 有効） | `remote_domains` にサービスのドメインを書く。agent 側で解決するとプライベート IP になり VPC CIDR に入る |
| Gateway endpoint（S3、DynamoDB） | `remote_services: [s3, dynamodb]`。prefix list の IP をリモート集合に足す |

README に書くこと: 条件が無い環境ではラップトップ経路のほうが緩い（NAT の無い VPC でもラップトップは自前で出られる）、CloudTrail の `sourceIPAddress` はラップトップの IP になる。

### 4.3 開発者の IAM ポリシー

design.md §9 のものに EC2 の読み取り 4 つを追加。

```json
{ "Sid": "DiscoverNetwork", "Effect": "Allow",
  "Action": ["ec2:DescribeSubnets", "ec2:DescribeVpcs",
             "ec2:DescribeManagedPrefixLists", "ec2:GetManagedPrefixListEntries"],
  "Resource": "*" }
```

---

## 5. agent

### 5.1 L7 プロキシ

- ALB はターゲットへの接続を keep-alive で使い回すので、振り分けは**リクエスト単位**。agent はセッションの有無にかかわらず常に HTTP/1.1 リバースプロキシ（`net/http` + `httputil.ReverseProxy`）。「素通し」= 全リクエストが app へ
- ヘルスチェックはヘッダーが無いので常に app へ
- WebSocket は upgrade を app にそのまま通す。steal はしない
- ターゲットグループが gRPC / HTTP2 のものは対象外（`doctor` が検出）
- `X-Forwarded-*`、`traceparent`、`X-Amzn-Trace-Id` は素通し

### 5.2 steal

- 一致条件は `X-Dev-User == user` かつ `X-Dev-Token == token`。比較は定数時間
- トークンは初回 `run` で CLI が生成（32 バイト乱数、base64url）して `~/.tetherd/config.yml` に保存する**ユーザー固定値**。`tetherd token rotate` で更新。`hello` で agent に渡す
- 転送: `ReverseProxy` の `Transport` に、`DialContext` が該当ユーザーの yamux ストリームを開くものを使う。CLI 側はそのストリームを `net.Listener` として `http.Serve` し、`localhost:<local_port>` への `ReverseProxy` で処理する。リクエストログ（method / path / status / 所要時間 / `X-Forwarded-For` の先頭）は CLI のハンドラで出す。agent からログは送らない
- フォールバック: dial 失敗（CLI 消失）ならそのリクエストを app へ。ボディは 1 MiB までバッファしてリプレイ可能にし、超えるものは dial 失敗時 502
- ヘッダー名は `incoming.match.header` / `token_header` で変更可

### 5.3 環境変数の読み取り

1. メタデータ v4 `${ECS_CONTAINER_METADATA_URI_V4}/task` から `TETHERD_APP_CONTAINER`（既定 `app`）のコンテナ ID を得る
2. `/proc/*/environ` を走査し、`ECS_CONTAINER_METADATA_URI_V4` がその ID を含むプロセスを集める
3. その中で `/proc/<pid>/stat` の starttime が最も古いもの（コンテナの init プロセス）の environ を採用。entrypoint がシェルスクリプトでも ECS が注入した env が取れる
4. 自プロセス以外が見えない（`pidMode: task` が無い）場合は `welcome.env_error = "pidMode task is not set on the task definition"` を返す。CLI は修正方法を出して終了（`--no-env` で続行可）

agent は root で動く（`SYS_PTRACE` を effective にするため。distroless `static` の既定ユーザー）。

### 5.4 セッション管理

- ユーザー名 → セッションのマップ。同名の二重接続は `error{code: "duplicate_user", from, since}` で拒否
- 制御ストリームで 5 秒おきに ping。3 回連続で pong が無ければセッションを破棄し、そのユーザーのルールを消す
- `TETHERD_ENV` が無ければ起動しない。値は `welcome.env` で CLI に返す

### 5.5 設定と配布

- env のみ: `TETHERD_ENV`（必須）、`TETHERD_LISTEN`（`:8080`）、`TETHERD_UPSTREAM`（`127.0.0.1:8081`）、`TETHERD_CONTROL`（`127.0.0.1:9900`）、`TETHERD_APP_CONTAINER`（`app`）
- AWS API は呼ばない
- イメージ `ghcr.io/kyosu-1/tetherd-agent`、distroless static、linux/arm64 + linux/amd64
- タスク定義の推奨: `essential: true`、`restartPolicy.enabled: true`、`linuxParameters.capabilities.add: ["SYS_PTRACE"]`、`pidMode: task`
- インターネットに出られない VPC 向けに ECR pull-through cache を README で案内

---

## 6. CLI

### 6.1 トランスポート

```go
type Transport interface {
    Dial(ctx context.Context, task Task) (net.Conn, error)
}
```

- `ssm`: SDK で `ssm:StartSession`（Target `ecs:<cluster>_<taskId>_<runtimeId>`、DocumentName `AWS-StartPortForwardingSession`、Parameters `portNumber: ["9900"]`, `localPortNumber: ["<空きポート>"]`）。応答を `session-manager-plugin` に AWS CLI と同じ引数（セッション JSON、リージョン、`StartSession`、プロファイル、パラメータ JSON、エンドポイント）で渡して子プロセスとして起動し、`127.0.0.1:<port>` に dial。AWS CLI 自体には依存しない
- `direct`: 指定アドレスに TCP。ローカル e2e と結合テスト用。`run --transport direct --agent-addr host:port --remote-cidr ...` で使う
- 再接続: yamux セッションが死んだら指数バックオフ（1s → 30s）で `StartSession` からやり直し、`hello` を再送。進行中の dial は切れる
- plugin の埋め込み（`aws/session-manager-plugin` の datachannel を組み込んで依存ゼロにする）は v2 候補。`Transport` の実装として足せる

### 6.2 タスクの発見と複数タスク

- `ListTasks(cluster, serviceName, desiredStatus=RUNNING)` → `DescribeTasks`。`tetherd-agent` コンテナがあり、`enableExecuteCommand` が true で、`managedAgents[ExecuteCommandAgent].lastStatus == RUNNING` のものを対象に
- 対象タスク**全部**に SSM + yamux + `hello`。steal はどのタスクからでも受ける
- `dial` / `resolve` / env は primary（起動が最も古いタスク）を使う。primary が落ちたら次に古いものへ
- 10 秒おきに `ListTasks` を再実行し、新しいタスクには接続、消えたタスクは片付ける（rolling deploy の追従）
- `--task ID` で明示した場合はそのタスクだけ

### 6.3 `run` の流れ

1. 設定読み込み（`.tetherd.yml` + `~/.tetherd/config.yml`）→ AWS 認証 → タスク発見
2. helper に接続、バージョン確認
3. 全タスクへ接続、primary の `welcome` から env と `TETHERD_ENV` を取得。`target.env` と不一致なら切断して終了
4. VPC CIDR / prefix list を取得 → リモート集合を組む → ローカル IF との重なりを警告
5. helper に `pf.apply` と（必要なら）`resolver.set`
6. 透過プロキシ（`127.0.0.1:<空きポート>`）、DNS リゾルバ（`127.0.0.1:53530`）、steal の `http.Serve` を起動
7. IAM 確認: `dial` ストリームで `169.254.170.2` からクレデンシャルを取り、`sts:GetCallerIdentity` を呼んで表示
8. ステータス行を出す
9. `/usr/local/libexec/tetherd/tetherd-exec -- <command>` を子プロセスとして起動。同じプロセスグループ、stdio 素通し、合成した env
10. 子の終了またはシグナルで: `bye` → `pf.clear` / `resolver.clear` → plugin 終了 → 子の終了コードで exit

Ctrl-C は端末がプロセスグループ全体に SIGINT を送るので、CLI は子の終了を待ってから片付ける。CLI が異常終了しても helper がソケット切断で掃除する。

### 6.4 env の合成

`ローカル env < タスク env < env.override`。

タスク env から既定で除外: `PATH HOME HOSTNAME USER LOGNAME SHELL TMPDIR PWD OLDPWD TERM LANG LC_* SHLVL _ AWS_EXECUTION_ENV`。

`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` と `ECS_CONTAINER_METADATA_URI_V4` は透過モードで必要なので残す。`--no-network` のときだけ除外する（残すと SDK が `169.254.170.2` に行って失敗し、フォールバックしない）。

### 6.5 コマンド

```
tetherd run [-s SERVICE] [--as NAME] [--task ID] [--local-port N]
            [--no-incoming] [--no-network] [--no-env] [-q]
            [--transport ssm|direct] [--agent-addr HOST:PORT] [--remote-cidr CIDR]...
            -- <command...>
tetherd env [--format dotenv|json|shell] [--reveal]
tetherd status
tetherd doctor
tetherd token rotate
```

- `env`: 既定は secrets をマスク。どれが secret かはタスク定義（`DescribeTaskDefinition`）の `secrets` ブロックの名前で判定
- `status`: タスクごとに接続中のユーザー、自分のルール、primary かどうか。agent の `status` メッセージで取る
- `doctor` の検査項目（各項目に「次に何をするか」を付ける）:
  helper が応答しバージョンが一致 / `tetherd` グループと setgid `tetherd-exec` / session-manager-plugin の有無 / AWS 認証 / サービスの `enableExecuteCommand` / タスクの agent コンテナと ExecuteCommandAgent / タスク定義の `pidMode: task` / ターゲットグループが HTTP1 / ECS・EC2 の読み取り権限 / VPC CIDR とローカル IF の重なり / `remote_domains` が agent 側で解けるか / `remote_cidrs` に `0.0.0.0/0` が無いか

### 6.6 出力

design.md §5 のとおり。`network` 行は `transparent (pf rdr, gid tetherd) · remote: 10.0.0.0/16, 169.254.170.0/24 · DNS: local (+ myapp.internal via VPC)` のように、何がリモートかを 1 行で示す。

### 6.7 設定ファイル

```yaml
# .tetherd.yml — リポジトリにコミット
version: 1
aws:
  profile: myapp-dev            # 個人設定で上書き可
  region: ap-northeast-1
target:
  cluster: myapp-dev
  service: api
  container: app                # env を読むコンテナ
  env: dev                      # welcome.env と照合
env:
  override:
    PORT: "8080"
  exclude: []
network:
  remote_cidrs: []              # VPC CIDR は自動。ピアリング先などを足す
  local_cidrs: []               # remote の中でラップトップから直接出す例外
  remote_domains: []            # /etc/resolver で agent 側解決にするドメイン
  remote_services: []           # s3 | dynamodb
incoming:
  local_port: 8080
  match:
    header: X-Dev-User
    token_header: X-Dev-Token
```

```yaml
# ~/.tetherd/config.yml — 個人。初回 run で生成
user: shota
token: <base64url 32 bytes>
aws:
  profile: myapp-dev-shota
```

---

## 7. 制御プロトコル（v1 の具体化）

design.md §8 のとおり yamux + JSON Lines。ストリーム 0 が制御。

**制御ストリーム**

| 方向 | メッセージ | フィールド |
|---|---|---|
| CLI → agent | `hello` | `version`, `user`, `token`, `incoming{enabled, header, token_header}` |
| agent → CLI | `welcome` | `version`, `task_arn`, `env`, `app_env{}`, `env_error`, `others[]` |
| agent → CLI | `error` | `code`（`duplicate_user` / `version_mismatch` / …）, `message`, 付随情報 |
| 双方向 | `ping` / `pong` | — |
| CLI → agent | `status` | — → `status_reply{sessions[{user, from, since}]}` |
| CLI → agent | `bye` | — |

**CLI が開くストリーム**（1 行目が JSON ヘッダー、以降は生バイト）

- `{"type":"dial","addr":"10.0.3.21:5432"}` → `{"ok":true}` または `{"ok":false,"error":"..."}` → 双方向コピー
- `{"type":"resolve","name":"api.myapp.internal","qtype":"A"}` → `{"ok":true,"addrs":[...],"ttl":30}` → close

**agent が開くストリーム**

- `{"type":"http"}` → 以降は HTTP/1.1（`net/http` 同士が keep-alive で複数リクエストを流す）

---

## 8. 配布とインストール

```
brew install kyosu-1/tetherd/tetherd
sudo tetherd-helper install        # sudo はこの 1 回
tetherd doctor
```

- tap `kyosu-1/homebrew-tetherd`。GoReleaser がタグ push で GitHub Release（darwin arm64 / amd64）、GHCR の agent と sampleapp イメージ、tap の formula 更新を行う
- formula は 3 バイナリを prefix に置くだけ。`service` ブロックは使わない（launchd の登録は helper 自身が行う）
- brew はインストール時に root の処理を実行できないので、root が要る初期化は `sudo tetherd-helper install` が行う: グループ `tetherd` の作成、`tetherd-exec` の `/usr/local/libexec/tetherd/` へのコピー（`root:tetherd`、`2755`）、`/Library/LaunchDaemons/dev.tetherd.helper.plist` の生成と `launchctl bootstrap`。`brew upgrade tetherd` の後は `sudo tetherd-helper install` を再実行する（冪等。`doctor` がバージョン不一致を検出して案内する）
- **helper は常駐しない。** plist の `Sockets` で launchd が `/var/run/tetherd.sock` を保持し、最初の接続で helper を root で起動する（ソケットアクティベーション。`launch_activate_socket()` は cgo を使わず `purego` で呼ぶ）。helper は接続が無くなって 30 秒でアイドル終了する。`KeepAlive: {SuccessfulExit: false}` で、クラッシュ時だけ launchd が再起動し、起動時の掃除で残留ルールが消える
- helper は起動のたびに残留アンカーと resolver ファイルを掃除してから listen する
- 署名・公証は v1 ではしない。GitHub Releases からの直接ダウンロードは非サポートと明記
- アンインストール: `sudo tetherd-helper uninstall`（`launchctl bootout`、plist、グループ、`/usr/local/libexec/tetherd` の削除）→ `brew uninstall tetherd`
- Ventura 以降の「バックグラウンド項目が追加されました」で無効化されたら `doctor` が案内。毎年の macOS メジャーリリースで動作確認

---

## 9. 検証環境とテスト

### 9.1 `deploy/dev-env/`（Terraform）

| リソース | 内容 |
|---|---|
| VPC | `10.0.0.0/16`、2 AZ、public（ALB / NAT）+ private（Fargate / RDS） |
| NAT Gateway | 1 つ。GHCR の pull と ssm-agent の外向き用 |
| ALB | public、HTTP :80。ターゲットグループ :8080、ヘルスチェック `/healthz` |
| ECS | cluster + Fargate service `api`（`desired_count` 変数、既定 1）。`enableExecuteCommand`、`pidMode: task`。`tetherd-agent`（`SYS_PTRACE`、`TETHERD_ENV=dev`、essential）+ `app` |
| RDS | Postgres `db.t4g.micro`、単一 AZ、非公開。パスワードは Secrets Manager → app の `secrets` で `DB_PASSWORD` |
| SSM Parameter | `FEATURE_FLAG` を `secrets` で注入 |
| Cloud Map | namespace `myapp.internal`、service `api` |
| IAM | タスクロール（ssmmessages 4 つ + `s3:ListAllMyBuckets`）、実行ロール、開発者ポリシー（ARN を output、アタッチは手動） |
| outputs | cluster / service / ALB DNS / VPC CIDR / RDS endpoint / `.tetherd.yml` |

概算 1 日 200 円弱。使わないときは destroy。

### 9.2 `examples/sampleapp/`

Go の HTTP サーバー。`/healthz`、`/`（hostname と `TETHERD_ENV`）、`/db`（`SELECT now()`）、`/whoami`（STS `GetCallerIdentity`）、`/items` CRUD。

### 9.3 テストの 4 層

1. **ユニット**（CI）: pf ルール生成、env 合成、設定、ヘッダー一致、`/proc` 走査（fixture）、helper プロトコル
2. **AWS なし・root なしの結合**（CI）: agent + CLI を `direct` で接続し、steal / dial / resolve / ping 断の復帰 / 二重接続拒否。environ 読み取りは Linux コンテナで `--pid` 共有して実機検証
3. **AWS なし・root ありの macOS e2e**（`hack/e2e-local.sh`）: `docker compose` で agent + postgres + sampleapp を Mac から直接届かない Docker ネットワーク（`172.20.0.0/16`）に立て、agent の `:9900` だけ公開。`tetherd run --transport direct --agent-addr 127.0.0.1:9900 --remote-cidr 172.20.0.0/16 -- psql -h 172.20.0.10` が通れば pf / gid / rdr / natlook / yamux / dial が本物で検証できる。bash / zsh / Go / Node の子プロセスからそれぞれ確認する
4. **AWS e2e**（手動チェックリスト）: RDS へ psql、`/whoami` がタスクロール、ALB 経由の steal（ヘッダーあり → ローカル、なし → タスク）、フォールバック、`desired_count = 2` での取りこぼしゼロ、`force-new-deployment` 中の再接続、Cloud Map 名の解決

CI: Linux で 1・2 + lint、macOS runner で build + 1。GoReleaser snapshot を PR で回す。

---

## 10. リポジトリ構成

```
cmd/{tetherd,tetherd-helper,tetherd-exec,tetherd-agent}/
internal/
  proto/        制御プロトコルのメッセージ型。CLI と agent で共有
  session/      yamux セッションのラッパー。Dial / Resolve / AcceptSteal / ping
  transport/    Transport。ssm/、direct/
  provider/ecs/ タスク発見、VPC CIDR、prefix list、secrets 名
  capture/      Capturer。darwin/
  helper/       helper の client / server、pf、resolver、natlook
  proxy/        透過プロキシ、DNS リゾルバ、steal の受け口
  env/          合成・除外・マスク
  agent/        L7 プロキシ、セッション、environ、dial / resolve
  config/
  cli/          cobra
  doctor/
examples/sampleapp/
deploy/dev-env/       Terraform
deploy/taskdef/       既存タスク定義に agent を足す JSON スニペット
hack/                 e2e-local.sh、docker-compose.yml
docs/                 design.md、specs/
.github/workflows/    ci.yml、release.yml
```

差し替え点となるインターフェース:

```go
// capture: 子プロセスの TCP を (元の宛先, 接続) として渡す。OS 依存はこの裏だけ
type Capturer interface {
    Start(ctx context.Context, spec Spec) error   // Spec{RemoteCIDRs, RedirectPort}
    Accept() (Conn, error)                        // Conn: net.Conn + OriginalDst
    Close() error
}

// transport: agent への 1 本の TCP
type Transport interface {
    Dial(ctx context.Context, task Task) (net.Conn, error)
}

// helper: root が要る操作の全集合
type Helper interface {
    PfApply(ctx context.Context, spec PfSpec) error
    PfClear(ctx context.Context) error
    ResolverSet(ctx context.Context, domains []string, port int) error
    ResolverClear(ctx context.Context) error
    NatLook(ctx context.Context, proto string, src, dst netip.AddrPort) (netip.AddrPort, error)
}
```

依存: `hashicorp/yamux`、`spf13/cobra`、`aws-sdk-go-v2`（ecs / ec2 / ssm / sts / servicediscovery）、`miekg/dns`、`gopkg.in/yaml.v3`、`golang.org/x/sys`。

---

## 11. 安全策（v1 での整理）

design.md §10 に加えて:

- steal はトークン一致が必須。公開 ALB でもユーザー名だけでは届かない
- 外向きの既定は「VPC 内だけリモート」。dev タスクを踏み台にした egress は既定で存在しない
- helper は `admin` グループのユーザーからのみ受け付け、操作は 5 つに固定。コマンド起動の操作は無い
- setgid `tetherd` の権限は pf に捕まることだけ
- `:9900` は無認証だが lo にしか bind せず、信頼境界は「タスク内」。design.md に明記する
- ローカルアプリは共有 dev DB に書く。ローカルブランチの auto-migrate が dev DB を変えうることを README で注意する

---

## 12. 先に潰す検証（順序つき）

設計の妥当性を左右するものから。1〜4 は AWS 不要。

1. `tetherd-exec`（`setregid`）+ pf `group` + `rdr` で、bash / zsh / Go / Node の子プロセスの TCP が捕まり、他プロセスは捕まらないこと（9.3 の第 3 層）
2. `DIOCNATLOOK` が macOS 26 で期待どおり元の宛先を返すこと（構造体レイアウト）
3. `DIOCCHANGERULE` でメインルールセットにアンカー参照を挿入できること。無理なら dump/reload に切り替える
4. `/etc/resolver/<domain>` + `port` が Go / Node（`dns.lookup`）/ JVM の `getaddrinfo` で効くこと
5. Fargate で `pidMode: task` + `SYS_PTRACE` + ECS Exec（ssm-agent 注入）が共存し、agent が app の environ を読めること
6. `ssm:StartSession` + `AWS-StartPortForwardingSession` で `ecs:` ターゲットの `127.0.0.1:9900` に届くこと、フロー確立の所要時間と RTT

---

## 13. design.md に反映する変更

この spec の承認後、design.md を以下の方針で改訂し、HTML 版（artifact）を再生成する。

- §1 / §6: 「ユーザー空間ネットワークスタックで終端」→ v1 は rdr + カーネル TCP。netstack は Linux / UDP 向けの将来案として残す
- §3.3 / §6: 補助グループ → primary gid、helper の spawn → setgid ラッパー、`set skip on lo0` の記述を削除、AWS CLI 依存を削除
- §5 / §7: コマンドと設定を v1 の形に。mirror / env-rewrite / `remote_localhost` / `init` は「v2 以降」節へ
- §4 / §10: ルーティング既定（VPC 内だけ）、トークン、VPC 外サービスの扱い、CloudTrail の注記
- §9: IAM に EC2 読み取り 4 つ、`essential` / `restartPolicy`、GHCR
- §14 未決事項: 外向き既定・pf 方式・イメージ配布先を決着済みに移す。`:9900` の信頼境界、複数セッション、auto-migrate の注意を追加
- §15 PoC 論点: 本 spec §12 に置き換え
- ロードマップ: v0.1 ローカル e2e（第 3 層）→ v0.2 dev-env + agent + SSM → v0.3 steal → v0.4 brew 配布 → v1.0
