# 設定ファイル

tetherd は 2 つの設定ファイルを読む。

| ファイル | 誰のもの | git | 何を書くか |
|---|---|---|---|
| `.tetherd.yml` | チーム共有 | **コミットする** | どのクラスタ・サービスに繋ぐか、何をリモートにするか |
| `~/.tetherd/config.yml` | 個人 | しない（ホームディレクトリ） | 自分のユーザー名、steal トークン、自分だけの AWS プロファイル |

優先順位は **フラグ > `~/.tetherd/config.yml` > `.tetherd.yml` > 既定値**。
「フラグで指定されたか」は値が空かどうかではなく `--flag` が実際に渡されたかで判定するので、既定値と同じ値を明示的に渡してもフラグが勝つ。

`.tetherd.yml` はカレントディレクトリから上に向かって探す（`/` まで）。見つけたパスは起動時の `config` 行に出るので、意図しないファイルを拾っていればそこで分かる。`--config <path>` で明示指定もでき、そのパスが存在しなければエラーになる（探索で見つからないのは正常、明示したものが無いのはタイプミス）。

## `.tetherd.yml`

```yaml
version: 1                      # 必須。無いとエラーになる
aws:
  profile: myapp-dev            # 個人設定で上書き可
  region: ap-northeast-1
target:
  cluster: myapp-dev
  service: api
  container: app                # 意図して読まれない（効くのは agent 側の TETHERD_APP_CONTAINER）
  env: dev                      # agent の TETHERD_ENV と照合し、違えば接続を拒否する
env:
  override:                     # タスクの env の上にかぶせる
    PORT: "8080"
  exclude: []                   # タスクの env から落とす名前（`LC_*` のような前置一致も可）
network:
  remote_cidrs: []              # agent 経由にする宛先を追加する。VPC CIDR は自動
  local_cidrs: []               # 上の範囲から除外して、ラップトップから直接出す
  remote_domains: []            # /etc/resolver 経由で agent 側に解決させるドメイン
  remote_services: []           # s3 | dynamodb
  pin_credential_route: false   # env を読まないツールのための逃げ道（下記。マシン全体に効く）
incoming:                       # v0.3a で有効。steal（下記）の受け口
  local_port: 8080
  match:
    header: X-Dev-User
    token_header: X-Dev-Token
```

キーを 1 つずつ:

- `version` — 今は `1` のみ。将来フォーマットを変えたときに、古い tetherd が「upgrade してください」と言えるようにするためのもの。未知のキーもエラーにするので、綴り間違いは黙って無視されない
- `aws.profile` / `aws.region` — SDK の既定チェーンの代わりに使うプロファイルとリージョン。個人設定の同じキーが勝つ
- `target.cluster` / `target.service` — 接続先の ECS サービス。この 2 つが書いてあれば `tetherd run -- <cmd>` だけで動く
- `target.container` — **意図して読まれない**（`incoming` は v0.3a/v0.3b で読まれるようになったが、このキーは違う）。env を読むコンテナを決めているのは**タスク定義の agent サイドカーに渡す `TETHERD_APP_CONTAINER`**（既定 `app`）で、CLI 側のこのキーではない。agent は `pidMode: task` で共有した pid 名前空間から、そのコンテナの最古のプロセスの `/proc/<pid>/environ` を読む。つまりアプリのコンテナ名が `web` なら、ここに `web` と書いても何も起きず、agent の環境変数に `TETHERD_APP_CONTAINER=web` を設定する必要がある（設定しないと `task env` が「container "app" is not in the task」で失敗する）。**配線しないことを v0.3b で決めた**: どのコンテナの env を読むかは agent の仕事で、agent には既に `TETHERD_APP_CONTAINER` がある。CLI 側からもう 1 つ名前を渡せば同じ事実が 2 箇所に散り、食い違ったときに黙って間違った env を注入する。キー自体は残す（`.tetherd.yml` は未知のキーをエラーにするので、消すと既にコミットされている設定ファイルが全部パースエラーになる）
- `target.env` — 環境ガード。agent が名乗る `TETHERD_ENV` と一致しなければ接続を拒否する。prod のタスクに誤って繋ぐのを防ぐための最後の砦
- `env.override` — タスクの env より強い。ローカルのポートだけ変えたいときなど
- `env.exclude` — タスクの env から落とす名前。tetherd が常に落とすもの（下記）に追加される
- `network.remote_cidrs` — VPC CIDR に**足す**宛先。ピアリング先の VPC、Transit Gateway の向こう側など。`--remote-cidr` と足し合わせになる（置き換えではない）
- `network.local_cidrs` — リモート集合から**引く**範囲。自宅 LAN が VPC CIDR と重なっているときの逃げ道
- `network.remote_domains` — このドメインの名前解決を agent に任せる。Cloud Map やプライベートホストゾーンの名前がここに入る
- `network.remote_services` — `s3` と `dynamodb` のみ。その managed prefix list の範囲をリモート集合に足す
- `network.pin_credential_route` — 既定 `false`。`169.254.170.2` を lo0 に固定して、env を読まずにこのアドレスを直書きしているツールにもタスクロールを届ける。**マシン全体に効く**ので、必要なときだけ（下記）
- `incoming` — v0.3a で有効。ALB に届いたリクエストのうち、`incoming.match.header`（既定 `X-Dev-User`）と `incoming.match.token_header`（既定 `X-Dev-Token`）の両方が一致するものだけを `run` 中のラップトップの `incoming.local_port`（既定 `8080`）に転送する（agent 側の steal。spec §5.2）。一致しないリクエストと、ヘッダーの無いヘルスチェック / WebSocket upgrade は常にタスクの app へ素通しする。既定値はこの CLI が適用するもので、`internal/config` 自体は何も既定を持たない（キーを省略すればそのフィールドはゼロ値のまま CLI に渡る）。フラグとの対応は `--no-incoming`（steal を止めて常時素通しにする）、`--local-port <port>`（`incoming.local_port` の上書き）、`--as <user>`（`~/.tetherd/config.yml` の `user` の上書き。一致条件の片方になる）。この配線により agent は誰も繋いでいなくても常に ALB のデータパス上に居続ける（素通しになるだけで、経路から外れるわけではない）ので、ALB のヘルスチェックは常に agent 経由で app に届く必要があり、agent が死ねばヘルスチェックも失敗する — agent の健全性がそのままサービスの健全性になる。**v0.3b から `tetherd run` はサービスの全タスクに接続する**（下記）

## `~/.tetherd/config.yml`

初回の `tetherd run` が 0600 で作る。中身は:

```yaml
user: shota
token: <base64url 32 bytes>
aws:
  profile: myapp-dev-shota      # 共有設定より強い
```

- `user` — agent に名乗る名前（`hello.user` ＝ `X-Dev-User` に載る値）。`--as` で上書きできる。ALB から自分宛のリクエストを識別する steal の一致条件の片方（v0.3a）
- `token` — steal の一致条件の片方になる秘密。`run` の開始時に `hello` で agent に渡り、`X-Dev-Token` ヘッダーと比較される。**ファイルは 0600 で、一度作られたトークンは再生成されない**。手で作ったファイルにトークンが無ければ、他のキーを保ったまま書き足す。ALB は public なので `X-Dev-User` と `X-Dev-Token` の両方が一致しない限りラップトップには届かない — `user` は秘密ではなく誰でも知り得る名前なので、片方だけでは steal は起きない。**トークンは任意ではなく必須の照合条件**であり、無くても動く利便のための仕組みではない
- `aws.profile` / `aws.region` — 共有設定の `aws` ブロックを個人的に上書きする。チームで 1 つのアカウントを共有していない場合に使う

トークンが漏れた（かもしれない）ときは `tetherd token rotate` で差し替える。**他のキー（`user`、`aws`）はそのまま残り**、ファイルは 0600 に直され（手で 0644 で作ったファイルも締め直す）、新しいトークンが標準出力に出る（ModHeader 側の `X-Dev-Token` を貼り替えるため）。**既に走っている `tetherd run` は古いトークンを持ち続ける** — agent は attach 時の `hello` で受け取った値と比較するため、ファイルを書き換えても走行中のセッションには効かない。漏れを閉じるには全てのセッションを再起動する。ファイルが無いマシンでは作成される（初回 `run` と同じ 0600 / 同じ生成器）。なお**書き戻しはこの構造体の再シリアライズなので、ファイルは整形され直す**（コメントは消え、キーの順序は `user` → `token` → `aws` になり、インデントは 4 スペースになり、`aws` に `profile` だけ書いていた場合は `region: ""` が明示的に付く）。いずれも動作には影響しない（空の値は読み側で無視される）が、手で編集してコメントを入れているなら消えることを承知しておくこと。`tetherd run` がトークンを書き足すとき（`token` の無いファイル）も同じ整形が起きる。

### 全タスクに接続する（v0.3b）

`tetherd run` はサービスの**対象タスク全部**に接続する（spec §6.2 / §6.3）。どのタスクにリクエストを落とすかを決めるのは ALB なので、1 タスクだけに繋いでいると `desired_count` が 2 以上のサービスでは steal が当たるか外れるかが運になる — これが v0.3b の本体。

- 起動時の `target` 行が `tetherd-dev/api  2 tasks (a1b2c3d4… primary, e5f6a7b8…)  (started 12m ago)` の形になる（1 タスクのときは従来の `task <id>` のまま）
- **primary** は起動が最も古いタスク。`dial`（pf で捕捉した TCP）・DNS の `resolve`・注入する env はここを通る。steal はどのタスクからでも受ける
- 10 秒おきにタスク一覧を読み直し、新しいタスクには接続し、消えたタスクは片付ける（`↻ session` 行）。rolling deploy の最中でも `run` は生き続け、新しいタスクに繋ぎ直る
- secondary が 1 本落ちても `run` は終わらない（そのタスク宛の steal だけが止まる）。**全**セッションが失われたときだけ `✗ agent session lost` で終了する
- primary のタスクが落ちたら、生き残りのうち最も古いものが primary に昇格する（`↻ session   task … went away; dial and DNS now go through task …`）
- `--task ID` を渡した場合はそのタスク 1 本だけ。追従もしない

### 誰がどのタスクに繋いでいるかを見る（`tetherd status`）

steal は共有の仕組みで、同じ dev サービスに複数人が繋ぐ。「自分のリクエストが来ない」の原因は多くの場合ヘッダーではなく人で、`tetherd status` がタスクごとに**誰が・どこから・いつから**繋いでいるかを出す。

```
  task a1b2c3d4…  (started 12m ago)
    abe    from 124.35.91.195  attached 3m ago
    shota  from 203.0.113.9    attached 11m ago
  task e5f6a7b8…  (started 2m ago)
    (nobody attached)
```

- `status` は**読むためだけに attach する**。開くセッションは `incoming` を無効にし、トークンを送らないので、他人のリクエストが `status` の側に流れ込むことはない（`status` 自身が steal の対象にならない）
- 読めなかったタスクも行として出て理由が付く（`(not read: …)`）。1 タスクが不通のサービスは「半分のリクエストだけ steal できる」状態で、それを見せるのがこのコマンドの仕事なので、最初の失敗で止まらない
- タスクの agent が古い（v0.3a）場合は名前だけが出て、どこから・いつからは出ない。その旨の 1 行が付く
- 探索は `run` と同じ（`.tetherd.yml` の `target`、`--task ID` も効く）。繋ぐ先が `run` と違えば診断にならないため

### ブラウザから steal を試す

`curl` と違い、ブラウザは `X-Dev-User` / `X-Dev-Token` を自分では付けない。ModHeader のような拡張機能で、dev の ALB のドメインだけに絞ったプロファイルを作り、2 つのヘッダーを常時付与するのが手っ取り早い。プロファイルを他のドメインに広げないこと（`token` はその拡張機能の設定に平文で残るため、dev 用の ALB 以外に漏らす意味も理由も無い）。チームで配るなら、ドメインは書いてもトークンは書かない ModHeader プロファイルを配布し、各自が自分の `~/.tetherd/config.yml` の `token` を値として入れる運用にする。

### 信頼境界

`.tetherd.yml` は**信頼された入力**として扱う。`env.override` は子プロセスの `PATH` や `DYLD_INSERT_LIBRARIES` も設定できるため、悪意のある `.tetherd.yml` を含むリポジトリで `tetherd run` すると任意コード実行になる。ただし `tetherd run -- go run ./cmd/api` はそもそもそのリポジトリのコードを実行するので、これは `env.override` があること自体に内在する性質であり、tetherd が新たに作った穴ではない。信頼していないリポジトリのコードを実行しないこと、という通常の前提がそのまま当てはまる。

## `tetherd env` と `tetherd run` の差

`tetherd env` は「`run` がタスクから注入する変数」を出す。上の除外リストと `env.override` / `env.exclude` は同じように適用されるので、`eval "$(tetherd env --format shell)"` でシェルに取り込んでも `HOME` や `PATH` は自分のものが残る。

ただし AWS の身元に関する変数だけは一致しない。`run` は透過モードで捕捉が張れているときだけ、タスクロールを使わせるために `AWS_CONFIG_FILE` / `AWS_SHARED_CREDENTIALS_FILE` を空ファイルに向け、ローカルの認証情報変数を取り除き、リージョンを補う。`env` は捕捉を張らないので、その判定ができず何もしない。`169.254.170.2` を指す 4 変数も、`run` は透過モードではループバック口に向け直す（下記）が、`env` は常に落とす（その口はセッション中しか無いため）。

つまり `eval "$(tetherd env)"` した後のシェルでは、AWS の身元は**自分のまま**。タスクロールで何かを実行したいなら `tetherd run -- <cmd>` を使う。

## リモート集合の組み立て

`tetherd run` が捕捉する宛先は次の式で決まる。

```
(VPC CIDR) + remote_cidrs + (remote_services の prefix list) + (pin_credential_route のとき 169.254.170.0/24) − local_cidrs
```

- **VPC CIDR** はタスクのサブネットから自動で引く（`--transport ssm` のとき）。書く必要はない
- **169.254.170.0/24** はタスクロールの認証情報エンドポイント。既定では捕捉範囲に**入らない** — 認証情報はループバック口から配るので、このアドレスに誰も接続しない。`pin_credential_route: true` にしたときだけ入る
- **`remote_services`** は `com.amazonaws.<region>.s3` のような managed prefix list を引いて、その IPv4 prefix を足す（`ap-northeast-1` の S3 は 15 件。件数はリージョンごとに違い、API は 1 ページ 100 件で切るので tetherd は `NextToken` を最後まで追う）。S3 と DynamoDB だけが対象（この 2 つだけが gateway endpoint を持ち、通信がパブリックアドレスのまま VPC を出るため、`aws:SourceVpc` のようなネットワーク条件付きポリシーを満たすには**タスクの ENI から出る必要がある**）。インターフェース型エンドポイントの他サービスは VPC 内の IP に解決されるので `remote_domains` の側で扱う
- **`local_cidrs`** は最後に引かれる。引き算は範囲を分割する正確なもので、`10.0.0.0/16` から `10.0.5.0/24` を引くと残りは 8 個のプレフィックスになる（pf は 1 つのテーブルに集合として持つので数は問題にならない）。`pin_credential_route` を有効にしているときだけ `169.254.170.0/24` が引かれない床になり、`local_cidrs` に何を書いても残る — 固定しておきながら捕捉から外すと、そのアドレスが lo0 に吸い込まれたまま誰も応答しない状態になるため。`local_cidrs` が（床以外の）すべてを消した場合は起動時にエラーになり、`local_cidrs` が名指しされる（`10.0.0.0/8` と書いて `10.0.0.0/16` の VPC を丸ごと消してしまう、が現実的な失敗）

起動時の `✓ network` 行に、この計算結果がそのまま出る。`tetherd doctor` が表示する集合も同じ関数から出るので、両者がずれることはない。

## DNS

`remote_domains` が空なら tetherd は `/etc/resolver` に触らない。

空でなければ:

1. CLI が `127.0.0.1:53530` に DNS サーバを立てる（ポートが埋まっていれば空きポートを取る）
2. helper が `/etc/resolver/<domain>` を作る。中身は `# managed by tetherd` / `nameserver 127.0.0.1` / `port <上のポート>`
3. そのドメインの問い合わせが CLI のリゾルバに来て、`resolve` ストリームで agent に転送され、agent がタスクの `resolv.conf`（= VPC リゾルバ）で解決する
4. `tetherd run` が終わると `/etc/resolver/<domain>` は消える。`# managed by tetherd` ヘッダーが無いファイルには最初から触らない

A レコードだけ答える。捕捉範囲は IPv4 のみなので、AAAA を返すと tetherd が運べない宛先に子プロセスを送ってしまうため。

注意: macOS は**負の応答もキャッシュする**。agent が名前解決に対応する前や、サービスがまだ登録される前にその名前を引くと、`mDNSResponder` が NXDOMAIN を TTL の間覚えてしまい、`tetherd run` の中でも `getaddrinfo` が失敗し続ける（`dig +short @127.0.0.1 -p 53530 <name>` で直接引くと正しく答えるので、これで切り分けられる）。解消するには:

```
sudo dscacheutil -flushcache; sudo killall -HUP mDNSResponder
```

注意: `/etc/resolver` を尊重するのは `getaddrinfo` 経由の解決だけ。Go の既定、Node の `dns.lookup`、JVM は尊重する。Node の `dns.resolve*`（c-ares）や `dig` は尊重しないので、それらで引くと解決できない。

RDS / ElastiCache / 内部 ALB のエンドポイント名はパブリック DNS でプライベート IP に解決でき、その IP が VPC CIDR に入るので、典型的な構成では `remote_domains` は不要。

## 常に除外される環境変数

`env.exclude` に書かなくても、tetherd は次を子プロセスに注入しない。

**ラップトップ側の値を守るもの** — `PATH`, `HOME`, `HOSTNAME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `PWD`, `OLDPWD`, `TERM`, `LANG`, `LC_*`, `SHLVL`, `_`, `AWS_EXECUTION_ENV`
コンテナの値を持ち込むと、子プロセスが自分のいる場所を誤認する。

**コンテナのファイルシステムを指すもの** — TLS 信頼ストア（`SSL_CERT_FILE`, `SSL_CERT_DIR`, `AWS_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`）、動的ローダ（`LD_LIBRARY_PATH`, `LD_PRELOAD`, `DYLD_LIBRARY_PATH`, `DYLD_INSERT_LIBRARIES`）、言語ランタイムの場所（`JAVA_HOME`, `GOROOT`, `PYTHONHOME`, `PYTHONPATH`）
実機で踏んだ: distroless イメージが `SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt` を設定していて、macOS にそのパスは無いため Go の子プロセスの TLS が全部 `certificate signed by unknown authority` で失敗した。

**ローカルの AWS 認証情報**（これは逆方向 — 子プロセスの env から取り除く） — `AWS_PROFILE`, `AWS_DEFAULT_PROFILE`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_CREDENTIAL_EXPIRATION`, `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`, `AWS_ROLE_SESSION_NAME`, `AWS_CONTAINER_CREDENTIALS_FULL_URI`, `AWS_CONTAINER_AUTHORIZATION_TOKEN`, `AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE`, `AWS_ACCESS_KEY`, `AWS_SECRET_KEY`, `AWS_SECURITY_TOKEN`
どれも SDK のチェーンでコンテナ認証情報より先に解決されるので、残っているとタスクロールにならない。これらを外すのは透過モードでタスクロールを配れているときだけで、そうでなければ開発者自身の身元のまま動かす（`✓ iam` 行に出る）。なお `AWS_CONTAINER_CREDENTIALS_FULL_URI` はこの一覧にあるが、これは**開発者が export していたもの**を落とすためで、`run` はそのあと自分のループバック口の値を入れ直す。

**タスク内でしか意味を持つもの** — `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`, `ECS_CONTAINER_METADATA_URI_V4`, `ECS_CONTAINER_METADATA_URI`, `ECS_AGENT_URI`
どれもタスクの中にしか無い `169.254.170.2` を指すので、そのまま渡しても子プロセスからは届かない。`--no-network` では 4 つとも落とす。透過モードでの扱いは 2 通り:

- メタデータの 3 変数（`ECS_CONTAINER_METADATA_URI_V4`, `ECS_CONTAINER_METADATA_URI`, `ECS_AGENT_URI`）は**ホストだけをループバック口に書き換える**。パスはそのまま転送されるので、1 ポートで 3 つとも賄える
- `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` は**取り除いて**、代わりに `AWS_CONTAINER_CREDENTIALS_FULL_URI` をループバック口の URL で与える。空文字にするのでは駄目で、消さなければならない: aws-sdk-go-v2 は relative を full より先に評価し、botocore は**値ではなく変数の存在**で分岐する（`return self.ENV_VAR in self._environ`）ので、空文字を残すと `aws` CLI と boto3 は `169.254.170.2` を叩きに行って認証情報を得られない

タスクの env の中に、この 4 つ以外でも `169.254.170.2` を含む値があれば（アプリ独自の変数など）、`run` が `⚠ env` 行でその名前を挙げる。書き換えようがないので、必要なら `pin_credential_route` を使う。

## `pin_credential_route` を使うとき

既定では、タスクロールの認証情報は CLI が開く `127.0.0.1:<空きポート>` の口から配られ、子プロセスには env（`AWS_CONTAINER_CREDENTIALS_FULL_URI` とメタデータの 3 変数）でそこを教える。AWS の各言語 SDK と `aws` CLI はこれに従う。root 操作も要らない。

従わないのは、env を読まずに `169.254.170.2` を直書きしているツールだけ。それに当たったときに `pin_credential_route: true` にすると、helper が `169.254.170.2` の host route を lo0 に張り、pf がそれを agent 経由に流す（`tetherd-helper` が動いている必要がある）。

有効にする前に知っておくこと:

- 固定は**マシン全体**に効く。セッション中は `tetherd` グループ以外のプロセスも `169.254.170.2` で dev タスクの認証情報に到達できる（pf の `rdr` は `group` 句を受け付けないので gid で絞れない）
- `amazon-ecs-local-container-endpoints` のようにこのアドレスをローカルで使うツールと衝突する。そちらが**タスクの**ロールを掴む形になる
- **`tetherd doctor` はこの設定をまだ見ない。** v0.3b で追加すると書いてあったが入らなかった（v0.3b の `doctor` に入ったのは `?` ステータス・`steal` の行・`task role` の行）。有効かどうかは `.tetherd.yml` と `run` の `✓ network` 行（有効なときだけ `remote:` に `169.254.170.0/24` が入る）で確認する
- `local_cidrs` に何を書いても `169.254.170.0/24` は捕捉から外れない（外すと誰も応答しないアドレスになる）

なお既定のループバック口も無認証で、**同じマシンのどのプロセスからでも**叩けばタスクロールの認証情報を得られる（ポートは毎回変わるが、秘密ではない）。信頼境界は SSM のローカルフォワードと同じ「同一マシンに閉じるが、それ自体が境界」。固定方式より狭いのは「アドレスが予測できない」点だけ。
