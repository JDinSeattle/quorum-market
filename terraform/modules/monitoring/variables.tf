variable "project_name" { type = string }
variable "aws_region" { type = string }

variable "alb_arn_suffix" { type = string }
variable "product_tg_arn_suffix" { type = string }
variable "cart_tg_arn_suffix" { type = string }
variable "gateway_tg_arn_suffix" { type = string }

variable "product_asg_name" { type = string }
variable "cart_asg_name" { type = string }
variable "gateway_asg_name" { type = string }

variable "product_asg_max" { type = number }
variable "cart_asg_max" { type = number }
variable "gateway_asg_max" { type = number }

variable "alarm_email" {
  description = "Address to notify on alarm. Empty disables notifications; the alarms still fire and are still visible in the console."
  type        = string
  default     = ""
}

variable "latency_threshold_seconds" {
  description = <<-EOT
    Target response time that counts as too slow, at p99.

    Set against what this system actually does rather than a generic number:
    handlers burn a log-normally distributed delay with a median near 245ms and
    a tail past a second, and a checkout fans out to three services in
    sequence. Two seconds is comfortably above normal and well below the
    client timeout.
  EOT
  type        = number
  default     = 2
}
