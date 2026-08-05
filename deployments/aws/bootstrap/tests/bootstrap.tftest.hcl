mock_provider "aws" {
  override_during = plan

  mock_data "aws_partition" {
    defaults = {
      partition = "aws"
    }
  }

  mock_data "aws_iam_policy_document" {
    defaults = {
      json = jsonencode({
        Version   = "2012-10-17"
        Statement = []
      })
    }
  }

  mock_resource "aws_s3_bucket" {
    defaults = {
      arn = "arn:aws:s3:::generals-server-terraform-state-test"
      id  = "generals-server-terraform-state-test"
    }
  }

  mock_resource "aws_ecr_repository" {
    defaults = {
      arn            = "arn:aws:ecr:us-west-2:123456789012:repository/generals-server"
      registry_id    = "123456789012"
      repository_url = "123456789012.dkr.ecr.us-west-2.amazonaws.com/generals-server"
    }
  }
}

variables {
  expected_aws_account_id = "123456789012"
  state_bucket_name       = "generals-server-terraform-state-test"
}

run "cost_effective_bootstrap" {
  command = plan

  assert {
    condition     = aws_s3_bucket.terraform_state.bucket == var.state_bucket_name
    error_message = "The bootstrap stack must create exactly the requested state bucket."
  }

  assert {
    condition     = aws_s3_bucket_versioning.terraform_state.versioning_configuration[0].status == "Enabled"
    error_message = "Terraform state must retain S3 object versions."
  }

  assert {
    condition = (
      aws_s3_bucket_public_access_block.terraform_state.block_public_acls &&
      aws_s3_bucket_public_access_block.terraform_state.block_public_policy &&
      aws_s3_bucket_public_access_block.terraform_state.ignore_public_acls &&
      aws_s3_bucket_public_access_block.terraform_state.restrict_public_buckets
    )
    error_message = "Every S3 public-access control must remain enabled for Terraform state."
  }

  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.terraform_state.rule).apply_server_side_encryption_by_default).sse_algorithm == "AES256"
    error_message = "Terraform state must use S3 server-side encryption."
  }

  assert {
    condition = (
      aws_ecr_repository.server.image_tag_mutability == "IMMUTABLE" &&
      !aws_ecr_repository.server.force_delete &&
      one(aws_ecr_repository.server.image_scanning_configuration).scan_on_push &&
      one(aws_ecr_repository.server.encryption_configuration).encryption_type == "AES256"
    )
    error_message = "ECR must keep immutable, scanned, encrypted images and refuse force deletion."
  }

  assert {
    condition = (
      contains(output.state_backend_init_arguments, "-backend-config=use_lockfile=true") &&
      contains(output.state_backend_init_arguments, "-backend-config=encrypt=true")
    )
    error_message = "The deployment backend arguments must enable native S3 locking and encryption."
  }

  assert {
    condition     = length(regexall("prevent_destroy\\s*=\\s*true", file("${path.module}/main.tf"))) >= 1
    error_message = "The bootstrap state bucket must retain its prevent_destroy safeguard."
  }
}

run "reject_invalid_state_bucket_name" {
  command = plan

  variables {
    state_bucket_name = "INVALID_BUCKET_NAME"
  }

  expect_failures = [var.state_bucket_name]
}

run "reject_invalid_expected_account" {
  command = plan

  variables {
    expected_aws_account_id = "not-an-account"
  }

  expect_failures = [var.expected_aws_account_id]
}
