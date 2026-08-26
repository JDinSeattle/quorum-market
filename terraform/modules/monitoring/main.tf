# Alarms and a dashboard for the deployed stack.
#
# Every alarm here is about something a customer would notice: requests
# failing, requests being slow, or capacity having run out. Alarms on raw
# resource numbers get muted within a week, and the muting outlasts the reason
# for it.
#
# treat_missing_data is "notBreaching" throughout: outside a load test this
# system is idle, and an alarm that goes red every night because nobody was
# shopping teaches everyone to ignore it.

locals {
  notify = var.alarm_email != "" ? [aws_sns_topic.alarms[0].arn] : []
}

resource "aws_sns_topic" "alarms" {
  count = var.alarm_email != "" ? 1 : 0
  name  = "${var.project_name}-alarms"
}

resource "aws_sns_topic_subscription" "email" {
  count     = var.alarm_email != "" ? 1 : 0
  topic_arn = aws_sns_topic.alarms[0].arn
  protocol  = "email"
  endpoint  = var.alarm_email
}

# ── Availability ─────────────────────────────────────────────────────────────

# 5xx from the targets themselves: the application is failing, as distinct from
# the load balancer having nowhere to send traffic.
resource "aws_cloudwatch_metric_alarm" "target_5xx" {
  alarm_name          = "${var.project_name}-target-5xx"
  alarm_description   = "Services are returning server errors. Check the service logs and /readyz before assuming it is the load balancer."
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HTTPCode_Target_5XX_Count"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 3
  threshold           = 10
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { LoadBalancer = var.alb_arn_suffix }
  alarm_actions       = local.notify
  ok_actions          = local.notify
}

# 5xx from the load balancer itself means it had no healthy target to route to,
# which is a different problem with a different fix.
resource "aws_cloudwatch_metric_alarm" "elb_5xx" {
  alarm_name          = "${var.project_name}-elb-5xx"
  alarm_description   = "The load balancer could not reach a healthy target. Check target group health, not application logs."
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HTTPCode_ELB_5XX_Count"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 5
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { LoadBalancer = var.alb_arn_suffix }
  alarm_actions       = local.notify
}

resource "aws_cloudwatch_metric_alarm" "unhealthy_targets" {
  for_each = {
    gateway = var.gateway_tg_arn_suffix
    product = var.product_tg_arn_suffix
    cart    = var.cart_tg_arn_suffix
  }

  alarm_name          = "${var.project_name}-${each.key}-unhealthy-targets"
  alarm_description   = "Instances in the ${each.key} target group are failing their health check."
  namespace           = "AWS/ApplicationELB"
  metric_name         = "UnHealthyHostCount"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 3
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    LoadBalancer = var.alb_arn_suffix
    TargetGroup  = each.value
  }
  alarm_actions = local.notify
}

# ── Latency ──────────────────────────────────────────────────────────────────

resource "aws_cloudwatch_metric_alarm" "latency" {
  for_each = {
    product = var.product_tg_arn_suffix
    cart    = var.cart_tg_arn_suffix
  }

  alarm_name        = "${var.project_name}-${each.key}-latency"
  alarm_description = "The ${each.key} service's p99 response time is above ${var.latency_threshold_seconds}s."
  namespace         = "AWS/ApplicationELB"
  metric_name       = "TargetResponseTime"
  # p99 rather than Average: an average hides the tail, and the tail is what a
  # customer experiences as the site being broken.
  extended_statistic  = "p99"
  period              = 60
  evaluation_periods  = 5
  threshold           = var.latency_threshold_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  dimensions = {
    LoadBalancer = var.alb_arn_suffix
    TargetGroup  = each.value
  }
  alarm_actions = local.notify
}

# ── Capacity ─────────────────────────────────────────────────────────────────

# An Auto Scaling Group sitting at its ceiling is not itself an outage, but it
# is the moment scaling stopped being the answer. Latency starts climbing next.
resource "aws_cloudwatch_metric_alarm" "asg_at_capacity" {
  for_each = {
    gateway = { name = var.gateway_asg_name, max = var.gateway_asg_max }
    product = { name = var.product_asg_name, max = var.product_asg_max }
    cart    = { name = var.cart_asg_name, max = var.cart_asg_max }
  }

  alarm_name          = "${var.project_name}-${each.key}-at-max-capacity"
  alarm_description   = "The ${each.key} group has scaled to its maximum and cannot absorb more load. Raise the ceiling or find the bottleneck."
  namespace           = "AWS/AutoScaling"
  metric_name         = "GroupInServiceInstances"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 2
  threshold           = each.value.max
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  dimensions          = { AutoScalingGroupName = each.value.name }
  alarm_actions       = local.notify
}

# ── Dashboard ────────────────────────────────────────────────────────────────

resource "aws_cloudwatch_dashboard" "main" {
  dashboard_name = var.project_name

  dashboard_body = jsonencode({
    widgets = [
      {
        type = "metric", x = 0, y = 0, width = 12, height = 6,
        properties = {
          title  = "Requests and errors"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["AWS/ApplicationELB", "RequestCount", "LoadBalancer", var.alb_arn_suffix, { stat = "Sum", label = "requests" }],
            [".", "HTTPCode_Target_5XX_Count", ".", ".", { stat = "Sum", label = "target 5xx" }],
            [".", "HTTPCode_ELB_5XX_Count", ".", ".", { stat = "Sum", label = "elb 5xx" }],
            [".", "HTTPCode_Target_4XX_Count", ".", ".", { stat = "Sum", label = "4xx (declines, out of stock)" }],
          ]
          period = 60
        }
      },
      {
        type = "metric", x = 12, y = 0, width = 12, height = 6,
        properties = {
          title  = "Response time"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["AWS/ApplicationELB", "TargetResponseTime", "TargetGroup", var.gateway_tg_arn_suffix, "LoadBalancer", var.alb_arn_suffix, { stat = "p99", label = "gateway p99 (what a customer sees)" }],
            ["AWS/ApplicationELB", "TargetResponseTime", "TargetGroup", var.cart_tg_arn_suffix, "LoadBalancer", var.alb_arn_suffix, { stat = "p50", label = "cart p50" }],
            ["...", { stat = "p99", label = "cart p99" }],
            ["AWS/ApplicationELB", "TargetResponseTime", "TargetGroup", var.product_tg_arn_suffix, "LoadBalancer", var.alb_arn_suffix, { stat = "p99", label = "product p99" }],
          ]
          period = 60
          annotations = {
            horizontal = [{ label = "alarm", value = var.latency_threshold_seconds }]
          }
        }
      },
      {
        type = "metric", x = 0, y = 6, width = 12, height = 6,
        properties = {
          title  = "Instances in service"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["AWS/AutoScaling", "GroupInServiceInstances", "AutoScalingGroupName", var.gateway_asg_name, { stat = "Maximum", label = "gateway" }],
            [".", ".", ".", var.product_asg_name, { stat = "Maximum", label = "product" }],
            [".", ".", ".", var.cart_asg_name, { stat = "Maximum", label = "cart" }],
          ]
          period = 60
        }
      },
      {
        type = "metric", x = 12, y = 6, width = 12, height = 6,
        properties = {
          title  = "Scaling signals"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            # The two services scale on different metrics on purpose: the
            # product service is CPU-bound, the cart service is bound by
            # fan-out and spends its time waiting.
            ["AWS/EC2", "CPUUtilization", "AutoScalingGroupName", var.product_asg_name, { stat = "Average", label = "product CPU %" }],
            ["AWS/ApplicationELB", "RequestCountPerTarget", "TargetGroup", var.cart_tg_arn_suffix, { stat = "Sum", label = "cart requests/target" }],
          ]
          period = 60
        }
      },
      {
        type = "metric", x = 0, y = 12, width = 24, height = 6,
        properties = {
          title  = "Target group health"
          region = var.aws_region
          view   = "timeSeries"
          metrics = [
            ["AWS/ApplicationELB", "HealthyHostCount", "TargetGroup", var.product_tg_arn_suffix, "LoadBalancer", var.alb_arn_suffix, { stat = "Minimum", label = "product healthy" }],
            [".", "UnHealthyHostCount", ".", ".", ".", ".", { stat = "Maximum", label = "product unhealthy" }],
            [".", "HealthyHostCount", ".", var.cart_tg_arn_suffix, ".", ".", { stat = "Minimum", label = "cart healthy" }],
            [".", "UnHealthyHostCount", ".", ".", ".", ".", { stat = "Maximum", label = "cart unhealthy" }],
          ]
          period = 60
        }
      },
    ]
  })
}
