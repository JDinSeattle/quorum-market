# An EC2 host for running Locust inside the same region as the stack.
#
# Driving load from a laptop measures the home connection as much as the
# system: a residential uplink saturates long before the ALB does. Generating
# load in-region keeps the bottleneck where it belongs.

variable "project_name" { type = string }
variable "subnet_id" { type = string }
variable "sg_id" { type = string }
variable "key_name" { type = string }
variable "instance_profile_name" { type = string }
variable "ami_id" { type = string }
variable "target_url" { type = string }

variable "instance_type" {
  description = "Load generation is CPU-bound in the client, so this is deliberately larger than the service instances."
  type        = string
  default     = "t3.large"
}

resource "aws_instance" "locust" {
  ami                         = var.ami_id
  instance_type               = var.instance_type
  subnet_id                   = var.subnet_id
  vpc_security_group_ids      = [var.sg_id]
  iam_instance_profile        = var.instance_profile_name
  key_name                    = var.key_name
  associate_public_ip_address = true
  user_data_replace_on_change = true

  tags = { Name = "${var.project_name}-load-generator" }

  user_data = <<-EOF
    #!/bin/bash
    set -euxo pipefail
    dnf install -y python3-pip git
    pip3 install locust

    echo 'export TARGET_URL=${var.target_url}' >> /etc/profile.d/locust.sh
  EOF
}

output "public_ip" { value = aws_instance.locust.public_ip }
output "target_url" { value = var.target_url }
