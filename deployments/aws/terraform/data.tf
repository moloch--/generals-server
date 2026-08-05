data "aws_partition" "current" {}
data "aws_caller_identity" "current" {}

data "aws_ec2_instance_type" "selected" {
  instance_type = var.instance_type
}

data "aws_ecr_repository" "server" {
  name = var.ecr_repository_name
}

data "aws_ecr_image" "server" {
  repository_name = data.aws_ecr_repository.server.name
  image_digest    = var.container_image_digest
}

data "aws_route53_zone" "server" {
  zone_id      = var.route53_zone_id
  private_zone = false
}

data "aws_ssm_parameter" "amazon_linux_2023" {
  name = var.architecture == "arm64" ? "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64" : "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

locals {
  name_prefix      = "${var.project_name}-${var.environment}"
  hosted_zone_name = trimsuffix(data.aws_route53_zone.server.name, ".")
  # This AWS-owned public parameter is schema-marked sensitive even though the
  # AMI ID is public and should remain reviewable in plans.
  selected_ami_id           = coalesce(var.ami_id, nonsensitive(data.aws_ssm_parameter.amazon_linux_2023.value))
  container_image           = "${data.aws_ecr_repository.server.repository_url}@${data.aws_ecr_image.server.image_digest}"
  ecr_registry              = split("/", data.aws_ecr_repository.server.repository_url)[0]
  admin_token_parameter     = "/${var.project_name}/${var.environment}/admin-token"
  desired_image_parameter   = "/${var.project_name}/${var.environment}/desired-image"
  deployment_platform       = var.architecture == "arm64" ? "linux/arm64" : "linux/amd64"
  compose_architecture_name = var.architecture == "arm64" ? "aarch64" : "x86_64"
}

check "account_guardrail" {
  assert {
    condition     = data.aws_caller_identity.current.account_id == var.expected_aws_account_id
    error_message = "The authenticated AWS account does not match expected_aws_account_id."
  }
}

check "availability_zone_region" {
  assert {
    condition     = startswith(var.availability_zone, var.aws_region)
    error_message = "availability_zone must belong to aws_region."
  }
}

check "hostname_in_public_zone" {
  assert {
    condition     = var.hostname == local.hosted_zone_name || endswith(var.hostname, ".${local.hosted_zone_name}")
    error_message = "hostname must equal or be a child of the selected public Route 53 hosted zone."
  }
}

check "instance_architecture" {
  assert {
    condition     = contains(data.aws_ec2_instance_type.selected.supported_architectures, var.architecture)
    error_message = "instance_type does not support the selected architecture."
  }
}
