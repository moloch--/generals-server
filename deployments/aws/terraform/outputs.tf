output "aws_region" {
  description = "AWS Region containing the deployment."
  value       = var.aws_region
}

output "public_ip" {
  description = "Stable Elastic IP used by the Route 53 A record."
  value       = aws_eip.server.public_ip
}

output "gameplay_hostname" {
  description = "Player-facing TLS hostname."
  value       = aws_route53_record.server.fqdn
}

output "public_hostname" {
  description = "Browser-facing HTTPS hostname."
  value       = aws_route53_record.public_web.fqdn
}

output "online_endpoint" {
  description = "Endpoint to pass to GeneralsX clients."
  value       = "tls://${var.hostname}:29900"
}

output "public_web_url" {
  description = "Public status, leaderboard, players, lobbies, and active-games web interface."
  value       = "https://${var.public_hostname}/"
}

output "instance_id" {
  description = "EC2 instance ID used for SSM operations."
  value       = aws_instance.server.id
}

output "data_volume_id" {
  description = "Retained EBS volume. Terraform prevents its destruction."
  value       = aws_ebs_volume.data.id
}

output "deployed_image" {
  description = "Immutable image selected through Parameter Store."
  value       = local.container_image
}

output "admin_token_parameter_name" {
  description = "SecureString name. Retrieve its value only when opening an admin session."
  value       = aws_ssm_parameter.admin_token.name
}

output "admin_tunnel_command" {
  description = "Starts an IAM-authenticated local tunnel to the private admin dashboard."
  value       = "aws ssm start-session --region ${var.aws_region} --target ${aws_instance.server.id} --document-name AWS-StartPortForwardingSession --parameters '{\"portNumber\":[\"8081\"],\"localPortNumber\":[\"8081\"]}'"
}

output "health_tunnel_command" {
  description = "Starts a local tunnel to the private health and metrics listener."
  value       = "aws ssm start-session --region ${var.aws_region} --target ${aws_instance.server.id} --document-name AWS-StartPortForwardingSession --parameters '{\"portNumber\":[\"8080\"],\"localPortNumber\":[\"8080\"]}'"
}

output "deployment_status_command" {
  description = "Reads the latest host deployment service status without opening SSH."
  value       = "aws ssm send-command --region ${var.aws_region} --instance-ids ${aws_instance.server.id} --document-name AWS-RunShellScript --parameters 'commands=[\"systemctl status --no-pager generals-server.service generals-deploy.service\"]'"
}
