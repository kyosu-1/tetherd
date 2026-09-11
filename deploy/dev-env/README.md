# tetherd dev-env

tetherd の検証用 dev 環境。design.md §9 の「インフラ側の変更」をそのまま形にしたもの。
**使うときだけ apply、終わったら destroy**（NAT + RDS + Fargate + ALB で 1 日 200 円弱）。

## 立てる

```
aws login --profile personal                  # セッションが切れていたら
cd deploy/dev-env
terraform init
terraform apply                               # 10〜15 分（RDS）
```

ECR ができたらイメージを push する（Terraform の apply と並行でよい。タスクは image が現れるまで起動失敗を繰り返すだけ）:

```
aws ecr get-login-password --profile personal | docker login --username AWS --password-stdin $(terraform output -raw ecr_registry)
make -C ../.. push-images ECR_REGISTRY=$(terraform output -raw ecr_registry)
aws ecs update-service --profile personal --cluster tetherd-dev --service api --force-new-deployment
```

## 使う

```
terraform output -raw tetherd_run_example
```

## 壊す

```
terraform destroy
```

## 中身

- VPC 10.0.0.0/16、public × 2（ALB / NAT）、private × 2（Fargate / RDS）、NAT 1 つ
- ALB :80 → app :8081（v0.3 で agent :8080 に切り替える）
- ECS cluster `tetherd-dev`、service `api`（`enableExecuteCommand`、`pidMode: task`）、コンテナ `tetherd-agent`（`SYS_PTRACE`）+ `app`
- RDS Postgres 16 `db.t4g.micro`、パスワードは Secrets Manager → app の `DB_PASSWORD`
- SSM Parameter `/tetherd-dev/FEATURE_FLAG` → app の `FEATURE_FLAG`
- Cloud Map `api.myapp.internal`
- IAM: タスクロール（ssmmessages + `s3:ListAllMyBuckets`）、実行ロール、開発者ポリシー（output の ARN。アタッチは手動）
