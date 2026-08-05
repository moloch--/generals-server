terraform {
  required_version = ">= 1.11.0, < 2.0.0"

  backend "s3" {
    encrypt      = true
    use_lockfile = true
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.58"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }
}

