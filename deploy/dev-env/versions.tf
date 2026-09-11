terraform {
  required_version = ">= 1.10" # backend "s3" use_lockfile (native S3 locking)
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 6.0" }
    random = { source = "hashicorp/random", version = "~> 3.6" }
  }

  # State lives in S3 (bucket created once by hand, see README) so the
  # environment survives worktree cleanup. Locking uses S3 conditional
  # writes (Terraform >= 1.10), no DynamoDB table needed. The backend does
  # not understand `aws login` sessions, so credentials come from the
  # environment: eval "$(aws configure export-credentials --profile personal --format env)"
  backend "s3" {
    bucket       = "tetherd-tfstate-738925651667"
    key          = "dev-env/terraform.tfstate"
    region       = "ap-northeast-1"
    use_lockfile = true
  }
}

provider "aws" {
  region  = var.region
  profile = var.profile
  default_tags {
    tags = { Project = "tetherd", Environment = "dev-env" }
  }
}
