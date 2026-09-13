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
| ルーティング | 宛先 IP。VPC CIDR + 追加 CIDR + prefix list だけリモート（`169.254.170.0/24` は `pin_credential_route` のときだけ） | 「全部リモート」「ホスト名で local 指定」→ CIDR ベース。§14 未決の第 1 項を決着 |
| steal の一致 | `X-Dev-User` + `X-Dev-Token`（ユーザー固定トークン） | ヘッダー 1 つ → 2 つ |
| 複数タスク | RUNNING な全タスクに接続 | 1 タスク → 全タスク |
| トランスポート | `ssm:StartSession` を SDK で呼び `session-manager-plugin` を子プロセスで起動 | AWS CLI 依存を削除 |
| agent イメージ | 利用者が自分のレジストリに push（`make push-images ECR_REGISTRY=...`） | 未決 → 当初 GHCR を想定したが、公開するパイプラインは未実装。v1.0 に持ち越し |
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
| Developer ID 署名・公証 | cask の `postflight` が quarantine を外す（brew でも属性は必ず付く。§8） | Developer ID 取得後 |
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

実機検証は `make e2e-local` の結果で確定。helper は `/etc/pf.conf` に一切触れず、既定の `/etc/pf.conf` が持つワイルドカード参照 `rdr-anchor "com.apple/*"` / `anchor "com.apple/*"` に乗る**子アンカー** `com.apple/900.tetherd` にルールをロードする（`set skip on lo0` が無いことも含め、手元の macOS 26 で確認済み）。ディスクには書かない。

```
table <tetherd_remote> { 10.0.0.0/16, ... }
rdr pass on lo0 inet proto tcp from any to <tetherd_remote> -> 127.0.0.1 port <redirect_port>
pass out route-to lo0 inet proto tcp from any to <tetherd_remote> group tetherd keep state
```

- `group tetherd` のプロセスが `<tetherd_remote>` 宛に出した TCP だけが lo0 に回り、rdr で CLI の透過ポートに落ちる。他プロセスの同じ宛先への通信は物理 IF から出る
- macOS の既定 `/etc/pf.conf` には `set skip on lo0` は**無い**（design.md の記述は OpenBSD の既定との混同。手元の macOS 26 で確認済み）
- メインルールセットには一切手を入れない。既定の `/etc/pf.conf` がすでに持つ `rdr-anchor "com.apple/*"` / `anchor "com.apple/*"` のワイルドカード参照が、名前が `com.apple/` で始まる子アンカーをそのまま拾う。`900.` は Apple 自身の子アンカー（`250.ApplicationFirewall` など）より後に評価される名前。専用アンカー `com.tetherd` を `DIOCCHANGERULE` ioctl でメインルールセットに挿す方式（sshuttle の `pf_add_anchor_rule` と同じ）は、この子アンカー方式で実機検証済みのため不要と判断し、以降は計画しない
- アンカー内のルールは `pfctl -a com.apple/900.tetherd -f -` で stdin からロード
- `pf.apply` で `pfctl -E`（参照カウント。stderr の `Token : N` を保持）、`pf.clear` と終了時に `pfctl -X N`。セッションの外では pf を有効化した状態すら残さない。`/etc/pf.conf` のコメントにある作法そのもの
- helper 起動時に `com.apple/900.tetherd` アンカーが残っていれば空にする（前回の異常終了対策）

### 3.3 元の宛先の復元

CLI が `127.0.0.1:<redirect_port>` で accept したら、helper に `natlook{proto, src, dst}` を送り、helper が `/dev/pf` に `DIOCNATLOOK`（direction `PF_OUT`）を発行して rdr 前の宛先を返す。UNIX ソケット 1 往復（< 1 ms）で、SSM の RTT（数十 ms）に対して無視できる。`/dev/pf` の fd を CLI に渡す最適化は取らない（helper の操作を数えられる範囲に閉じるため）。

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
- 1 接続 = 1 セッション。`pf.apply` は接続ごとに 1 回。接続が切れたら（CLI の異常終了含む）helper がそのセッションの pf ルール・resolver ファイル・host route を消す（消す順は resolver → route → pf）
- 同時セッションは 1 つ。2 つ目の `pf.apply` は `busy{pid, command, since}` で拒否
- 最初に `version` を交換。プロトコルバージョン不一致なら CLI が `brew upgrade tetherd && sudo tetherd-helper install`（その後 `sudo launchctl kickstart -k system/dev.tetherd.helper`）を案内する。helper は `/Library/LaunchDaemons/dev.tetherd.helper.plist` で launchd が持つので brew の service ではなく、`brew services restart tetherd` では再起動できない（§8）

| 操作 | 引数 | 内容 |
|---|---|---|
| `pf.apply` | `remote_cidrs[]`, `redirect_port` | テーブルとルールをアンカーにロード |
| `pf.clear` | — | アンカーを空にする |
| `resolver.set` | `domains[]`, `port` | `/etc/resolver/<domain>` を作成 |
| `resolver.clear` | — | tetherd 管理のファイルを削除 |
| `natlook` | `proto`, `src`, `dst` | 元の宛先を返す |
| `route.set` | `hosts[]` | host route を lo0 に張る（`pin_credential_route` のときだけ使う。§4.2） |
| `route.clear` | — | そのセッションが張ったルートを消す |

---

## 4. ルーティングと IAM

### 4.1 リモートに回す集合

`<tetherd_remote>` の中身。**宛先 IP で決める。既定は「VPC の中だけリモート」**。

1. 対象タスクの VPC の CIDR（セカンダリ含む）。`DescribeTasks` → attachments の subnetId → `DescribeSubnets` → `DescribeVpcs` の `CidrBlockAssociationSet`（state = associated）
2. `network.remote_cidrs`。ピアリング先 VPC、Transit Gateway 越しのオンプレなど
3. `network.remote_services` に書いた AWS サービスの managed prefix list（`com.amazonaws.<region>.s3` / `.dynamodb`）。`DescribeManagedPrefixLists` → `GetManagedPrefixListEntries`
4. `network.pin_credential_route: true` のときだけ `169.254.170.0/24`（§4.2 の逃げ道。既定では**入らない**）

から `network.local_cidrs` を除く。引き算は範囲を分割する正確なもので、`10.0.0.0/16` から `10.0.5.0/24` を除けば残りは 8 個のプレフィックスになる（pf は 1 つのテーブルに集合として持つ）。`pin_credential_route` が有効なときは `169.254.170.0/24` が引かれない床になり、`local_cidrs` に何を書いても残る — 固定しておきながら捕捉から外すと、そのアドレスが lo0 に吸い込まれたまま誰も応答しない状態になるため。`local_cidrs` が（床以外の）すべてを消した場合は、起動時にエラーにして `local_cidrs` を名指しする（`10.0.0.0/8` と書いて `10.0.0.0/16` の VPC を丸ごと消す、が現実的な失敗）。

v0.2b までは 2 番目が `169.254.170.0/24` で、タスクロールを「透過で通す」設計だった。実機でそれが**間欠的に壊れる**ことが分かったため（§4.2）、v0.3a でループバック方式に変えた。

それ以外（インターネット、localhost、LAN）は子プロセスからそのまま出る。`go run` のモジュール取得、`npm install`、外部 API はラップトップの回線。dev タスクの ENI を踏み台にインターネットへ出る経路は既定で無い。`remote_cidrs: [0.0.0.0/0]` を書けば可能だが `doctor` が警告する。

起動時にラップトップの IF アドレスとリモート集合の重なりを検査し、重なっていれば警告して `local_cidrs` を案内する。

### 4.2 タスクロールの届け方（v0.3a で変更）

タスクの env には `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` が入っていて、SDK はそれを見て `169.254.170.2` に認証情報を取りに行く。このアドレスはタスクの中にしか存在しない。

**v0.2b の方式（透過）と、それが壊れた理由**: `169.254.170.0/24` を捕捉範囲に入れて pf で agent へ流していた。ところが `connect()` のルート探索は pf の出力ルールより**先**に走る。macOS はこのアドレスへの ARP を LAN に投げて失敗し、en0 上に拒否ルートを残す（`netstat -rn` の `!`、`route -n get` の `LLINFO` と負の `expire`）。その拒否エントリが生きている間はカーネルが `EHOSTUNREACH` を即返し、**pf はパケットを一度も見ない**。実機で、同じ子プロセス・同じ gid なのに `curl` は 200 を得て AWS CLI 同梱の python は `Errno 65` で落ちた。ARP エントリの期限で成否が変わるので間欠的に壊れる。

lo0 への host route を張れば探索は必ず成功するが、**その固定はマシン全体に効く**。しかも pf の `rdr`（変換ルール）は `group` 句を受け付けないので（filter ルールは受け付ける）、gid で絞れない。つまりセッション中は `tetherd` グループ以外のプロセスも `169.254.170.2` で dev タスクの認証情報に到達する。`amazon-ecs-local-container-endpoints` はこのアドレスを lo0 に alias して使うので、衝突相手が実在する。

**v0.3a の方式（ループバック）**: CLI が `127.0.0.1` の空きポートに小さな HTTP 口を開き、来たリクエストを既存のセッション経由で `169.254.170.2` に転送する。子プロセスには env でそこを教える:

```
AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:<port>/v2/credentials/<uuid>
（AWS_CONTAINER_CREDENTIALS_RELATIVE_URI は子の env から取り除く。下記）
ECS_CONTAINER_METADATA_URI_V4=http://127.0.0.1:<port>/v4/<task>
ECS_CONTAINER_METADATA_URI=http://127.0.0.1:<port>/v3/<task>
ECS_AGENT_URI=http://127.0.0.1:<port>/v1
```

1 つのポートで 4 変数すべてを賄う（パスがそのまま転送されるため）。AWS SDK は `FULL_URI` のホストがループバックなら平文 HTTP をトークンなしで受理する（aws-sdk-go-v2 の `config/resolve_credentials.go` の `isAllowedHost` が `ip.IsLoopback()` を許可。他言語の SDK も同じ規則）。

`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` は**空にするのではなく取り除く**。理由が 2 段ある。第一に、aws-sdk-go-v2 の解決順は `case len(envConfig.ContainerCredentialsRelativePath) != 0` が `ContainerCredentialsEndpoint`（= `FULL_URI`）より**先**なので、両方セットされていれば relative が勝ち、SDK は `169.254.170.2` に行く。第二に、**botocore は値の有無ではなく変数の存在で分岐する** — `ContainerProvider._provided_relative_uri()` は `return self.ENV_VAR in self._environ` で、空文字でも「ある」と判定して `http://169.254.170.2` + `""` を取りに行く。つまり空にするだけでは Go の子プロセスは救われても、`aws` CLI と boto3 のプログラム（現実には大多数）は誰もルーティングしていないアドレスに行って認証情報を得られない。実機の botocore 1.43.89 で確認した。

これで:

- **root 操作が要らない**。helper に頼む必要が無い
- **アドレスが毎回変わり、広告もされない**。`169.254.170.2` のように「ECS を知っているツールが必ず試す先」ではなくなる。ただし**ループバックの listener は同一マシンの全プロセスから到達可能**で、uid やプロセスツリーによる絞り込みは無い（§11）
- **拒否ルートの問題が消える**。誰も `169.254.170.2` に接続しないため
- **pf に依存しない**。捕捉範囲に何が入っているかと無関係に動く

代償は 2 つ。第一に、これは env の書き換えなので「透過」ではない — env を読まずにアドレスを直書きしているツールには届かない。そのために `network.pin_credential_route: true` を残す（v0.2b の固定方式に戻す逃げ道。影響範囲は上記のまま）。第二に、ループバック口は無認証で、**同じマシンのどのプロセスからでも**（uid を問わず）叩けばタスクロールの認証情報を得られる。信頼境界は既存の SSM ローカルフォワード（`127.0.0.1:9900`）と同じで、§11 に記載する。固定方式より狭いのは「アドレスが予測できない」点だけで、「プロセス単位に閉じている」わけではない。

### 4.3 VPC 外の AWS サービス

S3 / DynamoDB / SQS / Secrets Manager / Bedrock など VPC 外のサービスは**既定のままで動く**。SDK は `169.254.170.2`（トンネル経由）から一時クレデンシャルを取り、以降は SigV4 署名でパブリックエンドポイントに直接送る。署名が正しければ送信元 IP がラップトップでも受け付けられる。

動かないのは IAM / バケット / SCP / エンドポイントポリシーに**ネットワーク条件**（`aws:SourceVpc` / `aws:SourceVpce` / `aws:SourceIp`）がある場合。対処:

| エンドポイント | 対処 |
|---|---|
| Interface endpoint（Secrets Manager、SQS、STS 等。private DNS 有効） | `remote_domains` にサービスのドメインを書く。agent 側で解決するとプライベート IP になり VPC CIDR に入る |
| Gateway endpoint（S3、DynamoDB） | `remote_services: [s3, dynamodb]`。prefix list の IP をリモート集合に足す |

README に書くこと: 条件が無い環境ではラップトップ経路のほうが緩い（NAT の無い VPC でもラップトップは自前で出られる）、CloudTrail の `sourceIPAddress` はラップトップの IP になる。

### 4.4 開発者の IAM ポリシー

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
- ターゲットグループが gRPC / HTTP2 のものは対象外（agent は HTTP/1.1 サーバなので steal も素通しも成立しない。**検出する `doctor` の行はまだ無い** — §6.5）
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

- ユーザー名 → セッションのマップ。**`incoming.enabled` を宣言したセッションのみ**を登録し、同名の二重接続は `error{code: "duplicate_user", from, since}` で拒否
- `incoming.enabled` が無いセッション（`tetherd env` / `doctor` / `status`、および `tetherd run --no-incoming`）は**登録しない**ため、この拒否の対象にもならない。リクエストを受け取らないセッションは、この規則が防いでいるルーティングの曖昧さを作れない（steal の照合は `incoming.enabled` が無いセッションを最初から飛ばす）。登録しないことで `welcome.others` / `welcome.sessions` は「リクエストを受け取れるのは誰か」を意味する — `tetherd status` が答える問いそのもの。登録していないセッションもアタッチ/デタッチはログに出す（ログだけ、レジストリには入れない）
- 1 台のマシンで `tetherd run` が二重に走らないことは CLI 側の pid ロックが保証する（agent のレジストリではない）
- 制御ストリームで 5 秒おきに ping。3 回連続で pong が無ければセッションを破棄し、そのユーザーのルールを消す
- `TETHERD_ENV` が無ければ起動しない。値は `welcome.env` で CLI に返す

### 5.5 設定と配布

- env のみ: `TETHERD_ENV`（必須）、`TETHERD_PROXY`（`0.0.0.0:8080`。ALB を受ける口）、`TETHERD_APP_ADDR`（`127.0.0.1:8081`。app への転送先）、`TETHERD_CONTROL`（`127.0.0.1:9900`）、`TETHERD_APP_CONTAINER`（`app`）、`TETHERD_TASK_ARN`（任意。メタデータが取れればそちらが優先）
- `TETHERD_CONTROL` は**実装上は固定**。agent 側の既定と CLI の `ssm` トランスポートが転送先ポートに入れる値は v0.4 で 1 つの定数（`internal/proto` の `DefaultControlPort`、`internal/transport/ssm` の `controlPort` がこれを参照）になったので両者がずれることは無いが、**これを変えた agent は依然として誰も繋げない listen になる** — CLI が新しいポートを知る経路は制御ポート自身しか無いため（循環）。失敗は「agent に届かない」として出る。テストと埋め込み用の口であり、デプロイのつまみではない。真に追随させるには CLI が接続前に読める場所（`.tetherd.yml`）に書かせる必要があり、それは v1 の判断
- AWS API は呼ばない
- イメージは distroless static、linux/arm64 + linux/amd64。**このリポジトリには公開レジストリへ push するパイプラインが無い**（v0.4 実測: `.goreleaser.yml` に `dockers:` / `kos:` は無く、イメージを扱うのは `make push-images` だけで、呼び出し元が渡す ECR レジストリへ push する）。利用者は `make push-images ECR_REGISTRY=...` で自分のレジストリに置く。公開レジストリでの配布は v1.0 に持ち越し
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

- `ssm`: SDK で `ssm:StartSession`（Target `ecs:<cluster>_<taskId>_<runtimeId>`、DocumentName `AWS-StartPortForwardingSession`、Parameters `portNumber: ["9900"]`, `localPortNumber: ["<空きポート>"]`）。**ターゲットの runtimeId はタスク内のどのコンテナでもよい**: awsvpc ではタスク内の全コンテナが同じネットワーク名前空間を共有するので、どのコンテナの ECS Exec エージェント経由で転送しても `127.0.0.1:9900` の agent に届く。`DescribeTasks` は SSM から実際には到達できないコンテナでも `ExecuteCommandAgent` を `RUNNING` と報告する（distroless の agent コンテナが実機でこれに該当し、`TargetNotConnected` になる）ため、CLI は候補（agent コンテナ → 他のコンテナ）を順に試し、`TargetNotConnected` なら次へ進む。応答を `session-manager-plugin` に AWS CLI と同じ引数（セッション JSON、リージョン、`StartSession`、プロファイル、パラメータ JSON、エンドポイント）で渡して子プロセスとして起動し、`127.0.0.1:<port>` に dial。AWS CLI 自体には依存しない
- `direct`: 指定アドレスに TCP。ローカル e2e と結合テスト用。`run --transport direct --agent-addr host:port --remote-cidr ...` で使う
- 再接続: yamux セッションが死んだら指数バックオフ（1s → 30s）で `StartSession` からやり直し、`hello` を再送。進行中の dial は切れる
- plugin の埋め込み（`aws/session-manager-plugin` の datachannel を組み込んで依存ゼロにする）は v2 候補。`Transport` の実装として足せる

### 6.2 タスクの発見と複数タスク

- `ListTasks(cluster, serviceName, desiredStatus=RUNNING)` → `DescribeTasks`。`tetherd-agent` コンテナがあり、`enableExecuteCommand` が true で、`managedAgents[ExecuteCommandAgent].lastStatus == RUNNING` のものを対象に
- 対象タスク**全部**に SSM + yamux + `hello`。steal はどのタスクからでも受ける
- タスクごとの SSM ターゲットは「そのタスクのコンテナのうち exec エージェントが繋がっているもの」。§6.1 のとおり候補を順に試す
- `dial` / `resolve` / env は primary（起動が最も古いタスク）を使う。primary が落ちたら次に古いものへ
- 10 秒おきに `ListTasks` を再実行し、新しいタスクには接続、消えたタスクは片付ける（rolling deploy の追従）
- `--task ID` で明示した場合はそのタスクだけ

### 6.3 `run` の流れ

1. 設定読み込み（`.tetherd.yml` + `~/.tetherd/config.yml`）→ AWS 認証 → タスク発見
2. helper に接続、バージョン確認
3. 全タスクへ接続、primary の `welcome` から env と `TETHERD_ENV` を取得。`target.env` と不一致なら切断して終了
4. VPC CIDR / prefix list を取得 → リモート集合を組む → ローカル IF との重なりを警告
5. helper に `pf.apply` と（必要なら）`resolver.set` と（`pin_credential_route` のときだけ）`route.set`
6. 透過プロキシ（`127.0.0.1:<空きポート>`）、DNS リゾルバ（`127.0.0.1:53530`）、認証情報の口（`127.0.0.1:<空きポート>`。§4.2）、steal の `http.Serve` を起動
7. IAM 確認: 認証情報の口を自分で叩いて（そこから `dial` ストリームでタスクの `169.254.170.2` へ）クレデンシャルを取り、`sts:GetCallerIdentity` を呼んで表示。子プロセスと同じ経路を通るので、この行が出れば子でも動く
8. ステータス行を出す
9. `/usr/local/libexec/tetherd/tetherd-exec -- <command>` を子プロセスとして起動。同じプロセスグループ、stdio 素通し、合成した env
10. 子の終了またはシグナルで: `bye` → `resolver.clear` → `route.clear` → `pf.clear` → plugin 終了 → 子の終了コードで exit

Ctrl-C は端末がプロセスグループ全体に SIGINT を送るので、CLI は子の終了を待ってから片付ける。CLI が異常終了しても helper がソケット切断で掃除する。

### 6.4 env の合成

`ローカル env < タスク env < env.override`。

タスク env から既定で除外: `PATH HOME HOSTNAME USER LOGNAME SHELL TMPDIR PWD OLDPWD TERM LANG LC_* SHLVL _ AWS_EXECUTION_ENV`。

これに加えて、**コンテナのファイルシステムを指し、ランタイムの信頼やコード解決を黙って変えてしまう変数**も既定で除外する: `SSL_CERT_FILE` `SSL_CERT_DIR` `AWS_CA_BUNDLE` `REQUESTS_CA_BUNDLE` `CURL_CA_BUNDLE` `NODE_EXTRA_CA_CERTS` `LD_LIBRARY_PATH` `LD_PRELOAD` `DYLD_LIBRARY_PATH` `DYLD_INSERT_LIBRARIES` `JAVA_HOME` `GOROOT` `PYTHONHOME` `PYTHONPATH`。実機で確認した例: distroless イメージは `SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt` を設定しており、macOS にこのパスは無いため、これを尊重する子プロセス（Go の `crypto/x509` など）の TLS が全部 `certificate signed by unknown authority` で落ちた（独自の CA バンドルを持つ `aws` CLI は影響を受けなかった）。

`169.254.170.2` を指すエンドポイント変数のうち、`ECS_CONTAINER_METADATA_URI_V4` / `ECS_CONTAINER_METADATA_URI` / `ECS_AGENT_URI` の 3 つは透過モードではホストをループバック口に**書き換えて**渡し（パスはそのまま転送される）、`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` は**取り除いて** `AWS_CONTAINER_CREDENTIALS_FULL_URI` をループバック口で与える（§4.2 の理由）。`--no-network` では 4 つとも落とす。

透過モードでタスクの env が `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` を持つときは、それだけでは子プロセスがタスクロールにならない。どの SDK も**共有設定プロファイルをコンテナクレデンシャルより先に**評価するので、開発者の `~/.aws/config` の `default` プロファイルが認証ソース（SSO、login session、`credential_process` など）を持っていると、そちらが勝つ（実機で確認: 子プロセスの `aws sts get-caller-identity` が「session has expired」を返し、共有設定を隠すと即座にタスクロールを返した）。そこで tetherd は静的キーと `AWS_PROFILE` を除去するだけでなく、`AWS_CONFIG_FILE` と `AWS_SHARED_CREDENTIALS_FILE` をセッション用の空ファイルに向け、`AWS_REGION` / `AWS_DEFAULT_REGION` をタスクのリージョンで明示する。認証情報そのものは `AWS_CONTAINER_CREDENTIALS_FULL_URI` で与えるので、自動更新は SDK 任せのまま。共有設定を隠すことは `✓ iam` の次の行に明示する。

`--no-network` では tetherd は認証情報に一切触らない（開発者自身の身元のまま）。タスクの `AWS_REGION` は注入されるが、`169.254.170.2` を指す 4 つの変数は落とす（ループバック口も開かない）。セッションの中で自分のプロファイルを使いたい場合は `--no-env`（タスクの env を注入しない）か `--no-network`（捕捉しない）を使う。

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
- `status`: タスクごとに接続中のユーザー、どこから、いつから。v0.3b で実装。専用のメッセージは足さず、attach の `welcome` に載る `sessions` を読む（`hello` 1 往復で済み、追加の状態も持たないため）。読むためだけに attach するので `incoming` を無効にし、トークンも送らない — `status` 自身が steal の宛先にならないことが要件（§11）。タスクごとに 1 行以上出し、読めなかったタスクも理由付きで出す
- `doctor` の検査項目（各項目に「次に何をするか」を付ける）:
  helper が応答しバージョンが一致 / `tetherd` グループと setgid `tetherd-exec` / session-manager-plugin の有無 / AWS 認証 / サービスの `enableExecuteCommand` / タスクの agent コンテナと ExecuteCommandAgent / タスク定義の `pidMode: task` / ターゲットグループが HTTP1 / ECS・EC2 の読み取り権限 / VPC CIDR とローカル IF の重なり / `remote_domains` が agent 側で解けるか / `remote_cidrs` に `0.0.0.0/0` が無いか

  v0.2b で実装したのは 9 項目（helper の応答とバージョン / `tetherd` グループと setgid `tetherd-exec` / `session-manager-plugin` / AWS 認証 / 接続可能なタスク / `pidMode: task` / 捕捉範囲の広さ / 捕捉範囲とローカル IF の重なり / `remote_domains` が agent 側で解けるか）。**ターゲットグループが HTTP1 かの検査は v0.4 で入れた**（`target group` の行。`aws-sdk-go-v2/service/elasticloadbalancingv2` を足し、developer policy に `elasticloadbalancing:DescribeTargetGroups` を戻した。v0.3b に入らなかった理由は §12 の v0.3b）。ECS・EC2 の読み取り権限は個別項目にせず、各検査が `AccessDenied` で失敗したときにそのメッセージで示す

  v0.3a / v0.3b で足したのは、`agent session`（tetherd 自身が通した handshake。ECS の見解とは別）・`task env`（agent が読めた変数と `env_error`）・`task role`（子プロセスと同じ経路でループバック口から取った認証情報の ARN）・`steal`（一致条件と、ラップトップ側に listener が居るか）の 4 行と、**`?`（検査できなかった）ステータス**。`?` は「動くが注意」の `⚠` と分けてあり、**どの行でも exit code を動かさない**（失敗した行は既にそれ自身で数えられているため）。`pin_credential_route` の行は入っていない

  v0.4 で足したのは `target group` の 1 行。`protocol_version` が `HTTP1` でなければ `✗`（spec §5.1 が gRPC / HTTP2 を対象外にしており、agent は HTTP/1.1 サーバなので ALB の h2c ヘルスチェックが落ちる）、`elasticloadbalancing:DescribeTargetGroups` が無ければ `?`（権限が古い開発者の環境は壊れていない）、**ターゲットグループのポートが agent の受け口と違えば `⚠` で、`✗` にはしない** — `TETHERD_PROXY` で動かせる以上、違うポートを向けた配置は正しく設定されている。その `TETHERD_PROXY` はタスク定義の agent コンテナの env から読むので、ポートの比較は既定値の当て推量ではなく実際の値どうしになる（env-file や Secrets 経由で設定されていて読めないときは `?`）

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
  # `container:` は書けない（v0.4 から起動時エラー、終了コード 2）。env を読む
  # コンテナを決めるのは agent 側の TETHERD_APP_CONTAINER（既定 app）
  agent_container: tetherd-agent # サイドカーを改名しているときだけ（既定 tetherd-agent）
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

- `{"type":"dial","addr":"10.0.3.21:5432"}` → `{"type":"dial","ok":true}` または `{"type":"dial","ok":false,"error":"..."}`（コーデックが常に `type` を付与する）→ 双方向コピー
- `{"type":"resolve","name":"api.myapp.internal","qtype":"A"}` → `{"ok":true,"addrs":[...],"ttl":30}` → close

**agent が開くストリーム**

- `{"type":"http"}` → 以降は HTTP/1.1（`net/http` 同士が keep-alive で複数リクエストを流す）

---

## 8. 配布とインストール

```
brew install kyosu-1/tap/tetherd
sudo tetherd-helper install        # sudo はこの 1 回
tetherd doctor
```

- **リリースは `v0.4.0` から始まる。`v0.1`〜`v0.3b` は `docs/plans/` の開発マイルストーンで、公開リリースは存在しない**（v0.4 実測: タグが 1 つも無い）。マイルストーン名はこの spec と Go のコメントに 241 箇所あり、その大半は「いつ・なぜ変わったか」の記録なので**番号を振り直さない**。歴史の記録を書き換えるより、タグ一覧に穴があるほうを選ぶ
- tap `kyosu-1/homebrew-tap`（`brew install kyosu-1/tap/tetherd`）。GoReleaser がタグ push で GitHub Release（darwin arm64 / amd64）と tap の cask 更新を行う。**イメージは含めない**（v0.4 実測: `.goreleaser.yml` に `dockers:` / `kos:` は無い。Task 1 の決定 1 で `tetherd-agent` を意図的に除外した）。agent / sampleapp イメージは利用者が `make push-images ECR_REGISTRY=...` で自分のレジストリに push する。公開レジストリへの publish は v1.0 に持ち越し
- **formula ではなく cask。** 3 バイナリを prefix に置くだけなのは変わらない（v0.4 実測: cask DSL の `binary` スタンザ 3 本）。cask にした理由は署名していないこと: Homebrew は cask のダウンロードを必ず quarantine するので、`postflight` で `xattr -dr com.apple.quarantine` を外す必要があり、formula にはその口が無い。`service` ブロックは使わない（launchd の登録は helper 自身が行う）。cask の `uninstall launchctl:` / `delete:` は `brew uninstall` が daemon を止めるための保険で、アンインストールの本体ではない（下記）
- brew はインストール時に root の処理を実行できないので、root が要る初期化は `sudo tetherd-helper install` が行う: グループ `tetherd` の作成、`tetherd-exec` の `/usr/local/libexec/tetherd/` へのコピー（`root:tetherd`、`2755`）、**`tetherd-helper` 自身の同じディレクトリへのコピー**（v0.4 で追加。plist が指すのは Homebrew の prefix ではなくこのコピー。理由は `tetherd-exec` と同じで「Homebrew の prefix はユーザが書ける」、しかも launchd が root で起動するのはこちらなのでより強く効く）、`/Library/LaunchDaemons/dev.tetherd.helper.plist` の生成と `launchctl bootstrap`。**`install` は転送先パスの各構成要素（`/`, `/usr`, `/usr/local`, `/usr/local/libexec`, `/usr/local/libexec/tetherd`）が root 所有・group/world 書き込み不可・かつディレクトリであることを先に確かめ、違えば何も書かずに失敗する。エラーは違反した構成要素と、それを直すコマンドの両方を出す**（v0.4。Homebrew がどのディレクトリを書き込み可能にするかを当てにしない）。**同じ検査を `/Library/LaunchDaemons` と `/var/log` にも掛ける**（plist は launchd が「root で何を起動するか」を決めるもう一方の入力であり、ディレクトリに group が書ければファイル自身の mode に関係なく差し替えられる。`/var/log` は plist の `StandardOutPath` / `StandardErrorPath` を launchd が **root で開く**先で、ここに書けるユーザは log のパスに symlink を置いて root の追記先を選べる。plist が root に触らせる 3 つのパスすべてを検査する）。`install` が作るディレクトリの mode は umask に任せず明示する（`sudo` は呼び出し元の umask と 0022 の和を使うので、`umask 077` の開発者では `MkdirAll` が 0700 を作り、setgid の `tetherd-exec` に到達できなくなる）。`brew upgrade tetherd` の後は `sudo tetherd-helper install` を再実行する（冪等。`doctor` がバージョン不一致を検出して案内する）
- **helper は常駐しない（v1.0 で実装した）。** 正確に言うと「アイドル中は root のプロセスが存在しない」で、**plist のロードごとに約 30 秒の窓が 1 回ある**（起動時と `sudo tetherd-helper install` の直後）。`man launchd.plist` が `KeepAlive` は `RunAtLoad` を含意すると述べており、それは `SuccessfulExit` のようなサブキーでは回避できないので、**`RunAtLoad` を消したのは見かけ上の変更**で、launchd はロードごとに 1 度 helper を起動する。root が居続けないようにしているのは**アイドル終了**のほう。**この含意は man page を読んだ結果で、実測ではない**（確認には `launchctl bootstrap` が必要）。実機で最初に確かめるべき主張はこれ。
  - **実装（v1.0）:** plist の `Sockets` に `SockPathName` = `/var/run/tetherd.sock` と `SockPathMode` = `0666` を置き、launchd がソケットを作って bind・listen する。helper は `launch_activate_socket()` を **`purego`** 経由で呼んで fd を受け取り（cgo を使わないので `CGO_ENABLED=0` が維持される）、自分では bind しない。接続が無くなって `helper.DefaultIdleTimeout`（30 秒）で `Serve` が戻り、**終了コード 0** で終わる。終了時に `platform.Shutdown()` が走るので pf と `/etc/resolver` の片付けは常駐時と同じ
  - **`--socket` は fallback になった。** `launch_activate_socket()` が「launchd 管理のプロセスではない」（`ESRCH`）または「そのジョブにソケットが無い」（`ENOENT`）と答えたときだけ、helper が自分で bind する。前面起動の `sudo tetherd-helper` と `hack/e2e-local.sh` がこの経路で、開発ループなので維持する
  - **`KeepAlive: {SuccessfulExit: false}` は v1.0 で初めて正しい形になった。** 意味は「終了コードが 0 以外なら再起動」（`man launchd.plist` 実測、Darwin 25.6.0: "If false, the job will be restarted in the inverse condition."）。**helper の通常の終わり方がアイドル終了の 0 になったので、この形はそれを放置しつつクラッシュだけ拾う。**素の `KeepAlive: true` だとアイドル終了の直後に起動し直して設計と正面衝突する。なお初期化失敗（終了コード 1）は `ThrottleInterval`（10 秒、launchd の既定値と同値だが明示する）ごとに無限に再試行され、`/var/log/tetherd-helper.log` に同じ行を書き続ける。**失敗を 0 にしてループを止めることはしない**（失敗を成功として報告する方が悪い）。v0.4 の当初の記述はこの key の意味を逆に書いていた
  - **cold start の代償:** 最初の接続が helper を起動するので、その接続が `dscl` のグループ確認とバイナリのコピーを待つ。CLI のハンドシェイクのタイムアウト内に収まるが、起動直後の最初の `tetherd` が 2 回目より遅いのはこれ
- helper は起動のたびに残留アンカーと resolver ファイルを掃除してから listen する。**ただしアクティベーションでは `/var/run/tetherd-helper.lock` の `flock` で守る必要がある。** 掃除は**マシン全体**に効くのに helper の識別は**ソケット単位**なので、常駐なら 1 回で済んでいたものが頻繁に走る。具体的な事故: 前面起動の `hack/e2e-local.sh` のセッションが生きている最中に `tetherd doctor` を打つと daemon が cold start し、そのセッションの pf アンカーを flush し `/etc/resolver` のファイルを消し `169.254.170.2` の pin を落とす — 黙って、しかも `savePins` が記録を切り詰めるので戻せない。helper はプロセスの生存期間だけ共有 `flock` を保持し、**排他で取れたときだけ掃除する**。帰結として、daemon が上がっている間に前面起動した helper は自分の掃除を飛ばす
- 署名・公証は v1 ではしない。GitHub Releases からの直接ダウンロードは非サポートと明記
- アンインストール: `sudo tetherd-helper uninstall`（`launchctl bootout`、plist、グループ、`/usr/local/libexec/tetherd` の削除。この 4 つを 1 コマンドで行い、途中が失敗しても残りを試す）→ `brew uninstall --cask tetherd`。**グループを消すのは仕様どおり**で、消さないと初回インストールの検証ができない（`EnsureGroup` が既存の gid を返してグループ作成の経路を通らない）
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
3. **AWS なし・root ありの macOS e2e**（`hack/e2e-local.sh`）: `docker compose` で agent + postgres + sampleapp を Mac から直接届かない Docker ネットワーク（`192.0.2.0/24`）に立て、agent の `:9900` だけ公開。`tetherd run --transport direct --agent-addr 127.0.0.1:9900 --remote-cidr 192.0.2.0/24 -- psql -h 192.0.2.10` が通れば pf / gid / rdr / natlook / yamux / dial が本物で検証できる。bash / zsh / Go / Node の子プロセスからそれぞれ確認する
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
- helper は `admin` グループのユーザーからのみ受け付け、操作は 7 つに固定。コマンド起動の操作は無い
- setgid `tetherd` の権限は pf に捕まることだけ
- **ソケットアクティベーションの代償: `/var/run/tetherd.sock` は `0666` で、helper が居なくても存在する。**だから**同一マシンの任意のプロセスが接続するだけで root の helper を起動させられる**。`0666` は必須（`tetherd` を実行する開発者が書き込める必要がある）で、launchd がソケットを持つ設計に内在する。**得られるのはプロセスであって操作ではない** — helper は各リクエストを呼び出し元の識別で認可し（`AllowAdmin`、操作は 7 つに固定）、認可したセッションのためにしか pf と `/etc/resolver` を触らない。**常駐をやめた代わりに「起動させられる root」を受け入れた**という交換であることを明示する。README の信頼境界にも書く
- `:9900` は無認証だが lo にしか bind せず、信頼境界は「タスク内」。design.md に明記する
- `.tetherd.yml` は**信頼された入力**として扱う。`env.override` は子プロセスの `PATH` や `DYLD_INSERT_LIBRARIES` も設定できるので、悪意ある `.tetherd.yml` を含むリポジトリで `tetherd run` すれば任意コード実行になる。ただし `tetherd run -- go run ./cmd/api` はそもそもそのリポジトリのコードを実行するので、これは `env.override` があること自体に内在する性質であり tetherd が新たに作った経路ではない。「信頼していないリポジトリのコードを実行しない」という通常の前提がそのまま当てはまる
- タスクロールを配るループバック口（`127.0.0.1:<空きポート>`）は無認証。同じマシンの他のローカルプロセスが叩けばタスクロールの認証情報を得られる。信頼境界は既存の SSM ローカルフォワード（`127.0.0.1:9900`）と同じ「同一マシンに閉じるが、それ自体が境界」。ポートは毎回変わり広告もされないが、秘密ではない。`hello` にユーザー単位のトークンを載せる v0.3 でも、この口は別経路なので閉じない
- `network.pin_credential_route: true` は**マシン全体**に効く host route を張る。セッション中は `tetherd` グループ以外のプロセスも `169.254.170.2` で dev タスクの認証情報に到達する（pf の `rdr` は `group` 句を受け付けないため gid で絞れない）。`amazon-ecs-local-container-endpoints` のようにこのアドレスをローカルで使うツールと衝突し、そちらが**タスクの**ロールを掴む。既定で無効。有効時に `doctor` が警告する行は v0.3b で追加する（v0.3a には無い）
- ローカルアプリは共有 dev DB に書く。ローカルブランチの auto-migrate が dev DB を変えうることを README で注意する
- セッション中は `tetherd-exec` が誰でも実行可能（mode `2755`、setgid `tetherd`）なので、同じマシンの他のローカルユーザーも gid `tetherd` でコマンドを起動しトンネルに到達できる。シングルユーザーのラップトップでは許容するが、その前提であることを明記する
- セッション中、ラップトップ上の SSM ローカルフォワードのポート（`127.0.0.1:9900`）は無認証で agent に届く経路になる: 同じマシンの他のローカルプロセスがそのポートに直接繋いで `welcome.app_env` を読んだり、VPC 内へ dial したりできる。lo にしか bind しないので同一マシンには閉じるが、それ自体が信頼境界。`hello` にユーザー単位のトークンを載せる（v0.3）までこの経路が開いていることを明記する

---

## 12. 先に潰す検証（順序つき）

### v0.4 の実機検証（2026-09-13）

**配布経路が他人の Mac で成立することを確認した。**`docs/e2e-aws.md` の 32〜38 行、リリース v0.4.0 と v0.4.1 に対して実施。

ローカルでは原理的に確かめられなかった 2 点が、どちらも成立した:

- **launchd はこの plist を受理して root でデーモンを起動する。** `ps` で uid 0 / ppid 1 / `/usr/local/libexec/tetherd/tetherd-helper` から走行。`plutil -p` で 7 キーすべて意図どおり（`ProgramArguments` の 4 パスが全て root 所有配下、`KeepAlive {SuccessfulExit: false}`、`RunAtLoad true`、`ThrottleInterval 10`）。**Homebrew の prefix を指していない** — 権限昇格を防ぐ設計が実機で成立
- **`EvalSymlinks` の前提が成立する。** `/opt/homebrew/bin/tetherd-helper` → `Caskroom/tetherd/<ver>/tetherd-helper` で、`tetherd-exec` が同じディレクトリに実在した

さらに: `doctor` は **14 行すべて `✓` で exit 0**、`tetherd run -- aws sts get-caller-identity` が子プロセスにタスクロールを渡し（helper ログに `pf enabled → applied → cleared` の全ライフサイクル、終了後 `/etc/resolver` は空）、`sudo tetherd-helper install` は冪等で daemon を入れ替える、`brew uninstall --cask` は daemon を落とす。quarantine は `postflight` で除去されている。

**この検証が見つけた欠陥 2 件:**

1. **v0.4.0 は `brew install` できなかった**（v0.4.1 で修正）。cask が `depends_on formula: ["session-manager-plugin"]` を宣言していたが、それは cask であって formula ではない。**計画に「formula として実在する（`brew info` で確認済み）」と書いたのが誤り** — `brew info` は formula と cask を横断して解決するので、主張した区別ができないコマンドで確認していた。正解は `internal/doctor/checks.go` に v0.2b から `--cask` として存在していた。依存は `cask:` に直すのではなく**宣言をやめた**: `depends_on` は「brew が入れたか」を問うが、必要なのは「PATH にあるか」で、AWS 公式インストーラは Homebrew に見えない場所に置く
2. **`postflight` は Homebrew 6.x で deprecated**（`Warning: Calling postflight is deprecated! Use postflight_steps instead.`）。GoReleaser の cask テンプレートが生成するので設定から変えられない。**現状は機能しているが、削除されたら quarantine の除去が黙って止まる — cask を選んだ唯一の理由なので、v1.0 の最優先項目**

設計の妥当性を左右するものから。1〜4 は AWS 不要。

1. `tetherd-exec`（`setregid`）+ pf `group` + `rdr` で、bash / zsh / Go / Node の子プロセスの TCP が捕まり、他プロセスは捕まらないこと（9.3 の第 3 層）
2. `DIOCNATLOOK` が macOS 26 で期待どおり元の宛先を返すこと（構造体レイアウト）
3. `DIOCCHANGERULE` でメインルールセットにアンカー参照を挿入できること。無理なら dump/reload に切り替える
4. `/etc/resolver/<domain>` + `port` が Go / Node（`dns.lookup`）/ JVM の `getaddrinfo` で効くこと
5. Fargate で `pidMode: task` + `SYS_PTRACE` + ECS Exec（ssm-agent 注入）が共存し、agent が app の environ を読めること
6. `ssm:StartSession` + `AWS-StartPortForwardingSession` で `ecs:` ターゲットの `127.0.0.1:9900` に届くこと、フロー確立の所要時間と RTT

---

### 検証結果（2026-09-12、macOS 26.6.2 / Apple Silicon、`make e2e-local`）

1〜3 は **通った**（`passed=7 failed=0`、全チェックで `from 192.0.2.10`）。

| # | 結果 |
|---|---|
| 1 | `tetherd-exec`（`setregid`）+ pf `group` + `rdr` で curl / bash / zsh / `go run` / node の子プロセスの TCP が捕まり、agent 経由で届いた。bash の子でも捕まるので real gid の変更が効いている。グループ `tetherd` は gid 309 で自動作成 |
| 2 | `DIOCNATLOOK`（84 バイト、`0xC0544417`）が元の宛先を正しく返した |
| 3 | `DIOCCHANGERULE` は不要。既定 `/etc/pf.conf` の `com.apple/*` に子アンカー `com.apple/900.tetherd` で乗り、セッションごとの `pfctl -E`/`-X` と `-F rules/nat/Tables` で終了後のアンカーは空 |
| 4 | 未検証（`remote_domains` を使う v0.2 で） |
| 5 | **通った**。`pidMode: task` + agent への `SYS_PTRACE` + ECS Exec（ssm-agent 注入）は同じタスク定義で共存し、agent が app コンテナの `/proc/<pid>/environ` から 19 個の env を読めた。Secrets Manager 由来の `DB_PASSWORD`（24 文字）と SSM Parameter Store 由来の `FEATURE_FLAG` が解決済みの値で入っていた |
| 6 | **通った**。`ssm:StartSession` + `AWS-StartPortForwardingSession` で `127.0.0.1:9900` に届く。ただしターゲットは **agent コンテナの runtimeId では駄目**だった（§6.1 参照）。所要時間は `StartSession` から `welcome` まで 2〜4 秒、確立後のフロー 1 本あたりの往復は RDS のクエリで体感できないレベル |

### AWS 検証結果（2026-09-12、`deploy/dev-env` + `docs/e2e-aws.md`）

実機の Fargate タスク（ap-northeast-1、ARM64、`pidMode: task`）に対して macOS 26.6.2 から実行。

| 検証 | 結果 |
|---|---|
| env 注入 | ✅ タスクの env 19 個。`DB_PASSWORD`（Secrets Manager）と `FEATURE_FLAG`（Parameter Store）が解決済みで届く。`PATH` などはローカルのまま |
| VPC 内への透過アクセス | ✅ ローカルの Go プロセスが `DB_HOST` の RDS に接続して `SELECT now()` を返した（pf rdr → `DIOCNATLOOK` → SSM → agent → RDS） |
| タスクのプライベート IP への到達 | ✅ `curl http://10.0.11.229:8081/` が通り、タスク側は `from 10.0.11.229`（自分の ENI）と認識した |
| タスクロール | ✅ 子プロセスの `aws sts get-caller-identity` が `assumed-role/tetherd-dev-api-task/…`。ただし §6.4 の対策（共有設定を隠す）が必要だった |
| VPC 外の AWS サービス | ✅ `aws s3 ls` がタスクロールで成功（署名ベースなのでラップトップの回線から出る） |
| gid のスコープ | ✅ `go run` がビルドして起動したバイナリも gid `tetherd`（309）。孫・ひ孫まで継承される |
| 環境ガード | ✅ `--env prod` で dev のタスクに繋ごうとすると `refusing to attach: agent reports TETHERD_ENV="dev", expected "prod"` で exit 1 |
| 1 台 1 セッション | ✅ 2 つ目の `run` が `another tetherd session is active (pid …, since …)` で exit 1 |
| セッション断 | ✅ トランスポートを殺すと即座に `✗ agent session lost: control stream closed: EOF`、子プロセスを停止して exit 1 |

実機でしか出なかった問題（すべて修正済み。詳細は §6.1、§6.4）:

1. `ssm:StartSession` のターゲットに agent コンテナの runtimeId を使うと `TargetNotConnected`。distroless の agent コンテナでは ECS Exec の SSM エージェントが接続できていないのに、`DescribeTasks` は `RUNNING` と報告する。awsvpc は netns を共有するので、同じタスクの別コンテナ経由で転送すれば `127.0.0.1:9900` に届く
2. 開発者の `~/.aws/config` の `default` プロファイルがコンテナクレデンシャルより先に評価され、タスクロールを覆い隠す
3. distroless イメージの `SSL_CERT_FILE`（コンテナ内のパス）が注入され、macOS 側の子プロセスの TLS が全部壊れる

### v0.2b（設定ファイル・DNS・`env`・`doctor`）

検証手順は `docs/e2e-aws.md`（20 行）。helper（sudo）が要らない行と要る行を分けてあり、結果は実施後にここに記録する。

| 行 | 検証 | 結果 |
|---|---|---|
| 0 | `.tetherd.yml` だけでフラグ無しに動く | ✅ `config` 行に読んだパスが出て、`tetherd-dev/api` のタスクに接続、19 変数注入、子プロセスが `PORT=8081` と secret を受け取る。フラグはゼロ |
| 7 | 環境ガード | ✅ `tetherd env --env prod` が `refusing to attach: agent reports TETHERD_ENV="dev", expected "prod"` で exit 1 |
| 11 | `tetherd env` の既定マスク | ✅ `DB_PASSWORD=***` / `FEATURE_FLAG=***`（タスク定義の `secrets` 2 件）、`PORT=8081` は素のまま |
| 12 | `--format json` と `--reveal` | ✅ JSON として妥当、`--reveal` で 24 文字の実値 |
| 13 | stdout と stderr の分離 | ✅ `eval "$(tetherd env --format shell)"` が成功し `PORT=8081`。ステータス行は 4 行すべて stderr |
| — | secret が stdout / stderr に漏れない | ✅ 実値 24 文字で grep して両方とも不在 |
| 1 | 捕捉ありの env 注入 | ✅ 19 変数、`✓ network` にリモート集合と DNS 行、`✓ iam` にタスクロール（`assumed-role/tetherd-dev-api-task/…`） |
| 2 | VPC 内の RDS に pf → SSM → agent で届く | ✅ `10.0.10.164:5432` に接続して Postgres の SSL 応答 `S` |
| 6 | `remote_domains` で Cloud Map の名前が解け、`/etc/resolver` が run 中だけ存在する | ✅ `dig @127.0.0.1 -p 53530 api.myapp.internal` → `10.0.11.30`、`getaddrinfo` も同じ、`curl http://api.myapp.internal:8081/` → `sampleapp on ip-10-0-11-30… from 10.0.11.30`（= 解決した IP も捕捉されて agent 経由で届いている）。run 中だけ `/etc/resolver/myapp.internal` が存在し、中身は `# managed by tetherd` / `nameserver 127.0.0.1` / `port 53530`、終了後に消える |
| 8 | 1 台 1 セッション | ✅ 2 つ目が `rejected by agent (duplicate_user)` |
| 14 | `tetherd doctor` の 10 項目 | ✅ helper・setgid・plugin・AWS 認証・タスク・pidMode・agent セッション・捕捉範囲・ローカルアドレスが緑。`remote domains` の行は偽陰性が見つかり修正（下記） |
| 19 | 存在しない名前が NXDOMAIN として即座に返る | ✅ `dns nope.myapp.internal: not found` が出て `gaierror` が 0.06 秒で返る（SERVFAIL のリトライ待ちが無い） |
| 15 | doctor が 1 つ失敗しても残りを続ける | ✅ `service: nope` にすると `✗ attachable task` に `ServiceNotFoundException` が出て exit 1、残る 6 行は `!` で「not checked: …」と理由付きで出続ける |
| 16 | `remote_services: [s3]` の prefix list が入る | ✅ `remote_services s3 → 15 prefixes` と出て、`✓ network` 行に `3.5.152.0/21` `52.219.0.0/20` などが並ぶ。`ap-northeast-1` の S3 は 15 件（当初「数百件」と書いていたのは誤り。API は 1 ページ 100 件で切るのでページングは依然必要で、`--max-results 5` を指定すると実際に `NextToken` が返る） |
| 17 | `local_cidrs` の分割引き算 | ✅ `local_cidrs: [10.0.5.0/24]` で `10.0.0.0/16` が `10.0.0.0/22, 10.0.4.0/24, 10.0.6.0/23, 10.0.8.0/21, 10.0.16.0/20, 10.0.32.0/19, 10.0.64.0/18, 10.0.128.0/17` の 8 本になる（ユニットテストの property 検証と完全に一致） |
| 18 | 全部消したときのエラー | ✅ `local_cidrs: [10.0.0.0/8, 169.254.0.0/16]` で `✗ network.local_cidrs excludes the entire remote set; nothing would be captured`、exit 1 |
| 11-13 | `tetherd env` | ✅（上記） |

検証のために agent イメージを再ビルド・再デプロイした（`make push-images` + `aws ecs update-service --force-new-deployment`、linux/amd64 + linux/arm64）。再デプロイ前は `✗ remote domains: this agent does not support name resolution; upgrade the sidecar` と出ており、今夜追加したバージョン不一致メッセージが実機で正しく機能することの確認にもなった。

実機で見つかった問題:

1. **`doctor` の `remote domains` が健全な環境で偽陰性を出す**（修正済み）。設定されたドメインそのものを名前として解決していたが、Cloud Map の名前空間は apex に A レコードを持たないので `name not found` になる。検査の本当の問いは「このドメインの問い合わせが VPC リゾルバに届くか」であり、**not found という応答自体が到達の証明**。存在しないことが保証された名前を引いて、not found を成功として扱う形に変更。
2. **macOS の負の DNS キャッシュ**。agent が resolve に対応する前に引いた名前は `mDNSResponder` に NXDOMAIN としてキャッシュされ、TTL の間 `getaddrinfo` が失敗し続ける（`dig` で直接引くと正しく答える）。`sudo dscacheutil -flushcache; sudo killall -HUP mDNSResponder` で解消。docs/config.md に記載。
3. **`169.254.170.2` のルートが pf より先に評価される**（v0.3a で設計変更により解消）。macOS がこのアドレスへの ARP に失敗して en0 上に拒否ルート（`UHLSW` + `LLINFO`、`netstat` の `!`）を残すため、`connect()` のルート探索が pf の `pass out route-to lo0` より先に `EHOSTUNREACH` を返すことがある。同じ子プロセス・同じ gid 309 で、curl と system python 3.9 は 200 を得るのに AWS CLI 2.34.49 が同梱する Homebrew python 3.14 は `Errno 65` で失敗し、同じ interpreter でも VPC 宛（RFC1918）は通る。ARP エントリの期限で成否が変わるので間欠的。当初は「セッション中だけ `169.254.170.2` の host route を lo0 に向ける」で直すつもりだったが、その固定はマシン全体に効き pf の `rdr` では gid で絞れないと分かったため、v0.3a では**このアドレスを使わない**方式（ループバック口 + env 書き換え、§4.2）に変えた。host route の固定は `pin_credential_route` として残してある。

実機で 1 件見つかった（修正済み）:

`tetherd env` がタスクの env を**そのまま**出していたため、`eval "$(tetherd env --format shell)"` が開発者のシェルを壊した。実測で `HOME` が `/home/nonroot`、`PATH` がコンテナの `PATH` に置き換わり、v0.2a で Go の子プロセスの TLS を全部壊した `SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt` もそのまま出ていた。`tetherd run` は同じ変数を除外して注入しているので、**`env` と `run` が違う env を作っていた** — `env` の存在理由（run が注入するものを見る・シェルに取り込む）に反する。`env` も §6.4 の除外・上書きを通すように修正。

### v0.3a（ルート固定と steal）

配線: ALB のターゲットグループを app の `:8081` から agent の `:8080` に移した（`deploy/dev-env/alb.tf` / `ecs.tf` / `rds.tf` の sg-app）。agent は `TETHERD_APP_ADDR=127.0.0.1:8081` で app へリバースプロキシし（§5.1）、`X-Dev-User` / `X-Dev-Token` が一致するリクエストだけを steal してラップトップへ転送する（§5.2）。ヘルスチェックは `/healthz`・matcher `200` のまま。agent はヘルスチェックを特別扱いしない（一致ヘッダーを持たないリクエストが app に行くだけ）ので、パスを変える必要は無かった。`healthy_threshold` だけ 3 → 2 に下げた（ターゲットグループ置き換え中の窓を短くするため）。

検証手順は `docs/e2e-aws.md` の 20〜26 行。**20〜26 すべて成功**（2026-09-12、ap-northeast-1、ALB `tetherd-dev-1301575947`）。

| # | 結果 |
|---|---|
| 20 | ラップトップが応答。CLI に `← GET /hello 200 1ms (from 124.35.91.195)`。転送先に `X-Forwarded-For: 124.35.91.195`、`X-Forwarded-Proto: http`、`Host` は ALB のホスト名のまま届いた |
| 21 | タスクの応答。`from 127.0.0.1:57164` — app が見る接続元が同一タスク内の agent になっており、経路が ALB → agent → app であることの証拠 |
| 22 | トークンを 1 文字変えた場合・トークン無しの場合ともタスクの応答。**ラップトップ側の受信数は 0**（応答の出どころだけでなく受信側でも確認した） |
| 23 | セッション断の直後に一致ヘッダーで叩いて 200・タスクの応答。502 ではない |
| 24 | タスクの応答 + CLI に `✗ steal  GET /row24  502  nothing is listening on 127.0.0.1:9321`。クライアントのレスポンスヘッダーに `X-Tetherd-No-Listener` は**出ない** |
| 25 | `--no-incoming` 中は一致ヘッダーでもタスクの応答。`✓ steal` 行は出ず、ラップトップ側の受信数は 0 |
| 26 | 20 秒間隔 9 サンプル（約 3 分）すべて `healthy` |

**手順に罠が 1 つあった。** 既定の `local_port: 8080` は現実の開発機で埋まっている。この Mac では無関係な Docker コンテナ（nginx）が `*:8080` を IPv6 で保持していて、ローカルサーバ（IPv4 の `127.0.0.1:8080`）と共存していた。そのため**ローカルサーバを落としても 8080 は 200 を返し続け**、24 行をそのまま実行すればダイヤルは nginx に成功して「何も listen していない」経路を一度も通らずに合格していた。24 行は `--local-port` で確実に空いているポート（9321）に移して実施した。24 行を再実施する者は、まずそのポートが**接続拒否を返すこと**を確認すること。

あわせて確認できたこと（v0.3a の設計変更の本体）:

```
✓ endpoint 127.0.0.1:58066 → the task's credential and metadata endpoint
           (the child is pointed here; 169.254.170.2 is never dialed)
✓ iam      arn:aws:sts::…:assumed-role/tetherd-dev-api-task/…  (via 127.0.0.1:58066 → the task)
✓ network  transparent (pf rdr, gid tetherd) · remote: 10.0.0.0/16
```

`remote:` に `169.254.170.0/24` が入らないこと（既定ではループバック口から配るため）、そして §12 の既知の穴 3 番（ARP 失敗の拒否ルートで間欠的に壊れる）の原因アドレスに**もう誰も接続しない**ことが実機で確認できた。

**`terraform apply` で 1 回失敗した。** ターゲットグループの `port` 変更は置き換えを強制するが、リスナが転送先にしている間は削除できないため `ResourceInUse` になる。しかも失敗が綺麗ではなく、セキュリティグループの更新だけ先に適用済みで、ALB からのインバウンドが 8080 のみ許可・ターゲットグループはまだ 8081 をヘルスチェック、という状態で止まり、dev 環境が一時的に 5xx になった。`create_before_destroy` と `name_prefix`（ターゲットグループは 6 文字まで）で解決（`f099788`）。次に同種の置き換えを含む変更を当てる者は、plan の `# forces replacement` を見た時点でこれを疑うこと。

### v0.3b（全タスク接続・deploy 追従・`status`・`token rotate`・`doctor`）

**§6.2 / §6.3 は最初から「対象タスク全部に接続」と書いてあり、実装が追いついていなかった。** v0.3a までの `run` は最も古い 1 本にしか繋いでおらず、`desired_count` が 2 以上のサービスでは ALB がどのタスクに落とすかで steal が当たるか外れるかが決まっていた。v0.3b で実装が仕様に一致した（`internal/cli/sessionset.go` の `SessionSet` が primary と secondary を持ち、`internal/cli/follow.go` の `Follower` が 10 秒おきにタスク一覧を読み直す）。**§6.2 / §6.3 の本文は書き換えていない** — 仕様が正しく、コードが後から揃った側なので、記録はこの節に置く。

あわせて入ったもの: `tetherd status`（§6.5。`welcome` の `sessions` を読む。専用メッセージは足していない）、`tetherd token rotate`（§5.2。0600 を保ち、`user` / `aws` を残す）、`doctor` の `?` ステータスと `steal` / `task role` の行（§6.5）。

**Terraform の変更は無い。** `deploy/dev-env` は v0.3a のまま。検証のために `desired_count` を動かすだけで、当てるべき差分は 1 行も無い。

**`doctor` のターゲットグループの検査は v0.4 に送った。** 計画には入っていたが、計画自身の「依存追加なし・`go.mod` は 1 行も変えない」と両立しない: `protocol_version` を報告する API は `elasticloadbalancing:DescribeTargetGroups` だけで、それを呼ぶ SDK（`aws-sdk-go-v2/service/elasticloadbalancingv2`）は `go.mod` に無い。**`ecs:DescribeServices` はターゲットグループの ARN とポートは返すが、`protocol_version` は返さない**（agent 側にも分からない。ALB のプロトコルバージョンは「パースできないリクエストが来る」としてしか現れない）。片方だけ入れても意味が無いので、**判定も、そのための developer policy の権限追加も、v0.3b には入れていない** — 実装の無い権限を配るのは、この計画が 9 タスクかけて消してきた「どこかに書いてあるが実装が無い」そのものだから。

v0.4 でやること 3 つ（どれも独立）: (1) `aws-sdk-go-v2/service/elasticloadbalancingv2` を入れて `DescribeTargetGroups` を呼び、developer policy に権限を足す。(2) agent の既定プロキシポート（`internal/agent` の非公開定数 `defaultProxy` = `0.0.0.0:8080`）を `internal/doctor` から読める場所に出す。(3) タスク定義の agent コンテナの env から `TETHERD_PROXY` を読み、ポートの判定を `⚠` から本当の検査にする。検証手順は `docs/e2e-aws.md` の「v0.4 に持ち越した行」にある（31 行。**ターゲットグループを HTTP2 にすると dev 環境が一時的に壊れる**ので、実施は最後に回してすぐ戻すこと）。

**なぜポートを「検査」できないのか**（上の (3) の背景）: agent のプロキシポートは `TETHERD_PROXY` で動かせるのに、`doctor` にはそれを知る手段が無い。`welcome` は `Version` / `TaskARN` / `Env` / `AppEnv` / `EnvError` / `Others` / `Sessions` だけで、CLI が繋ぐのは**制御**ポートであってプロキシポートではない。だから「既定と違う」は `⚠`（質問）にしかならず、`✗`（判定）にはできない — 既定と違うポートを向けている配置は**正しく設定されている**。安いのはタスク定義から `TETHERD_PROXY` を読むこと（`awsProvider` は `SecretNames` / `PIDMode` で既にタスク定義を読んでいるので同じ呼び出し形）。もう一方は `welcome` にフィールドを 1 つ足すこと（追加のみなので互換は保てる）。

**検証には `desired_count` を 2 にする必要がある**（27・28 行）。ALB がタスクを選ぶ以上、1 タスクでは「どのタスクに落ちても届く」は検証できない。**検証が終わったら 1 に戻す** — Fargate の課金が倍になるため。

検証手順は `docs/e2e-aws.md` の 27〜30 行（31 行は上記のとおり v0.4）。**27〜30 行すべて成功**（2026-09-13、ap-northeast-1、ALB `tetherd-dev-1301575947`、`desired_count = 2`、agent イメージは v0.3b を再デプロイ）。

| 行 | 検証 | 結果 |
|---|---|---|
| 前提 | ALB が 2 タスクに振り分けている | ✅ ヘッダー無しで 6 回叩いて `ip-10-0-10-72` と `ip-10-0-11-104` に **3 対 3**。どちらも `from 127.0.0.1:` なので agent 経由 |
| 27 | `desired_count = 2` で、一致するリクエストが**どのタスクに落ちても**届く | ✅ 起動行が `2 tasks (100f020e… primary, e7cd9413…)`。20 回叩いて**ラップトップ受信 20/20**、CLI のログ行 20 本、**アプリに落ちたリクエスト 0**。v0.3a の CLI ならおよそ半分がアプリに届き、開発者側に痕跡は残らなかった |
| 28 | deploy 追従 | ✅ `run` 実行中に `force-new-deployment`。新タスク 2 本に接続（`↻ … attached (3 total)` / `(4 total)`）、**起動時の 2 本が両方消えても run は生存**、昇格が 2 回（`dial and DNS now go through …`）。deploy 後に 10 回叩いて **10/10 がラップトップに届いた** — 繋ぎ直しただけでなく、入れ替わったタスクで steal が実際に機能している |
| 29 | `tetherd status` | ✅ **自分の `run` が動いている最中に読めた**。タスクごとに `abe  from 127.0.0.1:…  attached 58s ago`。narrowing 前は `duplicate_user` で拒否され、まさに必要な瞬間に使えなかった（`env` と `doctor` も同様で、v0.2b から入っていた欠陥） |
| 30 | `tetherd token rotate` | ✅ トークンが変わり、`user` は残り、権限は `-rw-------`。**実行中のセッションは古いトークンのまま**を実機で確認 — 新しいトークンで叩くとアプリが応答し、ラップトップの受信数は動かない（31 のまま）。計画の当初の期待は逆で、間違っていた |
| 後片付け | セッション終了後に残留しない | ✅ `/etc/resolver/` は空、ワーキングツリーに残骸なし |

**28 行が全体レビューの F1 を裏付けた。** 起動時の 2 タスクが両方死んで run が生きるという経路は、`run.go` のフォロワー側 `loss.watch(s)` が無ければ `every task tetherd was attached to has gone away` で終わっていた。**コードは正しく、テストがそれを留めていなかった** — 実機がそれを示した。

| 行 | 検証 | 結果 |
|---|---|---|
| 27 | `desired_count = 2` で、一致するヘッダーのリクエストがどのタスクに落ちてもラップトップに届く | 未実施 |
| 28 | rolling deploy 中に `run` が生き続け、新しいタスクに繋ぎ、古いタスクが落ちても終わらない | 未実施 |
| 29 | `tetherd status` が各タスクの接続者を出し、トークンを送らない | 未実施 |
| 30 | `token rotate` の直後は走行中のセッションが古いトークンのまま steal し続ける | 未実施 |

---

### v0.4（配布とインストール）

**§8 を実装に合わせて 5 点直した**（このセクションの本文に反映済み）: tap 名と `brew install` の行、formula → cask、`install` が `tetherd-helper` 自身もコピーして転送先パスの所有者を検査すること、そして **helper が v0.4 では常駐すること**（`RunAtLoad: true`。ソケットアクティベーションは `purego` という新しい依存が要るので次に送った。**黙って乖離させず、spec 側に書いた**）。5 点目は所有者検査の対象に `/var/log` を足したこと（最終レビュー R2。plist が root に書かせる 3 つ目のパスで、`docs/install.md` は元から 3 つを同じ文で「検査は通る」と書いていたのに、ループは 2 つしか見ていなかった）。

設計と実測の詳細は `docs/install.md`（cask 全文、root 所有ディレクトリに入れる理由、`KeepAlive` の判断、quarantine と署名の扱い、ラベルが Go と cask の 2 箇所にあること）。

**検証は実機のインストールが必要なので `docs/e2e-aws.md` の v0.4 の行で行う**（`brew install` からの初回インストール、`EvalSymlinks`、`launchctl print`、`doctor`、冪等な再実行、`brew uninstall --cask`）。単体テストで確かめられるのは plist の内容・所有者検査・`install`/`uninstall` の冪等性と launchctl の呼び出し順までで、**launchd が実際にこの plist を受け付けて helper を起動することは実機でしか分からない**。

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
