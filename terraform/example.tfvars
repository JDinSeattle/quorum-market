# Copy to terraform.tfvars and fill in before applying.
#
#   cd terraform
#   terraform init
#   terraform apply -var-file=terraform.tfvars

aws_region   = "us-east-1"
project_name = "quorum-market"

# The registry host only — the repository itself is created by Terraform.
ecr_registry = "123456789012.dkr.ecr.us-east-1.amazonaws.com"
image_tag    = "latest"

# AWS Learner Lab defaults.
key_name              = "vockey"
instance_profile_name = "LabInstanceProfile"

# Required, with no default. Generate one with: openssl rand -base64 48
# HS256 with a guessable secret is an authentication bypass, so this must never
# be a placeholder in anything that matters.
jwt_secret = "replace-me-with-openssl-rand-base64-48-output"

# Requests per minute per identity at the gateway.
rate_limit = 6000

# Scaling targets.
product_cpu_target     = 60
cart_request_target    = 50
product_service_max    = 4
cart_service_max       = 4
gateway_service_max    = 4
gateway_request_target = 80

# Notified when an alarm fires. Leave empty to skip the SNS topic; the alarms
# are created either way and are visible in the CloudWatch console.
alarm_email = ""

# Set to true to also create an in-region Locust host.
deploy_load_generator = false
