# Quorum Market — AWS topology
#
#                              internet
#                                  │
#                        ┌─────────▼──────────┐
#                        │  ALB  :80  (public)│
#                        └─────────┬──────────┘
#                            everything
#                        ┌─────────▼──────────┐
#                        │    Gateway ASG     │  auth, rate limiting, routing
#                        └─────────┬──────────┘
#                                  │
#                        ┌─────────▼──────────┐
#                        │ ALB :8081 (VPC only)│
#                        └──┬───┬───┬───┬───┬─┘
#          /product*        │   │   │   │   │   /identity*
#          /shopping-cart*  │   │   │   │   │   /orders*  /notifications*
#      ┌───────────────┐    │   │   │   │   │
#      │ product ASG   │◄───┘   │   │   │   └──►┌──────────────────────────┐
#      │ (CPU 60%)     │        │   │   │       │ services-2:              │
#      └───────┬───────┘        │   │   └──────►│  identity, orders,       │
#              │       ┌────────▼───▼──┐        │  notifications           │
#      ┌───────▼─────┐ │ cart ASG      │        └────────────┬─────────────┘
#      │ product-db  │ │ (req/target)  │                     │
#      │ leader + 4  │ └───┬───────┬───┘                     │
#      │ W=5 R=1     │     │       │                         │
#      └─────────────┘     │       │                         │
#              ┌───────────▼──┐    │      ┌──────────────────▼──────────┐
#              │ cart-db      │    │      │ core-db (3 nodes, W=2 R=2)  │
#              │ 5 nodes W3R3 │    │      │ accounts and orders         │
#              └──────────────┘    │      └─────────────────────────────┘
#                                  │
#      ┌───────────────────────────▼───────────────────────────┐
#      │ services-1: rabbitmq + redis + cca + warehouse        │
#      └───────────────────────────────────────────────────────┘
#
# Only the gateway is publicly reachable. Everything else answers on the
# internal listener, which the security group restricts to the VPC, so the
# identity headers the services trust cannot be set by anyone outside.
#
# The gateway, product and cart services autoscale, because their load tracks
# the number of shoppers. The databases, the broker, Redis and the remaining
# services are fixed instances: none of them scales with shopper count, and the
# warehouse holds its ledger in memory so it cannot be replicated at all.

locals {
  image = "${var.ecr_registry}/${var.project_name}:${var.image_tag}"

  # Every instance pulls from ECR the same way.
  ecr_login = "aws ecr get-login-password --region ${var.aws_region} | docker login --username AWS --password-stdin ${var.ecr_registry}"

  install_docker = <<-EOT
    dnf install -y docker
    systemctl enable --now docker
  EOT
}

data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-*-x86_64"]
  }
}

# One repository, one image. Every service is the same binary bundle selected
# by its container command, so splitting this into six repositories would mean
# six near-identical pushes on every deploy.
resource "aws_ecr_repository" "app" {
  name                 = var.project_name
  image_tag_mutability = "MUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }

  tags = { Name = var.project_name }
}

module "networking" {
  source       = "./modules/networking"
  project_name = var.project_name
  aws_region   = var.aws_region
}

# ── Databases ────────────────────────────────────────────────────────────────

# Read-heavy catalogue. W=5 makes a write visible on every replica before it is
# acknowledged; R=1 then makes reads a single local lookup on the leader.
module "product_db" {
  source = "./modules/kv-cluster"

  name                  = "product-db"
  project_name          = var.project_name
  aws_region            = var.aws_region
  image                 = local.image
  subnet_id             = module.networking.data_subnet_id
  sg_id                 = module.networking.internal_sg_id
  key_name              = var.key_name
  instance_profile_name = var.instance_profile_name
  instance_type         = var.db_instance_type

  node_count = 5
  ip_offset  = 20
  mode       = "leader-follower"

  write_quorum = 5
  read_quorum  = 1
}

# Write-heavy cart storage. Any node accepts a write, and W=3 with R=3 over 5
# nodes keeps the quorums overlapping so a cart read never misses a committed
# cart write.
module "cart_db" {
  source = "./modules/kv-cluster"

  name                  = "cart-db"
  project_name          = var.project_name
  aws_region            = var.aws_region
  image                 = local.image
  subnet_id             = module.networking.data_subnet_id
  sg_id                 = module.networking.internal_sg_id
  key_name              = var.key_name
  instance_profile_name = var.instance_profile_name
  instance_type         = var.db_instance_type

  node_count = 5
  ip_offset  = 60
  mode       = "leaderless"

  write_quorum = 3
  read_quorum  = 3
}

# Accounts and orders. Three nodes with W=2/R=2 rather than five with W=3/R=3:
# this data is written far less often than carts, so a smaller quorum costs
# less per write while still tolerating one node down.
#
# Identity and orders share it. Strict service-per-database would mean two more
# clusters to operate for two services that between them hold a few thousand
# small records; the key namespaces are separate, so splitting them later is a
# configuration change rather than a migration.
module "core_db" {
  source = "./modules/kv-cluster"

  name                  = "core-db"
  project_name          = var.project_name
  aws_region            = var.aws_region
  image                 = local.image
  subnet_id             = module.networking.data_subnet_id
  sg_id                 = module.networking.internal_sg_id
  key_name              = var.key_name
  instance_profile_name = var.instance_profile_name
  instance_type         = var.db_instance_type

  node_count = 3
  ip_offset  = 100
  mode       = "leaderless"

  write_quorum = 2
  read_quorum  = 2
}

# ── Broker, payments and warehouse ───────────────────────────────────────────

# These four share one host. None of them scales with shopper count: the broker
# is a single logical queue, Redis is a single node, the authorizer is a stub,
# and the warehouse holds inventory in memory and so cannot be replicated
# without splitting the ledger. Co-locating them also puts the warehouse next
# to the broker it consumes from.
#
# Redis would be ElastiCache in anything real. It is a container here because
# the Learner Lab does not permit ElastiCache, and because a single node is
# honest about what this is: a cache, a session store and a rate-limit counter,
# all of which the services are written to survive losing.
resource "aws_instance" "services_1" {
  ami                         = data.aws_ami.al2023.id
  instance_type               = var.service_instance_type
  subnet_id                   = module.networking.app_subnet_ids[0]
  vpc_security_group_ids      = [module.networking.internal_sg_id]
  iam_instance_profile        = var.instance_profile_name
  key_name                    = var.key_name
  associate_public_ip_address = true
  user_data_replace_on_change = true

  tags = { Name = "${var.project_name}-services-1" }

  user_data = <<-EOF
    #!/bin/bash
    set -euxo pipefail
    ${local.install_docker}
    ${local.ecr_login}

    # A user-defined network gives the containers DNS for each other, so the
    # warehouse can reach the broker by name instead of by bridge IP.
    docker network create mesh || true

    docker run -d --name rabbitmq --restart always --network mesh \
      -p 5672:5672 -p 15672:15672 \
      rabbitmq:3-management

    # Append-only, because not everything in here is a cache. A restart losing
    # the catalogue cache is free; a restart logging out every customer is not.
    docker run -d --name redis --restart always --network mesh \
      -p 6379:6379 \
      redis:7-alpine \
      redis-server --appendonly yes --maxmemory 512mb --maxmemory-policy allkeys-lru

    # No sleep here on purpose: the warehouse retries its connection to the
    # broker on a backoff, so it can start before the broker is ready.
    docker run -d --name cca-service --restart always --network mesh \
      -p 8083:8083 \
      -e SERVER_PORT=8083 \
      ${local.image} /usr/local/bin/ccasvc

    docker run -d --name warehouse-service --restart always --network mesh \
      -p 8084:8084 \
      -e SERVER_PORT=8084 \
      -e RABBITMQ_HOST=rabbitmq \
      -e RABBITMQ_PORT=5672 \
      -e RABBITMQ_USER=guest \
      -e RABBITMQ_PASS=guest \
      -e WAREHOUSE_INITIAL_STOCK=100 \
      -e RESERVATION_TTL_MS=30000 \
      ${local.image} /usr/local/bin/warehousesvc
  EOF
}

# Identity, orders and notifications. Fixed rather than autoscaled: their load
# is a fraction of the browse path, and the two event-driven ones are bounded
# by queue throughput rather than by request rate. Identity is the one that
# would move first — registration spikes are real and bcrypt is expensive by
# design — so it is the obvious next candidate for a group of its own.
resource "aws_instance" "services_2" {
  ami                         = data.aws_ami.al2023.id
  instance_type               = var.service_instance_type
  subnet_id                   = module.networking.app_subnet_ids[1]
  vpc_security_group_ids      = [module.networking.internal_sg_id]
  iam_instance_profile        = var.instance_profile_name
  key_name                    = var.key_name
  associate_public_ip_address = true
  user_data_replace_on_change = true

  tags = { Name = "${var.project_name}-services-2" }

  depends_on = [aws_instance.services_1, module.core_db]

  user_data = <<-EOF
    #!/bin/bash
    set -euxo pipefail
    ${local.install_docker}
    ${local.ecr_login}

    docker run -d --name identity-service --restart always \
      -p 8085:8085 \
      -e PORT=8085 \
      -e CORE_DB_URL=${module.core_db.entrypoint} \
      -e REDIS_ADDR=${aws_instance.services_1.private_ip}:6379 \
      -e JWT_SECRET='${var.jwt_secret}' \
      -e JWT_ISSUER=${var.project_name} \
      ${local.image} /usr/local/bin/identitysvc

    docker run -d --name order-service --restart always \
      -p 8086:8086 \
      -e PORT=8086 \
      -e CORE_DB_URL=${module.core_db.entrypoint} \
      -e RMQ_HOST=${aws_instance.services_1.private_ip} \
      -e RMQ_PORT=5672 -e RMQ_USER=guest -e RMQ_PASS=guest \
      ${local.image} /usr/local/bin/ordersvc

    docker run -d --name notification-service --restart always \
      -p 8087:8087 \
      -e PORT=8087 \
      -e REDIS_ADDR=${aws_instance.services_1.private_ip}:6379 \
      -e RMQ_HOST=${aws_instance.services_1.private_ip} \
      -e RMQ_PORT=5672 -e RMQ_USER=guest -e RMQ_PASS=guest \
      ${local.image} /usr/local/bin/notificationsvc
  EOF
}

# ── Load balancer routing ────────────────────────────────────────────────────

resource "aws_lb_target_group" "product" {
  name     = "${var.project_name}-product-tg"
  port     = 8081
  protocol = "HTTP"
  vpc_id   = module.networking.vpc_id

  health_check {
    path                = "/product/health"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  # Give in-flight checkouts a chance to finish before a scale-in removes the
  # instance handling them.
  deregistration_delay = 30
}

resource "aws_lb_target_group" "cart" {
  name     = "${var.project_name}-cart-tg"
  port     = 8082
  protocol = "HTTP"
  vpc_id   = module.networking.vpc_id

  health_check {
    path                = "/shopping-cart/health"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  deregistration_delay = 30
}

# One target group per fixed service, attached by instance id.
locals {
  fixed_services = {
    identity      = { port = 8085, health = "/identity/health", instance = aws_instance.services_2.id }
    orders        = { port = 8086, health = "/orders/health", instance = aws_instance.services_2.id }
    notifications = { port = 8087, health = "/notifications/health", instance = aws_instance.services_2.id }
  }
}

resource "aws_lb_target_group" "fixed" {
  for_each = local.fixed_services

  name     = "${var.project_name}-${each.key}-tg"
  port     = each.value.port
  protocol = "HTTP"
  vpc_id   = module.networking.vpc_id

  health_check {
    path                = each.value.health
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }
}

resource "aws_lb_target_group_attachment" "fixed" {
  for_each = local.fixed_services

  target_group_arn = aws_lb_target_group.fixed[each.key].arn
  target_id        = each.value.instance
  port             = each.value.port
}

resource "aws_lb_target_group" "gateway" {
  name     = "${var.project_name}-gateway-tg"
  port     = 8080
  protocol = "HTTP"
  vpc_id   = module.networking.vpc_id

  health_check {
    path                = "/health"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  deregistration_delay = 30
}

# Everything public goes to the gateway. There are no path rules on :80 at all,
# which is the point: a service can only be reached from outside by the gateway
# choosing to forward to it.
resource "aws_lb_listener_rule" "public_to_gateway" {
  listener_arn = module.networking.alb_listener_arn
  priority     = 1

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.gateway.arn
  }

  condition {
    path_pattern { values = ["/*"] }
  }
}

# ── Internal routing (:8081, VPC only) ───────────────────────────────────────

resource "aws_lb_listener_rule" "internal_fixed" {
  for_each = {
    identity      = { priority = 300, paths = ["/identity", "/identity/*"] }
    orders        = { priority = 310, paths = ["/orders", "/orders/*"] }
    notifications = { priority = 320, paths = ["/notifications", "/notifications/*"] }
  }

  listener_arn = module.networking.alb_internal_listener_arn
  priority     = each.value.priority

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.fixed[each.key].arn
  }

  condition {
    path_pattern { values = each.value.paths }
  }
}

resource "aws_lb_listener_rule" "product" {
  listener_arn = module.networking.alb_internal_listener_arn
  priority     = 100

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.product.arn
  }

  condition {
    path_pattern { values = ["/product", "/product/*"] }
  }
}

resource "aws_lb_listener_rule" "cart" {
  listener_arn = module.networking.alb_internal_listener_arn
  priority     = 200

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.cart.arn
  }

  condition {
    path_pattern { values = ["/shopping-cart", "/shopping-cart/*"] }
  }
}

# ── Product service (scales on CPU) ──────────────────────────────────────────

resource "aws_launch_template" "product_service" {
  name_prefix   = "${var.project_name}-product-"
  image_id      = data.aws_ami.al2023.id
  instance_type = var.service_instance_type
  key_name      = var.key_name

  iam_instance_profile { name = var.instance_profile_name }
  vpc_security_group_ids = [module.networking.internal_sg_id]

  user_data = base64encode(<<-EOF
    #!/bin/bash
    set -euxo pipefail
    ${local.install_docker}
    ${local.ecr_login}

    docker run -d --name product-service --restart always \
      -p 8081:8081 \
      -e PORT=8081 \
      -e PRODUCT_DB_URL=${module.product_db.entrypoint} \
      -e REDIS_ADDR=${aws_instance.services_1.private_ip}:6379 \
      ${local.image} /usr/local/bin/productsvc
  EOF
  )

  tag_specifications {
    resource_type = "instance"
    tags          = { Name = "${var.project_name}-product-service" }
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_autoscaling_group" "product_service" {
  name                      = "${var.project_name}-product-asg"
  min_size                  = 1
  max_size                  = var.product_service_max
  desired_capacity          = 1
  vpc_zone_identifier       = module.networking.app_subnet_ids
  target_group_arns         = [aws_lb_target_group.product.arn]
  health_check_type         = "ELB"
  health_check_grace_period = 180

  launch_template {
    id      = aws_launch_template.product_service.id
    version = "$Latest"
  }

  # Without this, changing the launch template only affects instances launched
  # afterwards: a deploy would sit inert until something happened to trigger a
  # scale event, and the fleet would run two versions indefinitely. The refresh
  # replaces instances in batches, keeping most of the capacity serving.
  instance_refresh {
    strategy = "Rolling"
    preferences {
      min_healthy_percentage = 50
      instance_warmup        = 180
    }
  }

  tag {
    key                 = "Name"
    value               = "${var.project_name}-product-service"
    propagate_at_launch = true
  }

  depends_on = [module.product_db, aws_instance.services_1]
}

# The product service does nothing but read from its database and burn the
# simulated processing time, so its load shows up almost entirely as CPU.
resource "aws_autoscaling_policy" "product_cpu" {
  name                   = "${var.project_name}-product-cpu"
  autoscaling_group_name = aws_autoscaling_group.product_service.name
  policy_type            = "TargetTrackingScaling"

  target_tracking_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ASGAverageCPUUtilization"
    }
    target_value = var.product_cpu_target
  }
}

# ── Shopping cart service (scales on request count) ──────────────────────────

resource "aws_launch_template" "cart_service" {
  name_prefix   = "${var.project_name}-cart-"
  image_id      = data.aws_ami.al2023.id
  instance_type = var.service_instance_type
  key_name      = var.key_name

  iam_instance_profile { name = var.instance_profile_name }
  vpc_security_group_ids = [module.networking.internal_sg_id]

  user_data = base64encode(<<-EOF
    #!/bin/bash
    set -euxo pipefail
    ${local.install_docker}
    ${local.ecr_login}

    docker run -d --name shopping-cart-service --restart always \
      -p 8082:8082 \
      -e PORT=8082 \
      -e PRODUCT_SERVICE_URL=${module.networking.internal_base_url} \
      -e WAREHOUSE_SERVICE_URL=http://${aws_instance.services_1.private_ip}:8084 \
      -e CCA_SERVICE_URL=http://${aws_instance.services_1.private_ip}:8083 \
      -e CART_DB_URL=${module.cart_db.entrypoint} \
      -e RMQ_HOST=${aws_instance.services_1.private_ip} \
      -e RMQ_PORT=5672 \
      -e RMQ_USER=guest \
      -e RMQ_PASS=guest \
      ${local.image} /usr/local/bin/cartsvc
  EOF
  )

  tag_specifications {
    resource_type = "instance"
    tags          = { Name = "${var.project_name}-shopping-cart-service" }
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_autoscaling_group" "cart_service" {
  name                      = "${var.project_name}-cart-asg"
  min_size                  = 1
  max_size                  = var.cart_service_max
  desired_capacity          = 1
  vpc_zone_identifier       = module.networking.app_subnet_ids
  target_group_arns         = [aws_lb_target_group.cart.arn]
  health_check_type         = "ELB"
  health_check_grace_period = 180

  launch_template {
    id      = aws_launch_template.cart_service.id
    version = "$Latest"
  }

  # Without this, changing the launch template only affects instances launched
  # afterwards: a deploy would sit inert until something happened to trigger a
  # scale event, and the fleet would run two versions indefinitely. The refresh
  # replaces instances in batches, keeping most of the capacity serving.
  instance_refresh {
    strategy = "Rolling"
    preferences {
      min_healthy_percentage = 50
      instance_warmup        = 180
    }
  }

  tag {
    key                 = "Name"
    value               = "${var.project_name}-shopping-cart-service"
    propagate_at_launch = true
  }

  depends_on = [aws_instance.services_1, module.cart_db, module.product_db]
}

# The cart service scales on requests per instance rather than CPU. It spends
# most of a checkout waiting on the warehouse and the authorizer, so its CPU
# can look comfortable while its connection and goroutine budget is exhausted.
# Request count is the metric that actually tracks that pressure.
resource "aws_autoscaling_policy" "cart_requests" {
  name                   = "${var.project_name}-cart-requests"
  autoscaling_group_name = aws_autoscaling_group.cart_service.name
  policy_type            = "TargetTrackingScaling"

  target_tracking_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ALBRequestCountPerTarget"
      resource_label         = "${module.networking.alb_arn_suffix}/${aws_lb_target_group.cart.arn_suffix}"
    }
    target_value = var.cart_request_target
  }
}

# ── Alarms and dashboard ─────────────────────────────────────────────────────

module "monitoring" {
  source = "./modules/monitoring"

  project_name = var.project_name
  aws_region   = var.aws_region

  alb_arn_suffix        = module.networking.alb_arn_suffix
  product_tg_arn_suffix = aws_lb_target_group.product.arn_suffix
  cart_tg_arn_suffix    = aws_lb_target_group.cart.arn_suffix
  gateway_tg_arn_suffix = aws_lb_target_group.gateway.arn_suffix

  product_asg_name = aws_autoscaling_group.product_service.name
  cart_asg_name    = aws_autoscaling_group.cart_service.name
  gateway_asg_name = aws_autoscaling_group.gateway.name
  product_asg_max  = var.product_service_max
  cart_asg_max     = var.cart_service_max
  gateway_asg_max  = var.gateway_service_max

  alarm_email = var.alarm_email
}

# ── Gateway (the public entry, scales on request count) ──────────────────────

resource "aws_launch_template" "gateway" {
  name_prefix   = "${var.project_name}-gateway-"
  image_id      = data.aws_ami.al2023.id
  instance_type = var.service_instance_type
  key_name      = var.key_name

  iam_instance_profile { name = var.instance_profile_name }
  vpc_security_group_ids = [module.networking.internal_sg_id]

  user_data = base64encode(<<-EOF
    #!/bin/bash
    set -euxo pipefail
    ${local.install_docker}
    ${local.ecr_login}

    docker run -d --name gateway --restart always \
      -p 8080:8080 \
      -e PORT=8080 \
      -e IDENTITY_SERVICE_URL=${module.networking.internal_base_url} \
      -e PRODUCT_SERVICE_URL=${module.networking.internal_base_url} \
      -e CART_SERVICE_URL=${module.networking.internal_base_url} \
      -e ORDER_SERVICE_URL=${module.networking.internal_base_url} \
      -e NOTIFICATION_SERVICE_URL=${module.networking.internal_base_url} \
      -e REDIS_ADDR=${aws_instance.services_1.private_ip}:6379 \
      -e JWT_SECRET='${var.jwt_secret}' \
      -e JWT_ISSUER=${var.project_name} \
      -e RATE_LIMIT=${var.rate_limit} \
      ${local.image} /usr/local/bin/gatewaysvc
  EOF
  )

  tag_specifications {
    resource_type = "instance"
    tags          = { Name = "${var.project_name}-gateway" }
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_autoscaling_group" "gateway" {
  name                      = "${var.project_name}-gateway-asg"
  min_size                  = 1
  max_size                  = var.gateway_service_max
  desired_capacity          = 1
  vpc_zone_identifier       = module.networking.app_subnet_ids
  target_group_arns         = [aws_lb_target_group.gateway.arn]
  health_check_type         = "ELB"
  health_check_grace_period = 180

  launch_template {
    id      = aws_launch_template.gateway.id
    version = "$Latest"
  }

  instance_refresh {
    strategy = "Rolling"
    preferences {
      min_healthy_percentage = 50
      instance_warmup        = 180
    }
  }

  tag {
    key                 = "Name"
    value               = "${var.project_name}-gateway"
    propagate_at_launch = true
  }

  depends_on = [
    aws_instance.services_1,
    aws_instance.services_2,
    aws_autoscaling_group.product_service,
    aws_autoscaling_group.cart_service,
  ]
}

# The gateway does almost no work of its own — verify a signature, check a
# counter, proxy the body — so its cost is per request rather than per unit of
# computation. Request count is the metric that tracks that; CPU would stay
# comfortable while its connection budget ran out.
resource "aws_autoscaling_policy" "gateway_requests" {
  name                   = "${var.project_name}-gateway-requests"
  autoscaling_group_name = aws_autoscaling_group.gateway.name
  policy_type            = "TargetTrackingScaling"

  target_tracking_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ALBRequestCountPerTarget"
      resource_label         = "${module.networking.alb_arn_suffix}/${aws_lb_target_group.gateway.arn_suffix}"
    }
    target_value = var.gateway_request_target
  }
}

# ── Optional in-region load generator ────────────────────────────────────────

module "load_generator" {
  count  = var.deploy_load_generator ? 1 : 0
  source = "./modules/load-generator"

  project_name          = var.project_name
  subnet_id             = module.networking.app_subnet_ids[0]
  sg_id                 = module.networking.internal_sg_id
  key_name              = var.key_name
  instance_profile_name = var.instance_profile_name
  ami_id                = data.aws_ami.al2023.id
  target_url            = "http://${module.networking.alb_dns_name}"
}
