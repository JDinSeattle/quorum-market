output "alb_dns_name" {
  description = "Public entry point for the whole system."
  value       = module.networking.alb_dns_name
}

output "api_base_url" {
  description = "The only public entry point. Every path is served through the gateway."
  value       = "http://${module.networking.alb_dns_name}"
}

output "internal_base_url" {
  description = "How services reach each other. VPC only."
  value       = module.networking.internal_base_url
}

output "ecr_repository_url" {
  description = "Push the application image here before applying."
  value       = aws_ecr_repository.app.repository_url
}

output "services_1_public_ip" {
  description = "RabbitMQ management UI is on :15672 of this host."
  value       = aws_instance.services_1.public_ip
}

output "services_1_private_ip" {
  description = "RabbitMQ, Redis, the authorizer and the warehouse."
  value       = aws_instance.services_1.private_ip
}

output "services_2_public_ip" {
  description = "Identity, orders and notifications."
  value       = aws_instance.services_2.public_ip
}

output "core_db_urls" {
  value = module.core_db.node_urls
}

output "product_db_urls" {
  value = module.product_db.node_urls
}

output "cart_db_urls" {
  value = module.cart_db.node_urls
}

output "dashboard_url" {
  description = "CloudWatch dashboard for the deployed stack."
  value       = module.monitoring.dashboard_url
}

output "load_generator_public_ip" {
  description = "SSH here and run Locust, when deploy_load_generator is true."
  value       = var.deploy_load_generator ? module.load_generator[0].public_ip : null
}
