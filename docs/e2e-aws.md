# AWS e2e（手動）

`deploy/dev-env` を立てた状態で、手元の Mac から確認する。所要 10 分。

前提: `sudo tetherd-helper` が動いている（v0.4 までは `hack/e2e-local.sh` と同じく前面起動: `sudo ./bin/tetherd-helper --socket /var/run/tetherd.sock --exec-src $PWD/bin/tetherd-exec &`）、`aws login --profile personal` 済み、`session-manager-plugin` あり、`psql` あり（`brew install libpq`）。

```
cd deploy/dev-env && eval "RDS=$(terraform output -raw rds_endpoint)" && cd ../..
RUN="./bin/tetherd run --profile personal --cluster tetherd-dev --service api"
```

| # | コマンド | 期待 | 確認すること |
|---|---|---|---|
| 1 | `$RUN -- env \| grep -E '^(DB_PASSWORD\|FEATURE_FLAG\|PORT\|PATH)='` | `DB_PASSWORD=<24 文字>`、`FEATURE_FLAG=from-parameter-store`、`PORT=8081`、`PATH` はローカルのまま | Secrets Manager / Parameter Store 経由の値が `/proc` 経由で届き、除外リストが効いている |
| 2 | `$RUN -- psql "postgres://tetherd@$RDS/app?sslmode=require" -c 'select now()'` に `PGPASSWORD` を env から: `$RUN -- sh -c 'PGPASSWORD=$DB_PASSWORD psql -h '"$RDS"' -U tetherd -d app -c "select now()"'` | `now` の 1 行 | VPC 内の RDS に pf → SSM → agent 経由で届き、パスワードはタスクの secret |
| 3 | `$RUN -- aws sts get-caller-identity` | `Arn` が `assumed-role/tetherd-dev-api-task/…` | 169.254.170.2 が透過で通り、ローカルの SDK がタスクロールになる。起動時の `✓ iam` 行も同じ ARN |
| 4 | `$RUN -- aws s3 ls` | バケット一覧（無ければ空）で **エラーなし** | VPC 外の AWS サービスにタスクロールで届く（署名ベース） |
| 5 | `$RUN -- curl -s http://$(cd deploy/dev-env && terraform output -raw alb_dns_name)/` | `sampleapp on … from 10.0.x.x` | ALB は public なので、これは agent 経由ではなくラップトップから直接（`from` が ALB の IP）。既定「VPC 内だけリモート」の確認 |
| 6 | `$RUN -- curl -s http://api.myapp.internal:8081/` | 失敗（v0.2a では DNS は解けない） | `remote_domains` は v0.2b。`$RUN -- curl -s http://<task private IP>:8081/` は `from 10.0.x.x`（agent の IP）で通る |
| 7 | `$RUN --env prod -- true` | `refusing to attach: agent reports TETHERD_ENV="dev", expected "prod"` で exit 1 | 環境ガード |
| 8 | 2 つ目のターミナルで `$RUN -- sleep 60` を動かしたまま 1 つ目で `$RUN -- true` | `another tetherd session is active` で exit 1 | 1 台 1 セッション |
| 9 | 2 の実行中に `aws ecs stop-task --profile personal --cluster tetherd-dev --task <id>` | `✗ agent session lost` が出て psql が終了、exit 1、`pfctl -a com.apple/900.tetherd -sr` が空 | セッション断の片付け |

所要時間の目安: `StartSession` → `welcome` まで 2〜4 秒、psql の接続確立 +50〜100 ms。

結果は `docs/specs/2026-09-12-v1-macos-design.md` §12 の 5・6 に追記する。
