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
