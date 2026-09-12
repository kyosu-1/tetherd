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
