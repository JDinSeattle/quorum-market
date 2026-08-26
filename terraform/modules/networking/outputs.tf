output "vpc_id" { value = aws_vpc.main.id }
output "vpc_cidr" { value = aws_vpc.main.cidr_block }
output "app_subnet_ids" { value = aws_subnet.app[*].id }
output "data_subnet_id" { value = aws_subnet.data.id }
output "alb_dns_name" { value = aws_lb.main.dns_name }
output "alb_arn_suffix" { value = aws_lb.main.arn_suffix }
output "alb_listener_arn" { value = aws_lb_listener.http.arn }
output "alb_internal_listener_arn" { value = aws_lb_listener.internal.arn }

output "internal_base_url" {
  description = "Base URL services use to reach each other through the internal listener."
  value       = "http://${aws_lb.main.dns_name}:8081"
}
output "alb_sg_id" { value = aws_security_group.alb.id }
output "internal_sg_id" { value = aws_security_group.internal.id }
