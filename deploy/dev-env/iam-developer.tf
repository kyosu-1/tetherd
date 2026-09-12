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
  # `tetherd doctor`'s target group row: steal only works behind an HTTP1
  # target group (spec §5.1 puts gRPC and HTTP2 out of scope), and
  # protocol_version is knowable only from DescribeTargetGroups - the ECS
  # service says which target group a task is registered in, not what
  # protocol version it speaks. The same call gives the row the target
  # group's port, which it reports as a warning rather than a failure when
  # it differs from the agent's default: TETHERD_PROXY can move the port
  # the agent serves the ALB on, so a difference is a question, not a
  # verdict.
  #
  # resources = ["*"] is the narrowest scope this action has. Elastic Load
  # Balancing's Describe* actions take no resource ARN at all (the Service
  # Authorization Reference lists no resource type for
  # DescribeTargetGroups), so an ARN here would deny every call. It is a
  # read of the target group's own configuration and carries no target
  # health or traffic.
  statement {
    sid       = "InspectTargetGroup"
    actions   = ["elasticloadbalancing:DescribeTargetGroups"]
    resources = ["*"]
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
