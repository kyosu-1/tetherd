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
