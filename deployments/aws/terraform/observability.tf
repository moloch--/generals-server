resource "aws_cloudwatch_log_group" "server" {
  name              = "/${var.project_name}/${var.environment}/server"
  retention_in_days = var.cloudwatch_log_retention_days
}

resource "aws_cloudwatch_metric_alarm" "system_recovery" {
  alarm_name          = "${local.name_prefix}-system-recovery"
  alarm_description   = "Recover the single EC2 host after consecutive system status-check failures"
  namespace           = "AWS/EC2"
  metric_name         = "StatusCheckFailed_System"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  datapoints_to_alarm = 2
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "missing"

  dimensions = {
    InstanceId = aws_instance.server.id
  }

  alarm_actions = [
    "arn:${data.aws_partition.current.partition}:automate:${var.aws_region}:ec2:recover",
  ]
}
