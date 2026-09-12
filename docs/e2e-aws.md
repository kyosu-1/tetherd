# AWS e2e（手動）

`deploy/dev-env` を立てた状態で、手元の Mac から確認する。所要 10 分。

前提: `sudo tetherd-helper` が動いている（v0.4 までは `hack/e2e-local.sh` と同じく前面起動: `sudo ./bin/tetherd-helper --socket /var/run/tetherd.sock --exec-src $PWD/bin/tetherd-exec &`）、`aws login --profile personal` 済み、`session-manager-plugin` あり、`psql` あり（`brew install libpq`）。

```
cd deploy/dev-env && eval "RDS=$(terraform output -raw rds_endpoint)" && cd ../..
RUN="./bin/tetherd run --profile personal --cluster tetherd-dev --service api"
```

`deploy/dev-env` の最初のタスク起動そのものが spec §12 の 5 番目の検証を兼ねる:
`pidMode: task` + `SYS_PTRACE` + ECS Exec + `restartPolicy` が `RegisterTaskDefinition` に受理され、タスクが `RUNNING` になっていること（そうでなければ `tetherd run` 自体が「no attachable task」で失敗する）。
なお agent コンテナ（distroless）の ECS Exec エージェントは SSM に接続できないことがあり、その場合 CLI は同じタスクの別コンテナ経由で転送する（§6.1）。`tetherd` の出力に `ssm target … is not connected; trying the next container` が出るのは正常。

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 1 | `$RUN -- env \| grep -E '^(DB_PASSWORD\|FEATURE_FLAG\|PORT\|PATH)='` | `DB_PASSWORD=<24 文字>`、`FEATURE_FLAG=from-parameter-store`、`PORT=8081`、`PATH` はローカルのまま | Secrets Manager / Parameter Store 経由の値が `/proc` 経由で届き、除外リストが効いている |
| 2 | `$RUN -- psql "postgres://tetherd@$RDS/app?sslmode=require" -c 'select now()'` に `PGPASSWORD` を env から: `$RUN -- sh -c 'PGPASSWORD=$DB_PASSWORD psql -h '"$RDS"' -U tetherd -d app -c "select now()"'` | `now` の 1 行 | VPC 内の RDS に pf → SSM → agent 経由で届き、パスワードはタスクの secret |
| 3 | `$RUN -- aws sts get-caller-identity` | `Arn` が `assumed-role/tetherd-dev-api-task/…` | 169.254.170.2 が透過で通り、ローカルの SDK がタスクロールになる。起動時の `✓ iam` 行も同じ ARN。`env -u AWS_PROFILE` の前置きはもう不要 — tetherd がローカルの `AWS_PROFILE` / `AWS_ACCESS_KEY_ID` 等を子プロセスの env から自動で取り除くので、手元に `aws login` 済みのプロファイルがあってもタスクロールが優先される（`✓ env      local AWS credentials (...) removed …` 行が出る）。ローカルの `~/.aws` の設定は子プロセスから隠されるので、`AWS_PROFILE` が何であっても結果は変わらない |
| 4 | `$RUN -- aws s3 ls` | バケット一覧（無ければ空）で **エラーなし** | VPC 外の AWS サービスにタスクロールで届く（署名ベース）。同じく、共有設定に引っ張られずタスクロールで署名される |
| 5 | `$RUN -- curl -s http://$(cd deploy/dev-env && terraform output -raw alb_dns_name)/` | `sampleapp on … from 10.0.x.x` | ALB は public なので、これは agent 経由ではなくラップトップから直接（`from` が ALB の IP）。既定「VPC 内だけリモート」の確認 |
| 6 | `$RUN -- curl -s http://api.myapp.internal:8081/` | 失敗（v0.2a では DNS は解けない） | `remote_domains` は v0.2b。`$RUN -- curl -s http://<task private IP>:8081/` は `from 10.0.x.x`（agent の IP）で通る |
| 7 | `$RUN --env prod -- true` | `refusing to attach: agent reports TETHERD_ENV="dev", expected "prod"` で exit 1 | 環境ガード |
| 8 | 2 つ目のターミナルで `$RUN -- sleep 60` を動かしたまま 1 つ目で `$RUN -- true` | `another tetherd session is active` で exit 1 | 1 台 1 セッション |
| 9 | 実行中にトランスポートを落とす: `kill $(pgrep -n -f session-manager-plugin)` | `✗ agent session lost: … control stream closed: EOF` が即座に出て子プロセスが終了、exit 1、`sudo pfctl -a com.apple/900.tetherd -sr` が空 | セッション断の検出と片付け。`aws ecs stop-task` ではタスクが `DEACTIVATING`（ALB のデレジストレーション待ち、既定 300 秒）の間コンテナが動き続けるため、セッションはすぐには切れない。検出経路を試すならトランスポートを殺すほうが速く確実 |
| 10 | 別ターミナルで、実行中に `lsof -nP -iTCP -sTCP:LISTEN \| grep session-manager` | `127.0.0.1:<port>` だけが LISTEN（外部 IF には無い） | plugin のローカルフォワードは `127.0.0.1` にしか bind しない。無認証だが同一マシンに閉じることの確認（spec §11） |

所要時間の目安: `StartSession` → `welcome` まで 2〜4 秒、psql の接続確立 +50〜100 ms。

結果は `docs/specs/2026-09-12-v1-macos-design.md` §12 の 5・6 に追記する。
