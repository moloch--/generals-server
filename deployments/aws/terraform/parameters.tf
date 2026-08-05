ephemeral "random_password" "admin_token" {
  length  = 64
  special = false
}

resource "aws_ssm_parameter" "admin_token" {
  name        = local.admin_token_parameter
  description = "Bearer token materialized into the private Generals admin container mount"
  type        = "SecureString"
  tier        = "Standard"

  value_wo         = ephemeral.random_password.admin_token.result
  value_wo_version = var.admin_token_version

  allowed_pattern = "^[A-Za-z0-9]{64}$"
}

resource "aws_ssm_parameter" "desired_image" {
  name        = local.desired_image_parameter
  description = "Immutable ECR image selected for the Generals server host"
  type        = "String"
  tier        = "Standard"
  value       = local.container_image

  allowed_pattern = "^[0-9]+\\.dkr\\.ecr\\.[a-z0-9-]+\\.amazonaws\\.com(?:\\.cn)?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$"
}

