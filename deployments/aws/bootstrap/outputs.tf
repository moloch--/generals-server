output "aws_region" {
  description = "AWS Region containing the ECR repository."
  value       = var.aws_region
}

output "state_bucket_name" {
  description = "S3 bucket used by the deployment stack backend."
  value       = aws_s3_bucket.terraform_state.id
}

output "state_backend_init_arguments" {
  description = "Arguments to pass to terraform init in ../terraform."
  value = [
    "-backend-config=bucket=${aws_s3_bucket.terraform_state.id}",
    "-backend-config=key=generals-server/prod.tfstate",
    "-backend-config=region=${var.aws_region}",
    "-backend-config=use_lockfile=true",
    "-backend-config=encrypt=true",
  ]
}

output "ecr_repository_name" {
  description = "ECR repository name consumed by the deployment stack."
  value       = aws_ecr_repository.server.name
}

output "ecr_repository_url" {
  description = "Registry/repository URL to tag when publishing an image."
  value       = aws_ecr_repository.server.repository_url
}

output "ecr_repository_arn" {
  description = "ECR repository ARN."
  value       = aws_ecr_repository.server.arn
}
