resource "aws_ecr_repository" "agent" {
  name         = "tetherd-agent"
  force_delete = true
}

resource "aws_ecr_repository" "sampleapp" {
  name         = "tetherd-sampleapp"
  force_delete = true
}
