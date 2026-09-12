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
  description = "AWS CLI profile (empty = default credential chain). Defaults to \"personal\", the profile this repo's own dev env is deployed under; override it for any other account"
  type        = string
  default     = "personal"
}

variable "allowed_ingress_cidrs" {
  type        = list(string)
  default     = ["0.0.0.0/0"]
  description = "CIDRs allowed to reach the ALB on :80; narrow to your IP for anything beyond a throwaway env"
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
