resource "aws_security_group" "alb" {
  name   = "${var.name}-alb"
  vpc_id = aws_vpc.this.id
  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = var.allowed_ingress_cidrs
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

# The agent is always on the data path: it reverse-proxies to the app on
# 8081 and steals only requests whose user and token match an attached
# session. With nobody attached this is a pass-through (spec §5.1).
resource "aws_lb_target_group" "app" {
  # name_prefix, not name, and create_before_destroy below: changing `port`
  # forces replacement, and a target group cannot be deleted while a listener
  # still forwards to it. Destroy-then-create therefore fails with
  # ResourceInUse -- which it did, on the first apply of the move to 8080,
  # after the security group had already been updated. That left the ALB
  # unable to reach the app at all: ingress allowed only 8080 while the target
  # group still health-checked 8081.
  #
  # create_before_destroy makes the new group, repoints the listener and the
  # service, then deletes the old one. A fixed name would collide during the
  # overlap (DuplicateTargetGroupName), so the name is generated. Target group
  # name_prefix is limited to 6 characters by the API.
  name_prefix = "tethrd"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.this.id

  lifecycle {
    create_before_destroy = true
  }

  health_check {
    # Stays /healthz, the endpoint the app serves for exactly this purpose.
    # The agent does not special-case the health check: a request carrying no
    # matching steal headers goes to the app, and the ALB's never carries
    # them. So the path is the agent's business either way, and a dedicated
    # endpoint is still worth having - it keeps the health check off the
    # application's root handler.
    path                = "/healthz"
    matcher             = "200"
    interval            = 15
    healthy_threshold   = 2
    unhealthy_threshold = 3
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
