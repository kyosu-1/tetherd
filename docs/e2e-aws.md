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
| 14 | `./bin/tetherd doctor` | helper を起動していれば全 11 行 `✓`（`agent session` は `handshake ok, protocol 1, TETHERD_ENV=dev`、`task env` は読めた変数の数、`remote domains` は `myapp.internal … resolve through the agent`）で exit 0。helper を落としていれば `✗ helper` とその次の一手が出て exit 1、**残りの行は出続ける** | 11 項目の検査と、失敗しても続けること。`agent session` と `task env` は ECS の見解ではなく tetherd 自身のハンドシェイク結果なので、サイドカーが落ちていれば他が全部緑でもここで落ちる |
| 15 | `.tetherd.yml` の `service` を `nope` にして `./bin/tetherd doctor` | `✗ attachable task` に `no RUNNING tasks in service tetherd-dev/nope` が出て exit 1。他の行は出続ける | 1 つ失敗しても残りの検査が走る（1 問ずつ直して再実行させない） |
| 16 | `.tetherd.yml` に `remote_services: [s3]` を足して `$RUN -- sh -c 'aws s3 ls && curl -s -o /dev/null -w "%{http_code}\n" https://example.com'` | `aws s3 ls` が成功し `example.com` も 200。`✓ network` 行に S3 の prefix が並ぶ（ap-northeast-1 では 15 件） | prefix list 由来の CIDR が捕捉範囲に入る。件数はリージョンごとに違い、API は 1 ページ 100 件で切るので、ページングしていなければ大きいリージョンで静かに取りこぼす。それ以外の宛先はラップトップから直接出る |
| 17 | `ipconfig getifaddr en0` の /24 を `local_cidrs` に足して `$RUN -- true` と `./bin/tetherd doctor` | 起動時の重なり警告が消え、doctor の `local addresses` が `!` → `✓` | 分割による引き算（VPC /16 から自宅 /24 だけを抜く）が効いている |
| 18 | `local_cidrs: [10.0.0.0/8]`（VPC を丸ごと消す）で `$RUN -- true` | `network.local_cidrs excludes the entire remote set` で exit 2（設定の誤りなので usage 扱い） | 設定ミスで捕捉範囲が空になったら黙って起動しない。タスクロールのエンドポイントを足し戻して「1 件あるから OK」にしない |
| 19 | `$RUN -- python3 -c "import socket,time; t=time.time()\ntry: socket.gethostbyname('nope.myapp.internal')\nexcept socket.gaierror as e: print('gaierror in %.1fs' % (time.time()-t))"` | 1 秒未満で `gaierror` | 存在しない名前が NXDOMAIN として返る。SERVFAIL だと macOS がリトライして数秒待たされる |

所要時間の目安: `StartSession` → `welcome` まで 2〜4 秒、psql の接続確立 +50〜100 ms。

結果は `docs/specs/2026-09-12-v1-macos-design.md` §12 の 5・6 に追記する。
