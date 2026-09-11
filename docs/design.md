# tetherd — 設計書

サーバーレスコンテナ（ECS Fargate、後続で Cloud Run）のための mirrord 的ローカル開発環境。
ローカルのプロセスを、開発環境の ECS タスクの「中で動いているかのように」振る舞わせる CLI とサイドカー。

- 実装: Go / 単一バイナリ（CLI）+ distroless イメージ（agent）
- v1 対象: ALB + ECS Fargate + RDS
- 次: Cloud Run（プロバイダとして追加）
- 名前: `tetherd`（テザード）。tether = テザリング。タスクのネットワークと身元をラップトップに分けてもらう

---

## 1. 一文で言うと

```
$ tetherd run -- go run ./cmd/api
```

これ一発で、ローカルの `go run ./cmd/api` が開発環境の `api` タスクの一部として振る舞う。

- 環境変数とシークレットは、実行中の app コンテナから読んだ本物
- そのプロセスが出す通信は、宛先が何であれ、DNS も含めて、タスクの ENI から出ていく
- AWS SDK はタスクロールとして振る舞う
- ALB に届いたリクエストのうち `X-Dev-User: shota` が付いたものだけがラップトップの `:8080` に流れてくる

> mirrord は syscall をフックして「プロセスの目を騙す」。tetherd はフックの代わりに **カーネルのネットワーク層で捕まえ、ユーザー空間のネットワークスタックで終端する**。透過度は mirrord と同等で、ランタイム（Go の静的バイナリ、JVM、Node、macOS の SIP）に一切依存しない。捨てるのはファイルシステムの透過だけ。

---

## 2. やること・やらないこと

### やること

- app コンテナの **実行中の環境変数**（secrets 解決済み）を agent が読み取り、ローカルプロセスに注入する。開発者に Secrets Manager の権限は不要
- 子プロセス（と子孫）の全 TCP / UDP / DNS をカーネル層で捕まえ、タスクの ENI から出す（透過 outgoing）
- タスクロールの認証情報エンドポイント（169.254.170.2）もそのまま通し、SDK をタスクロールとして動かす
- ALB → サイドカー経由で、条件に合う HTTP リクエストを手元に引き込む（steal）
- 条件に合うリクエストのコピーを手元にも流す（mirror、レスポンスは捨てる）
- 以上を 1 本の制御チャネル（ECS Exec / SSM）に多重化する

### やらないこと

- syscall フック（LD_PRELOAD / DYLD 系）。ランタイムごとの地雷を踏み続ける宿命があるため
- ファイルシステムの透過アクセス
- 本番環境への適用（設定とサイドカーの二重ガードで拒否）
- 動いているタスクへの「後から差し込む」注入。サイドカーはタスク定義に最初から含める
- HTTP 以外のプロトコルの steal（Fargate では NET_ADMIN / NET_RAW が取れない）

---

## 3. 全体構成

```
 ┌──────────────┐  ① X-Dev-User: shota  ┌──────────────────────────────────────────┐
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
 │  tetherd CLI ─ env 注入 / netstack(全 TCP·UDP·DNS → agent)   │
 │               / steal → :8080                                │
 │  go run ./cmd/api :8080  ← netns / pf group でスコープ        │
 └──────────────────────────────────────────────────────────────┘
```

部品は 2 つ。タスクに同居するサイドカー（agent）と、ラップトップの CLI。

### 3.1 tetherd-agent（サイドカー）

- Go 製の単一バイナリ、distroless イメージ。タスク定義にコンテナを 1 つ足す
- awsvpc モードではタスク内の全コンテナがネットワーク名前空間を共有するため、mirrord のエージェントと同じ立ち位置（同じ ENI、同じ SG、同じ IP）に立てる
- `:8080` で ALB からのトラフィックを受け、開発者セッションが無ければ全て `:8081` の app へ素通し
- セッション中だけルーティングテーブルを持ち、ヘッダーが一致したリクエストを該当ユーザーのラップトップへ流す
- `:9900` は制御ポート。`127.0.0.1` のみ bind（SSM フォワードは同一ネットワーク名前空間から来る）
- `TETHERD_ENV=dev` が無ければ起動を拒否する
- 既存のヘルスチェックパスはそのまま透過（agent はヘルスチェックを横取りしない）
- セッションが切れたら即座に素通しに戻す（リクエスト途中のものは完了まで待つ）
- AWS API は呼ばない。Linux capability は env 読み取りのための `SYS_PTRACE` のみ
- listen ポート（8080 / 9900）は設定で変更可

### 3.2 環境変数と secrets の取得 — 実行中のプロセスから読む

タスク定義を読んで Secrets Manager / SSM から自分で引く方式は取らない。mirrord が対象コンテナの `/proc/<pid>/environ` を読むのと同じく、**agent が app コンテナの実行中プロセスの環境変数を読み取って** `welcome` で CLI に渡す。ECS が起動時に解決した secrets がそのまま入っているので、開発者側に Secrets Manager / SSM / KMS の権限は一切要らない。信頼境界が ECS Exec と同じ（タスクに入れる人は env を見られる）に揃う。

Fargate でこれを可能にするための設定が 2 つ:

- タスク定義で `pidMode: task`（コンテナ間で PID 名前空間を共有。Fargate platform 1.4+）
- agent コンテナの `linuxParameters.capabilities.add: ["SYS_PTRACE"]`（他ユーザーのプロセスの environ を読むのに必要。Fargate で追加を許されている唯一の capability）

app コンテナの特定: タスクメタデータエンドポイント（`ECS_CONTAINER_METADATA_URI_V4/task`）で app コンテナの ID を得て、`/proc/*/environ` の中から `ECS_CONTAINER_METADATA_URI_V4` がその ID を指しているプロセスを探す。環境変数の値はプロセス起動時点のものになるが、これはコンテナの env の性質そのもの。

フォールバック（`pidMode: task` を入れたくない場合）:

- agent コンテナ定義に app と同じ `secrets` ブロックを複製する（ECS が agent 用にも解決してくれる。`init` が生成）
- CLI がタスク定義 + Secrets Manager から自分で解決する（`env.source: api`。開発者に GetSecretValue が必要）

既定は agent 経由。

### 3.3 tetherd CLI（ラップトップ）

- 依存は AWS CLI と session-manager-plugin、それに `brew install` 時に一度だけ入る特権ヘルパー（macOS のみ、§6）
- `run` 一発で、タスク選択 → SSM ポートフォワード → yamux セッション（ここで agent から app の env を受け取る）→ 子プロセス用のネットワーク隔離を用意 → env を注入して `exec`、までを行う
- 子プロセスの終了か Ctrl-C で隔離を片付け、agent に `bye` を送り、素通しに戻す
- 中核は gVisor netstack（Go 製のユーザー空間 TCP/IP スタック）。捕まえた IP パケットを TCP / UDP のフローとして終端し、フローごとに agent へのストリームに変換する。この部分は OS に依存せず、「どうパケットを捕まえるか」（§6）とは完全に分離されている

---

## 4. ネットワーク構成

いちばん大事な性質は、**ラップトップが VPC に一切入らない**こと。VPN もリレーも ALB のルール追加も要らず、すべて SSM を中継点にした「双方向アウトバウンド」で成立する。

```
 Internet ── ① HTTPS (X-Dev-User: shota) ──▶ ALB · sg-alb (in 443 ← 0/0)     [public subnet]
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

サブコマンドは `run` が主役。ほかはトラブルシュートと導入補助の 4 つに絞り、それ以上増やさない。

```
$ tetherd run -- go run ./cmd/api
tetherd  dev/api  task 3f9c…  (started 12m ago)
  ✓ env      41 vars, 6 secrets resolved
  ✓ network  transparent (macOS: utun3 + pf group tetherd) · DNS via VPC resolver
  ✓ iam      task role arn:aws:iam::…:role/myapp-dev-api-task  (via 169.254.170.2)
  ✓ steal    X-Dev-User: shota  → localhost:8080
  ▶ go run ./cmd/api
2026/09/11 10:12:03 listening on :8080
2026/09/11 10:12:03 connected to postgres myapp-dev…rds.amazonaws.com:5432
  ...
tetherd  ← GET  /api/orders/123   200   84ms   (from 10.0.3.21)
tetherd  ← POST /api/orders       500  1.2s   ← local error
```

- tetherd 自身のログは stderr に `tetherd` プレフィックス付きで出し、子プロセスの stdout / stderr はそのまま流す。子プロセスのログを汚さない
- steal したリクエストは 1 行ずつ出し、`--quiet` で消せる

```
tetherd run [flags] -- <command...>
  -s, --service    対象サービス（複数サービスのリポジトリ用）
      --as NAME    X-Dev-User の値を上書き（他人の代わりにデバッグ）
      --mode       steal | mirror | off
      --no-incoming
      --network    transparent | env-rewrite | off   （既定 transparent。§6）
      --local HOST,...   透過モードでもローカルから直接出す宛先（例: localhost, *.stripe.com）
      --task ID    タスクを明示（既定は自動選択）
  -q, --quiet

tetherd env [--format dotenv|json|shell] [--reveal]
    解決済み env を出力。eval "$(tetherd env --format shell)" で既存の起動に混ぜる。既定は secrets をマスク。
tetherd status
    今 dev/api の agent に誰が繋いでいるか、どんなルールか。
tetherd doctor
    ECS Exec 有効か / plugin があるか / agent 入りタスクが RUNNING か / IAM が通るか / (macOS) ヘルパーが応答するか。
tetherd init
    タスク定義を読んで .tetherd.yml の雛形を生成。remote_domains の候補を Cloud Map から提案。
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
        tetherd never attaches to production. Check target.cluster in .tetherd.yml.
```

ブラウザからはヘッダーが必要なので、ModHeader 等の拡張で開発環境ドメインだけに `X-Dev-User` を付けるプロファイルをチームに配る。将来ホスト名ルーティングも欲しくなれば、agent の match に `host:` を足すだけで済む設計にしておく。

---

## 6. 透過ネットワーク — カーネル層で捕まえ、ユーザー空間で終端する

透過性を得るための介入点は 3 つしかない。アプリ層（env 書き換え）は限界があり、syscall 層（mirrord のフック）はランタイムごとの地雷を踏み続ける。**カーネルのネットワーク層** はランタイムを一切見ないので、パケットを出す限り必ず捕まる。Tailscale・sshuttle・gVisor 系ツール（gvisor-tap-vsock、tun2socks）と同じ系統。

```
 子プロセス（go run ./cmd/api）  connect("payment-svc:8080")   ← コード変更なし
        │
        ▼
 ① 捕まえる層（OS 依存）           ② 終端する層（OS 非依存）        ③ 運ぶ層
   Linux: unshare(user+net)+TUN  ─▶  gVisor netstack in CLI   ─▶  SSM port-forward + yamux
   macOS: utun + pf `group tetherd`   IP → TCP/UDP フロー            {"type":"dial","proto":"tcp",
   プロセス単位。子孫も継承            フロー 1 本 = ストリーム 1 本      "addr":"payment-svc:8080"}
                                                                          │
                                                                          ▼
                                                                 ④ 出す層 · tetherd-agent
                                                                   タスクの netns から dial / 名前解決
                                                                   送信元 = タスク IP · sg-app · VPC 経路
                                                                   → RDS · Cloud Map · 内部 ALB · VPC endpoints · 169.254.170.2
```

①だけが OS ごとに変わる。②〜④は共通。

### ① 捕まえる層

**Linux** はいちばん綺麗で、`unshare` でユーザー名前空間 + ネットワーク名前空間を作り、中に TUN を一枚立て、そこで子プロセスを起動する。root 不要。そのプロセスの通信は物理的に TUN 以外に出口が無い（slirp4netns、rootless Docker と同じ）。

**macOS** には netns が無いので utun + pf を使う。pf はソケット所有者の `group` でマッチできるので、専用グループ `tetherd` を作り、子プロセスをその補助グループ付きで起動し、「group tetherd の TCP/UDP は utun へ route-to」の 1 ルールを入れる。子孫プロセスもグループを継承するので、`go run` がビルドして起動する子バイナリや、air のようなホットリロードツールが起動するプロセスも全部乗る。Linux で netns を使わない場合も iptables の `owner --gid-owner` で同じ形になり、両 OS で「gid でプロセスをスコープする」という一貫した考え方が取れる。

macOS の pf・utun・gid 付き起動には root が要る。Tailscale / Docker Desktop / OrbStack と同じく、`brew install` 時に小さな特権ヘルパーを LaunchDaemon として一度だけ入れ、CLI はそれに頼むだけにする。Network Extension（透過プロキシプロバイダ）は署名済みシステム拡張が要り配布が重く、pf で同じことが達成できる以上そこまで行く理由がない。

### macOS ヘルパーの配布と保守

- 入れるのは root の LaunchDaemon 1 つ（Network Extension でも kext でもない）
- 配布は Homebrew formula の `service` ブロックに `require_root true` を書き、`brew install tetherd && sudo brew services start tetherd`。Tailscale の OSS 版 CLI と同じ経路で、sudo はこの 1 回だけ
- brew 経由のバイナリには quarantine 属性が付かないので署名・公証は必須ではないが、GitHub Releases 直接ダウンロード向けに Developer ID + GoReleaser + quill で CI から公証しておく
- 依存 API は pf（Lion 以降、Apple 自身が使用、`pfctl` は現行 OS に健在）、utun（Tailscale / WireGuard-go が 10 年以上同じコード）、`setgroups` + `posix_spawn` の 3 つ。いずれも廃止の兆しは無い
- pf の唯一の罠は `/etc/pf.conf` にアンカー参照を書くと OS 更新で消えること。そこで **ディスクには触らず**、起動時に現在のルールセットを読んで自分のアンカー参照を足したものをメモリ上でロードし、終了時に戻す（sshuttle が 2011 年から macOS でやっている方式）
- Ventura 以降は LaunchDaemon 追加時に「バックグラウンド項目が追加されました」と通知が出てユーザーが無効化できるので、`doctor` がヘルパー無応答を検出して「システム設定 → 一般 → ログイン項目」を案内する
- 新 macOS の初期リリースでファイアウォール周りが変わることがある（Sequoia 15.0 で一部 VPN が数週間通信不能になった例）ので、毎年夏のベータで動作確認する
- ヘルパーは launchd のソケットアクティベーションで常駐させない
- ヘルパーを入れられない端末向けには、env-rewrite モードか、OrbStack / Docker Desktop の Linux VM 内で netns 方式を使う逃げ道を用意する。後者は macOS 側に何も入れない

### ② 終端する層

CLI に gVisor netstack を組み込む。TUN から来た IP パケットを TCP / UDP のフローとして終端し、フロー 1 本ごとに agent へ yamux ストリームを 1 本開いて `dial` を頼む。UDP はフロー単位のストリームにデータグラムを長さ付きでフレーミング。この層は捕まえ方から完全に独立しているので、先に Linux の netns で固めてから macOS の捕まえ方を足せる。

### DNS

**Linux（netns）**: 名前空間内の `/etc/resolv.conf` を netstack 内のリゾルバに差し替える。`getaddrinfo` も Go の内蔵リゾルバもそこを読むので、既定で mirrord と同じく **全部 agent 側で解決**（タスクの resolv.conf、つまり VPC リゾルバ）。Cloud Map の名前、プライベートホストゾーン、RDS のエンドポイント名がコードを触らずに解ける。`network.local` のパターン（`localhost`、`*.stripe.com` など）だけラップトップで解決し、その宛先への通信もラップトップから直接出す。

**macOS**: アプリ自身は UDP 53 を送らない。Go（既定）・Node の `dns.lookup`・JVM はいずれもシステムリゾルバ経由で、実際に問い合わせを送るのは root の `mDNSResponder` なので、pf の group マッチでは捕まらない。そこで `/etc/resolver/<domain>`（macOS 標準の per-domain リゾルバ設定。Docker Desktop や dnsmasq 利用者が長年使う仕組み）をヘルパーがセッション中だけ作り、`nameserver 127.0.0.1 port 53530` で tetherd のリゾルバに向け、agent 経由で VPC リゾルバに転送する。対象ドメインは `network.remote_domains`（Cloud Map の名前空間、プライベートホストゾーン）に列挙し、`init` が候補を提案する。マシン単位の設定になるが、対象は「ラップトップでは元々解けない VPC 内ドメイン」に限られるので他プロセスに影響しない。RDS のエンドポイント名はパブリック DNS でプライベート IP に解けるため対象外でよい。「全ドメインをリモート解決」は macOS ではプロセス単位に実現できず、Linux 限定の挙動とする（Go アプリに限り `GODEBUG=netdns=go` を注入すれば内蔵リゾルバに切り替わり pf で捕まえられるが、Go 限定の裏技なので既定にしない）。

### 副産物: タスクロールが自動で効く

タスクの env には `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` が入っていて、SDK はそれを見て `169.254.170.2` に認証情報を取りに行く。透過モードではこの通信も捕まって agent 経由でタスク内の本物のエンドポイントに届くので、**追加実装なしで SDK がタスクロールとして振る舞う**。`ECS_CONTAINER_METADATA_URI_V4` も同様。

### タスク側 localhost への到達 — 他のサイドカー（otel collector など）との共存

agent は他のサイドカーと独立に共存できる（ポートが重ならなければよい）。それに加えて、**ローカルプロセスから見た「localhost」の一部をタスク側の localhost に向ける** 例外を持たせる。app が `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317` で同居する collector に送っているなら、この env はそのまま注入され、4317 への通信がトンネル経由でタスク内の collector に届く。ALB から steal したリクエストは agent が `traceparent` / `X-Amzn-Trace-Id` を素通しするので、ALB → agent → ラップトップのアプリ → RDS が開発環境の可観測性基盤上で 1 本のトレースになる。

- Linux の netns 方式では名前空間内の lo がホストと別なので netstack が自然に拾える
- macOS では pf の既定ルールセットに `set skip on lo0` があるため、メモリ上でロードする自分のルールセットではこれを外し、group tetherd の lo0 宛 TCP で `remote_localhost` のポートだけを route-to する
- collector が無いタスクでは `remote_localhost` を書かなければ従来どおり全 localhost がラップトップ側

### フォールバック: env-rewrite モード

特権ヘルパーを入れられない環境向けに `--network env-rewrite` を残す。env の値（`DATABASE_URL` など）から VPC 内の宛先を見つけ、宛先ごとにローカルポートを立てて agent へフォワードし、env の値を `localhost:15432` に書き換えて子プロセスへ渡す。env に載っていない宛先には届かず、タスクロールも効かないが、ALB + Fargate + RDS の典型構成では日常の大半をカバーする。

### mirrord との透過度の差

ネットワーク（TCP / UDP / DNS / 任意の宛先）は同等。残る差はファイルシステムの透過（捨てる判断）と、agent 側が Fargate の制約で受信を L7 でしか扱えないこと（Fargate を使う限り誰にも越えられない線）の 2 点。

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

env:
  source: agent                 # agent | api（api は開発者に GetSecretValue が必要）
  override:
    PORT: "8080"
  exclude:
    - AWS_EXECUTION_ENV

network:
  mode: transparent             # transparent | env-rewrite | off
  local:                        # これだけはラップトップから直接出す（DNS もローカル）
    - localhost
    - "*.stripe.com"
  remote_domains:               # macOS の /etc/resolver 対象。Linux では不要（全部リモート解決）
    - "*.myapp.internal"
  remote_localhost: [4317, 4318]  # この localhost ポートだけはタスク側へ（collector が無ければ書かない）
  # env-rewrite モードで env に載らない宛先を足すとき
  # forward: [{ local: 18080, remote: payment-svc.internal:8080 }]

telemetry:
  resource_attributes:          # OTEL_RESOURCE_ATTRIBUTES に追記。service.name は変えない
    tetherd.user: "${user}"

incoming:
  local_port: 8080
  mode: steal                   # steal | mirror | off
  match:
    header: X-Dev-User          # 値の既定は user（個人設定）
  # mirror の例:  mode: mirror / match: { path_prefix: /api/orders, sample: 0.1 }
```

```yaml
# ~/.tetherd/config.yml — 個人
user: shota
aws:
  profile: myapp-dev-shota
```

---

## 8. 制御プロトコル

トランスポートは SSM ポートフォワードで届く 1 本の TCP。その上に yamux で多重化し、最初のストリームを制御用にする。メッセージは JSON Lines（デバッグしやすさ優先。性能は問題にならない）。

```
CLI (laptop)                         agent                          app / RDS
  │── hello {user, incoming, forwards} ─▶│
  │◀─ welcome {task, env:"dev",          │
  │            app_env:{…41 vars},       │
  │            others:["taro"]} ─────────│
  │   ping / pong · 5 秒おき · 3 回落ちたら CLI が再接続
  │                                      │
  │  steal                               │◀── ALB → :8080  X-Dev-User: shota ──│
  │◀─ new stream · {"type":"http", remote_addr} + raw HTTP/1.1 ──│
  │   CLI は localhost:8080 に dial して双方向コピー
  │   ラップトップが落ちていれば agent はそのリクエストだけ :8081 へフォールバック
  │                                      │
  │  dial                                │
  │── new stream · {"type":"dial","proto":"tcp|udp","addr":"payment-svc:8080"} ─▶│
  │                                      │── 名前解決 + dial（タスクの netns）──▶│
  │   netstack のフロー 1 本 = ストリーム 1 本。UDP は長さ付きフレーム
  │   DNS は {"type":"resolve"} で別に持つ
  │                                      │
  │── bye ──────────────────────────────▶│
```

- mirror は steal と同じだが agent はレスポンスを読み捨て、2 秒でタイムアウト
- SSM 経由は接続確立に 2〜3 秒、帯域も細めで、フロー 1 本ごとにラップトップ ↔ SSM ↔ タスクの往復（数十 ms）が乗る。DB と HTTP には十分だが、N+1 が多いコードは体感で重くなる
- 大きなファイルや低レイテンシが要る用途が出た時点で、③運ぶ層だけを Tailscale（タスクに tailscaled サイドカー、直接 P2P）に差し替えられる設計にしておく。①②④は変わらない。運ぶ層は最初からインターフェースを持つ
- SSM セッションの最大継続時間を設定しているアカウントでは途中で切れるため、CLI の自動再接続が前提

---

## 9. インフラ側の変更（1 回だけ）

1. 開発環境のタスク定義に `tetherd-agent` コンテナを追加し、app の listen ポートを 8081 に。`pidMode: task` と agent への `SYS_PTRACE` を設定。`TETHERD_ENV=dev` も入れる
2. ALB のターゲットグループのポートを 8080（agent）に向ける。sg-app のインバウンドも 8080 に
3. サービスで `enableExecuteCommand: true`、タスクロールに SSM の権限（`ssmmessages:CreateControlChannel` / `CreateDataChannel` / `OpenControlChannel` / `OpenDataChannel`）。無ければ SSM の VPC Endpoint 3 つ
4. 開発者の IAM に ECS の読み取りと `ssm:StartSession`（対象を dev クラスターのタスク ARN に限定）。Secrets Manager / SSM Parameter / KMS の権限は **不要**（env は agent が渡す）。`ecs:ExecuteCommand` も不要（ポートフォワードは StartSession を直接呼ぶ）
5. 開発者のラップトップに `brew install tetherd`。macOS では特権ヘルパー（LaunchDaemon）が同時に入る。Linux は netns を使うので不要

ALB にルールを足す必要はない（agent が L7 で振り分ける）。本番のタスク定義には agent 自体を入れない。agent 側に AWS 権限は要らず、Linux capability は env 読み取りのための SYS_PTRACE のみ。

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

- 認証は AWS SDK の標準チェーン（SSO プロファイル、アクセスキー、AssumeRole）をそのまま使う。tetherd が `ssm:StartSession` を呼んでストリーム URL とトークンを得て session-manager-plugin に渡す（AWS CLI と同じ手順）
- IAM Identity Center（SSO）では `${aws:username}` が無いので `OwnSessions` の Resource は `arn:aws:ssm:*:*:session/*` にする
- 透過モードでは 169.254.170.2 が素通しになるので、開発者はローカルからタスクロールの権限を実質的に使える。「dev タスクができることは開発者もできる」という意味で dev では通常許容範囲だが、タスクロールが必要以上に広くないかは一度見ておく

---

## 10. 安全策

**本番への誤接続**

- agent は `TETHERD_ENV` が dev 系でなければ起動しない
- CLI は `welcome.env` を確認し、不一致なら切断
- 本番タスク定義には agent を入れない（存在しなければ繋げない）

**踏み台化・横取り**

- agent の dial 先はタスクの SG と VPC 経路に従う（タスクが元々できることを超えない）。外向き（0.0.0.0/0）を agent 経由にするかは `network.local` の既定で外向きドメインをラップトップに向けて抑える
- 同一ユーザー名の二重接続は拒否
- 制御ポートは 127.0.0.1 のみ bind

**特権ヘルパー（macOS）**

- 受け付ける操作は「pf アンカーへのルール投入／削除」「指定 gid 付きで指定ユーザーとしてコマンド起動」「`/etc/resolver/` 配下の tetherd 管理ファイルの作成／削除」の 3 つだけ。任意コマンドの root 実行はできない
- 接続元は同一ユーザーの tetherd CLI に限定（UNIX ソケット + peer credential 検証）
- CLI が異常終了しても pf ルールや resolver ファイルが残らないよう、ヘルパーが CLI の生存を監視して掃除する

**開発環境への影響**

- 誰も繋いでいなければ完全な素通し
- ラップトップ側が落ちたら 502 を返さず、そのリクエストだけ本流へフォールバック
- セッション切断で即時に素通しへ復帰

**シークレット**

- ローカルのディスクには書かない
- `tetherd env --format dotenv` は明示的に使ったときだけ、既定はマスク

---

## 11. mirrord との違い

| 観点 | mirrord | tetherd |
|---|---|---|
| 対象への介入 | 何も仕込まず、K8s API で後から注入 | dev 環境のタスク定義にサイドカーを事前に含める |
| ローカルの透過性 | syscall フック。任意の宛先・DNS・ファイルが透過 | カーネル層で捕捉（netns / pf group）+ ユーザー空間 netstack。任意の宛先・DNS・UDP が透過。ファイルは対象外 |
| ランタイム依存 | あり（SIP 下の再署名、静的 Go、JVM / Node の独自スタック） | なし。パケットを出す限り捕まる |
| 権限 | ローカルは不要。agent 側に特権が必要 | Linux は不要。macOS はインストール時に特権ヘルパー 1 回。agent 側は SYS_PTRACE のみ |
| env / secrets | agent が Pod 内プロセスの /proc/pid/environ を読む | 同じ。agent が app の /proc/pid/environ を読む（pidMode: task）。開発者に Secrets Manager 権限は不要 |
| 受信の取り方 | L4（iptables / raw socket）。任意 TCP | L7 リバースプロキシ。HTTP/1.1 中心、gRPC / WS は個別対応 |
| IAM の身元 | Pod の SA トークンが env / ファイル経由で効く | 169.254.170.2 が透過で通り、タスクロールが自動で効く |
| 接続体験 | kube port-forward、1 秒未満 | SSM 中継、2〜3 秒。VPN 不要。運ぶ層は Tailscale に差し替え可 |
| 複数人 | OSS 版は弱く、Operator（有償）で解決 | ユーザー名単位の多重接続とヘッダー振り分けが中核 |
| 本番観察 | 読み取り専用で可能 | 意図的に禁止 |

どちらも「ローカルのプロセスが、向こうで動いているように見える」。違いは見せ方で、mirrord はプロセスの目を騙し、tetherd はプロセスの足元の配線を替える。ネットワークの透過度は同等、ファイルシステムだけ tetherd は持たない。壊れる場所は mirrord がランタイムの数だけあるのに対し、tetherd は OS の数（2 つ）だけ。

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
| sshuttle / tun2socks / gvisor-tap-vsock | 汎用 | 透過（pf/iptables + ユーザー空間スタック） | — | — | §6 の技術的前例 |
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
| 複数インスタンス | タスクを 1 つ選んで繋ぐ | リクエストの着地インスタンスを制御できない。dev は min=max=1 なら (a) で足りる。それ以外は (b) のリレーが必要 |
| CPU / スケール | 常時割り当て | リクエスト外は CPU スロットリング。(a) は WebSocket がリクエスト扱いで CPU が付くが 60 分で切れるので再接続。(b) は CPU 常時割り当てが必要。scale-to-zero を避けるため min=1 |
| env | agent が /proc から | 同じ（マルチコンテナで PID 共有可否は要確認。不可なら Secret Manager 経由のフォールバック） |
| IAM の透過 | 169.254.170.2（タスクロール） | metadata.google.internal（サービスアカウント） |
| 認証 | IAM（ssm:StartSession） | Cloud Run IAM（invoker）+ ID トークン |

**実装方針**: v1 ではプロバイダのインターフェースを切らず、ECS 固有部分を `internal/provider/ecs` に寄せるだけにする（実装が 1 つの段階で抽象を切ると ECS の都合に歪む）。ただし③運ぶ層は ECS 版の中で SSM と Tailscale の 2 択を想定しているので、ここだけは最初からインターフェースを持つ。Cloud Run を足すときに `provider/cloudrun` を書きながら共通インターフェースを抽出する。リレーサービスが必要になれば同じリポジトリのオプションコンポーネントとして置く。

---

## 14. 未決事項

- 外向き（インターネット宛）の通信を既定で agent 経由にするか、ラップトップから出すか。mirrord は前者（egress IP まで Pod）、開発体験としては後者のほうが速い。`network.local` の既定値の問題
- macOS の pf は `route-to` で utun に向ける方式と、`rdr` でローカルポートに向けて元宛先を `DIOCNATLOOK` で引く方式のどちらが安定か
- Linux の netns 方式で、子プロセスが localhost（ホスト側）のサービスに繋ぎたい場合の扱い（netns 内の lo はホストと別。`network.local` の localhost を netstack がホストの lo に橋渡しする）
- IPv6 を netstack で扱うか、v0 では捨てるか
- WebSocket / SSE / gRPC(HTTP/2) の steal
- agent イメージの配布先（ECR Public か GHCR か）
- Cloud Run のマルチコンテナで PID 名前空間の共有が可能か（env 取得方式に影響）

---

## 15. 最初に確認すべきこと（PoC の論点）

順序や期間は決めないが、設計の妥当性を左右する検証点は次の通り。

- Linux netns + gVisor netstack + SSM で、`psql -h <rds-endpoint>` がコード変更なしで通ること（②終端層の成立）
- SSM 経由のレイテンシ（フロー確立と RTT）が DB クエリの体感として許容範囲か
- `pidMode: task` + `SYS_PTRACE` で agent が app の `/proc/<pid>/environ` を読めること
- 169.254.170.2 が透過で通り、ローカルの AWS SDK がタスクロールとして動くこと
- macOS で pf `group` マッチ + utun `route-to` がプロセス単位で機能すること、`/etc/resolver/` による per-domain 転送が Go / Node / JVM で効くこと