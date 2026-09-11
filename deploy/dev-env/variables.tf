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
