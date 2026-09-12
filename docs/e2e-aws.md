# AWS e2e（手動）

`deploy/dev-env` を立てた状態で、手元の Mac から確認する。所要 10 分。

前提: `sudo tetherd-helper` が動いている（v0.4 までは `hack/e2e-local.sh` と同じく前面起動: `sudo ./bin/tetherd-helper --socket /var/run/tetherd.sock --exec-src $PWD/bin/tetherd-exec &`）、`aws login --profile personal` 済み、`session-manager-plugin` あり、`psql` あり（`brew install libpq`）。

```
cd deploy/dev-env && eval "RDS=$(terraform output -raw rds_endpoint)" && cd ../..
cp examples/.tetherd.yml .tetherd.yml   # v0.2b: これでフラグが要らなくなる
RUN="./bin/tetherd run"
```

v0.2b から、cluster / service / profile / region / env は `.tetherd.yml` から来る。フラグで上書きしたい場合は従来どおり `--cluster` などが効く（フラグ > 個人設定 > 共有設定）。

**helper（sudo）が要る行と要らない行**: `tetherd env`（11〜13）と `tetherd doctor`（14・15）は pf 捕捉を張らないので helper 不要。それ以外の `tetherd run` を使う行は helper が動いている必要がある。helper が無い状態の `doctor` は `✗ helper` を出すのが正しい挙動で、それ自体が 14 行目の検査対象。

`deploy/dev-env` の最初のタスク起動そのものが spec §12 の 5 番目の検証を兼ねる:
`pidMode: task` + `SYS_PTRACE` + ECS Exec + `restartPolicy` が `RegisterTaskDefinition` に受理され、タスクが `RUNNING` になっていること（そうでなければ `tetherd run` 自体が「no attachable task」で失敗する）。
なお agent コンテナ（distroless）の ECS Exec エージェントは SSM に接続できないことがあり、その場合 CLI は同じタスクの別コンテナ経由で転送する（§6.1）。`tetherd` の出力に `ssm target … is not connected; trying the next container` が出るのは正常。

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 0 | `./bin/tetherd run -- true` | exit 0。冒頭に `config     .tetherd.yml` 行が出る | 共有設定だけで探索・接続が解決し、フラグが 1 つも要らない（v0.2b の主目的） |
| 1 | `$RUN -- env \| grep -E '^(DB_PASSWORD\|FEATURE_FLAG\|PORT\|PATH)='` | `DB_PASSWORD=<24 文字>`、`FEATURE_FLAG=from-parameter-store`、`PORT=8081`、`PATH` はローカルのまま | Secrets Manager / Parameter Store 経由の値が `/proc` 経由で届き、除外リストが効いている |
| 2 | `$RUN -- psql "postgres://tetherd@$RDS/app?sslmode=require" -c 'select now()'` に `PGPASSWORD` を env から: `$RUN -- sh -c 'PGPASSWORD=$DB_PASSWORD psql -h '"$RDS"' -U tetherd -d app -c "select now()"'` | `now` の 1 行 | VPC 内の RDS に pf → SSM → agent 経由で届き、パスワードはタスクの secret |
| 3 | `$RUN -- aws sts get-caller-identity` | `Arn` が `assumed-role/tetherd-dev-api-task/…` | tetherd がループバック口で配る認証情報でローカルの SDK がタスクロールになる（§4.2）。起動時の `✓ iam` 行も同じ ARN で、`(via 127.0.0.1:<port> → the task)` が付く。`env -u AWS_PROFILE` の前置きはもう不要 — tetherd がローカルの `AWS_PROFILE` / `AWS_ACCESS_KEY_ID` 等を子プロセスの env から自動で取り除くので、手元に `aws login` 済みのプロファイルがあってもタスクロールが優先される（`✓ env      local AWS credentials (...) removed …` 行が出る）。ローカルの `~/.aws` の設定は子プロセスから隠されるので、`AWS_PROFILE` が何であっても結果は変わらない |
| 4 | `$RUN -- aws s3 ls` | バケット一覧（無ければ空）で **エラーなし** | VPC 外の AWS サービスにタスクロールで届く（署名ベース）。同じく、共有設定に引っ張られずタスクロールで署名される |
| 5 | `$RUN -- curl -s http://$(cd deploy/dev-env && terraform output -raw alb_dns_name)/` | `sampleapp on … from 10.0.x.x` | ALB は public なので、これは agent 経由ではなくラップトップから直接（`from` が ALB の IP）。既定「VPC 内だけリモート」の確認 |
| 6 | `$RUN -- curl -s http://api.myapp.internal:8081/` | `sampleapp on … from 10.0.x.x` | `remote_domains: [myapp.internal]` で Cloud Map の名前が VPC リゾルバ経由で解け、その IP が捕捉範囲に入って agent 経由で届く。別端末で実行中に `cat /etc/resolver/myapp.internal` の 1 行目が `# managed by tetherd`、`port` が起動ログの `DNS:` に出たポートと一致し、終了後にファイルが消えていること |
| 7 | `$RUN --env prod -- true` | `refusing to attach: agent reports TETHERD_ENV="dev", expected "prod"` で exit 1 | 環境ガード |
| 8 | 2 つ目のターミナルで `$RUN -- sleep 60` を動かしたまま 1 つ目で `$RUN -- true` | `another tetherd session is active` で exit 1 | 1 台 1 セッション |
| 9 | 実行中にトランスポートを落とす: `kill $(pgrep -n -f session-manager-plugin)` | `✗ agent session lost: … control stream closed: EOF` が即座に出て子プロセスが終了、exit 1、`sudo pfctl -a com.apple/900.tetherd -sr` が空 | セッション断の検出と片付け。`aws ecs stop-task` ではタスクが `DEACTIVATING`（ALB のデレジストレーション待ち、既定 300 秒）の間コンテナが動き続けるため、セッションはすぐには切れない。検出経路を試すならトランスポートを殺すほうが速く確実 |
| 10 | 別ターミナルで、実行中に `lsof -nP -iTCP -sTCP:LISTEN \| grep session-manager` | `127.0.0.1:<port>` だけが LISTEN（外部 IF には無い） | plugin のローカルフォワードは `127.0.0.1` にしか bind しない。無認証だが同一マシンに閉じることの確認（spec §11） |
| 11 | `./bin/tetherd env \| head -20` | `KEY=value` の一覧。`DB_PASSWORD=***` | secrets はタスク定義の `secrets` ブロックから名前を取ってマスクされる。値も長さも漏れない |
| 12 | `./bin/tetherd env --format json \| jq -r '.DB_PASSWORD, .PORT'` / `./bin/tetherd env --reveal --format json \| jq -r '.DB_PASSWORD'` | 前者は `***` と `8081`、後者は 24 文字の実値 | 既定はマスク、`--reveal` で実値。出力が JSON として妥当 |
| 13 | `eval "$(./bin/tetherd env --format shell)" && echo "$PORT"` | `8081` | 変数は stdout、ステータス行は stderr。混ざっていれば `eval` が壊れる |
| 14 | `./bin/tetherd doctor` | helper を起動していれば 13 行のうち 12 行が `✓`（`agent session` は `handshake ok, protocol 1, TETHERD_ENV=dev`、`task env` は読めた変数の数、`task role` はタスクロールの ARN と `(via 127.0.0.1:<port> → the task)`、`remote domains` は `myapp.internal … resolve through the agent`）、`steal` は自分のサーバーをまだ立てていなければ `⚠`（`nothing is listening on 127.0.0.1:<port>`。20 行目のようにサーバーを立ててから実行すれば `X-Dev-User (+ X-Dev-Token) → 127.0.0.1:<port>, which has a listener` で `✓`。`?` になるのは `incoming.local_port` がポート番号として不正なとき（`incoming.local_port: 70000` → `? steal  not checked: local port 70000 is not a port number`）か、probe が時間内に答えられなかったとき。トークンが無いケースは起きない — `doctor` 自身の `applyConfig` が `~/.tetherd/config.yml` にトークンを生成してから判定する。`?` はどの行でも exit code を動かさない）で exit 0。helper を落としていれば `✗ helper` とその次の一手が出て exit 1、**残りの行は出続ける** | 13 項目の検査と、失敗しても続けること。`agent session`・`task env`・`task role` は ECS の見解でなく tetherd 自身が通した経路の結果なので、サイドカーが落ちていれば他が全部緑でもここで落ちる。`task role` は `run` の `✓ iam` と同じ経路（loopback → セッション → タスクのエンドポイント）を通るので、`your AWS identity`（開発者自身の ARN）とは別の行 |
| 15 | `.tetherd.yml` の `service` を `nope` にして `./bin/tetherd doctor` | `✗ attachable task` に `no RUNNING tasks in service tetherd-dev/nope` が出て exit 1。他の行は出続ける | 1 つ失敗しても残りの検査が走る（1 問ずつ直して再実行させない） |
| 16 | `.tetherd.yml` に `remote_services: [s3]` を足して `$RUN -- sh -c 'aws s3 ls && curl -s -o /dev/null -w "%{http_code}\n" https://example.com'` | `aws s3 ls` が成功し `example.com` も 200。`✓ network` 行に S3 の prefix が並ぶ（ap-northeast-1 では 15 件） | prefix list 由来の CIDR が捕捉範囲に入る。件数はリージョンごとに違い、API は 1 ページ 100 件で切るので、ページングしていなければ大きいリージョンで静かに取りこぼす。それ以外の宛先はラップトップから直接出る |
| 17 | `ipconfig getifaddr en0` の /24 を `local_cidrs` に足して `$RUN -- true` と `./bin/tetherd doctor` | 起動時の重なり警告が消え、doctor の `local addresses` が `⚠` → `✓` | 分割による引き算（VPC /16 から自宅 /24 だけを抜く）が効いている |
| 18 | `local_cidrs: [10.0.0.0/8]`（VPC を丸ごと消す）で `$RUN -- true` | `network.local_cidrs excludes the entire remote set` で exit 2（設定の誤りなので usage 扱い） | 設定ミスで捕捉範囲が空になったら黙って起動しない。タスクロールのエンドポイントを足し戻して「1 件あるから OK」にしない |
| 19 | `$RUN -- python3 -c "import socket,time; t=time.time()\ntry: socket.gethostbyname('nope.myapp.internal')\nexcept socket.gaierror as e: print('gaierror in %.1fs' % (time.time()-t))"` | 1 秒未満で `gaierror` | 存在しない名前が NXDOMAIN として返る。SERVFAIL だと macOS がリトライして数秒待たされる |

v0.3a から、agent は常に ALB のデータパス上に居る（§5.1）。一致するヘッダーが無い限り app への素通しなので、これまでの 0〜19 行の挙動はそのまま変わらない。以下は steal（一致時にラップトップへ届く経路）の確認。

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 20 | `$RUN -- <自分のサーバ>` を起動した状態で `curl -H 'X-Dev-User: abe' -H "X-Dev-Token: $(grep token ~/.tetherd/config.yml \| awk '{print $2}')" http://<alb>/` | ラップトップのプロセスの応答。CLI に `← GET / 200 <ms> (from <自分の IP>)` が出る | ALB → agent → yamux → ラップトップが通る |
| 21 | ヘッダー無しで `curl http://<alb>/` | `sampleapp on ip-10-0-…`（タスクの応答） | 一致しないリクエストは素通し |
| 22 | トークンを 1 文字変えて `curl` | タスクの応答。ラップトップには来ない | 公開 ALB でユーザー名だけでは届かない |
| 23 | `$RUN` を Ctrl-C した直後に `curl -H 'X-Dev-User: abe' -H 'X-Dev-Token: …' http://<alb>/` | タスクの応答（502 ではない） | セッション断で即時に素通しへ復帰 |
| 24 | ラップトップのサーバだけ落として `curl`（`$RUN` は生かす） | タスクの応答。CLI に `502 nothing is listening on 127.0.0.1:8080` | dial 失敗はそのリクエストだけ app にフォールバック |
| 25 | `$RUN --no-incoming -- sleep 60` 中に一致するヘッダーで `curl` | タスクの応答。CLI に `✓ steal` 行が無い | `--no-incoming` は何も取らない |
| 26 | ALB のヘルスチェックが 2 分間 healthy のまま | ターゲットが healthy | ヘルスチェックは常に app に届く（agent 経由。§5.1） |

v0.3b から、`tetherd run` は**サービスの対象タスク全部**に接続し、rolling deploy に追従する（§6.2 / §6.3）。以下はそれと `status` / `token rotate` / `doctor` の確認。

**27 と 28 は `desired_count = 2` が必要**（`terraform apply -var desired_count=2`）。ALB がどのタスクにリクエストを落とすかを決めるので、1 タスクでは「どのタスクに落ちても届く」を検証できない。**終わったら 1 に戻す** — Fargate の課金が倍になる。

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 27 | `desired_count = 2` にして、まず**ヘッダー無し**で `for i in $(seq 20); do curl -s http://<alb>/; done \| sort \| uniq -c`。次に `$RUN -- <自分のサーバ>` を起動し、一致するヘッダーで同じ 20 回 | ヘッダー無しの 20 回が**2 つのタスク両方**から答える（`sampleapp on ip-10-0-…` が 2 種類出る）。`$RUN` の `target` 行が `2 tasks (<id>… primary, <id>…)`。一致するヘッダーの 20 回は**20 回すべて**ラップトップのプロセスが答え、CLI に `←` が 20 行出る。ラップトップ側の受信数も 20 | **どのタスクに落ちても steal できる（v0.3b の本体）。** ヘッダー無しの 1 周目を先に取るのが要点 — ALB が実際に 2 タスクへ振り分けていることを確かめずに 20/20 を見ても、たまたま片方に寄っただけで通ってしまう。v0.3a のコードは最も古い 1 本にしか繋がないので、この形では繋いでいないタスクに落ちた分が app の応答になる |
| 28 | `$RUN -- <自分のサーバ>` を動かしたまま別端末で `aws ecs update-service --cluster tetherd-dev --service api --force-new-deployment`。入れ替わりの間、一致するヘッダーで `curl` を続ける | 新しいタスクに `↻ session   task <id>… attached (N total)`（primary でなければ `(N total)`、primary なら `attached and is now the primary`）が出て繋がり、古いタスクが落ちると `↻ session   task <id>… went away (…); N left`。**`run` は生き続け、子プロセスも動き続ける**。primary が入れ替わった場合は `dial and DNS now go through task <id>…` が出る。`curl` はその間もラップトップに届く | deploy 追従。タスク一覧は 10 秒おきなので反映に最大 10 秒の遅れがある。**secondary が 1 本落ちても `run` は終わらない**こと（`✗ agent session lost` が出ないこと）が見どころ — 終わるのは全セッションを失ったときだけ |
| 29 | `$RUN` を別端末で生かしたまま `./bin/tetherd status`。続けて `$RUN` を止めてもう一度 | 1 回目はタスクごとの節に自分の名前・`from <自分の IP>`・`attached <n>s ago` が出る（`run` は全タスクに繋ぐので**両方のタスクに**出る）。2 回目は両タスクが `(nobody attached)`。exit 0。helper（sudo）は要らない | 共有時の診断。`status` は読むためだけに attach するので、**トークンを送らず steal の対象にならない**（29 の実行中に一致するヘッダーで `curl` してもタスクの応答になり、`status` 側には何も来ない）。タスクが 1 本読めなくても残りが出ること（`(not read: …)`）も、片方を `aws ecs stop-task` して確認できる |
| 30 | `$RUN` を生かしたまま別の端末で `./bin/tetherd token rotate`、その後 **古い**トークンのヘッダーで `curl` | 新しいトークンが表示され、`~/.tetherd/config.yml` は 0600 のまま `user` と `aws` も残る。**古いトークンの `curl` はまだラップトップに届く**（`$RUN` を再起動すると届かなくなり、新しいトークンで届くようになる） | 回転はファイルを差し替えるだけで、走行中のセッションは attach 時の `hello` の値で照合し続ける — 漏洩を閉じるには再起動が必要（出力もそう言う） |

所要時間の目安: `StartSession` → `welcome` まで 2〜4 秒、psql の接続確立 +50〜100 ms。

結果は `docs/specs/2026-09-12-v1-macos-design.md` §12 の 5・6、および v0.3a / v0.3b の節に追記する。

## v0.4 に持ち越した行

`doctor` のターゲットグループの検査は **v0.3b に入らなかった**（理由は spec §12 の v0.3b）。行が入るまでこの 31 行は実施できないので、番号だけ確保してここに置いてある。26〜30 行の番号は動かしていない。

**この行は dev 環境を一時的に壊す。** ターゲットグループを HTTP2 にすると ALB は h2c でタスクに話しかけるが、agent は HTTP/1.1 サーバなのでヘルスチェックが落ち、ターゲットが unhealthy になって ALB が 5xx を返す。**実施は最後に回し、確認できたらすぐ HTTP1 に戻すこと。** `protocol_version` は作成時にしか設定できない属性なので（`ModifyTargetGroup` では変えられない）Terraform は置き換えになるが、`create_before_destroy` + `name_prefix` が入っているので v0.3a の `ResourceInUse`（§12）は起きない。

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 31 | developer policy に `elasticloadbalancing:DescribeTargetGroups` を当てる**前**に `./bin/tetherd doctor`、当てた**後**にもう一度、最後に `alb.tf` の `aws_lb_target_group.app` に `protocol_version = "HTTP2"` を足して `terraform apply` してもう一度 | 当てる前は `? target group  not checked: …`（`elasticloadbalancing:DescribeTargetGroups` を名指しし、**exit code は 0 のまま**）。当てた後は `✓`。HTTP2 にすると `✗` で `protocol_version` を名指しして exit 1 | steal の要件検査（spec §5.1 が gRPC / HTTP2 を対象外としている）。`?` が exit code を動かさないこと — 権限が古い開発者の環境は壊れていないので `✗` にしてはいけない — が 1 段目の要点 |

## v0.4: 初回インストールの検証（32〜38、未実施）

v0.4 の主張は「tetherd が入っていない Mac で、`brew install` から始めて動く」（spec §8）。開発機はそうではない状態にある（`tetherd` グループ、`/usr/local/libexec/tetherd` が既に存在する）ので、これらの行を実施する前に `docs/uninstall.md` を**全部**実行し、開発機を初回インストール前の状態に戻す。以下の行はその後の検証計画であり、**cask がまだ `kyosu-1/homebrew-tap` に届いていない**（tap への反映は Task 5 でタグを打った人間にしか行えない）ため、**まだ実施できない**。番号だけ確保してある — 実施したら結果をここと spec §8 に追記する。

32 と 33 の後は brew で入れたバイナリと launchd 常駐の helper に対して検証する。0〜31 行のようにフォアグラウンドで起動した helper（`hack/e2e-local.sh` と同じ前面起動）に対する検証ではない。

36 行は `deploy/dev-env` に触る（`tetherd run` がタスクへ実際に接続する）。実施前に `desired_count` と ECS の課金状態を確認しておくこと。

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 32 | `docs/uninstall.md` を全部実行して開発機を未インストール状態に戻す → `brew install kyosu-1/tap/tetherd` | `tetherd` / `tetherd-helper` / `tetherd-exec` の**3 本**が `PATH` に入る（`which -a` で 3 つとも Homebrew prefix 配下を指す） | リリースアーカイブの中身が `.goreleaser.yml` の `archives.ids`（`tetherd`, `tetherd-helper`, `tetherd-exec`）および cask の `binaries`（同じ 3 つ）と一致していること（spec §8「3 バイナリを prefix に置くだけ」）。`internal/helper`'s `TestCaskAgreesWithTheGoConstants` は `.goreleaser.yml` の記述しか見ないので、実際に配られたアーカイブの中身と一致するかはこの行でしか確認できない |
| 33 | `sudo tetherd-helper install` | ログに `group tetherd: gid <n>`、`tetherd-helper: /usr/local/libexec/tetherd/tetherd-helper`、`tetherd-exec: ... (setgid tetherd)`、`plist: /Library/LaunchDaemons/dev.tetherd.helper.plist`、`launchd: bootstrapped ...` が出て（`cmd/tetherd-helper`の`doInstall`）、`/usr/local/libexec/tetherd/` に **2 本**（`tetherd-helper` と `tetherd-exec`）が入り、daemon が上がる | **`filepath.EvalSymlinks` が効いていること** — Homebrew は `tetherd-helper` を `bin` から staged path への symlink として置くので、`os.Executable()` の結果をそのまま使うと隣に `tetherd-exec` が見つからない（`cmd/tetherd-helper/main.go` の `doInstall`）。**シミュレーションでは検証できず、実機の brew install でしか分からない** |
| 34 | `sudo launchctl print system/dev.tetherd.helper` | 走っている状態で表示される。起動コマンドが `/usr/local/libexec/tetherd/tetherd-helper --socket /var/run/tetherd.sock --exec-src /usr/local/libexec/tetherd/tetherd-exec --install-dir /usr/local/libexec/tetherd` を指す | **Homebrew の prefix（`/opt/homebrew/...` や `/usr/local/Cellar/...`）を指していないこと** — plist が指すのは `install` が作った root 所有のコピーで、Homebrew 側の `tetherd-helper` ではない（`internal/helper/daemon.go`: root の LaunchDaemon がユーザ書き込み可能なパスを起動しないための構成）。**`RunAtLoad: true` で常駐していること**（Ruling S。plist に `Sockets` が無いので `RunAtLoad` 以外に起動するものが無い）。ソケットアクティベーションを実装したら、この行の期待値（「常駐している」）は変わる |
| 35 | `tetherd doctor` | `✓ helper`（resident daemon に dial できる）と `✓ setgid tetherd-exec`（`tetherd` グループが見つかり、その gid に setgid した `tetherd-exec` がある）が出る | **`internal/doctor` に「グループ」単独の行は無い** — グループの検証は `setgid tetherd-exec` 行の中（`internal/cli/doctor.go` のステップ 2、`internal/doctor/checks.go` の `CheckExecSetgid`）に含まれている。全体では 13 行中この 2 行が「今インストールしたものが動いているか」を直接見る行で、残り 11 行は dev 環境と agent 側の検証（0〜31 行で既に確認済み）。この 2 行が ✓ で exit 0 であること |
| 36 | `tetherd run -- aws sts get-caller-identity` | タスクロールの ARN | **brew で入れたバイナリで、かつ launchd 経由で常駐している helper に対して**一通り動く（フォアグラウンド起動の helper に対する検証は 0〜31 行で既に済んでいる）。3 の直接検証と同じ経路をここでは resident daemon 越しに通す |
| 37 | `sudo tetherd-helper install` をもう一度 | 33 と同じログが出て成功し、daemon が上がり直す（`launchctl bootout` してから `bootstrap` するので、走っていた古い daemon が新しい方に入れ替わる） | **冪等性。** `docs/install.md` の「Upgrading」節が `brew upgrade tetherd && sudo tetherd-helper install` をアップグレード手順として案内しているので、ここが動かないとその案内が嘘になる |
| 38 | `brew uninstall --cask tetherd` | daemon が落ち、`/Library/LaunchDaemons/dev.tetherd.helper.plist` と `/usr/local/libexec/tetherd` が消える | cask の `uninstall launchctl: [dev.tetherd.helper]` / `delete: [...]`（`.goreleaser.yml`）が**単独で**効くこと — ここでは `sudo tetherd-helper uninstall` を先に走らせない。`docs/install.md` が書いているとおり、これは安全網であって本体の手順ではないので、**`tetherd` グループはここでは消えない**（`brew uninstall` の後も `dscl . -read /Groups/tetherd` は見つかるはず）。真っさらに戻すには `docs/uninstall.md` の手順が別途要る |
