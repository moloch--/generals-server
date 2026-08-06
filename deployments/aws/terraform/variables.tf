variable "aws_region" {
  description = "AWS Region in which to deploy the server."
  type        = string
  default     = "us-west-2"

  validation {
    condition     = can(regex("^[a-z]{2}(-[a-z]+)+-[0-9]+$", var.aws_region))
    error_message = "aws_region must be a valid AWS Region name."
  }
}

variable "expected_aws_account_id" {
  description = "Required 12-digit account guardrail. The provider refuses any other account."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.expected_aws_account_id))
    error_message = "expected_aws_account_id must be a 12-digit AWS account ID."
  }
}

variable "availability_zone" {
  description = "Single Availability Zone shared by the subnet, EC2 instance, and retained EBS volume."
  type        = string
  default     = "us-west-2a"

  validation {
    condition     = can(regex("^[a-z]{2}(-[a-z]+)+-[0-9]+[a-z]$", var.availability_zone))
    error_message = "availability_zone must be a valid Availability Zone name such as us-west-2a."
  }
}

variable "project_name" {
  description = "Lowercase name used for resource names and tags."
  type        = string
  default     = "generals-server"

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$", var.project_name))
    error_message = "project_name must be 3-32 lowercase letters, digits, or hyphens."
  }
}

variable "environment" {
  description = "Short lowercase environment name."
  type        = string
  default     = "prod"

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{0,14}[a-z0-9]$|^[a-z0-9]$", var.environment))
    error_message = "environment must be 1-16 lowercase letters, digits, or hyphens."
  }
}

variable "hostname" {
  description = "Public lowercase gameplay FQDN used by clients and the ACME certificate."
  type        = string

  validation {
    condition = (
      length(var.hostname) <= 253 &&
      can(regex("^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$", var.hostname))
    )
    error_message = "hostname must be a lowercase RFC 1123-style fully qualified domain name."
  }
}

variable "public_hostname" {
  description = "Public lowercase FQDN for the HTTPS website and canonical HTTP redirect target."
  type        = string

  validation {
    condition = (
      length(var.public_hostname) <= 253 &&
      can(regex("^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$", var.public_hostname))
    )
    error_message = "public_hostname must be a lowercase RFC 1123-style fully qualified domain name."
  }
}

variable "route53_zone_id" {
  description = "ID of the existing public Route 53 hosted zone containing hostname and public_hostname."
  type        = string

  validation {
    condition     = can(regex("^[A-Z0-9]+$", var.route53_zone_id))
    error_message = "route53_zone_id must be a Route 53 hosted zone ID without a /hostedzone/ prefix."
  }
}

variable "acme_email" {
  description = "Email address registered with Let's Encrypt for expiration and account notices."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$", var.acme_email))
    error_message = "acme_email must be a valid email address."
  }
}

variable "allowed_gameplay_ipv4_cidrs" {
  description = "IPv4 CIDRs allowed to reach public TCP 29900 and UDP 27901."
  type        = set(string)
  default     = ["0.0.0.0/0"]

  validation {
    condition = alltrue([
      for cidr in var.allowed_gameplay_ipv4_cidrs : can(cidrnetmask(cidr)) && can(regex("\\.", cidr))
    ])
    error_message = "allowed_gameplay_ipv4_cidrs must contain valid IPv4 CIDR blocks."
  }
}

variable "allowed_public_web_ipv4_cidrs" {
  description = "IPv4 CIDRs allowed to reach the public HTTPS interface on TCP 443 and HTTP redirect on TCP 80."
  type        = set(string)
  default     = ["0.0.0.0/0"]

  validation {
    condition = alltrue([
      for cidr in var.allowed_public_web_ipv4_cidrs : can(cidrnetmask(cidr)) && can(regex("\\.", cidr))
    ])
    error_message = "allowed_public_web_ipv4_cidrs must contain valid IPv4 CIDR blocks."
  }
}

variable "allowed_admin_ipv4_cidrs" {
  description = "Exact public IPv4 hosts allowed to reach the HTTPS admin interface on TCP 8081. An empty set keeps admin bound to host loopback."
  type        = set(string)
  default     = []

  validation {
    condition = alltrue([
      for cidr in var.allowed_admin_ipv4_cidrs : can(cidrnetmask(cidr)) && can(regex("\\.", cidr)) && endswith(cidr, "/32")
    ])
    error_message = "allowed_admin_ipv4_cidrs must contain only valid IPv4 /32 host CIDRs."
  }
}

variable "vpc_cidr" {
  description = "CIDR for the dedicated single-subnet VPC."
  type        = string
  default     = "10.42.0.0/16"

  validation {
    condition     = can(cidrnetmask(var.vpc_cidr)) && can(regex("\\.", var.vpc_cidr))
    error_message = "vpc_cidr must be a valid IPv4 CIDR block."
  }
}

variable "public_subnet_cidr" {
  description = "CIDR for the one public subnet."
  type        = string
  default     = "10.42.1.0/24"

  validation {
    condition     = can(cidrnetmask(var.public_subnet_cidr)) && can(regex("\\.", var.public_subnet_cidr))
    error_message = "public_subnet_cidr must be a valid IPv4 CIDR block."
  }
}

variable "instance_type" {
  description = "EC2 instance type. t4g.small is the cost-conscious default with enough memory for host agents."
  type        = string
  default     = "t4g.small"

  validation {
    condition     = can(regex("^[a-z0-9]+\\.[a-z0-9]+$", var.instance_type))
    error_message = "instance_type must be a valid EC2 instance type name."
  }
}

variable "architecture" {
  description = "CPU architecture used by both the EC2 AMI and container image."
  type        = string
  default     = "arm64"

  validation {
    condition     = contains(["arm64", "x86_64"], var.architecture)
    error_message = "architecture must be arm64 or x86_64."
  }
}

variable "ami_id" {
  description = "Optional pinned Amazon Linux 2023 AMI ID. Null selects the current public AL2023 parameter only at initial creation."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.ami_id == null || can(regex("^ami-[0-9a-f]{8,17}$", var.ami_id))
    error_message = "ami_id must be null or a valid AMI ID."
  }
}

variable "root_volume_size_gib" {
  description = "Encrypted gp3 root-volume size."
  type        = number
  default     = 16

  validation {
    condition     = var.root_volume_size_gib == floor(var.root_volume_size_gib) && var.root_volume_size_gib >= 10 && var.root_volume_size_gib <= 100
    error_message = "root_volume_size_gib must be an integer from 10 through 100."
  }
}

variable "data_volume_size_gib" {
  description = "Encrypted retained gp3 volume size for SQLite, WAL sidecars, and ACME state."
  type        = number
  default     = 8

  validation {
    condition     = var.data_volume_size_gib == floor(var.data_volume_size_gib) && var.data_volume_size_gib >= 1 && var.data_volume_size_gib <= 100
    error_message = "data_volume_size_gib must be an integer from 1 through 100."
  }
}

variable "ecr_repository_name" {
  description = "Existing ECR repository created by ../bootstrap."
  type        = string
  default     = "generals-server"

  validation {
    condition     = can(regex("^[a-z0-9]+(?:[._/-][a-z0-9]+)*$", var.ecr_repository_name))
    error_message = "ecr_repository_name must use valid lowercase ECR repository characters."
  }
}

variable "container_image_digest" {
  description = "Immutable linux/architecture image digest already published to ECR."
  type        = string

  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.container_image_digest))
    error_message = "container_image_digest must have the form sha256:<64 lowercase hexadecimal characters>."
  }
}

variable "admin_token_version" {
  description = "Increment to rotate the generated state-free admin token."
  type        = number
  default     = 1

  validation {
    condition     = var.admin_token_version == floor(var.admin_token_version) && var.admin_token_version >= 1
    error_message = "admin_token_version must be a positive integer."
  }
}

variable "cloudwatch_log_retention_days" {
  description = "Short CloudWatch application-log retention period."
  type        = number
  default     = 7

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90], var.cloudwatch_log_retention_days)
    error_message = "cloudwatch_log_retention_days must be one of 1, 3, 5, 7, 14, 30, 60, or 90."
  }
}

variable "additional_tags" {
  description = "Additional tags applied by the AWS provider."
  type        = map(string)
  default     = {}
}
