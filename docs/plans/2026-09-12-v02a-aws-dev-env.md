# tetherd v0.2a — AWS 検証環境と SSM 経由の `tetherd run` 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 個人 AWS アカウント（738925651667、ap-northeast-1）に Terraform で検証環境を立て、`tetherd run --cluster tetherd-dev --service api -- psql -h <rds-endpoint> -U tetherd -c 'select now()'` が、SSM ポートフォワード経由で agent に繋ぎ、app コンテナの本物の env（Secrets Manager 由来の `DB_PASSWORD` 込み）を注入した状態で通ること（spec §6.3、§16 v0.2 の前半）。

**Architecture:** v0.1 の経路（pf rdr → helper natlook → yamux → agent dial）はそのまま。足すのは (1) `ssm` トランスポート（SDK で `ssm:StartSession` → `session-manager-plugin` を子プロセスで起動 → `127.0.0.1:<port>` に dial）、(2) ECS プロバイダ（タスク発見、VPC CIDR 自動取得）、(3) agent の `/proc/<pid>/environ` 読み取りと `welcome.app_env`、(4) CLI 側の env 合成と注入、(5) 169.254.170.2 経由のクレデンシャル取得で `sts:GetCallerIdentity` を呼ぶ IAM 確認、(6) Terraform の検証環境と ECR へのイメージ push。DNS（`/etc/resolver`）、prefix list、設定ファイル、`env` コマンドは v0.2b。

**Tech Stack:** Go 1.27、`aws-sdk-go-v2`（config, ecs, ec2, ssm, sts）、`jackc/pgx/v5`（sampleapp のみ）、Terraform 1.14 + AWS provider 6.x、`session-manager-plugin`（インストール済み）、Docker buildx。

**Spec:** `docs/specs/2026-09-12-v1-macos-design.md` §4.1、§5.3、§5.5、§6.1〜§6.4、§6.6、§9.1、§9.2。design.md §5、§9。

## Global Constraints

- モジュール `github.com/kyosu-1/tetherd`、Go 1.27。新規依存は `github.com/aws/aws-sdk-go-v2`（config / credentials / service/{ecs,ec2,ssm,sts} / feature/ec2/imds は不要）と `github.com/jackc/pgx/v5` のみ
- agent は AWS API を呼ばない（メタデータエンドポイントは AWS API ではない）。設定は env のみ（spec §5.5）
- env の合成順は `ローカル env < タスク env < override`。既定除外は `PATH HOME HOSTNAME USER LOGNAME SHELL TMPDIR PWD OLDPWD TERM LANG LC_* SHLVL _ AWS_EXECUTION_ENV`。`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` と `ECS_CONTAINER_METADATA_URI_V4` は透過モードでは残し、`--no-network` のときだけ除外（spec §6.4）
- リモート集合 = VPC CIDR（自動、セカンダリ含む）+ `169.254.170.0/24` + `--remote-cidr`（spec §4.1）
- SSM target は `ecs:<cluster>_<taskId>_<runtimeId>`、document `AWS-StartPortForwardingSession`、`portNumber: ["9900"]`（spec §6.1）。AWS CLI には依存しない
- タスク発見: RUNNING、`tetherd-agent` コンテナあり、`enableExecuteCommand`、`managedAgents[ExecuteCommandAgent].lastStatus == RUNNING`。v0.2a は **最も古い 1 タスク**に繋ぐ（全タスク接続は v0.3）
- `welcome.env` が `--env`（既定 `dev`）と不一致なら切断して終了（spec §5.4 / §6.3）
- Terraform: 検証専用。`terraform apply` はユーザーが実行する（課金）。エージェントは `terraform fmt -check` / `validate` / `plan` まで
- コミットメッセージ `<type>: <summary>`。TDD（RED → GREEN を report に）

## ファイル構成

```
deploy/dev-env/                 Terraform（versions/variables/network/ecr/alb/secrets/rds/ecs/discovery/iam-developer/outputs + README）
deploy/dev-env/README.md        立て方・壊し方・費用
examples/sampleapp/main.go      /db と /whoami を追加
Makefile                        push-images
internal/agent/environ.go       メタデータ v4 + /proc 走査（procRoot と metadata URL を注入可能に）
internal/agent/config.go        TETHERD_APP_CONTAINER
internal/agent/agent.go         Hello で AppEnv / EnvError / TaskARN を返す
internal/env/env.go             Merge（合成・除外・override）
internal/provider/ecs/          Discover（タスク）、VPCCIDRs（ENI → subnet → VPC）
internal/transport/ssm/         StartSession + plugin 起動 + dial
internal/awsid/                 169.254.170.2 からクレデンシャル取得 → sts GetCallerIdentity
internal/cli/run.go             ssm トランスポート、発見、env 注入、IAM 確認、ステータス行
internal/cli/root.go            新フラグ
docs/e2e-aws.md                 AWS e2e の手動チェックリスト
```

---

### Task 1: sampleapp に `/db` と `/whoami`、`make push-images`

**Files:**
- Modify: `examples/sampleapp/main.go`
- Modify: `Makefile`
- Test: `examples/sampleapp/main_test.go`

**Interfaces:**
- Produces: sampleapp は env `DB_HOST`/`DB_PORT`(既定 5432)/`DB_USER`/`DB_NAME`/`DB_PASSWORD` から DSN を組み、`/db` で `SELECT now()`、`/whoami` で STS `GetCallerIdentity` の ARN を返す。`make push-images ECR_REGISTRY=<acct>.dkr.ecr.ap-northeast-1.amazonaws.com [TAG=dev]` が linux/arm64 のイメージ 2 つを push する

- [ ] **Step 1: 失敗するテストを書く**

`examples/sampleapp/main_test.go`:

```go
package main

import "testing"

func TestDSN(t *testing.T) {
	getenv := func(k string) string {
		return map[string]string{
			"DB_HOST": "db.example", "DB_USER": "tetherd", "DB_NAME": "app", "DB_PASSWORD": "p@ss word",
		}[k]
	}
	got := dsn(getenv)
	want := "postgres://tetherd:p%40ss%20word@db.example:5432/app?sslmode=require"
	if got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}
	if dsn(func(string) string { return "" }) != "" {
		t.Fatal("dsn must be empty when DB_HOST is unset")
	}
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./examples/sampleapp/`
Expected: FAIL（`undefined: dsn`）

- [ ] **Step 3: 実装**

`examples/sampleapp/main.go` を置き換え:

```go
// sampleapp is the verification target: it reports its hostname, TETHERD_ENV
// and the peer address so you can tell whether a request came through the
// agent (peer = agent IP) or directly. /db proves the task can reach RDS
// with the secret-injected password; /whoami proves the task role.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// dsn builds a postgres URL from DB_* env vars; empty when DB_HOST is unset.
func dsn(getenv func(string) string) string {
	host := getenv("DB_HOST")
	if host == "" {
		return ""
	}
	port := getenv("DB_PORT")
	if port == "" {
		port = "5432"
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(getenv("DB_USER"), getenv("DB_PASSWORD")),
		Host:     host + ":" + port,
		Path:     "/" + getenv("DB_NAME"),
		RawQuery: "sslmode=require",
	}
	return u.String()
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	check := flag.Bool("check", false, "check /healthz on 127.0.0.1:PORT and exit (no server started)")
	flag.Parse()
	if *check {
		c := http.Client{Timeout: 2 * time.Second}
		resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	host, _ := os.Hostname()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "sampleapp on %s env=%s from %s\n", host, os.Getenv("TETHERD_ENV"), r.RemoteAddr)
	})
	mux.HandleFunc("/db", func(w http.ResponseWriter, r *http.Request) {
		d := dsn(os.Getenv)
		if d == "" {
			http.Error(w, "DB_HOST is not set", http.StatusServiceUnavailable)
			return
		}
		db, err := sql.Open("pgx", d)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var now time.Time
		if err := db.QueryRowContext(ctx, "SELECT now()").Scan(&now); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, "db now=%s host=%s\n", now.Format(time.RFC3339), os.Getenv("DB_HOST"))
	})
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, "whoami %s\n", *out.Arn)
	})
	log.Printf("sampleapp listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
```

依存を追加:

```bash
go get github.com/aws/aws-sdk-go-v2/config@latest github.com/aws/aws-sdk-go-v2/service/sts@latest github.com/jackc/pgx/v5@latest
go mod tidy
```

`Makefile` に追加（`.PHONY` にも足す）:

```makefile
ECR_REGISTRY ?=
TAG ?= dev

# Build linux/arm64 images (Fargate runs Graviton in dev-env) and push to ECR.
# Usage: aws ecr get-login-password --profile personal | docker login --username AWS --password-stdin $ECR_REGISTRY
#        make push-images ECR_REGISTRY=738925651667.dkr.ecr.ap-northeast-1.amazonaws.com
push-images:
	@test -n "$(ECR_REGISTRY)" || { echo "ECR_REGISTRY is required"; exit 2; }
	docker buildx build --platform linux/arm64 -f deploy/docker/agent.Dockerfile -t $(ECR_REGISTRY)/tetherd-agent:$(TAG) --push .
	docker buildx build --platform linux/arm64 -f deploy/docker/sampleapp.Dockerfile -t $(ECR_REGISTRY)/tetherd-sampleapp:$(TAG) --push .
```

- [ ] **Step 4: 確認**

Run: `go test ./examples/sampleapp/ && make lint && make test && docker build -f deploy/docker/sampleapp.Dockerfile -t sampleapp-check . && docker run --rm -e PORT=8081 -d --name sa sampleapp-check && sleep 1 && docker exec sa /sampleapp -check; echo "check=$?"; docker rm -f sa`
Expected: テスト PASS、イメージがビルドでき `check=0`。`/db` は DB が無いので 503（`DB_HOST is not set`）— `docker run -p 18081:8081` で `curl localhost:18081/db` してもよい

- [ ] **Step 5: Commit**

```bash
git add examples/sampleapp Makefile go.mod go.sum
git commit -m "feat(sampleapp): /db and /whoami endpoints, push-images target"
```

---

### Task 2: Terraform 検証環境（`deploy/dev-env/`）

**Files:**
- Create: `deploy/dev-env/versions.tf`, `variables.tf`, `network.tf`, `ecr.tf`, `alb.tf`, `secrets.tf`, `rds.tf`, `ecs.tf`, `discovery.tf`, `iam-developer.tf`, `outputs.tf`, `README.md`, `.gitignore`
- Test: `terraform fmt -check -recursive`, `terraform validate`, `terraform plan`（`personal` プロファイルで。apply はしない）

**Interfaces:**
- Produces: outputs `cluster_name`（`tetherd-dev`）、`service_name`（`api`）、`vpc_cidr`、`rds_endpoint`、`alb_dns_name`、`ecr_registry`、`developer_policy_arn`、`tetherd_run_example`。タスク定義: `pid_mode = "task"`、agent コンテナ `tetherd-agent`（`SYS_PTRACE`、`TETHERD_ENV=dev`、`TETHERD_APP_CONTAINER=app`）、app コンテナ `app`（`PORT=8081`、`DB_*`、`secrets` で `DB_PASSWORD` と `FEATURE_FLAG`）。サービスは `enable_execute_command = true`

- [ ] **Step 1: ファイルを書く**

`deploy/dev-env/versions.tf`:

```hcl
terraform {
  required_version = ">= 1.6"
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 6.0" }
    random = { source = "hashicorp/random", version = "~> 3.6" }
  }
}

provider "aws" {
  region  = var.region
  profile = var.profile
  default_tags {
    tags = { Project = "tetherd", Environment = "dev-env" }
  }
}
```

`deploy/dev-env/variables.tf`:

```hcl
variable "name" {
  description = "Prefix for every resource; also the ECS cluster name"
  type        = string
  default     = "tetherd-dev"
}

variable "region" {
  type    = string
  default = "ap-northeast-1"
}

variable "profile" {
  description = "AWS CLI profile (empty = default credential chain)"
  type        = string
  default     = "personal"
}

variable "vpc_cidr" {
  type    = string
  default = "10.0.0.0/16"
}

variable "azs" {
  type    = list(string)
  default = ["ap-northeast-1a", "ap-northeast-1c"]
}

variable "desired_count" {
  description = "Tasks in the api service (set 2 to test multi-task in v0.3)"
  type        = number
  default     = 1
}

variable "image_tag" {
  description = "Tag of the agent and sampleapp images in ECR (make push-images TAG=...)"
  type        = string
  default     = "dev"
}

variable "db_instance_class" {
  type    = string
  default = "db.t4g.micro"
}
```

`deploy/dev-env/network.tf`:

```hcl
resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = var.name }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = var.name }
}

resource "aws_subnet" "public" {
  count                   = length(var.azs)
  vpc_id                  = aws_vpc.this.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, count.index)
  availability_zone       = var.azs[count.index]
  map_public_ip_on_launch = true
  tags                    = { Name = "${var.name}-public-${count.index}" }
}

resource "aws_subnet" "private" {
  count             = length(var.azs)
  vpc_id            = aws_vpc.this.id
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, 10 + count.index)
  availability_zone = var.azs[count.index]
  tags              = { Name = "${var.name}-private-${count.index}" }
}

resource "aws_eip" "nat" {
  domain = "vpc"
  tags   = { Name = "${var.name}-nat" }
}

# One NAT gateway is enough for a verification environment.
resource "aws_nat_gateway" "this" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public[0].id
  tags          = { Name = var.name }
  depends_on    = [aws_internet_gateway.this]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
  tags = { Name = "${var.name}-public" }
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.this.id
  }
  tags = { Name = "${var.name}-private" }
}

resource "aws_route_table_association" "public" {
  count          = length(var.azs)
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "private" {
  count          = length(var.azs)
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}
```

`deploy/dev-env/ecr.tf`:

```hcl
resource "aws_ecr_repository" "agent" {
  name         = "tetherd-agent"
  force_delete = true
}

resource "aws_ecr_repository" "sampleapp" {
  name         = "tetherd-sampleapp"
  force_delete = true
}
```

`deploy/dev-env/alb.tf`:

```hcl
resource "aws_security_group" "alb" {
  name   = "${var.name}-alb"
  vpc_id = aws_vpc.this.id
  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_lb" "this" {
  name               = var.name
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.public[*].id
}

# v0.2: the target is the app directly. v0.3 moves this to the agent's :8080.
resource "aws_lb_target_group" "app" {
  name        = "${var.name}-app"
  port        = 8081
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.this.id
  health_check {
    path     = "/healthz"
    interval = 15
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.this.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }
}
```

`deploy/dev-env/secrets.tf`:

```hcl
resource "random_password" "db" {
  length  = 24
  special = false
}

resource "aws_secretsmanager_secret" "db_password" {
  name                    = "${var.name}/db-password"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "db_password" {
  secret_id     = aws_secretsmanager_secret.db_password.id
  secret_string = random_password.db.result
}

resource "aws_ssm_parameter" "feature_flag" {
  name  = "/${var.name}/FEATURE_FLAG"
  type  = "String"
  value = "from-parameter-store"
}
```

`deploy/dev-env/rds.tf`:

```hcl
resource "aws_security_group" "app" {
  name   = "${var.name}-app"
  vpc_id = aws_vpc.this.id
  ingress {
    from_port       = 8081
    to_port         = 8081
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "rds" {
  name   = "${var.name}-rds"
  vpc_id = aws_vpc.this.id
  ingress {
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }
}

resource "aws_db_subnet_group" "this" {
  name       = var.name
  subnet_ids = aws_subnet.private[*].id
}

resource "aws_db_instance" "this" {
  identifier             = var.name
  engine                 = "postgres"
  engine_version         = "16"
  instance_class         = var.db_instance_class
  allocated_storage      = 20
  db_name                = "app"
  username               = "tetherd"
  password               = random_password.db.result
  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [aws_security_group.rds.id]
  publicly_accessible    = false
  skip_final_snapshot    = true
  apply_immediately      = true
}
```

`deploy/dev-env/ecs.tf`:

```hcl
data "aws_caller_identity" "current" {}

resource "aws_ecs_cluster" "this" {
  name = var.name
}

resource "aws_cloudwatch_log_group" "this" {
  name              = "/ecs/${var.name}"
  retention_in_days = 7
}

# Execution role: pull images, write logs, resolve secrets at start.
data "aws_iam_policy_document" "ecs_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  name               = "${var.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "execution_secrets" {
  name = "secrets"
  role = aws_iam_role.execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = aws_secretsmanager_secret.db_password.arn },
      { Effect = "Allow", Action = ["ssm:GetParameters"], Resource = aws_ssm_parameter.feature_flag.arn },
    ]
  })
}

# Task role: what the app (and, through tetherd, the developer) can do.
resource "aws_iam_role" "task" {
  name               = "${var.name}-api-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

resource "aws_iam_role_policy" "task" {
  name = "task"
  role = aws_iam_role.task.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "ssmmessages:CreateControlChannel",
          "ssmmessages:CreateDataChannel",
          "ssmmessages:OpenControlChannel",
          "ssmmessages:OpenDataChannel",
        ]
        Resource = "*"
      },
      { Effect = "Allow", Action = ["s3:ListAllMyBuckets", "sts:GetCallerIdentity"], Resource = "*" },
    ]
  })
}

locals {
  registry = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${var.region}.amazonaws.com"
  log_config = {
    logDriver = "awslogs"
    options = {
      awslogs-group         = aws_cloudwatch_log_group.this.name
      awslogs-region        = var.region
      awslogs-stream-prefix = "api"
    }
  }
}

resource "aws_ecs_task_definition" "api" {
  family                   = "${var.name}-api"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  pid_mode                 = "task"
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([
    {
      name      = "tetherd-agent"
      image     = "${local.registry}/tetherd-agent:${var.image_tag}"
      essential = true
      linuxParameters = {
        capabilities = { add = ["SYS_PTRACE"] }
      }
      environment = [
        { name = "TETHERD_ENV", value = "dev" },
        { name = "TETHERD_APP_CONTAINER", value = "app" },
      ]
      restartPolicy    = { enabled = true, restartAttemptPeriod = 60 }
      logConfiguration = local.log_config
    },
    {
      name         = "app"
      image        = "${local.registry}/tetherd-sampleapp:${var.image_tag}"
      essential    = true
      portMappings = [{ containerPort = 8081, protocol = "tcp" }]
      environment = [
        { name = "PORT", value = "8081" },
        { name = "TETHERD_ENV", value = "dev" },
        { name = "DB_HOST", value = aws_db_instance.this.address },
        { name = "DB_PORT", value = "5432" },
        { name = "DB_USER", value = "tetherd" },
        { name = "DB_NAME", value = "app" },
      ]
      secrets = [
        { name = "DB_PASSWORD", valueFrom = aws_secretsmanager_secret.db_password.arn },
        { name = "FEATURE_FLAG", valueFrom = aws_ssm_parameter.feature_flag.arn },
      ]
      logConfiguration = local.log_config
    },
  ])
}

resource "aws_ecs_service" "api" {
  name                   = "api"
  cluster                = aws_ecs_cluster.this.id
  task_definition        = aws_ecs_task_definition.api.arn
  desired_count          = var.desired_count
  launch_type            = "FARGATE"
  enable_execute_command = true

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.app.id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.app.arn
    container_name   = "app"
    container_port   = 8081
  }

  service_registries {
    registry_arn = aws_service_discovery_service.api.arn
  }

  depends_on = [aws_lb_listener.http]
}
```

`deploy/dev-env/discovery.tf`:

```hcl
resource "aws_service_discovery_private_dns_namespace" "this" {
  name = "myapp.internal"
  vpc  = aws_vpc.this.id
}

resource "aws_service_discovery_service" "api" {
  name = "api"
  dns_config {
    namespace_id = aws_service_discovery_private_dns_namespace.this.id
    dns_records {
      type = "A"
      ttl  = 10
    }
  }
  health_check_custom_config {
    failure_threshold = 1
  }
}
```

`deploy/dev-env/iam-developer.tf`（design.md §9 のポリシー）:

```hcl
data "aws_iam_policy_document" "developer" {
  statement {
    sid       = "DiscoverTarget"
    actions   = ["ecs:ListTasks", "ecs:DescribeTasks", "ecs:DescribeServices", "ecs:ListServices", "ecs:DescribeTaskDefinition"]
    resources = ["*"]
  }
  statement {
    sid       = "DiscoverNetwork"
    actions   = ["ec2:DescribeSubnets", "ec2:DescribeVpcs", "ec2:DescribeManagedPrefixLists", "ec2:GetManagedPrefixListEntries"]
    resources = ["*"]
  }
  statement {
    sid     = "ControlChannel"
    actions = ["ssm:StartSession"]
    resources = [
      "arn:aws:ecs:${var.region}:${data.aws_caller_identity.current.account_id}:task/${var.name}/*",
      "arn:aws:ssm:${var.region}::document/AWS-StartPortForwardingSession",
    ]
  }
  statement {
    sid       = "OwnSessions"
    actions   = ["ssm:TerminateSession", "ssm:ResumeSession"]
    resources = ["arn:aws:ssm:*:*:session/*"]
  }
  statement {
    sid       = "Optional"
    actions   = ["sts:GetCallerIdentity", "servicediscovery:ListNamespaces"]
    resources = ["*"]
  }
}

resource "aws_iam_policy" "developer" {
  name   = "${var.name}-developer"
  policy = data.aws_iam_policy_document.developer.json
}
```

`deploy/dev-env/outputs.tf`:

```hcl
output "cluster_name" {
  value = aws_ecs_cluster.this.name
}

output "service_name" {
  value = aws_ecs_service.api.name
}

output "vpc_cidr" {
  value = aws_vpc.this.cidr_block
}

output "rds_endpoint" {
  value = aws_db_instance.this.address
}

output "alb_dns_name" {
  value = aws_lb.this.dns_name
}

output "ecr_registry" {
  value = local.registry
}

output "developer_policy_arn" {
  value = aws_iam_policy.developer.arn
}

output "tetherd_run_example" {
  value = "tetherd run --profile ${var.profile} --region ${var.region} --cluster ${aws_ecs_cluster.this.name} --service ${aws_ecs_service.api.name} -- psql -h ${aws_db_instance.this.address} -U tetherd -d app -c 'select now()'"
}
```

`deploy/dev-env/.gitignore`:

```
.terraform/
*.tfstate
*.tfstate.backup
.terraform.lock.hcl
terraform.tfvars
```

`deploy/dev-env/README.md`:

```markdown
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
```

- [ ] **Step 2: fmt / validate / plan**

Run:
```bash
cd deploy/dev-env && terraform fmt -recursive && terraform fmt -check -recursive && terraform init -input=false >/dev/null && terraform validate && terraform plan -input=false -out=/tmp/tetherd-dev.plan 2>&1 | tail -3
```
Expected: `Success! The configuration is valid.` と `Plan: N to add, 0 to change, 0 to destroy.`（N は 40 前後）。**apply はしない**。plan の出力末尾を report に貼る

- [ ] **Step 3: Commit**

```bash
git add deploy/dev-env
git commit -m "feat(dev-env): terraform verification environment (VPC, ALB, Fargate, RDS, ECR, Cloud Map)"
```

---

### Task 3: agent の environ 読み取り（`internal/agent/environ.go`）

**Files:**
- Create: `internal/agent/environ.go`
- Modify: `internal/agent/config.go`, `internal/agent/agent.go`, `cmd/tetherd-agent/main.go`
- Test: `internal/agent/environ_test.go`, `internal/agent/agent_test.go`（追記）

**Interfaces:**
- Consumes: `proto.Welcome{AppEnv, EnvError, TaskARN}`（既存）
- Produces:
  ```go
  // environ.go
  type EnvReader interface { Read(ctx context.Context) (env map[string]string, taskARN string, err error) }
  type ProcEnvReader struct{ MetadataURL, ProcRoot, AppContainer string; HTTP *http.Client }
  func (r *ProcEnvReader) Read(ctx) (map[string]string, string, error)
  func ParseEnviron(b []byte) map[string]string           // NUL 区切り
  func procStartTime(statLine string) (uint64, error)      // stat の第 22 フィールド
  // config.go
  Config{ …, AppContainer string /* TETHERD_APP_CONTAINER, 既定 app */, MetadataURL string /* ECS_CONTAINER_METADATA_URI_V4 */ }
  // agent.go
  func New(cfg Config, logf) *Agent   // cfg.MetadataURL != "" なら ProcEnvReader を使う
  func (a *Agent) SetEnvReader(r EnvReader)  // テスト用差し替え
  ```
  `Hello` は毎回 `Read` を呼び、成功なら `Welcome.AppEnv` と `TaskARN`（cfg.TaskARN が空なら metadata の値）、失敗なら `Welcome.EnvError = err.Error()`。EnvReader が無い（ECS 外）ときは `EnvError = "no ECS metadata endpoint (agent is not running in ECS)"`

- [ ] **Step 1: 失敗するテストを書く**

`internal/agent/environ_test.go`:

```go
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnviron(t *testing.T) {
	got := ParseEnviron([]byte("A=1\x00B=x=y\x00\x00NOEQ\x00"))
	if got["A"] != "1" || got["B"] != "x=y" || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestProcStartTime(t *testing.T) {
	// pid (comm) state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime priority nice num_threads itrealvalue starttime ...
	line := "42 (my app) S 1 42 42 0 -1 4194560 100 0 0 0 5 3 0 0 20 0 1 0 12345 1000 200 18446744073709551615"
	st, err := procStartTime(line)
	if err != nil || st != 12345 {
		t.Fatalf("starttime = %d, err = %v", st, err)
	}
}

// writeProc lays out a fake /proc with the given processes.
func writeProc(t *testing.T, procs map[int]struct {
	env       string
	starttime string
}) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range procs {
		dir := filepath.Join(root, itoa(pid))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "environ"), []byte(p.env), 0o644)
		os.WriteFile(filepath.Join(dir, "stat"), []byte(itoa(pid)+" (x) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 "+p.starttime+" 0 0 0\n"), 0o644)
	}
	os.MkdirAll(filepath.Join(root, "self"), 0o755) // non-numeric entries must be skipped
	return root
}

func metadata(t *testing.T, containers string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4/abc/task" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"TaskARN":"arn:aws:ecs:ap-northeast-1:1:task/c/t1","Containers":[` + containers + `]}`))
	}))
}

func TestReadPicksOldestProcessOfAppContainer(t *testing.T) {
	srv := metadata(t, `{"DockerId":"app123","Name":"app"},{"DockerId":"agent456","Name":"tetherd-agent"}`)
	defer srv.Close()
	root := writeProc(t, map[int]struct{ env, starttime string }{
		1:  {"ECS_CONTAINER_METADATA_URI_V4=" + srv.URL + "/v4/agent456\x00TETHERD_ENV=dev\x00", "100"},
		7:  {"ECS_CONTAINER_METADATA_URI_V4=" + srv.URL + "/v4/app123\x00PORT=8081\x00DB_PASSWORD=s3cret\x00", "200"},
		9:  {"ECS_CONTAINER_METADATA_URI_V4=" + srv.URL + "/v4/app123\x00PORT=9999\x00CHILD=1\x00", "250"},
		11: {"", "50"},
	})
	r := &ProcEnvReader{MetadataURL: srv.URL + "/v4/abc", ProcRoot: root, AppContainer: "app"}
	env, arn, err := r.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if arn != "arn:aws:ecs:ap-northeast-1:1:task/c/t1" {
		t.Fatalf("arn = %q", arn)
	}
	if env["PORT"] != "8081" || env["DB_PASSWORD"] != "s3cret" || env["CHILD"] != "" {
		t.Fatalf("env = %v (must be pid 7, the oldest app process)", env)
	}
}

func TestReadErrors(t *testing.T) {
	srv := metadata(t, `{"DockerId":"app123","Name":"app"}`)
	defer srv.Close()
	// no process of the app container visible → pidMode hint
	root := writeProc(t, map[int]struct{ env, starttime string }{
		1: {"ECS_CONTAINER_METADATA_URI_V4=" + srv.URL + "/v4/agent456\x00", "100"},
	})
	r := &ProcEnvReader{MetadataURL: srv.URL + "/v4/abc", ProcRoot: root, AppContainer: "app"}
	if _, _, err := r.Read(context.Background()); err == nil || !strings.Contains(err.Error(), "pidMode") {
		t.Fatalf("want pidMode hint, got %v", err)
	}
	// unknown container name
	r.AppContainer = "web"
	if _, _, err := r.Read(context.Background()); err == nil || !strings.Contains(err.Error(), `"web"`) {
		t.Fatalf("want unknown-container error, got %v", err)
	}
	// metadata unreachable
	r.MetadataURL = "http://127.0.0.1:1/v4/abc"
	if _, _, err := r.Read(context.Background()); err == nil {
		t.Fatal("want metadata error")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
```

`internal/agent/agent_test.go` に追記:

```go
type fakeEnv struct {
	env map[string]string
	arn string
	err error
}

func (f fakeEnv) Read(context.Context) (map[string]string, string, error) { return f.env, f.arn, f.err }

func TestWelcomeCarriesAppEnv(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	a := New(Config{Env: "dev"}, nil)
	a.SetEnvReader(fakeEnv{env: map[string]string{"PORT": "8081"}, arn: "arn:task"})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Serve(ctx, ln)
	c, err := connect(t, ln.Addr().String(), "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w := c.Welcome()
	if w.AppEnv["PORT"] != "8081" || w.TaskARN != "arn:task" || w.EnvError != "" {
		t.Fatalf("welcome = %+v", w)
	}
}

func TestWelcomeReportsEnvError(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	a := New(Config{Env: "dev", TaskARN: "arn:cfg"}, nil)
	a.SetEnvReader(fakeEnv{err: errors.New("no process of container \"app\" visible; is pidMode \"task\" set")})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Serve(ctx, ln)
	c, err := connect(t, ln.Addr().String(), "shota")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w := c.Welcome()
	if w.EnvError == "" || w.AppEnv != nil || w.TaskARN != "arn:cfg" {
		t.Fatalf("welcome = %+v", w)
	}
}

func TestWelcomeWithoutMetadata(t *testing.T) {
	c, err := connect(t, startAgent(t), "shota") // startAgent has no MetadataURL
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if w := c.Welcome(); !strings.Contains(w.EnvError, "not running in ECS") {
		t.Fatalf("welcome = %+v", w)
	}
}
```

（`agent_test.go` の import に `strings` を足す。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/agent/`
Expected: FAIL（`undefined: ParseEnviron`, `SetEnvReader`）

- [ ] **Step 3: 実装**

`internal/agent/environ.go`:

```go
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EnvReader returns the app container's environment and the task ARN.
type EnvReader interface {
	Read(ctx context.Context) (env map[string]string, taskARN string, err error)
}

// ProcEnvReader implements EnvReader the way mirrord does: ask the ECS
// metadata endpoint which container is the app, then read the environ of
// that container's oldest process from /proc (spec §5.3). Needs
// pidMode: task and CAP_SYS_PTRACE.
type ProcEnvReader struct {
	MetadataURL  string // ECS_CONTAINER_METADATA_URI_V4 of the agent container
	ProcRoot     string // "/proc"
	AppContainer string // TETHERD_APP_CONTAINER
	HTTP         *http.Client
}

type taskMetadata struct {
	TaskARN    string `json:"TaskARN"`
	Containers []struct {
		DockerID string `json:"DockerId"`
		Name     string `json:"Name"`
	} `json:"Containers"`
}

// Read implements EnvReader.
func (r *ProcEnvReader) Read(ctx context.Context) (map[string]string, string, error) {
	if r.MetadataURL == "" {
		return nil, "", errors.New("no ECS metadata endpoint (agent is not running in ECS)")
	}
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.MetadataURL, "/")+"/task", nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("task metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("task metadata: HTTP %d", resp.StatusCode)
	}
	var md taskMetadata
	if err := json.NewDecoder(resp.Body).Decode(&md); err != nil {
		return nil, "", fmt.Errorf("task metadata: %w", err)
	}
	var dockerID string
	var names []string
	for _, c := range md.Containers {
		names = append(names, c.Name)
		if c.Name == r.AppContainer {
			dockerID = c.DockerID
		}
	}
	if dockerID == "" {
		return nil, "", fmt.Errorf("container %q is not in the task (containers: %s); set TETHERD_APP_CONTAINER", r.AppContainer, strings.Join(names, ", "))
	}

	// Find processes whose own metadata URI points at the app container.
	entries, err := os.ReadDir(r.ProcRoot)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", r.ProcRoot, err)
	}
	var bestEnv map[string]string
	var bestStart uint64
	found := false
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r.ProcRoot, e.Name(), "environ"))
		if err != nil || len(raw) == 0 {
			continue
		}
		env := ParseEnviron(raw)
		if !strings.Contains(env["ECS_CONTAINER_METADATA_URI_V4"], dockerID) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(r.ProcRoot, e.Name(), "stat"))
		if err != nil {
			continue
		}
		start, err := procStartTime(string(stat))
		if err != nil {
			continue
		}
		if !found || start < bestStart {
			bestEnv, bestStart, found = env, start, true
		}
		_ = pid
	}
	if !found {
		return nil, "", fmt.Errorf("no process of container %q is visible from the agent; is pidMode \"task\" set on the task definition (and SYS_PTRACE added to the agent)?", r.AppContainer)
	}
	return bestEnv, md.TaskARN, nil
}

// ParseEnviron splits the NUL-separated KEY=VALUE list of /proc/<pid>/environ.
func ParseEnviron(b []byte) map[string]string {
	out := map[string]string{}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if len(kv) == 0 {
			continue
		}
		k, v, ok := strings.Cut(string(kv), "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// procStartTime returns field 22 (starttime, clock ticks since boot) of a
// /proc/<pid>/stat line. The comm field is parenthesised and may contain
// spaces, so parse from the last ')'.
func procStartTime(statLine string) (uint64, error) {
	i := strings.LastIndex(statLine, ")")
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	fields := strings.Fields(statLine[i+1:])
	// fields[0] is state (field 3); starttime is field 22 → index 19.
	if len(fields) < 20 {
		return 0, errors.New("short stat")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
```

`internal/agent/config.go` を置き換え:

```go
// Package agent is the sidecar: it accepts CLI sessions on the control
// port, dials VPC destinations on their behalf and (from v0.3) proxies ALB
// traffic. It never calls AWS APIs.
package agent

import "errors"

// Config comes from environment variables only.
type Config struct {
	Env          string // TETHERD_ENV, required. Refuse to start without it.
	TaskARN      string // TETHERD_TASK_ARN, optional; metadata wins when present.
	Control      string // TETHERD_CONTROL, default 127.0.0.1:9900.
	AppContainer string // TETHERD_APP_CONTAINER, default app.
	MetadataURL  string // ECS_CONTAINER_METADATA_URI_V4, set by ECS; empty outside ECS.
}

// ConfigFromEnv builds Config from getenv (os.Getenv in main).
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Env:          getenv("TETHERD_ENV"),
		TaskARN:      getenv("TETHERD_TASK_ARN"),
		Control:      getenv("TETHERD_CONTROL"),
		AppContainer: getenv("TETHERD_APP_CONTAINER"),
		MetadataURL:  getenv("ECS_CONTAINER_METADATA_URI_V4"),
	}
	if cfg.Env == "" {
		return Config{}, errors.New("TETHERD_ENV is not set; tetherd-agent refuses to start without it")
	}
	if cfg.Control == "" {
		cfg.Control = "127.0.0.1:9900"
	}
	if cfg.AppContainer == "" {
		cfg.AppContainer = "app"
	}
	return cfg, nil
}
```

`internal/agent/agent.go` の変更点:

```go
// Agent に追加
	env EnvReader // nil outside ECS

// New の末尾（return の前）
	a := &Agent{cfg: cfg, logf: logf, sessions: map[string]SessionInfo{}}
	if cfg.MetadataURL != "" {
		a.env = &ProcEnvReader{MetadataURL: cfg.MetadataURL, ProcRoot: "/proc", AppContainer: cfg.AppContainer}
	}
	return a

// SetEnvReader replaces the env source (tests).
func (a *Agent) SetEnvReader(r EnvReader) { a.env = r }

// handler.Hello: register 成功の後、Welcome を組む前に
	w := proto.Welcome{Version: proto.Version, TaskARN: h.a.cfg.TaskARN, Env: h.a.cfg.Env, Others: h.a.others(hello.User)}
	if h.a.env == nil {
		w.EnvError = "no ECS metadata endpoint (agent is not running in ECS)"
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		env, arn, err := h.a.env.Read(ctx)
		cancel()
		if err != nil {
			w.EnvError = err.Error()
			h.a.logf("env for %q: %v", hello.User, err)
		} else {
			w.AppEnv = env
			if arn != "" {
				w.TaskARN = arn
			}
			h.a.logf("env for %q: %d vars from container %q", hello.User, len(env), h.a.cfg.AppContainer)
		}
	}
	return w, nil
```

`cmd/tetherd-agent/main.go` は変更不要（`ConfigFromEnv(os.Getenv)` が新しいキーを拾う）。`hack/docker-compose.yml` の agent にも変更不要（メタデータ無し → `EnvError`）。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./internal/agent/ && make lint`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "feat(agent): read the app container's environ via ECS metadata and /proc"
```

---

### Task 4: env の合成（`internal/env`）

**Files:**
- Create: `internal/env/env.go`
- Test: `internal/env/env_test.go`

**Interfaces:**
- Produces:
  ```go
  var DefaultExclude = []string{"PATH","HOME","HOSTNAME","USER","LOGNAME","SHELL","TMPDIR","PWD","OLDPWD","TERM","LANG","LC_*","SHLVL","_","AWS_EXECUTION_ENV"}
  var AWSContainerVars = []string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI","ECS_CONTAINER_METADATA_URI_V4"}
  type Options struct{ Override map[string]string; Exclude []string; DropAWSContainer bool }
  func Merge(local []string, task map[string]string, opts Options) []string   // 決定的順序（キーでソート）
  func Excluded(name string, patterns []string) bool                          // 完全一致 or 末尾 * の前方一致
  ```

- [ ] **Step 1: 失敗するテストを書く**

`internal/env/env_test.go`:

```go
package env

import (
	"sort"
	"strings"
	"testing"
)

func toMap(kvs []string) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestExcluded(t *testing.T) {
	if !Excluded("LC_ALL", DefaultExclude) || !Excluded("PATH", DefaultExclude) || Excluded("PORT", DefaultExclude) || Excluded("LCX", DefaultExclude) {
		t.Fatal("pattern matching is wrong")
	}
}

func TestMergePrecedenceAndExclusion(t *testing.T) {
	local := []string{"PATH=/usr/bin", "HOME=/Users/me", "PORT=3000", "KEEP=local"}
	task := map[string]string{
		"PATH": "/usr/local/sbin", "HOME": "/root", "HOSTNAME": "abc", "LC_ALL": "C",
		"PORT": "8081", "DB_PASSWORD": "s3cret", "AWS_EXECUTION_ENV": "AWS_ECS_FARGATE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/creds", "ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2/v4/x",
	}
	got := toMap(Merge(local, task, Options{Override: map[string]string{"PORT": "8080"}}))
	want := map[string]string{
		"PATH": "/usr/bin", "HOME": "/Users/me", "KEEP": "local", // local survives for excluded names
		"PORT": "8080", "DB_PASSWORD": "s3cret", // override > task > local
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/creds", "ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2/v4/x",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"HOSTNAME", "LC_ALL", "AWS_EXECUTION_ENV"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must be excluded", k)
		}
	}
}

func TestMergeDropAWSContainer(t *testing.T) {
	task := map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/creds", "ECS_CONTAINER_METADATA_URI_V4": "u", "X": "1"}
	got := toMap(Merge(nil, task, Options{DropAWSContainer: true}))
	if _, ok := got["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]; ok || got["X"] != "1" {
		t.Fatalf("got %v", got)
	}
}

func TestMergeUserExcludeAndDeterministicOrder(t *testing.T) {
	task := map[string]string{"B": "2", "A": "1", "SECRET_X": "s"}
	out := Merge(nil, task, Options{Exclude: []string{"SECRET_*"}})
	if !sort.StringsAreSorted(out) || len(out) != 2 {
		t.Fatalf("got %v", out)
	}
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/env/`
Expected: FAIL

- [ ] **Step 3: 実装**

`internal/env/env.go`:

```go
// Package env merges the task's environment into the local one for the
// child process: local < task < override, with names that describe the
// container rather than the app (PATH, HOME, …) left alone (spec §6.4).
package env

import (
	"sort"
	"strings"
)

// DefaultExclude are task variables never injected. "LC_*" style patterns
// match a prefix.
var DefaultExclude = []string{
	"PATH", "HOME", "HOSTNAME", "USER", "LOGNAME", "SHELL", "TMPDIR", "PWD", "OLDPWD",
	"TERM", "LANG", "LC_*", "SHLVL", "_", "AWS_EXECUTION_ENV",
}

// AWSContainerVars make the AWS SDK fetch the task role through
// 169.254.170.2. They are kept in transparent mode and dropped with
// --no-network (the SDK would fail hard, not fall back).
var AWSContainerVars = []string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "ECS_CONTAINER_METADATA_URI_V4"}

// Options tune Merge.
type Options struct {
	Override         map[string]string
	Exclude          []string // in addition to DefaultExclude
	DropAWSContainer bool
}

// Excluded reports whether name matches any pattern (exact, or "PREFIX*").
func Excluded(name string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
		} else if p == name {
			return true
		}
	}
	return false
}

// Merge returns KEY=VALUE pairs sorted by key.
func Merge(local []string, task map[string]string, opts Options) []string {
	out := map[string]string{}
	for _, kv := range local {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			out[k] = v
		}
	}
	exclude := append(append([]string{}, DefaultExclude...), opts.Exclude...)
	if opts.DropAWSContainer {
		exclude = append(exclude, AWSContainerVars...)
	}
	for k, v := range task {
		if Excluded(k, exclude) {
			continue
		}
		out[k] = v
	}
	for k, v := range opts.Override {
		out[k] = v
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	res := make([]string, 0, len(keys))
	for _, k := range keys {
		res = append(res, k+"="+out[k])
	}
	return res
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race ./internal/env/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/env
git commit -m "feat(env): merge task environment into the child's with default exclusions"
```

---

### Task 5: ECS プロバイダ — タスク発見と VPC CIDR（`internal/provider/ecs`）

**Files:**
- Create: `internal/provider/ecs/discover.go`, `internal/provider/ecs/vpc.go`
- Modify: `internal/transport/transport.go`（`Task` に `SubnetID`、`StartedAt` を追加）
- Test: `internal/provider/ecs/discover_test.go`, `internal/provider/ecs/vpc_test.go`

**Interfaces:**
- Consumes: `transport.Task`
- Produces:
  ```go
  // transport.Task に追加
  SubnetID  string
  StartedAt time.Time

  // discover.go
  type ECSAPI interface {
      ListTasks(ctx context.Context, in *ecs.ListTasksInput, opts ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
      DescribeTasks(ctx context.Context, in *ecs.DescribeTasksInput, opts ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
  }
  type Target struct{ Cluster, Service, TaskID, AgentContainer string }  // TaskID が空なら自動選択。AgentContainer 既定 "tetherd-agent"
  func Discover(ctx context.Context, api ECSAPI, t Target) (transport.Task, error)   // RUNNING かつ条件を満たす中で StartedAt が最古
  type NotReadyError struct{ Reasons []string }   // 候補はあるが条件を満たさないときの理由（ExecuteCommand 無効、agent 無し、exec agent が RUNNING でない）

  // vpc.go
  type EC2API interface {
      DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
      DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, opts ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
  }
  func VPCCIDRs(ctx context.Context, api EC2API, subnetID string) ([]netip.Prefix, error)  // associated な CidrBlock 全部（IPv4）
  var TaskRoleCIDR = netip.MustParsePrefix("169.254.170.0/24")
  ```

- [ ] **Step 1: 失敗するテストを書く**

`internal/provider/ecs/discover_test.go`:

```go
package ecs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

type fakeECS struct {
	arns  []string
	tasks []types.Task
	list  *awsecs.ListTasksInput
}

func (f *fakeECS) ListTasks(_ context.Context, in *awsecs.ListTasksInput, _ ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error) {
	f.list = in
	return &awsecs.ListTasksOutput{TaskArns: f.arns}, nil
}

func (f *fakeECS) DescribeTasks(_ context.Context, in *awsecs.DescribeTasksInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error) {
	return &awsecs.DescribeTasksOutput{Tasks: f.tasks}, nil
}

func task(id string, started time.Time, exec bool, agentStatus string, withAgent bool) types.Task {
	t := types.Task{
		TaskArn:              aws.String("arn:aws:ecs:ap-northeast-1:1:task/c/" + id),
		LastStatus:           aws.String("RUNNING"),
		EnableExecuteCommand: exec,
		StartedAt:            aws.Time(started),
		Attachments: []types.Attachment{{
			Type:    aws.String("ElasticNetworkInterface"),
			Details: []types.KeyValuePair{{Name: aws.String("subnetId"), Value: aws.String("subnet-" + id)}},
		}},
		Containers: []types.Container{{Name: aws.String("app"), RuntimeId: aws.String(id + "-app")}},
	}
	if withAgent {
		c := types.Container{Name: aws.String("tetherd-agent"), RuntimeId: aws.String(id + "-rt")}
		if agentStatus != "" {
			c.ManagedAgents = []types.ManagedAgent{{Name: types.ManagedAgentNameExecuteCommandAgent, LastStatus: aws.String(agentStatus)}}
		}
		t.Containers = append(t.Containers, c)
	}
	return t
}

func TestDiscoverPicksOldestEligible(t *testing.T) {
	now := time.Now()
	f := &fakeECS{
		arns: []string{"a", "b", "c"},
		tasks: []types.Task{
			task("b", now.Add(-1*time.Hour), true, "RUNNING", true),
			task("a", now.Add(-2*time.Hour), true, "RUNNING", true),
			task("c", now.Add(-3*time.Hour), false, "RUNNING", true), // exec disabled → skipped
		},
	}
	got, err := Discover(context.Background(), f, Target{Cluster: "c", Service: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "a" || got.RuntimeID != "a-rt" || got.SubnetID != "subnet-a" || got.Cluster != "c" {
		t.Fatalf("got %+v", got)
	}
	if aws.ToString(f.list.Cluster) != "c" || aws.ToString(f.list.ServiceName) != "api" || f.list.DesiredStatus != types.DesiredStatusRunning {
		t.Fatalf("ListTasks input = %+v", f.list)
	}
}

func TestDiscoverExplicitTaskID(t *testing.T) {
	now := time.Now()
	f := &fakeECS{arns: []string{"a", "b"}, tasks: []types.Task{
		task("a", now.Add(-2*time.Hour), true, "RUNNING", true),
		task("b", now.Add(-1*time.Hour), true, "RUNNING", true),
	}}
	got, err := Discover(context.Background(), f, Target{Cluster: "c", Service: "api", TaskID: "b"})
	if err != nil || got.ID != "b" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestDiscoverNotReadyReasons(t *testing.T) {
	now := time.Now()
	f := &fakeECS{arns: []string{"a", "b", "c"}, tasks: []types.Task{
		task("a", now, false, "RUNNING", true),
		task("b", now, true, "PENDING", true),
		task("c", now, true, "", false),
	}}
	_, err := Discover(context.Background(), f, Target{Cluster: "c", Service: "api"})
	var nr *NotReadyError
	if !errors.As(err, &nr) || len(nr.Reasons) != 3 {
		t.Fatalf("want NotReadyError with 3 reasons, got %v", err)
	}
}

func TestDiscoverNoTasks(t *testing.T) {
	_, err := Discover(context.Background(), &fakeECS{}, Target{Cluster: "c", Service: "api"})
	if err == nil {
		t.Fatal("want error when no tasks are running")
	}
}
```

`internal/provider/ecs/vpc_test.go`:

```go
package ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type fakeEC2 struct{ subnets, vpcs []string }

func (f *fakeEC2) DescribeSubnets(_ context.Context, in *awsec2.DescribeSubnetsInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error) {
	f.subnets = in.SubnetIds
	return &awsec2.DescribeSubnetsOutput{Subnets: []types.Subnet{{VpcId: aws.String("vpc-1")}}}, nil
}

func (f *fakeEC2) DescribeVpcs(_ context.Context, in *awsec2.DescribeVpcsInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeVpcsOutput, error) {
	f.vpcs = in.VpcIds
	return &awsec2.DescribeVpcsOutput{Vpcs: []types.Vpc{{CidrBlockAssociationSet: []types.VpcCidrBlockAssociation{
		{CidrBlock: aws.String("10.0.0.0/16"), CidrBlockState: &types.VpcCidrBlockState{State: types.VpcCidrBlockStateCodeAssociated}},
		{CidrBlock: aws.String("10.1.0.0/16"), CidrBlockState: &types.VpcCidrBlockState{State: types.VpcCidrBlockStateCodeAssociated}},
		{CidrBlock: aws.String("10.9.0.0/16"), CidrBlockState: &types.VpcCidrBlockState{State: types.VpcCidrBlockStateCodeDisassociated}},
	}}}}, nil
}

func TestVPCCIDRs(t *testing.T) {
	f := &fakeEC2{}
	got, err := VPCCIDRs(context.Background(), f, "subnet-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "10.0.0.0/16" || got[1].String() != "10.1.0.0/16" {
		t.Fatalf("got %v", got)
	}
	if f.subnets[0] != "subnet-a" || f.vpcs[0] != "vpc-1" {
		t.Fatalf("inputs: %v %v", f.subnets, f.vpcs)
	}
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go get github.com/aws/aws-sdk-go-v2/service/ecs@latest github.com/aws/aws-sdk-go-v2/service/ec2@latest && go test ./internal/provider/...`
Expected: FAIL（`undefined: Discover`）

- [ ] **Step 3: 実装**

`internal/transport/transport.go` の `Task` に追加:

```go
	// SubnetID and StartedAt come from DescribeTasks (ECS provider).
	SubnetID  string
	StartedAt time.Time
```

（`import "time"` を足す。）

`internal/provider/ecs/discover.go`:

```go
// Package ecs is the ECS-specific side of tetherd: which task to attach to
// and what its VPC looks like. Everything AWS-flavoured lives here so a
// second provider (Cloud Run) can be added beside it.
package ecs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/kyosu-1/tetherd/internal/transport"
)

// ECSAPI is the subset of the ECS client Discover needs.
type ECSAPI interface {
	ListTasks(ctx context.Context, in *awsecs.ListTasksInput, opts ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error)
	DescribeTasks(ctx context.Context, in *awsecs.DescribeTasksInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error)
}

// Target names the service to attach to.
type Target struct {
	Cluster        string
	Service        string
	TaskID         string // optional: pin one task
	AgentContainer string // default "tetherd-agent"
}

// NotReadyError explains why running tasks were rejected.
type NotReadyError struct {
	Reasons []string
}

func (e *NotReadyError) Error() string {
	return "no attachable task:\n        " + strings.Join(e.Reasons, "\n        ")
}

// Discover returns the oldest RUNNING task that has the agent container,
// ECS Exec enabled and its ExecuteCommandAgent running (spec §6.2).
func Discover(ctx context.Context, api ECSAPI, t Target) (transport.Task, error) {
	if t.AgentContainer == "" {
		t.AgentContainer = "tetherd-agent"
	}
	list, err := api.ListTasks(ctx, &awsecs.ListTasksInput{
		Cluster:       aws.String(t.Cluster),
		ServiceName:   aws.String(t.Service),
		DesiredStatus: types.DesiredStatusRunning,
	})
	if err != nil {
		return transport.Task{}, fmt.Errorf("ListTasks %s/%s: %w", t.Cluster, t.Service, err)
	}
	if len(list.TaskArns) == 0 {
		return transport.Task{}, fmt.Errorf("no RUNNING tasks in service %s/%s", t.Cluster, t.Service)
	}
	desc, err := api.DescribeTasks(ctx, &awsecs.DescribeTasksInput{Cluster: aws.String(t.Cluster), Tasks: list.TaskArns})
	if err != nil {
		return transport.Task{}, fmt.Errorf("DescribeTasks: %w", err)
	}

	var eligible []transport.Task
	var reasons []string
	for _, task := range desc.Tasks {
		id := taskID(aws.ToString(task.TaskArn))
		if t.TaskID != "" && id != t.TaskID {
			continue
		}
		if aws.ToString(task.LastStatus) != "RUNNING" {
			continue
		}
		if !task.EnableExecuteCommand {
			reasons = append(reasons, fmt.Sprintf("task %s: ECS Exec is disabled (aws ecs update-service --enable-execute-command, then force a new deployment)", id))
			continue
		}
		var agent *types.Container
		for i := range task.Containers {
			if aws.ToString(task.Containers[i].Name) == t.AgentContainer {
				agent = &task.Containers[i]
			}
		}
		if agent == nil {
			reasons = append(reasons, fmt.Sprintf("task %s: no container named %q in the task", id, t.AgentContainer))
			continue
		}
		execStatus := ""
		for _, m := range agent.ManagedAgents {
			if m.Name == types.ManagedAgentNameExecuteCommandAgent {
				execStatus = aws.ToString(m.LastStatus)
			}
		}
		if execStatus != "RUNNING" {
			reasons = append(reasons, fmt.Sprintf("task %s: ExecuteCommandAgent is %q (not RUNNING yet?)", id, execStatus))
			continue
		}
		tt := transport.Task{Cluster: t.Cluster, ARN: aws.ToString(task.TaskArn), ID: id, RuntimeID: aws.ToString(agent.RuntimeId)}
		if task.StartedAt != nil {
			tt.StartedAt = *task.StartedAt
		}
		for _, a := range task.Attachments {
			if aws.ToString(a.Type) != "ElasticNetworkInterface" {
				continue
			}
			for _, d := range a.Details {
				if aws.ToString(d.Name) == "subnetId" {
					tt.SubnetID = aws.ToString(d.Value)
				}
			}
		}
		eligible = append(eligible, tt)
	}
	if len(eligible) == 0 {
		if t.TaskID != "" && len(reasons) == 0 {
			return transport.Task{}, fmt.Errorf("task %s is not RUNNING in %s/%s", t.TaskID, t.Cluster, t.Service)
		}
		return transport.Task{}, &NotReadyError{Reasons: reasons}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].StartedAt.Before(eligible[j].StartedAt) })
	return eligible[0], nil
}

// taskID is the last path segment of a task ARN.
func taskID(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}
```

`internal/provider/ecs/vpc.go`:

```go
package ecs

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EC2API is the subset of the EC2 client VPCCIDRs needs.
type EC2API interface {
	DescribeSubnets(ctx context.Context, in *awsec2.DescribeSubnetsInput, opts ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error)
	DescribeVpcs(ctx context.Context, in *awsec2.DescribeVpcsInput, opts ...func(*awsec2.Options)) (*awsec2.DescribeVpcsOutput, error)
}

// TaskRoleCIDR covers the task-role credential and metadata endpoints
// (169.254.170.2). It is always routed through the agent (spec §4.1).
var TaskRoleCIDR = netip.MustParsePrefix("169.254.170.0/24")

// VPCCIDRs returns every associated IPv4 CIDR of the VPC that subnetID
// belongs to (primary and secondary blocks).
func VPCCIDRs(ctx context.Context, api EC2API, subnetID string) ([]netip.Prefix, error) {
	sn, err := api.DescribeSubnets(ctx, &awsec2.DescribeSubnetsInput{SubnetIds: []string{subnetID}})
	if err != nil {
		return nil, fmt.Errorf("DescribeSubnets %s: %w", subnetID, err)
	}
	if len(sn.Subnets) == 0 {
		return nil, fmt.Errorf("subnet %s not found", subnetID)
	}
	vpcID := aws.ToString(sn.Subnets[0].VpcId)
	vp, err := api.DescribeVpcs(ctx, &awsec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil {
		return nil, fmt.Errorf("DescribeVpcs %s: %w", vpcID, err)
	}
	if len(vp.Vpcs) == 0 {
		return nil, fmt.Errorf("vpc %s not found", vpcID)
	}
	var out []netip.Prefix
	for _, a := range vp.Vpcs[0].CidrBlockAssociationSet {
		if a.CidrBlockState == nil || a.CidrBlockState.State != types.VpcCidrBlockStateCodeAssociated {
			continue
		}
		p, err := netip.ParsePrefix(aws.ToString(a.CidrBlock))
		if err != nil || !p.Addr().Is4() {
			continue
		}
		out = append(out, p.Masked())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("vpc %s has no associated IPv4 CIDR", vpcID)
	}
	return out, nil
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race ./internal/provider/... ./internal/transport/... && make lint`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/provider internal/transport/transport.go
git commit -m "feat(provider/ecs): task discovery and VPC CIDR lookup"
```

---

### Task 6: SSM トランスポート（`internal/transport/ssm`）

**Files:**
- Create: `internal/transport/ssm/ssm.go`
- Test: `internal/transport/ssm/ssm_test.go`

**Interfaces:**
- Consumes: `transport.Task{Cluster, ID, RuntimeID}`（Task 5）
- Produces:
  ```go
  type API interface {
      StartSession(ctx context.Context, in *ssm.StartSessionInput, opts ...func(*ssm.Options)) (*ssm.StartSessionOutput, error)
      TerminateSession(ctx context.Context, in *ssm.TerminateSessionInput, opts ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error)
  }
  type Transport struct{ API API; Region, Profile string; PluginPath string /* default "session-manager-plugin" */; Logf func(string, ...any) }
  func (t *Transport) Dial(ctx context.Context, task transport.Task) (net.Conn, error)
  func SessionTarget(task transport.Task) string   // "ecs:<cluster>_<id>_<runtimeId>"
  func PluginArgs(out *ssm.StartSessionOutput, in *ssm.StartSessionInput, region, profile string) ([]string, error)
  ```
  返る `net.Conn` の `Close()` は TCP を閉じ、plugin を kill し、`TerminateSession` を呼ぶ

- [ ] **Step 1: 失敗するテストを書く**

`internal/transport/ssm/ssm_test.go`（テストバイナリ自身を偽 plugin として再実行する）:

```go
package ssm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/kyosu-1/tetherd/internal/transport"
)

// TestMain doubles as the fake session-manager-plugin when re-executed with
// TETHERD_FAKE_PLUGIN=1: it validates argv, listens on localPortNumber and
// pipes bytes to FAKE_TARGET.
func TestMain(m *testing.M) {
	if os.Getenv("TETHERD_FAKE_PLUGIN") == "1" {
		os.Exit(fakePlugin(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakePlugin(args []string) int {
	if len(args) != 6 || args[2] != "StartSession" {
		fmt.Fprintf(os.Stderr, "bad argv: %q\n", args)
		return 2
	}
	var sess struct{ SessionId, TokenValue, StreamUrl string }
	if err := json.Unmarshal([]byte(args[0]), &sess); err != nil || sess.SessionId == "" {
		fmt.Fprintln(os.Stderr, "bad session json:", args[0])
		return 2
	}
	var params struct {
		Target       string
		DocumentName string
		Parameters   map[string][]string
	}
	if err := json.Unmarshal([]byte(args[4]), &params); err != nil || params.DocumentName != "AWS-StartPortForwardingSession" {
		fmt.Fprintln(os.Stderr, "bad params json:", args[4])
		return 2
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+params.Parameters["localPortNumber"][0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return 0
		}
		go func() {
			defer c.Close()
			t, err := net.Dial("tcp", os.Getenv("FAKE_TARGET"))
			if err != nil {
				return
			}
			defer t.Close()
			go io.Copy(t, c)
			io.Copy(c, t)
		}()
	}
}

type fakeAPI struct {
	started    *awsssm.StartSessionInput
	terminated string
}

func (f *fakeAPI) StartSession(_ context.Context, in *awsssm.StartSessionInput, _ ...func(*awsssm.Options)) (*awsssm.StartSessionOutput, error) {
	f.started = in
	return &awsssm.StartSessionOutput{SessionId: aws.String("sess-1"), TokenValue: aws.String("tok"), StreamUrl: aws.String("wss://example")}, nil
}

func (f *fakeAPI) TerminateSession(_ context.Context, in *awsssm.TerminateSessionInput, _ ...func(*awsssm.Options)) (*awsssm.TerminateSessionOutput, error) {
	f.terminated = aws.ToString(in.SessionId)
	return &awsssm.TerminateSessionOutput{}, nil
}

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func TestSessionTarget(t *testing.T) {
	got := SessionTarget(transport.Task{Cluster: "tetherd-dev", ID: "3f9c", RuntimeID: "3f9c-rt"})
	if got != "ecs:tetherd-dev_3f9c_3f9c-rt" {
		t.Fatalf("got %q", got)
	}
}

func TestDialThroughPlugin(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("TETHERD_FAKE_PLUGIN", "1")
	t.Setenv("FAKE_TARGET", echoServer(t))
	api := &fakeAPI{}
	tr := &Transport{API: api, Region: "ap-northeast-1", Profile: "personal", PluginPath: exe, Logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := tr.Dial(ctx, transport.Task{Cluster: "c", ID: "t1", RuntimeID: "rt1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(api.started.Target); got != "ecs:c_t1_rt1" {
		t.Fatalf("target = %q", got)
	}
	if aws.ToString(api.started.DocumentName) != "AWS-StartPortForwardingSession" || api.started.Parameters["portNumber"][0] != "9900" {
		t.Fatalf("input = %+v", api.started)
	}
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if api.terminated != "sess-1" {
		t.Fatalf("TerminateSession not called (got %q)", api.terminated)
	}
}

func TestPluginArgs(t *testing.T) {
	out := &awsssm.StartSessionOutput{SessionId: aws.String("s"), TokenValue: aws.String("t"), StreamUrl: aws.String("wss://x")}
	in := &awsssm.StartSessionInput{Target: aws.String("ecs:c_t_r"), DocumentName: aws.String("AWS-StartPortForwardingSession"), Parameters: map[string][]string{"portNumber": {"9900"}, "localPortNumber": {"15000"}}}
	args, err := PluginArgs(out, in, "ap-northeast-1", "personal")
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 6 || args[1] != "ap-northeast-1" || args[2] != "StartSession" || args[3] != "personal" || args[5] != "https://ssm.ap-northeast-1.amazonaws.com" {
		t.Fatalf("args = %q", args)
	}
	if !strings.Contains(args[0], `"SessionId":"s"`) || !strings.Contains(args[4], `"Target":"ecs:c_t_r"`) {
		t.Fatalf("json args = %q %q", args[0], args[4])
	}
}

func TestDialFailsWhenPluginMissing(t *testing.T) {
	tr := &Transport{API: &fakeAPI{}, Region: "r", PluginPath: "/nonexistent/session-manager-plugin"}
	if _, err := tr.Dial(context.Background(), transport.Task{Cluster: "c", ID: "t", RuntimeID: "r"}); err == nil || !strings.Contains(err.Error(), "session-manager-plugin") {
		t.Fatalf("want plugin-missing error, got %v", err)
	}
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go get github.com/aws/aws-sdk-go-v2/service/ssm@latest && go test ./internal/transport/ssm/`
Expected: FAIL

- [ ] **Step 3: 実装**

`internal/transport/ssm/ssm.go`:

```go
// Package ssm reaches the agent's control port through an SSM port
// forwarding session, exactly as `aws ssm start-session` does: call
// StartSession with the SDK, hand the reply to session-manager-plugin, and
// dial the local port the plugin opens. The AWS CLI itself is not needed.
package ssm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/kyosu-1/tetherd/internal/transport"
)

// API is the subset of the SSM client used here.
type API interface {
	StartSession(ctx context.Context, in *awsssm.StartSessionInput, opts ...func(*awsssm.Options)) (*awsssm.StartSessionOutput, error)
	TerminateSession(ctx context.Context, in *awsssm.TerminateSessionInput, opts ...func(*awsssm.Options)) (*awsssm.TerminateSessionOutput, error)
}

// Transport implements transport.Transport over SSM.
type Transport struct {
	API        API
	Region     string
	Profile    string // passed to the plugin; "" is fine
	PluginPath string // default "session-manager-plugin" (looked up in PATH)
	Logf       func(string, ...any)
}

const (
	document    = "AWS-StartPortForwardingSession"
	controlPort = "9900"
	startupWait = 20 * time.Second
)

// SessionTarget formats the ECS Exec target for StartSession.
func SessionTarget(task transport.Task) string {
	return fmt.Sprintf("ecs:%s_%s_%s", task.Cluster, task.ID, task.RuntimeID)
}

// PluginArgs builds session-manager-plugin's argv the way the AWS CLI does:
// session JSON, region, "StartSession", profile, request JSON, endpoint.
func PluginArgs(out *awsssm.StartSessionOutput, in *awsssm.StartSessionInput, region, profile string) ([]string, error) {
	sess, err := json.Marshal(map[string]string{
		"SessionId":  aws.ToString(out.SessionId),
		"TokenValue": aws.ToString(out.TokenValue),
		"StreamUrl":  aws.ToString(out.StreamUrl),
	})
	if err != nil {
		return nil, err
	}
	req, err := json.Marshal(map[string]any{
		"Target":       aws.ToString(in.Target),
		"DocumentName": aws.ToString(in.DocumentName),
		"Parameters":   in.Parameters,
	})
	if err != nil {
		return nil, err
	}
	return []string{string(sess), region, "StartSession", profile, string(req), "https://ssm." + region + ".amazonaws.com"}, nil
}

// Dial implements transport.Transport.
func (t *Transport) Dial(ctx context.Context, task transport.Task) (net.Conn, error) {
	plugin := t.PluginPath
	if plugin == "" {
		plugin = "session-manager-plugin"
	}
	if _, err := exec.LookPath(plugin); err != nil {
		return nil, fmt.Errorf("session-manager-plugin not found (%v); install it: https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html", err)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	in := &awsssm.StartSessionInput{
		Target:       aws.String(SessionTarget(task)),
		DocumentName: aws.String(document),
		Parameters:   map[string][]string{"portNumber": {controlPort}, "localPortNumber": {strconv.Itoa(port)}},
	}
	out, err := t.API.StartSession(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("ssm:StartSession for %s: %w", aws.ToString(in.Target), err)
	}
	args, err := PluginArgs(out, in, t.Region, t.Profile)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(plugin, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil // the plugin prints "Starting session…"; keep our stderr clean
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.terminate(out)
		return nil, fmt.Errorf("start session-manager-plugin: %w", err)
	}
	t.logf("ssm session %s → 127.0.0.1:%d", aws.ToString(out.SessionId), port)

	// Wait for the plugin to open the local port.
	deadline := time.Now().Add(startupWait)
	var conn net.Conn
	for {
		if ctx.Err() != nil {
			cmd.Process.Kill()
			t.terminate(out)
			return nil, ctx.Err()
		}
		conn, err = net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
		if err == nil {
			break
		}
		if cmd.ProcessState != nil || time.Now().After(deadline) {
			cmd.Process.Kill()
			t.terminate(out)
			return nil, fmt.Errorf("session-manager-plugin did not open 127.0.0.1:%d within %s: %v", port, startupWait, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return &pluginConn{Conn: conn, cmd: cmd, t: t, out: out}, nil
}

func (t *Transport) terminate(out *awsssm.StartSessionOutput) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := t.API.TerminateSession(ctx, &awsssm.TerminateSessionInput{SessionId: out.SessionId}); err != nil {
		t.logf("ssm:TerminateSession: %v", err)
	}
}

func (t *Transport) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

// pluginConn ties the TCP connection to the plugin process and the session.
type pluginConn struct {
	net.Conn
	cmd  *exec.Cmd
	t    *Transport
	out  *awsssm.StartSessionOutput
	once sync.Once
}

func (c *pluginConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.cmd.Process != nil {
			c.cmd.Process.Kill()
			c.cmd.Wait()
		}
		c.t.terminate(c.out)
	})
	return err
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

var _ transport.Transport = (*Transport)(nil)
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test -race -count=1 ./internal/transport/... && make lint`
Expected: PASS（`TestDialThroughPlugin` はテストバイナリを偽 plugin として再実行する）

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/transport/ssm
git commit -m "feat(transport/ssm): StartSession + session-manager-plugin port forwarding"
```

---

### Task 7: タスクロールの確認（`internal/awsid`）

**Files:**
- Create: `internal/awsid/awsid.go`
- Test: `internal/awsid/awsid_test.go`

**Interfaces:**
- Consumes: `session.Client.DialTCP` の型 `func(ctx context.Context, addr string) (net.Conn, error)`（Task 3 の v0.1 セッション）
- Produces:
  ```go
  const CredentialsHost = "169.254.170.2"
  func FetchContainerCredentials(ctx context.Context, dial func(ctx context.Context, addr string) (net.Conn, error), relativeURI string) (aws.Credentials, error)
  func CallerIdentity(ctx context.Context, creds aws.Credentials, region string, optFns ...func(*sts.Options)) (arn string, err error)
  ```

- [ ] **Step 1: 失敗するテストを書く**

`internal/awsid/awsid_test.go`:

```go
package awsid

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func TestFetchContainerCredentialsThroughDialer(t *testing.T) {
	var gotPath, gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost = r.URL.Path, r.Host
		w.Write([]byte(`{"AccessKeyId":"AKIA","SecretAccessKey":"sk","Token":"tok","Expiration":"2030-01-01T00:00:00Z","RoleArn":"arn:aws:iam::1:role/x"}`))
	}))
	defer srv.Close()
	var dialedAddr string
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		dialedAddr = addr
		return net.Dial("tcp", srv.Listener.Addr().String()) // pretend this is the agent dialing 169.254.170.2
	}
	creds, err := FetchContainerCredentials(context.Background(), dial, "/v2/credentials/abc")
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessKeyID != "AKIA" || creds.SecretAccessKey != "sk" || creds.SessionToken != "tok" || !creds.CanExpire {
		t.Fatalf("creds = %+v", creds)
	}
	if dialedAddr != "169.254.170.2:80" || gotPath != "/v2/credentials/abc" || gotHost != "169.254.170.2" {
		t.Fatalf("dialed %q path %q host %q", dialedAddr, gotPath, gotHost)
	}
}

func TestCallerIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unsigned", 403)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:sts::1:assumed-role/task/abc</Arn><UserId>u</UserId><Account>1</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`))
	}))
	defer srv.Close()
	arn, err := CallerIdentity(context.Background(), aws.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "sk", SessionToken: "tok"}, "ap-northeast-1",
		func(o *sts.Options) { o.BaseEndpoint = aws.String(srv.URL) })
	if err != nil {
		t.Fatal(err)
	}
	if arn != "arn:aws:sts::1:assumed-role/task/abc" {
		t.Fatalf("arn = %q", arn)
	}
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/awsid/`
Expected: FAIL

- [ ] **Step 3: 実装**

`internal/awsid/awsid.go`:

```go
// Package awsid proves the task role reaches the laptop: it fetches the
// container credentials from 169.254.170.2 through the agent (the same
// path the child's AWS SDK takes) and calls sts:GetCallerIdentity with
// them (spec §6.3 step 7).
package awsid

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// CredentialsHost is the ECS task-role credential endpoint.
const CredentialsHost = "169.254.170.2"

type containerCreds struct {
	AccessKeyID     string    `json:"AccessKeyId"`
	SecretAccessKey string    `json:"SecretAccessKey"`
	Token           string    `json:"Token"`
	Expiration      time.Time `json:"Expiration"`
}

// FetchContainerCredentials GETs http://169.254.170.2<relativeURI> using
// dial for the TCP connection (session.Client.DialTCP in production).
func FetchContainerCredentials(ctx context.Context, dial func(ctx context.Context, addr string) (net.Conn, error), relativeURI string) (aws.Credentials, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return dial(ctx, addr) },
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+CredentialsHost+relativeURI, nil)
	if err != nil {
		return aws.Credentials{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("fetch task credentials via agent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return aws.Credentials{}, fmt.Errorf("task credentials endpoint: HTTP %d", resp.StatusCode)
	}
	var c containerCreds
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return aws.Credentials{}, fmt.Errorf("task credentials: %w", err)
	}
	return aws.Credentials{
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.Token,
		CanExpire: true, Expires: c.Expiration, Source: "tetherd(169.254.170.2)",
	}, nil
}

// CallerIdentity returns the ARN sts sees for creds.
func CallerIdentity(ctx context.Context, creds aws.Credentials, region string, optFns ...func(*sts.Options)) (string, error) {
	cfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken),
	}
	out, err := sts.NewFromConfig(cfg, optFns...).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("sts:GetCallerIdentity with task credentials: %w", err)
	}
	return aws.ToString(out.Arn), nil
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go get github.com/aws/aws-sdk-go-v2/credentials@latest && go test -race ./internal/awsid/ && make lint`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/awsid
git commit -m "feat(awsid): fetch task credentials through the agent and verify with STS"
```

---

### Task 8: `tetherd run` を SSM・発見・env 注入・IAM 確認に接続

**Files:**
- Modify: `internal/cli/run.go`, `internal/cli/root.go`
- Test: `internal/cli/run_test.go`（追記・修正）

**Interfaces:**
- Consumes: `ecsprov.Discover/VPCCIDRs/TaskRoleCIDR/NotReadyError`（Task 5）、`ssmtr.Transport`（Task 6）、`awsid.*`（Task 7）、`env.Merge`（Task 4）、`proto.Welcome.AppEnv/EnvError`（Task 3）
- Produces:
  ```go
  type RunOptions struct {
      Transport, AgentAddr string; RemoteCIDRs []string; HelperSocket, ExecPath, User string; NoNetwork bool; Command []string
      // new
      Profile, Region, Cluster, Service, TaskID, TargetEnv string
      NoEnv bool
  }
  func LocalOverlaps(cidrs []netip.Prefix, addrs []net.Addr) []string   // 重なる "cidr ⟷ local addr" の一覧
  ```
  フラグ: `--transport` 既定 `ssm`、`--profile`、`--region`、`--cluster`、`--service`、`--task`、`--env`（既定 `dev`）、`--no-env`。`--remote-cidr` は「追加分」

- [ ] **Step 1: 失敗するテストを書く**

`internal/cli/run_test.go` の `TestRunCommandParsesFlagsAndCommand` の SetArgs を次に置き換え、期待を足す:

```go
	root.SetArgs([]string{"run", "--profile", "personal", "--region", "ap-northeast-1", "--cluster", "tetherd-dev", "--service", "api",
		"--env", "dev", "--remote-cidr", "10.1.0.0/16", "--user", "shota", "--no-env",
		"--", "psql", "-h", "db"})
	...
	if captured.Transport != "ssm" || captured.Profile != "personal" || captured.Cluster != "tetherd-dev" || captured.Service != "api" || captured.TargetEnv != "dev" || !captured.NoEnv {
		t.Fatalf("opts = %+v", captured)
	}
	if len(captured.RemoteCIDRs) != 1 || captured.RemoteCIDRs[0] != "10.1.0.0/16" || len(captured.Command) != 3 || captured.Command[0] != "psql" {
		t.Fatalf("opts = %+v", captured)
	}
```

（既存の `--transport direct --agent-addr` のテストは `TestRunCommandDirectFlags` として残す。）

追加:

```go
func TestLocalOverlaps(t *testing.T) {
	cidrs := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("169.254.170.0/24")}
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("10.0.5.7"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("192.168.1.20"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
	}
	got := LocalOverlaps(cidrs, addrs)
	if len(got) != 1 || !strings.Contains(got[0], "10.0.0.0/16") || !strings.Contains(got[0], "10.0.5.7") {
		t.Fatalf("got %v", got)
	}
}

func TestRunSSMRequiresClusterAndService(t *testing.T) {
	code, err := Run(context.Background(), RunOptions{Transport: "ssm", Command: []string{"true"}}, io.Discard)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--cluster") {
		t.Fatalf("code %d err %v", code, err)
	}
}
```

（import に `context`, `io`, `net`, `net/netip`, `strings` を足す。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/cli/`
Expected: FAIL（unknown flag `--profile`、`undefined: LocalOverlaps`）

- [ ] **Step 3: 実装**

`internal/cli/root.go` の `newRunCommand` のフラグ部分を置き換え:

```go
	f := cmd.Flags()
	f.StringVar(&opts.Transport, "transport", "ssm", "how to reach the agent: ssm | direct")
	f.StringVar(&opts.Profile, "profile", "", "AWS profile (default: SDK default chain)")
	f.StringVar(&opts.Region, "region", "", "AWS region (default: from the profile)")
	f.StringVar(&opts.Cluster, "cluster", "", "ECS cluster of the dev service")
	f.StringVar(&opts.Service, "service", "", "ECS service to attach to")
	f.StringVar(&opts.TaskID, "task", "", "attach to this task ID instead of the oldest running one")
	f.StringVar(&opts.TargetEnv, "env", "dev", "expected TETHERD_ENV of the agent; refuse to attach otherwise")
	f.StringVar(&opts.AgentAddr, "agent-addr", "", "agent control address for --transport direct (host:port)")
	f.StringArrayVar(&opts.RemoteCIDRs, "remote-cidr", nil, "additional destination CIDR to route through the agent (repeatable; the VPC CIDR is added automatically with --transport ssm)")
	f.StringVar(&opts.HelperSocket, "helper-socket", helper.DefaultSocket, "tetherd-helper socket")
	f.StringVar(&opts.ExecPath, "exec-path", helper.ExecInstallDir+"/"+helper.ExecName, "path of the setgid tetherd-exec")
	f.StringVar(&opts.User, "user", "", "user name sent to the agent (default $USER)")
	f.BoolVar(&opts.NoNetwork, "no-network", false, "do not capture traffic (only connect to the agent)")
	f.BoolVar(&opts.NoEnv, "no-env", false, "do not inject the task's environment into the command")
```

`internal/cli/run.go` を置き換え:

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/kyosu-1/tetherd/internal/awsid"
	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/capture/pfrdr"
	"github.com/kyosu-1/tetherd/internal/env"
	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/proto"
	ecsprov "github.com/kyosu-1/tetherd/internal/provider/ecs"
	"github.com/kyosu-1/tetherd/internal/proxy"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
	"github.com/kyosu-1/tetherd/internal/transport/direct"
	ssmtr "github.com/kyosu-1/tetherd/internal/transport/ssm"
)

// RunOptions are the flags of `tetherd run`.
type RunOptions struct {
	Transport    string
	AgentAddr    string
	RemoteCIDRs  []string
	HelperSocket string
	ExecPath     string
	User         string
	NoNetwork    bool
	Command      []string

	Profile   string
	Region    string
	Cluster   string
	Service   string
	TaskID    string
	TargetEnv string
	NoEnv     bool
}

// ParseRemoteCIDRs parses IPv4 prefixes.
func ParseRemoteCIDRs(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("--remote-cidr %q: %w", s, err)
		}
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("--remote-cidr %q: only IPv4 is supported in v1", s)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// LocalOverlaps lists remote CIDRs that contain one of the laptop's own
// addresses: traffic from the child to that part of the LAN would be
// routed through the agent (spec §4.1).
func LocalOverlaps(cidrs []netip.Prefix, addrs []net.Addr) []string {
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.Is4() || ip.IsLoopback() {
			continue
		}
		for _, c := range cidrs {
			if c.Contains(ip) {
				out = append(out, fmt.Sprintf("%s ⟷ local %s", c, ip))
			}
		}
	}
	return out
}

// Run connects to the agent, installs capture, runs the command and cleans
// up. It returns the child's exit code.
func Run(ctx context.Context, opts RunOptions, stderr io.Writer) (int, error) {
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, "tetherd  "+format+"\n", args...) }
	if len(opts.Command) == 0 {
		return 2, errors.New("no command given")
	}
	extra, err := ParseRemoteCIDRs(opts.RemoteCIDRs)
	if err != nil {
		return 2, err
	}

	// 1. transport, task, remote set
	var tr transport.Transport
	var task transport.Task
	var cidrs []netip.Prefix
	region := opts.Region
	switch opts.Transport {
	case "direct":
		if opts.AgentAddr == "" {
			return 2, errors.New("--transport direct needs --agent-addr")
		}
		if !opts.NoNetwork && len(extra) == 0 {
			return 2, errors.New("no --remote-cidr given (or use --no-network)")
		}
		tr, task, cidrs = direct.Transport{}, transport.Task{ID: "direct", Addr: opts.AgentAddr}, extra
	case "ssm":
		if opts.Cluster == "" || opts.Service == "" {
			return 2, errors.New("--transport ssm needs --cluster and --service")
		}
		var loadOpts []func(*awsconfig.LoadOptions) error
		if opts.Profile != "" {
			loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(opts.Profile))
		}
		if opts.Region != "" {
			loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
		}
		awscfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
		if err != nil {
			return 1, fmt.Errorf("aws config: %w", err)
		}
		region = awscfg.Region
		task, err = ecsprov.Discover(ctx, awsecs.NewFromConfig(awscfg), ecsprov.Target{Cluster: opts.Cluster, Service: opts.Service, TaskID: opts.TaskID})
		if err != nil {
			return 1, err
		}
		logf("%s/%s  task %s  (started %s ago)", opts.Cluster, opts.Service, short(task.ID), time.Since(task.StartedAt).Round(time.Minute))
		if !opts.NoNetwork {
			vpc, err := ecsprov.VPCCIDRs(ctx, awsec2.NewFromConfig(awscfg), task.SubnetID)
			if err != nil {
				return 1, err
			}
			cidrs = append(append(vpc, ecsprov.TaskRoleCIDR), extra...)
		}
		tr = &ssmtr.Transport{API: awsssm.NewFromConfig(awscfg), Region: awscfg.Region, Profile: opts.Profile, Logf: logf}
	default:
		return 2, fmt.Errorf("unknown transport %q (ssm | direct)", opts.Transport)
	}

	// 2. session
	conn, err := tr.Dial(ctx, task)
	if err != nil {
		return 1, fmt.Errorf("connect to agent: %w", err)
	}
	sess, err := session.Dial(ctx, conn, proto.Hello{Version: proto.Version, User: opts.User}, session.Options{})
	if err != nil {
		conn.Close()
		return 1, err
	}
	defer sess.Close()
	w := sess.Welcome()
	if opts.Transport == "ssm" && w.Env != opts.TargetEnv {
		return 1, fmt.Errorf("refusing to attach: agent reports TETHERD_ENV=%q, expected %q\n        tetherd never attaches to an environment it was not pointed at; check --env / --cluster", w.Env, opts.TargetEnv)
	}
	taskEnv := w.AppEnv
	switch {
	case opts.NoEnv:
		taskEnv = nil
		logf("env      skipped (--no-env)")
	case w.EnvError != "" && opts.Transport == "direct":
		taskEnv = nil
		logf("env      unavailable: %s", w.EnvError)
	case w.EnvError != "":
		return 1, fmt.Errorf("env: %s\n        Use --no-env to run without the task's environment", w.EnvError)
	default:
		logf("✓ env      %d vars from the task", len(taskEnv))
	}

	// 3. capture (helper + pf)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !opts.NoNetwork {
		if ifs, err := net.InterfaceAddrs(); err == nil {
			for _, o := range LocalOverlaps(cidrs, ifs) {
				logf("⚠ remote CIDR overlaps this machine's network: %s (that part of the LAN is routed through the agent for the child)", o)
			}
		}
		hc, err := helper.Dial(opts.HelperSocket)
		if err != nil {
			return 1, err
		}
		defer hc.Close()
		cap := pfrdr.New(hc)
		cap.Logf = logf
		if err := cap.Start(ctx, capture.Spec{RemoteCIDRs: cidrs}); err != nil {
			var busy *helper.BusyError
			if errors.As(err, &busy) {
				return 1, fmt.Errorf("%w\n        Stop the other `tetherd run` first (one session per machine in v1)", busy)
			}
			return 1, err
		}
		// cancel() must run before cap.Close() (see proxy.Forwarder.Run).
		defer func() {
			cancel()
			cap.Close()
		}()
		fw := &proxy.Forwarder{Capturer: cap, Dial: sess.DialTCP, Logf: logf}
		go func() {
			if err := fw.Run(ctx); err != nil && ctx.Err() == nil {
				logf("✗ capture stopped: %v", err)
			}
		}()
		logf("✓ network  transparent (pf rdr, gid tetherd) · remote: %s", joinPrefixes(cidrs))

		// 4. task role: fetch credentials the way the child's SDK will.
		if uri := taskEnv["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]; uri != "" {
			ictx, icancel := context.WithTimeout(ctx, 15*time.Second)
			creds, err := awsid.FetchContainerCredentials(ictx, sess.DialTCP, uri)
			var arn string
			if err == nil {
				arn, err = awsid.CallerIdentity(ictx, creds, region)
			}
			icancel()
			if err != nil {
				logf("⚠ iam      %v", err)
			} else {
				logf("✓ iam      %s  (via 169.254.170.2)", arn)
			}
		}
	}

	// 5. child
	var child *exec.Cmd
	if opts.NoNetwork {
		child = exec.CommandContext(ctx, opts.Command[0], opts.Command[1:]...)
	} else {
		if _, err := os.Stat(opts.ExecPath); err != nil {
			return 1, fmt.Errorf("%s not found; is tetherd-helper running? (%w)", opts.ExecPath, err)
		}
		child = exec.CommandContext(ctx, opts.ExecPath, append([]string{"--"}, opts.Command...)...)
	}
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Env = env.Merge(os.Environ(), taskEnv, env.Options{DropAWSContainer: opts.NoNetwork})
	child.Cancel = func() error { return child.Process.Signal(os.Interrupt) }
	child.WaitDelay = 5 * time.Second
	logf("▶ %s", joinArgs(opts.Command))
	if err := child.Start(); err != nil {
		return 1, fmt.Errorf("start %s: %w", opts.Command[0], err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- child.Wait() }()
	select {
	case err := <-waitErr:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		if err != nil {
			return 1, err
		}
		return 0, nil
	case <-sess.Done():
		logf("✗ agent session lost: %v", sess.Err())
		cancel()
		<-waitErr
		return 1, sess.Err()
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func joinPrefixes(ps []netip.Prefix) string {
	s := ""
	for i, p := range ps {
		if i > 0 {
			s += ", "
		}
		s += p.String()
	}
	return s
}

func joinArgs(a []string) string {
	s := ""
	for i, x := range a {
		if i > 0 {
			s += " "
		}
		s += x
	}
	return s
}
```

`hack/e2e-local.sh` は `--transport direct` を明示しているので変更不要。`env.Merge(os.Environ(), nil, …)` は `taskEnv` が nil のとき単にローカル env を返す。

- [ ] **Step 4: テストとビルドを確認**

Run: `go test -race -count=1 ./internal/cli/ && make lint && make test && GOOS=linux go vet ./... && make build && ./bin/tetherd run --help | grep -E 'cluster|no-env'`
Expected: PASS、ヘルプに `--cluster` と `--no-env` が出る。可能なら `make e2e-local`（sudo）で v0.1 の経路が壊れていないことも確認する（`env      unavailable: no ECS metadata endpoint …` の行が増える以外は同じ）

- [ ] **Step 5: Commit**

```bash
git add internal/cli go.mod go.sum
git commit -m "feat(cli): ssm transport, task discovery, VPC routing, env injection and task-role check"
```

---

### Task 9: AWS e2e チェックリストとドキュメント

**Files:**
- Create: `docs/e2e-aws.md`
- Modify: `docs/design.md`（§5 の `run` フラグに `--cluster/--service/--profile/--env/--no-env` を追記）、`docs/specs/2026-09-12-v1-macos-design.md` §12（5・6 の検証結果は実行後に追記する旨）

- [ ] **Step 1: チェックリストを書く**

`docs/e2e-aws.md`:

```markdown
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
```

- [ ] **Step 2: design.md / spec の追記**

`docs/design.md` §5 の `tetherd run [flags]` ブロックに次を足す:

```
      --profile / --region   AWS プロファイルとリージョン
      --cluster / --service  対象の ECS サービス（v0.2b で .tetherd.yml から）
      --env NAME   agent の TETHERD_ENV と照合（既定 dev）
      --no-env     タスクの env を注入しない
      --transport  ssm（既定）| direct（ローカル e2e 用）
```

`docs/specs/2026-09-12-v1-macos-design.md` §12 の表の 5・6 行を「v0.2a の `docs/e2e-aws.md` で検証」に更新する（結果は実行後に追記）。

- [ ] **Step 3: Commit**

```bash
git add docs
git commit -m "docs: AWS e2e checklist and run flags for v0.2a"
```

---

## 完了条件

- `make lint test build` が Linux / macOS で通る（CI）
- `make e2e-local` が引き続き通る（v0.1 の経路の非回帰）
- `terraform validate` / `plan` が通り、**ユーザーが apply** して `make push-images` 後にタスクが RUNNING になる
- `docs/e2e-aws.md` の 1〜4、7〜9 が手元で通る（5・6 は観察項目）
- spec §12 の 5・6 に結果を追記する

## 次の計画（v0.2b）

`.tetherd.yml` / `~/.tetherd/config.yml` の読み込み、`/etc/resolver` を使う DNS リゾルバと `resolve` ストリーム、`remote_services`（managed prefix list）、`local_cidrs`、`env` コマンド（secrets マスクは `DescribeTaskDefinition` の `secrets` 名で）、`doctor` の AWS 側検査。
