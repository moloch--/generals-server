provider "aws" {
  region              = var.aws_region
  allowed_account_ids = [var.expected_aws_account_id]

  default_tags {
    tags = merge(var.additional_tags, {
      Environment = var.environment
      ManagedBy   = "Terraform"
      Project     = var.project_name
    })
  }
}
