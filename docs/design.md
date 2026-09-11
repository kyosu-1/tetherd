# tetherd — 設計書

サーバーレスコンテナ（ECS Fargate、後続で Cloud Run）のための mirrord 的ローカル開発環境。
ローカルのプロセスを、開発環境の ECS タスクの「中で動いているかのように」振る舞わせる CLI とサイドカー。

- 実装: Go / 単一バイナリ（CLI）+ distroless イメージ（agent）
- v1 対象: **macOS** × ALB + ECS Fargate + RDS。詳細は [specs/2026-09-12-v1-macos-design.md](specs/2026-09-12-v1-macos-design.md)
- 次: Linux（捕まえる層の追加）、Cloud Run（プロバイダとして追加）
- 名前: `tetherd`（テザード）。tether = テザリング。タスクのネットワークと身元をラップトップに分けてもらう

---

## 1. 一文で言うと

```
$ tetherd run -- go run ./cmd/api
```

これ一発で、ローカルの `go run ./cmd/api` が開発環境の `api` タスクの一部として振る舞う。

- 環境変数とシークレットは、実行中の app コンテナから読んだ本物
- そのプロセスが VPC 内に出す通信は、DNS も含めて、タスクの ENI から出ていく
- AWS SDK はタスクロールとして振る舞う（VPC 外の S3 や DynamoDB に対しても）
- ALB に届いたリクエストのうち `X-Dev-User: shota` とそのトークンが付いたものだけがラップトップの `:8080` に流れてくる

> mirrord は syscall をフックして「プロセスの目を騙す」。tetherd はプロセスには何も仕込まず、**カーネルのネットワーク層で捕まえる**。v1（macOS）は pf で捕まえてカーネルの TCP でそのまま終端し、Linux は netns + TUN で捕まえる予定。どちらもランタイム（Go の静的バイナリ、JVM、Node、macOS の SIP）に一切依存しない。捨てるのはファイルシステムの透過だけ。

---

## 2. やること・やらないこと

### やること

- app コンテナの **実行中の環境変数**（secrets 解決済み）を agent が読み取り、ローカルプロセスに注入する。開発者に Secrets Manager の権限は不要
- 子プロセス（と子孫）の VPC 内宛ての TCP をカーネル層で捕まえ、タスクの ENI から出す（透過 outgoing）。DNS は VPC 内ドメインだけ agent 側で解決する
- タスクロールの認証情報エンドポイント（169.254.170.2）もそのまま通し、SDK をタスクロールとして動かす
- ALB → サイドカー経由で、ユーザー名 + トークンが一致する HTTP リクエストを手元に引き込む（steal）
- 以上を 1 本の制御チャネル（ECS Exec / SSM）に多重化する。タスクが複数あれば全部に繋ぐ

### やらないこと

- syscall フック（LD_PRELOAD / DYLD 系）。ランタイムごとの地雷を踏み続ける宿命があるため
- ファイルシステムの透過アクセス
- 本番環境への適用（設定とサイドカーの二重ガードで拒否）
- 動いているタスクへの「後から差し込む」注入。サイドカーはタスク定義に最初から含める
- HTTP 以外のプロトコルの steal（Fargate では NET_ADMIN / NET_RAW が取れない）
- dev タスクを踏み台にしたインターネットへの egress（既定では VPC 内だけをリモートに回す）

### v2 以降に送るもの

mirror（共有 DB への二重書き込みの扱いを決めてから）、UDP / IPv6、`remote_localhost`（タスク側 localhost への到達）、env-rewrite モード、`init`、WebSocket / gRPC の steal、同一マシンでの複数セッション、Linux、Cloud Run。

---

## 3. 全体構成

```
 ┌──────────────┐  ① X-Dev-User + Token  ┌──────────────────────────────────────────┐
 │  ALB (dev)   │ ─────────────────────▶ │ ECS Task · dev/api · awsvpc              │
 └──────────────┘                        │  ┌────────────────┐      ┌────────────┐ │
                                         │  │ tetherd-agent  │ 通常時│ app        │ │
                                         │  │ :8080 L7 proxy │ ────▶│ :8081      │ │
                                         │  │ :9900 control  │      └────────────┘ │
                                         │  │  (127.0.0.1)   │                     │
                                         │  └───┬───────┬────┘                     │
                                         │      │       │ ⑤ forward: タスク IP から dial
                                         │  [ssm-agent] │                          │
                                         └──────┼───────┼──────────────────────────┘
                                                │       └──▶ RDS / ElastiCache / Cloud Map / 169.254.170.2
                                     ③ outbound 443
                                                ▼
                                         ┌──────────────┐
                                         │  AWS SSM     │  中継 · IAM 認証
                                         └──────┬───────┘
                                                │ ③′ start-session · 1 本の TCP を yamux で多重化
                                                ▼
 ┌──────────────────────────────────────────────────────────────┐
 │ Laptop                                                       │
 │  tetherd CLI ─ env 注入 / 透過プロキシ(VPC 宛 TCP → agent)    │
 │               / DNS(VPC 内ドメイン → agent) / steal → :8080   │
 │  tetherd-helper (root) ─ pf ルール · /etc/resolver · natlook  │
 │  go run ./cmd/api :8080  ← gid tetherd でスコープ             │
 └──────────────────────────────────────────────────────────────┘
```

部品はラップトップに 3 つ、タスクに 1 つ。

### 3.1 tetherd-agent（サイドカー）

- Go 製の単一バイナリ、distroless イメージ（`ghcr.io/kyosu-1/tetherd-agent`、arm64 / amd64）。タスク定義にコンテナを 1 つ足す
- awsvpc モードではタスク内の全コンテナがネットワーク名前空間を共有するため、mirrord のエージェントと同じ立ち位置（同じ ENI、同じ SG、同じ IP）に立てる
- `:8080` で ALB からのトラフィックを受け、常に HTTP/1.1 リバースプロキシとして動く。ALB は接続を keep-alive で使い回すので振り分けはリクエスト単位。セッションが無ければ全リクエストが `:8081` の app へ
- セッション中だけルーティングテーブルを持ち、ユーザー名とトークンが一致したリクエストを該当ユーザーのラップトップへ流す
- `:9900` は制御ポート。`127.0.0.1` のみ bind（SSM フォワードは同一ネットワーク名前空間から来る）
- `TETHERD_ENV` が無ければ起動を拒否する
- 既存のヘルスチェックパスはそのまま透過（ヘッダーが無いので常に app へ）。WebSocket の upgrade も app へ素通し
- セッションが切れたら即座に素通しに戻す（リクエスト途中のものは完了まで待つ）
- AWS API は呼ばない。Linux capability は env 読み取りのための `SYS_PTRACE` のみ。root で動かす（capability を effective にするため）
- 常時データパスにいるので `essential: true` と `restartPolicy` を推奨
- listen ポート（8080 / 9900）は env で変更可

### 3.2 環境変数と secrets の取得 — 実行中のプロセスから読む

タスク定義を読んで Secrets Manager / SSM から自分で引く方式は取らない。mirrord が対象コンテナの `/proc/<pid>/environ` を読むのと同じく、**agent が app コンテナの実行中プロセスの環境変数を読み取って** `welcome` で CLI に渡す。ECS が起動時に解決した secrets がそのまま入っているので、開発者側に Secrets Manager / SSM / KMS の権限は一切要らない。信頼境界が ECS Exec と同じ（タスクに入れる人は env を見られる）に揃う。

Fargate でこれを可能にするための設定が 2 つ:

- タスク定義で `pidMode: task`（コンテナ間で PID 名前空間を共有。Fargate platform 1.4+）
- agent コンテナの `linuxParameters.capabilities.add: ["SYS_PTRACE"]`（他ユーザーのプロセスの environ を読むのに必要。Fargate で追加を許されている唯一の capability）

app コンテナの特定: タスクメタデータエンドポイント（`ECS_CONTAINER_METADATA_URI_V4/task`）で app コンテナの ID を得て、`/proc/*/environ` の中から `ECS_CONTAINER_METADATA_URI_V4` がその ID を指しているプロセスを探し、その中で最も古い（= コンテナの init）プロセスの environ を採用する。entrypoint がシェルスクリプトでも「ECS が注入した env」が取れる。環境変数の値はプロセス起動時点のものになるが、これはコンテナの env の性質そのもの。`pidMode: task` が無ければ `welcome` でその旨を返し、CLI が修正方法を案内する。

### 3.3 tetherd CLI とヘルパー（ラップトップ）

- 依存は session-manager-plugin と、`brew install` 時に一度だけ入る特権ヘルパー（macOS、§6）。AWS CLI は不要（`ssm:StartSession` は SDK で呼ぶ）
- `run` 一発で、タスク選択 → SSM ポートフォワード → yamux セッション（ここで agent から app の env を受け取る）→ ルーティング集合の算出 → ヘルパーに pf ルール投入 → env を注入して子プロセスを起動、までを行う
- 子プロセスの終了か Ctrl-C で pf ルールを片付け、agent に `bye` を送り、素通しに戻す。CLI が異常終了してもヘルパーが掃除する
- 「どうパケットを捕まえるか」（§6）は `Capturer` インターフェースの裏に閉じ、「(元の宛先, 接続) を渡す」と定義する。v1 の macOS 実装は pf rdr でカーネルの TCP に終端させる。Linux で netns + TUN を足すときはユーザー空間スタック（gVisor netstack）で同じインターフェースに変換する

---

## 4. ネットワーク構成

いちばん大事な性質は、**ラップトップが VPC に一切入らない**こと。VPN もリレーも ALB のルール追加も要らず、すべて SSM を中継点にした「双方向アウトバウンド」で成立する。

```
 Internet ── ① HTTPS (X-Dev-User + Token) ──▶ ALB · sg-alb (in 443 ← 0/0)     [public subnet]
                                              │ ② :8080 (target group を 8081 → 8080 に)
                                              ▼
   ┌──────────────────────────────────────────────────────────────┐ [private subnet]
   │ ECS Task · sg-app   in 8080 ← sg-alb / out 5432 → sg-rds, 443 → endpoints
   │   agent :8080 / :9900(lo only)   app :8081   ssm-agent        │
   │        ⑤ 5432 ──▶ RDS · sg-rds (in 5432 ← sg-app · 既存のまま) │
   │   ssm-agent ── ③ out 443 ──▶ VPC Interface Endpoints          │
   │                              (ssmmessages · ssm · ec2messages)│
   └──────────────────────────────────────────────────────────────┘
                                              ▲
   Laptop ── ③′ 443 ──▶ SSM (public) ─────────┘  SSM が ③ と ③′ を突き合わせる
   ④ ssm-agent はタスク内で 127.0.0.1:9900 に終端 → ENI からは到達不能
```

1. **ユーザー → ALB。** 従来どおり HTTPS。ヘッダーが付いているかどうかだけが違い、ALB はヘッダーを見ない
2. **ALB → タスク :8080。** ターゲットグループのポートを app の 8081 から agent の 8080 に変える。sg-app のインバウンドも 8080 に
3. **ssm-agent → SSM。** ECS Exec を有効にすると Fargate が ssm-agent をタスクに差し込み、ssmmessages / ssm / ec2messages に 443 でアウトバウンド接続する。プライベートサブネットなので VPC Endpoint 3 つか NAT が必要（既に ECS Exec を使っていれば追加なし）。ラップトップ側（③′）も SSM に 443 で繋ぎ、SSM が両者を中継する。VPC にインバウンドの穴は開かない
4. **ssm-agent → agent :9900。** ポートフォワードはタスクのネットワーク名前空間内で `127.0.0.1:9900` に終端する。agent は制御ポートを lo にしか bind しないので ENI からは到達できず、SG の変更も不要
5. **agent → RDS（や任意の VPC 内宛先）。** 子プロセスが出した通信は agent がタスクの ENI からそのまま dial する。送信元はタスクの IP、適用されるのは sg-app、経路はタスクのルートテーブル。VPC から見るとタスク本体の通信と区別がつかない

SG の変更は sg-app のインバウンド 8081 → 8080 の 1 点だけ。RDS、ALB、エンドポイントの SG はそのまま。

---

## 5. 使い方

サブコマンドは `run` が主役。ほかはトラブルシュートと導入補助に絞り、それ以上増やさない。

```
$ tetherd run -- go run ./cmd/api
tetherd  dev/api  2 tasks (3f9c… primary, a17e…)  (started 12m ago)
  ✓ env      41 vars, 6 secrets resolved
  ✓ network  transparent (pf rdr, gid tetherd) · remote: 10.0.0.0/16, 169.254.170.0/24 · DNS: local (+ myapp.internal via VPC)
  ✓ iam      arn:aws:sts::…:assumed-role/myapp-dev-api-task/…  (via 169.254.170.2)
  ✓ steal    X-Dev-User: shota (+ X-Dev-Token)  → localhost:8080
  ▶ go run ./cmd/api
2026/09/11 10:12:03 listening on :8080
2026/09/11 10:12:03 connected to postgres myapp-dev…rds.amazonaws.com:5432
  ...
tetherd  ← GET  /api/orders/123   200   84ms   (from 203.0.113.5)
tetherd  ← POST /api/orders       500  1.2s   ← local error
```

- tetherd 自身のログは stderr に `tetherd` プレフィックス付きで出し、子プロセスの stdout / stderr はそのまま流す。子プロセスのログを汚さない
- steal したリクエストは 1 行ずつ出し、`--quiet` で消せる

```
tetherd run [flags] -- <command...>
  -s, --service    対象サービス（複数サービスのリポジトリ用）
      --as NAME    X-Dev-User の値を上書き（他人の代わりにデバッグ）
      --task ID    タスクを明示（既定は RUNNING な全タスク）
      --local-port N
      --no-incoming / --no-network / --no-env
  -q, --quiet

tetherd env [--format dotenv|json|shell] [--reveal]
    解決済み env を出力。eval "$(tetherd env --format shell)" で既存の起動に混ぜる。既定は secrets をマスク。
tetherd status
    今 dev/api の各タスクの agent に誰が繋いでいるか、どんなルールか。
tetherd doctor
    ヘルパー / plugin / ECS Exec / pidMode / IAM / VPC CIDR とローカル LAN の重なり、などを検査して次の一手を出す。
tetherd token rotate
    steal 用の個人トークンを更新。
```

### 失敗の伝え方

「何が悪くて、次に何をすればいいか」を 1 画面で言う。

```
tetherd  ✗ ECS Exec is not enabled on service dev/api
        Enable it with:
          aws ecs update-service --cluster myapp-dev --service api --enable-execute-command
        then force a new deployment.  See: tetherd doctor

tetherd  ✗ Another session for user "shota" is already attached (from 192.168.1.20, 3m ago)
        Use --as shota-2 to run in parallel, or stop the other session.

tetherd  ✗ Refusing to attach: agent reports TETHERD_ENV=prod
        tetherd never attaches to production. Check target.env in .tetherd.yml.
```

ブラウザからはヘッダーが必要なので、ModHeader 等の拡張で開発環境ドメインだけに `X-Dev-User` と `X-Dev-Token` を付けるプロファイルをチームに配る。将来ホスト名ルーティングも欲しくなれば、agent の match に `host:` を足すだけで済む設計にしておく。

---

## 6. 透過ネットワーク — カーネル層で捕まえる

透過性を得るための介入点は 3 つしかない。アプリ層（env 書き換え）は限界があり、syscall 層（mirrord のフック）はランタイムごとの地雷を踏み続ける。**カーネルのネットワーク層** はランタイムを一切見ないので、パケットを出す限り必ず捕まる。sshuttle・Tailscale・gVisor 系ツール（gvisor-tap-vsock、tun2socks）と同じ系統。

```
 子プロセス（go run ./cmd/api）  connect("10.0.3.21:5432")   ← コード変更なし
        │
        ▼
 ① 捕まえる層（OS 依存）            ② 終端する層                 ③ 運ぶ層
   macOS (v1): pf `group tetherd`   macOS (v1): カーネル TCP。     SSM port-forward + yamux
     + rdr → 127.0.0.1:N              CLI が accept し natlook で   {"type":"dial",
   Linux (v2): unshare(user+net)      元の宛先を復元                 "addr":"10.0.3.21:5432"}
     + TUN                          Linux (v2): gVisor netstack       │
   プロセス単位。子孫も継承            フロー 1 本 = ストリーム 1 本      ▼
                                                                 ④ 出す層 · tetherd-agent
                                                                   タスクの netns から dial / 名前解決
                                                                   送信元 = タスク IP · sg-app · VPC 経路
                                                                   → RDS · Cloud Map · 内部 ALB · 169.254.170.2
```

①②が OS ごとに変わる。③④は共通。①②の境界は `Capturer` インターフェース（「(元の宛先, 接続) を渡す」）。

### ① 捕まえる層（macOS、v1）

**gid でプロセスをスコープする。** pf はソケット所有者の `group` でマッチできる。専用グループ `tetherd` を作り、子プロセスを **primary gid = tetherd** で起動する。子孫プロセスも gid を継承するので、`go run` がビルドして起動する子バイナリや、air のようなホットリロードツールが起動するプロセスも全部乗る。

- pf が見るのは **effective gid** なので、補助グループでは一致しない。effective だけ変えると bash が起動時に「rgid ≠ egid」を検知して egid を戻すため、**real と effective の両方**を `tetherd` にする（`setregid`）
- 非 root プロセスは自分の所属グループにも `setgid` できないので、root 所有・setgid `tetherd` の小さなラッパー `tetherd-exec`（`setregid` → `exec` だけ）をヘルパーがインストール時に置く。CLI はそれ経由で子を起動するので、子は CLI の素直な子プロセスのまま（tty、シグナル、終了コードがそのまま）
- setgid `tetherd` で増える権限は「pf に捕まる」ことだけ

**pf ルール**（ヘルパーがアンカー `com.tetherd` にメモリ上でロード。ディスクには書かない）

```
table <tetherd_remote> { 10.0.0.0/16, 169.254.170.0/24 }
rdr pass on lo0 inet proto tcp from any to <tetherd_remote> -> 127.0.0.1 port 15300
pass out route-to lo0 inet proto tcp from any to <tetherd_remote> group tetherd keep state
```

sshuttle が 2011 年から macOS で使っている形。group tetherd のプロセスが `<tetherd_remote>` 宛に出した TCP だけが lo0 に回され、rdr で CLI の透過ポートに落ちる。他プロセスの同じ宛先への通信は物理 IF からそのまま出る。CLI は accept した接続について、ヘルパーに `DIOCNATLOOK` で rdr 前の宛先を引いてもらい、agent に `dial` を頼む。終端はカーネルの TCP なので、ユーザー空間スタックの面倒（MTU、輻輳、状態管理）が無い。

utun に `route-to` してユーザー空間スタックで終端する案は、macOS でプロセス単位に使った前例が無いので v1 では取らない。UDP はこの方式ではフロー単位の元宛先が取れないため v2（netstack）で扱う。

**特権ヘルパー**: pf、`/etc/resolver`、`/dev/pf` には root が要る。Tailscale / Docker Desktop / OrbStack と同じく、`brew install` 時に小さな特権ヘルパーを LaunchDaemon として一度だけ入れ、CLI はそれに頼むだけにする。Network Extension（透過プロキシプロバイダ）は署名済みシステム拡張が要り配布が重く、pf で同じことが達成できる以上そこまで行く理由がない。

### macOS ヘルパーの配布と保守

- 入れるのは root の LaunchDaemon 1 つ（Network Extension でも kext でもない）
- 配布は Homebrew tap。formula の `service` ブロックに `require_root true` を書き、`brew install kyosu-1/tetherd/tetherd && sudo brew services start tetherd`。sudo はこの 1 回だけ。root が要る初期化（グループ作成、`tetherd-exec` の配置、残留ルールの掃除）はヘルパーが起動時に自分で行う
- brew 経由のバイナリには quarantine 属性が付かないので署名・公証は v1 ではしない。GitHub Releases からの直接ダウンロードは Developer ID を取ってから
- 依存 API は pf（Lion 以降、Apple 自身が使用、`pfctl` は現行 OS に健在）、`DIOCNATLOOK` / `DIOCCHANGERULE`（sshuttle が使用）、`setregid` の 3 つ。いずれも廃止の兆しは無い
- pf の唯一の罠は `/etc/pf.conf` にアンカー参照を書くと OS 更新で消えること。そこで **ディスクには触らず**、`DIOCCHANGERULE` でメインルールセットにアンカー参照をメモリ上で挿入し、終了時に戻す。`pfctl -E` / `-X` の参照カウントで有効化する（`/etc/pf.conf` のコメントに書かれている作法）
- Ventura 以降は LaunchDaemon 追加時に「バックグラウンド項目が追加されました」と通知が出てユーザーが無効化できるので、`doctor` がヘルパー無応答を検出して「システム設定 → 一般 → ログイン項目」を案内する
- 新 macOS の初期リリースでファイアウォール周りが変わることがある（Sequoia 15.0 で一部 VPN が数週間通信不能になった例）ので、毎年夏のベータで動作確認する
- ヘルパーが受け付ける操作は `pf.apply` / `pf.clear` / `resolver.set` / `resolver.clear` / `natlook` の 5 つだけ。コマンドを起動する操作は無い。接続元は `admin` グループのユーザーに限定（UNIX ソケット + peer credential）
- 同一マシンでの同時セッションは 1 つ（gid を共有する以上 pf が区別できない）。複数サービス同時接続は v2 でセッションごとに gid を分ける
- ヘルパーを入れられない端末向けには、OrbStack / Docker Desktop の Linux VM 内で v2 の netns 方式を使う逃げ道を想定する

### ルーティング方針 — 何をリモートに回すか

**宛先 IP で決める。既定は「VPC の中だけリモート、それ以外は全部ラップトップから」。** `<tetherd_remote>` の中身:

1. 対象タスクが属する VPC の CIDR（`DescribeTasks` → ENI の subnet → `DescribeSubnets` → `DescribeVpcs`。セカンダリ含む。自動）
2. `169.254.170.0/24`（タスクロールの認証情報 + タスクメタデータ）
3. `network.remote_cidrs`（ピアリング先 VPC、Transit Gateway 越しのオンプレなど）
4. `network.remote_services` に書いた AWS サービスの managed prefix list（S3 / DynamoDB）

から `network.local_cidrs` を除く。インターネット、localhost、LAN は子プロセスからそのまま出る。`go run` のモジュール取得や `npm install`、外部 API はラップトップの回線で、dev タスクの ENI を踏み台にした egress は既定で存在しない。mirrord の「egress IP まで Pod」とは逆の側を取る。

macOS の DNS は `mDNSResponder` が出すので pf の group マッチでは見えず、**ホスト名での振り分けは不可**。これが CIDR ベースにする理由でもある。VPC CIDR とラップトップの LAN が重なると、その範囲の LAN 宛通信（子プロセスのものだけ）がリモートに回るので、`run` が起動時に警告して `local_cidrs` を案内する。

### VPC 外の AWS サービスとタスクロール

S3 / DynamoDB / SQS / Secrets Manager / Bedrock など VPC 外のサービスは既定のままで動く。SDK は `169.254.170.2`（トンネル経由）から一時クレデンシャルを取り、以降は SigV4 署名でパブリックエンドポイントに直接送る。署名が正しければ送信元 IP がラップトップでも受け付けられる。

動かないのは IAM / バケット / SCP / エンドポイントポリシーに **ネットワーク条件**（`aws:SourceVpc` / `aws:SourceVpce` / `aws:SourceIp`）がある場合。Interface endpoint（private DNS 有効）なら `remote_domains` にサービスのドメインを書けば agent 側でプライベート IP に解けて VPC CIDR に入る。Gateway endpoint（S3 / DynamoDB）は `remote_services` で prefix list をリモート集合に足す。条件が無い環境ではラップトップ経路のほうが緩く（NAT の無い VPC でもラップトップは自前で出られる）、CloudTrail の `sourceIPAddress` はラップトップの IP になる。

### DNS

**macOS（v1）**: アプリ自身は UDP 53 を送らない。Go（既定）・Node の `dns.lookup`・JVM はいずれもシステムリゾルバ経由で、実際に問い合わせを送るのは root の `mDNSResponder` なので、pf の group マッチでは捕まらない。そこで `/etc/resolver/<domain>`（macOS 標準の per-domain リゾルバ設定。Docker Desktop や dnsmasq 利用者が長年使う仕組み）をヘルパーがセッション中だけ作り、`nameserver 127.0.0.1 port 53530` で tetherd のリゾルバに向け、agent 経由で VPC リゾルバに転送する。対象ドメインは `network.remote_domains`（Cloud Map の名前空間、プライベートホストゾーン）に列挙する。マシン単位の設定になるが、対象は「ラップトップでは元々解けない VPC 内ドメイン」に限られるので他プロセスに影響しない。RDS / ElastiCache / 内部 ALB のエンドポイント名はパブリック DNS でプライベート IP に解け、その IP が VPC CIDR に入るので、**典型構成では `remote_domains` 無しで動く**。Node の `dns.resolve*`（c-ares）と `dig` は `/etc/resolver` を見ない。

**Linux（v2、netns）**: 名前空間内の `/etc/resolv.conf`（mount namespace も必要）をリゾルバに差し替え、既定で全部 agent 側で解決する。

### 副産物: タスクロールが自動で効く

タスクの env には `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` が入っていて、SDK はそれを見て `169.254.170.2` に認証情報を取りに行く。透過モードではこの通信も捕まって agent 経由でタスク内の本物のエンドポイントに届くので、**追加実装なしで SDK がタスクロールとして振る舞う**。`ECS_CONTAINER_METADATA_URI_V4` も同様。`run` は起動時に同じ経路でクレデンシャルを取り `sts:GetCallerIdentity` で確認して表示する。

### v2 で扱うもの

- **タスク側 localhost への到達**（`remote_localhost`）: app が `localhost:4317` の otel collector に送っている場合に、その localhost ポートだけタスク側へ向ける。macOS では lo0 宛の group マッチが rdr の評価順序上まっすぐ書けないので、netstack 導入時に扱う
- **env-rewrite モード**: 特権ヘルパーを入れられない環境向けに、env の値から VPC 内の宛先を見つけてローカルポートに書き換える方式
- **Linux**: `unshare` でユーザー名前空間 + ネットワーク名前空間を作り、中に TUN を立てて gVisor netstack で終端する。root 不要（ただし Ubuntu 24.04 以降の unprivileged userns 制限と、netns 内の listen ポートにホストから届かない問題への対処が要る）

### mirrord との透過度の差

TCP と DNS（任意の宛先）は同等。残る差は UDP（v2）、ファイルシステムの透過（捨てる判断）、agent 側が Fargate の制約で受信を L7 でしか扱えないこと（Fargate を使う限り誰にも越えられない線）。

---

## 7. 設定ファイル

リポジトリ直下の `.tetherd.yml`（共有）と `~/.tetherd/config.yml`（個人）。方針は「タスク（実行中の env）を正とし、差分だけを書く」。`.env` を別に育てる文化に戻らないようにするのが狙い。

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
  env: dev                      # agent の TETHERD_ENV と照合

env:
  override:
    PORT: "8080"
  exclude: []                   # PATH / HOME / HOSTNAME などは既定で除外済み

network:
  remote_cidrs: []              # VPC CIDR は自動。ピアリング先などを足す
  local_cidrs: []               # remote の中でラップトップから直接出す例外
  remote_domains:               # /etc/resolver で agent 側解決にするドメイン
    - myapp.internal
  remote_services: []           # s3 | dynamodb（ネットワーク条件付きポリシーがある場合）

incoming:
  local_port: 8080
  match:
    header: X-Dev-User          # 値の既定は user（個人設定）
    token_header: X-Dev-Token
```

```yaml
# ~/.tetherd/config.yml — 個人。初回 run で生成
user: shota
token: <base64url 32 bytes>     # tetherd token rotate で更新
aws:
  profile: myapp-dev-shota
```

タスク env から既定で除外するもの: `PATH HOME HOSTNAME USER LOGNAME SHELL TMPDIR PWD OLDPWD TERM LANG LC_* SHLVL _ AWS_EXECUTION_ENV`。`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` と `ECS_CONTAINER_METADATA_URI_V4` は透過モードで必要なので残し、`--no-network` のときだけ除外する（残すと SDK が失敗する）。

---

## 8. 制御プロトコル

トランスポートは SSM ポートフォワードで届く 1 本の TCP。その上に yamux で多重化し、最初のストリームを制御用にする。メッセージは JSON Lines（デバッグしやすさ優先。性能は問題にならない）。

```
CLI (laptop)                         agent                          app / RDS
  │── hello {user, token, incoming} ─▶│
  │◀─ welcome {task_arn, env:"dev",    │
  │            app_env:{…41 vars},     │
  │            others:["taro"]} ───────│
  │   ping / pong · 5 秒おき · 3 回落ちたら破棄、CLI は再接続
  │                                    │
  │  steal                             │◀── ALB → :8080  X-Dev-User + X-Dev-Token ──│
  │◀─ new stream · {"type":"http"} + HTTP/1.1 (keep-alive で複数リクエスト) ──│
  │   CLI はストリームを http.Serve し localhost:8080 へ ReverseProxy
  │   ラップトップが落ちていれば agent はそのリクエストだけ :8081 へフォールバック（ボディ 1 MiB まで）
  │                                    │
  │  dial                              │
  │── new stream · {"type":"dial","addr":"10.0.3.21:5432"} ─▶│
  │                                    │── dial（タスクの netns）──▶│
  │   捕まえた接続 1 本 = ストリーム 1 本
  │── new stream · {"type":"resolve","name":"api.myapp.internal"} ─▶│ → addrs
  │                                    │
  │── bye ────────────────────────────▶│
```

- CLI は対象サービスの RUNNING な全タスクに同じ手順で繋ぐ。steal はどのタスクからでも受け、dial / resolve / env は primary（最も古いタスク）を使う。deploy 中のタスクの出入りは `ListTasks` のポーリングで追う
- SSM 経由は接続確立に 2〜3 秒、帯域も細めで、フロー 1 本ごとにラップトップ ↔ SSM ↔ タスクの往復（数十 ms）が乗る。DB と HTTP には十分だが、N+1 が多いコードは体感で重くなる
- 大きなファイルや低レイテンシが要る用途が出た時点で、③運ぶ層だけを Tailscale（タスクに tailscaled サイドカー、直接 P2P）に差し替えられる設計にしておく。①②④は変わらない。運ぶ層は最初から `Transport` インターフェースを持ち、v1 は `ssm`（session-manager-plugin を子プロセスで起動）と `direct`（テスト用）の 2 実装
- SSM セッションの最大継続時間を設定しているアカウントでは途中で切れるため、CLI の自動再接続が前提

---

## 9. インフラ側の変更（1 回だけ）

1. 開発環境のタスク定義に `tetherd-agent` コンテナを追加し、app の listen ポートを 8081 に。`pidMode: task` と agent への `SYS_PTRACE`、`essential: true`、`restartPolicy` を設定。`TETHERD_ENV=dev` も入れる
2. ALB のターゲットグループのポートを 8080（agent）に向ける。sg-app のインバウンドも 8080 に
3. サービスで `enableExecuteCommand: true`、タスクロールに SSM の権限（`ssmmessages:CreateControlChannel` / `CreateDataChannel` / `OpenControlChannel` / `OpenDataChannel`）。無ければ SSM の VPC Endpoint 3 つか NAT
4. 開発者の IAM に ECS / EC2 の読み取りと `ssm:StartSession`（対象を dev クラスターのタスク ARN に限定）。Secrets Manager / SSM Parameter / KMS の権限は **不要**（env は agent が渡す）。`ecs:ExecuteCommand` も不要（ポートフォワードは StartSession を直接呼ぶ）
5. 開発者のラップトップに `brew install kyosu-1/tetherd/tetherd && sudo brew services start tetherd`

ALB にルールを足す必要はない（agent が L7 で振り分ける）。本番のタスク定義には agent 自体を入れない。agent 側に AWS 権限は要らず、Linux capability は env 読み取りのための SYS_PTRACE のみ。インターネットに出られない VPC では agent イメージを ECR pull-through cache 経由で取る。`deploy/dev-env/`（Terraform）がこの手順の実例。

### 開発者に付ける IAM ポリシー

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "DiscoverTarget",
      "Effect": "Allow",
      "Action": [
        "ecs:ListTasks",
        "ecs:DescribeTasks",
        "ecs:DescribeServices",
        "ecs:ListServices",
        "ecs:DescribeTaskDefinition"
      ],
      "Resource": "*"
    },
    {
      "Sid": "DiscoverNetwork",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeSubnets",
        "ec2:DescribeVpcs",
        "ec2:DescribeManagedPrefixLists",
        "ec2:GetManagedPrefixListEntries"
      ],
      "Resource": "*"
    },
    {
      "Sid": "ControlChannel",
      "Effect": "Allow",
      "Action": "ssm:StartSession",
      "Resource": [
        "arn:aws:ecs:ap-northeast-1:123456789012:task/myapp-dev/*",
        "arn:aws:ssm:ap-northeast-1::document/AWS-StartPortForwardingSession"
      ]
    },
    {
      "Sid": "OwnSessions",
      "Effect": "Allow",
      "Action": ["ssm:TerminateSession", "ssm:ResumeSession"],
      "Resource": "arn:aws:ssm:*:*:session/${aws:username}-*"
    },
    {
      "Sid": "Optional",
      "Effect": "Allow",
      "Action": ["sts:GetCallerIdentity", "servicediscovery:ListNamespaces"],
      "Resource": "*"
    }
  ]
}
```

- 認証は AWS SDK の標準チェーン（SSO プロファイル、アクセスキー、AssumeRole）をそのまま使う。tetherd が `ssm:StartSession` を呼んでストリーム URL とトークンを得て session-manager-plugin に渡す（AWS CLI と同じ手順だが AWS CLI 自体は不要）
- IAM Identity Center（SSO）では `${aws:username}` が無いので `OwnSessions` の Resource は `arn:aws:ssm:*:*:session/*` にする
- 透過モードでは 169.254.170.2 が素通しになるので、開発者はローカルからタスクロールの権限を実質的に使える。「dev タスクができることは開発者もできる」という意味で dev では通常許容範囲だが、タスクロールが必要以上に広くないかは一度見ておく

---

## 10. 安全策

**本番への誤接続**

- agent は `TETHERD_ENV` が無ければ起動しない
- CLI は `welcome.env` を `target.env` と照合し、不一致なら切断
- 本番タスク定義には agent を入れない（存在しなければ繋げない）

**踏み台化・横取り**

- 外向きの既定は「VPC 内だけリモート」。dev タスクの ENI からインターネットに出る経路は既定で無い。`remote_cidrs: [0.0.0.0/0]` は `doctor` が警告する
- agent の dial 先はタスクの SG と VPC 経路に従う（タスクが元々できることを超えない）
- steal はユーザー名 + トークンの両方が一致したときだけ。公開 ALB でもユーザー名を知っているだけではラップトップに届かない
- 同一ユーザー名の二重接続は拒否
- 制御ポートは 127.0.0.1 のみ bind。無認証だが信頼境界は「タスク内」（同じタスクに入れるものは同じ権限を持つ）

**特権ヘルパー（macOS）**

- 受け付ける操作は `pf.apply` / `pf.clear` / `resolver.set` / `resolver.clear` / `natlook` の 5 つだけ。任意コマンドの root 実行はできず、コマンドを起動する操作も無い
- 接続元は `admin` グループのユーザーの tetherd CLI に限定（UNIX ソケット + peer credential 検証）
- CLI が異常終了しても pf ルールや resolver ファイルが残らないよう、ヘルパーが CLI の接続断で掃除する
- setgid `tetherd` のラッパーで増える権限は「pf に捕まる」ことだけ

**開発環境への影響**

- 誰も繋いでいなければ完全な素通し
- ラップトップ側が落ちたら 502 を返さず、そのリクエストだけ本流へフォールバック（dial 失敗時のみ。ボディを送った後は戻せないので 502）
- セッション切断で即時に素通しへ復帰
- ローカルアプリは共有 dev DB に書く。ローカルブランチの auto-migrate が dev DB を変えうることを README で注意する

**シークレット**

- ローカルのディスクには書かない
- `tetherd env --format dotenv` は明示的に使ったときだけ、既定はマスク

---

## 11. mirrord との違い

| 観点 | mirrord | tetherd |
|---|---|---|
| 対象への介入 | 何も仕込まず、K8s API で後から注入 | dev 環境のタスク定義にサイドカーを事前に含める |
| ローカルの透過性 | syscall フック。任意の宛先・DNS・ファイルが透過 | カーネル層で捕捉（pf rdr / netns）。任意の宛先・DNS が透過。UDP は v2、ファイルは対象外 |
| ランタイム依存 | あり（SIP 下の再署名、静的 Go、JVM / Node の独自スタック） | なし。パケットを出す限り捕まる |
| 権限 | ローカルは不要。agent 側に特権が必要 | macOS はインストール時に特権ヘルパー 1 回。agent 側は SYS_PTRACE のみ |
| env / secrets | agent が Pod 内プロセスの /proc/pid/environ を読む | 同じ。agent が app の /proc/pid/environ を読む（pidMode: task）。開発者に Secrets Manager 権限は不要 |
| 受信の取り方 | L4（iptables / raw socket）。任意 TCP | L7 リバースプロキシ。HTTP/1.1 中心、gRPC / WS は個別対応 |
| 振り分けの粒度 | `connect()` 単位。`getaddrinfo` も見ているのでホスト名で local / remote を選べる | 宛先 CIDR。macOS の DNS は mDNSResponder が出すのでホスト名では見えない |
| IAM の身元 | Pod の SA トークンが env / ファイル経由で効く | 169.254.170.2 が透過で通り、タスクロールが自動で効く |
| 接続体験 | kube port-forward、1 秒未満 | SSM 中継、2〜3 秒。VPN 不要。運ぶ層は Tailscale に差し替え可 |
| 複数人 | OSS 版は弱く、Operator（有償）で解決 | ユーザー名単位の多重接続とヘッダー振り分けが中核 |
| 本番観察 | 読み取り専用で可能 | 意図的に禁止 |

どちらも「ローカルのプロセスが、向こうで動いているように見える」。違いは見せ方で、mirrord はプロセスの目を騙し、tetherd はプロセスの足元の配線を替える。トレードオフは「ラップトップに root を 1 回入れる代わりに、ランタイム非依存とカーネル TCP を取る」。mirrord がフック方式の代償として払っているもの（SIP 再署名、Go の frida パッチ、ランタイムごとの known issues）を tetherd は払わず、代わりにヘルパーの保守と粒度の粗さを引き受ける。壊れる場所は mirrord がランタイムの数だけあるのに対し、tetherd は OS の数だけ。

### PR ごとの ECS 環境との比較

競合ではなく補完。PR 環境は「押して待って、できたものを見に行く」（初回 5〜15 分、変更ごとに数分）、tetherd は「繋いだら、いつものローカル開発が dev 環境の一部になる」（接続 3 秒、変更は 1 秒未満、デバッガが刺せる）。PR 環境にしかできないのは他人に URL を渡すことと、本番と同じイメージで動く忠実度。内側のループは tetherd、外側のループは PR 環境、と役割を分け、tetherd の target を PR 環境に向ければ両者は組み合わせられる。

---

## 12. 既存ツールとの位置づけ・実装価値の評価（2026 年 9 月時点）

「ECS Fargate に対して mirrord 的な体験を提供するもの」は OSS・製品ともに存在しない。唯一近かった AWS 公式の Copilot `run local --proxy` は 2026 年 6 月 12 日にサポート終了し、空白になっている。

| ツール | 対象 | outgoing | incoming | env / IAM | tetherd との関係 |
|---|---|---|---|---|---|
| mirrord / Telepresence / Gefyra / Signadot | Kubernetes のみ | 透過（フック or VPN） | L4 steal / mirror | Pod の env・SA | ECS には持ち込めない。Fargate は特権もノードも無いため移植も不可。mirrord に既存 agent へ TCP で繋ぐ設定は現行ドキュメントに無く、layer を借りる案は成立しない |
| AWS Copilot `run local --proxy` | ECS（Copilot 管理下） | Service Connect のサービスと RDS のみ。Docker の pause コンテナ内で宛先ごとに SSM ポートフォワード + iptables REDIRECT + /etc/hosts | なし | タスク定義の env/secrets を注入。IAM はラップトップの資格情報 | コンセプトの先行例。Docker 内実行・既知ホスト限定・受信なし。**2026-06-12 サポート終了** |
| ecsta（fujiwara） | ECS | 1 ポートの SSM フォワード | なし | なし | ③運ぶ層の部品として同じ API を使う。`doctor` / タスク選択 UX の参考 |
| Tailscale subnet router on Fargate | VPC 全体 | マシン単位で VPC に透過到達 | なし | なし | ネットワークだけなら最短の代替。プロセス単位でなく、env・IAM・steal は別途必要。将来の③の選択肢 |
| amazon-ecs-local-container-endpoints | ローカル Docker | なし | なし | 169.254.170.2 をローカルで模倣（資格情報はラップトップのもの） | tetherd は透過経路で本物に届くので模倣が不要 |
| sshuttle / tun2socks / gvisor-tap-vsock | 汎用 | 透過（pf/iptables + ユーザー空間スタック） | — | — | §6 の技術的前例。v1 の pf rdr + `DIOCNATLOOK` は sshuttle の macOS 実装そのもの |
| Cloud Code / `gcloud beta code dev`（Cloud Run） | ローカル Docker | なし | なし | ADC or SA 鍵 | Cloud Run 側には Copilot 相当の先行例すら無い |

**評価**: 埋める価値がある空白。ECS Fargate は特に国内で利用者が多く、K8s 向けツール群は Fargate の制約のため原理的に移植できない。AWS 自身は Copilot で一度この方向に踏み出し、引き上げた（Copilot 全体が CDK と Express Mode に置き換えられたためで、ローカル開発体験が理由ではない。後継の Express Mode も CDK もローカル開発の接続体験は提供しない）。競合は「ツール」ではなく「間に合わせ」（docker compose + ローカル DB、Tailscale + ecsta + env ダンプの寄せ集め）で、tetherd の差分は (1) プロセス単位の透過で VPN 不要、(2) タスクロールが自動で効く、(3) ヘッダーによる受信 steal、(4) 1 コマンド。(2) と (3) は寄せ集めでは実現できない。

**リスク**: macOS 特権ヘルパーの保守、SSM 経由のレイテンシ、Fargate の制約で受信が L7 に限られること、AWS が ECS Exec 周辺に投資しているため将来一次機能が出る可能性（出ても①②層の設計はそのまま使える）。Express Mode で作られたサービス（共有 ALB + ターゲットグループ）も対象にできることは早めに確認する。

---

## 13. プロバイダ抽象と Cloud Run への展開

tetherd は「Kubernetes を使わないサーバーレスコンテナのための mirrord」を狙う。v1 は ECS Fargate 専用として出すが、リポジトリ・バイナリ・agent イメージは 1 つに保ち、Cloud Run を 2 つ目のプロバイダとして足す。四層のうち①捕まえる層・②終端する層・④出す層は共通で、変わるのは **③運ぶ層・対象の発見・env の取得・IAM エンドポイントのアドレス** の 4 点。

| | ECS Fargate（v1） | Cloud Run（後続） |
|---|---|---|
| agent の同居 | サイドカー。awsvpc で netns 共有 | マルチコンテナ。localhost 共有。agent を ingress コンテナにして app へ L7 プロキシ |
| ③運ぶ層 | ECS Exec / SSM ポートフォワードで agent の lo:9900 へ | exec 相当が無い。(a) サービス URL に WebSocket を張り ingress の agent が制御チャネルとして受ける、または (b) agent がリレーへアウトバウンド接続 |
| 複数インスタンス | RUNNING な全タスクに繋ぐ | リクエストの着地インスタンスを制御できない。dev は min=max=1 なら (a) で足りる。それ以外は (b) のリレーが必要 |
| CPU / スケール | 常時割り当て | リクエスト外は CPU スロットリング。(a) は WebSocket がリクエスト扱いで CPU が付くが 60 分で切れるので再接続。(b) は CPU 常時割り当てが必要。scale-to-zero を避けるため min=1 |
| env | agent が /proc から | 同じ（マルチコンテナで PID 共有可否は要確認。不可なら Secret Manager 経由のフォールバック） |
| IAM の透過 | 169.254.170.2（タスクロール） | metadata.google.internal（サービスアカウント） |
| 認証 | IAM（ssm:StartSession） | Cloud Run IAM（invoker）+ ID トークン |

**実装方針**: v1 ではプロバイダのインターフェースを切らず、ECS 固有部分を `internal/provider/ecs` に寄せるだけにする（実装が 1 つの段階で抽象を切ると ECS の都合に歪む）。ただし③運ぶ層（`Transport`）と①捕まえる層（`Capturer`）は v1 から インターフェースを持つ。Cloud Run を足すときに `provider/cloudrun` を書きながら共通インターフェースを抽出する。リレーサービスが必要になれば同じリポジトリのオプションコンポーネントとして置く。

---

## 14. 未決事項

- mirror の方針。共有 DB への二重書き込みをどう防ぐか（GET 限定、明示的な opt-in など）
- Linux の netns 方式で、ホストから netns 内の listen ポート（steal の宛先、`curl localhost:8080`）にどう届けるか。rootlesskit の port driver と同じく CLI がホスト側 netns に `setns` したスレッドで listen して橋渡しする案が有力。逆方向（子 → ホストの localhost）も同様
- Linux で unprivileged user namespace が使えない環境（Ubuntu 24.04 の AppArmor 制限など）の逃げ道
- IPv6 を netstack で扱うか
- WebSocket / SSE / gRPC(HTTP/2) の steal
- 同一マシンでの複数セッション（セッションごとに gid を分ける）
- session-manager-plugin の埋め込み（`aws/session-manager-plugin` の datachannel を組み込んで依存ゼロにする）
- Cloud Run のマルチコンテナで PID 名前空間の共有が可能か（env 取得方式に影響）

決着済み: 外向き通信の既定（VPC 内だけリモート）、macOS の pf 方式（rdr + `DIOCNATLOOK`）、agent イメージの配布先（GHCR）。

---

## 15. 先に潰す検証

設計の妥当性を左右するものから。1〜4 は AWS 不要で、`hack/e2e-local.sh`（Docker ネットワーク上の agent + postgres に `direct` トランスポートで繋ぐ）で確認する。

1. `tetherd-exec`（`setregid`）+ pf `group` + `rdr` で、bash / zsh / Go / Node の子プロセスの TCP が捕まり、他プロセスは捕まらないこと
2. `DIOCNATLOOK` が現行 macOS で期待どおり元の宛先を返すこと
3. `DIOCCHANGERULE` でメインルールセットにアンカー参照を挿入できること。無理なら `pfctl` の dump / reload に切り替える
4. `/etc/resolver/<domain>` + `port` が Go / Node（`dns.lookup`）/ JVM の `getaddrinfo` で効くこと
5. Fargate で `pidMode: task` + `SYS_PTRACE` + ECS Exec（ssm-agent 注入）が共存し、agent が app の environ を読めること
6. `ssm:StartSession` + `AWS-StartPortForwardingSession` で `ecs:` ターゲットの `127.0.0.1:9900` に届くこと、フロー確立の所要時間と RTT

---

## 16. ロードマップ

| | 内容 |
|---|---|
| v0.1 | ローカル e2e。helper + `tetherd-exec` + pf rdr + natlook + yamux + agent の `dial` を Docker ネットワーク相手に通す（§15 の 1〜3）。`psql -h 172.20.0.10` がコード変更なしで通る |
| v0.2 | `deploy/dev-env`（Terraform）と agent の environ 読み取り、SSM トランスポート、env 注入、DNS、タスクロール確認（§15 の 4〜6）。`tetherd run -- psql -h <rds>` が通る |
| v0.3 | steal（agent の L7 プロキシ、トークン、フォールバック）、全タスク接続と deploy 追従、`status` / `doctor` |
| v0.4 | Homebrew tap、LaunchDaemon、GoReleaser、README。チームに配れる |
| v1.0 | 上記の安定化 |
| 以降 | Linux（netns + netstack）、mirror、UDP、`remote_localhost`、plugin 埋め込み、Tailscale トランスポート、Cloud Run |
