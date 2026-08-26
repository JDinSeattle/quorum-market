# VPC, subnets, load balancer and the two security groups everything shares.
#
# There is no NAT gateway: the AWS Learner Lab does not permit one, so every
# subnet routes straight to the internet gateway. Instances are reachable from
# outside, which is what makes SSH debugging possible, and the internal
# security group is what actually restricts service-to-service traffic.

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = "${var.project_name}-vpc" }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${var.project_name}-igw" }
}

# Two application subnets in different AZs, because an ALB requires at least
# two and the autoscaled services should survive losing one.
resource "aws_subnet" "app" {
  count                   = 2
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(aws_vpc.main.cidr_block, 8, count.index + 1)
  availability_zone       = data.aws_availability_zones.available.names[count.index]
  map_public_ip_on_launch = true

  tags = { Name = "${var.project_name}-app-${count.index + 1}" }
}

# A separate subnet for the database clusters.
#
# KV nodes are given static private IPs so they know their peers before boot,
# and keeping them out of the application subnets means DHCP can never hand one
# of those addresses to an autoscaled instance first.
resource "aws_subnet" "data" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(aws_vpc.main.cidr_block, 8, 10)
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = { Name = "${var.project_name}-data" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = { Name = "${var.project_name}-public-rt" }
}

resource "aws_route_table_association" "app" {
  count          = length(aws_subnet.app)
  subnet_id      = aws_subnet.app[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "data" {
  subnet_id      = aws_subnet.data.id
  route_table_id = aws_route_table.public.id
}

# ── Security groups ──────────────────────────────────────────────────────────

resource "aws_security_group" "alb" {
  name        = "${var.project_name}-alb-sg"
  description = "Public HTTP ingress to the load balancer"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTP from anywhere"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # The internal listener, reachable only from inside the VPC.
  #
  # Service-to-service calls need a stable address for backends that live in
  # Auto Scaling Groups, and this gives them one without a second load
  # balancer. Restricting it to the VPC CIDR is what keeps the services behind
  # the gateway rather than beside it — the gateway is the only thing on :80,
  # so it is the only way in from outside.
  ingress {
    description = "Internal service routing, VPC only"
    from_port   = 8081
    to_port     = 8081
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.main.cidr_block]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "${var.project_name}-alb-sg" }
}

# One internal group covering service-to-service calls and database
# replication. Everything inside the VPC may talk to everything else: the
# services form a mesh and the KV nodes replicate peer to peer, so enumerating
# the pairs would be a long list that changes with every topology tweak.
resource "aws_security_group" "internal" {
  name        = "${var.project_name}-internal-sg"
  description = "Intra-VPC traffic between services and database nodes"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "Any TCP from inside the VPC"
    from_port   = 0
    to_port     = 65535
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.main.cidr_block]
  }

  ingress {
    description = "SSH for debugging"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "${var.project_name}-internal-sg" }
}

# ── Load balancer ────────────────────────────────────────────────────────────

resource "aws_lb" "main" {
  name               = "${var.project_name}-alb"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.app[*].id

  tags = { Name = "${var.project_name}-alb" }
}

# The public listener. Its default action is overridden in the root module to
# forward everything to the gateway, which is the only publicly reachable
# service; the fixed response here is what answers before that is wired.
resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "fixed-response"

    fixed_response {
      content_type = "application/json"
      message_body = "{\"error\":\"no route matches this path\"}"
      status_code  = "404"
    }
  }

  lifecycle {
    # The root module replaces the default action with a forward to the
    # gateway; without this, every apply would fight it back to the 404.
    ignore_changes = [default_action]
  }
}

# The internal listener. Same load balancer, different port, locked to the VPC
# by the security group above.
#
# Unmatched paths get an explicit 404 rather than a default backend, so a
# missing rule shows up as a routing error instead of quietly landing on
# whichever service happens to be first.
resource "aws_lb_listener" "internal" {
  load_balancer_arn = aws_lb.main.arn
  port              = 8081
  protocol          = "HTTP"

  default_action {
    type = "fixed-response"

    fixed_response {
      content_type = "application/json"
      message_body = "{\"error\":\"no internal route matches this path\"}"
      status_code  = "404"
    }
  }
}
