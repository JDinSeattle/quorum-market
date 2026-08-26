variable "aws_region" {
  description = "Region for every resource in this stack."
  type        = string
  default     = "us-east-1"
}

variable "project_name" {
  description = "Prefix applied to every resource name and tag."
  type        = string
  default     = "quorum-market"
}

variable "ecr_registry" {
  description = "ECR registry host, e.g. 123456789012.dkr.ecr.us-east-1.amazonaws.com."
  type        = string
}

variable "image_tag" {
  description = "Tag of the application image to deploy."
  type        = string
  default     = "latest"
}

variable "key_name" {
  description = "EC2 key pair for SSH access. The AWS Learner Lab calls this 'vockey'."
  type        = string
  default     = "vockey"
}

variable "instance_profile_name" {
  description = "Existing IAM instance profile granting ECR pull access."
  type        = string
  default     = "LabInstanceProfile"
}

variable "db_instance_type" {
  description = "Instance type for KV database nodes."
  type        = string
  default     = "t3.micro"
}

variable "service_instance_type" {
  description = "Instance type for the autoscaled services."
  type        = string
  default     = "t3.micro"
}

variable "jwt_secret" {
  description = <<-EOT
    Signing secret for access tokens, shared by the gateway and the identity
    service.

    There is no default on purpose. HS256 with a guessable or checked-in secret
    is an authentication bypass: anyone holding it can mint a token for any
    customer. Generate one with `openssl rand -base64 48` and keep it out of
    version control.
  EOT
  type        = string
  sensitive   = true

  validation {
    condition     = length(var.jwt_secret) >= 32
    error_message = "jwt_secret must be at least 32 characters; a short HMAC key is brute-forceable offline from a single captured token."
  }
}

variable "gateway_service_max" {
  description = "Maximum gateway instances. The gateway is the only public entry, so it carries every request."
  type        = number
  default     = 4
}

variable "gateway_request_target" {
  description = "Target ALB requests per gateway instance."
  type        = number
  default     = 80
}

variable "product_service_max" {
  description = "Maximum product service instances."
  type        = number
  default     = 4
}

variable "cart_service_max" {
  description = "Maximum shopping cart instances."
  type        = number
  default     = 4
}

variable "product_cpu_target" {
  description = "Target average CPU utilisation for the product service, in percent."
  type        = number
  default     = 60
}

variable "cart_request_target" {
  description = "Target ALB requests per cart service instance."
  type        = number
  default     = 50
}

variable "alarm_email" {
  description = "Address to notify when an alarm fires. Empty disables notifications; the alarms still fire and remain visible in CloudWatch."
  type        = string
  default     = ""
}

variable "rate_limit" {
  description = "Requests per minute per identity at the gateway. Generous by default so a load test measures the system rather than the limiter."
  type        = number
  default     = 6000
}

variable "deploy_load_generator" {
  description = "Create an in-region EC2 host for running Locust against the ALB."
  type        = bool
  default     = false
}
