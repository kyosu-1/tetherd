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
  container: app                # env を読むコンテナ（既定 app）
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
incoming:                       # v0.3。今は読まれるだけで使われない
  local_port: 8080
  match:
    header: X-Dev-User
    token_header: X-Dev-Token
```

キーを 1 つずつ:

- `version` — 今は `1` のみ。将来フォーマットを変えたときに、古い tetherd が「upgrade してください」と言えるようにするためのもの。未知のキーもエラーにするので、綴り間違いは黙って無視されない
- `aws.profile` / `aws.region` — SDK の既定チェーンの代わりに使うプロファイルとリージョン。個人設定の同じキーが勝つ
- `target.cluster` / `target.service` — 接続先の ECS サービス。この 2 つが書いてあれば `tetherd run -- <cmd>` だけで動く
- `target.container` — env を読むコンテナ名。agent は `pidMode: task` で共有した pid 名前空間からこのコンテナの最古のプロセスの `/proc/<pid>/environ` を読む
- `target.env` — 環境ガード。agent が名乗る `TETHERD_ENV` と一致しなければ接続を拒否する。prod のタスクに誤って繋ぐのを防ぐための最後の砦
- `env.override` — タスクの env より強い。ローカルのポートだけ変えたいときなど
- `env.exclude` — タスクの env から落とす名前。tetherd が常に落とすもの（下記）に追加される
- `network.remote_cidrs` — VPC CIDR に**足す**宛先。ピアリング先の VPC、Transit Gateway の向こう側など。`--remote-cidr` と足し合わせになる（置き換えではない）
- `network.local_cidrs` — リモート集合から**引く**範囲。自宅 LAN が VPC CIDR と重なっているときの逃げ道
- `network.remote_domains` — このドメインの名前解決を agent に任せる。Cloud Map やプライベートホストゾーンの名前がここに入る
- `network.remote_services` — `s3` と `dynamodb` のみ。その managed prefix list の範囲をリモート集合に足す

## `~/.tetherd/config.yml`

初回の `tetherd run` が 0600 で作る。中身は:

```yaml
user: shota
token: <base64url 32 bytes>
aws:
  profile: myapp-dev-shota      # 共有設定より強い
```

- `user` — agent に名乗る名前。`--user` で上書きできる。ALB から自分宛のリクエストを識別するのにも使う（v0.3）
- `token` — v0.3 の steal で `X-Dev-Token` として照合される秘密。**ファイルは 0600 で、一度作られたトークンは再生成されない**。手で作ったファイルにトークンが無ければ、他のキーを保ったまま書き足す
- `aws.profile` / `aws.region` — 共有設定の `aws` ブロックを個人的に上書きする。チームで 1 つのアカウントを共有していない場合に使う

### 信頼境界

`.tetherd.yml` は**信頼された入力**として扱う。`env.override` は子プロセスの `PATH` や `DYLD_INSERT_LIBRARIES` も設定できるため、悪意のある `.tetherd.yml` を含むリポジトリで `tetherd run` すると任意コード実行になる。ただし `tetherd run -- go run ./cmd/api` はそもそもそのリポジトリのコードを実行するので、これは `env.override` があること自体に内在する性質であり、tetherd が新たに作った穴ではない。信頼していないリポジトリのコードを実行しないこと、という通常の前提がそのまま当てはまる。

## `tetherd env` と `tetherd run` の差

`tetherd env` は「`run` がタスクから注入する変数」を出す。上の除外リストと `env.override` / `env.exclude` は同じように適用されるので、`eval "$(tetherd env --format shell)"` でシェルに取り込んでも `HOME` や `PATH` は自分のものが残る。

ただし AWS の身元に関する変数だけは一致しない。`run` は透過モードで捕捉が張れているときだけ、タスクロールを使わせるために `AWS_CONFIG_FILE` / `AWS_SHARED_CREDENTIALS_FILE` を空ファイルに向け、ローカルの認証情報変数を取り除き、リージョンを補う。`env` は捕捉を張らないので、その判定ができず何もしない。`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` などの 4 変数も、`run` は透過モードで残す（agent 経由で `169.254.170.2` に届く）が、`env` は常に落とす（誰も応答しないため）。

つまり `eval "$(tetherd env)"` した後のシェルでは、AWS の身元は**自分のまま**。タスクロールで何かを実行したいなら `tetherd run -- <cmd>` を使う。

## リモート集合の組み立て

`tetherd run` が捕捉する宛先は次の式で決まる。

```
(VPC CIDR) + 169.254.170.0/24 + remote_cidrs + (remote_services の prefix list) − local_cidrs
```

- **VPC CIDR** はタスクのサブネットから自動で引く（`--transport ssm` のとき）。書く必要はない
- **169.254.170.0/24** はタスクロールの認証情報エンドポイント。これが捕捉範囲に入っていることが、ローカルの SDK がタスクロールで署名できる条件
- **`remote_services`** は `com.amazonaws.<region>.s3` のような managed prefix list を引いて、その IPv4 prefix を足す（`ap-northeast-1` の S3 は 15 件。件数はリージョンごとに違い、API は 1 ページ 100 件で切るので tetherd は `NextToken` を最後まで追う）。S3 と DynamoDB だけが対象（この 2 つだけが gateway endpoint を持ち、通信がパブリックアドレスのまま VPC を出るため、`aws:SourceVpc` のようなネットワーク条件付きポリシーを満たすには**タスクの ENI から出る必要がある**）。インターフェース型エンドポイントの他サービスは VPC 内の IP に解決されるので `remote_domains` の側で扱う
- **`local_cidrs`** は最後に引かれる。引き算は範囲を分割する正確なもので、`10.0.0.0/16` から `10.0.5.0/24` を引くと残りは 8 個のプレフィックスになる（pf は 1 つのテーブルに集合として持つので数は問題にならない）。ただし `169.254.170.0/24` は引かれない床で、`local_cidrs` に何を書いても残る — ここが外れると子プロセスはタスクロールを失い、開発者自身の身元で動いてしまうため。`local_cidrs` が床以外のすべてを消した場合は起動時にエラーになり、`local_cidrs` が名指しされる（`10.0.0.0/8` と書いて `10.0.0.0/16` の VPC を丸ごと消してしまう、が現実的な失敗）

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
どれも SDK のチェーンでコンテナ認証情報より先に解決されるので、残っているとタスクロールにならない。これらを外すのは透過モードでタスクロールのエンドポイントが捕捉できているときだけで、そうでなければ開発者自身の身元のまま動かす（`✓ iam` 行に出る）。

**タスク内でしか意味を持つもの**（`--no-network` のときだけ落とす） — `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`, `ECS_CONTAINER_METADATA_URI_V4`, `ECS_CONTAINER_METADATA_URI`, `ECS_AGENT_URI`
これらは `169.254.170.2` を指す。透過モードでは agent 経由で届くので残すが、捕捉していないモードでは誰も応答しないので落とす。
