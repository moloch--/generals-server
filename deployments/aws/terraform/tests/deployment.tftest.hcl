mock_provider "aws" {
  override_during = plan

  mock_data "aws_partition" {
    defaults = {
      partition = "aws"
    }
  }

  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:iam::123456789012:user/terraform-test"
      user_id    = "AIDATERRAFORMTEST"
    }
  }

  mock_data "aws_ec2_instance_type" {
    defaults = {
      burstable_performance_supported = true
      supported_architectures         = ["arm64"]
    }
  }

  mock_data "aws_ecr_repository" {
    defaults = {
      arn            = "arn:aws:ecr:us-west-2:123456789012:repository/generals-server"
      name           = "generals-server"
      registry_id    = "123456789012"
      repository_url = "123456789012.dkr.ecr.us-west-2.amazonaws.com/generals-server"
    }
  }

  mock_data "aws_ecr_image" {
    defaults = {
      image_digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    }
  }

  mock_data "aws_route53_zone" {
    defaults = {
      name         = "example.com."
      private_zone = false
      zone_id      = "Z1234567890TEST"
    }
  }

  mock_data "aws_ssm_parameter" {
    defaults = {
      arn     = "arn:aws:ssm:us-west-2::parameter/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
      name    = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
      type    = "String"
      value   = "ami-0123456789abcdef0"
      version = 1
    }
  }

  mock_data "aws_iam_policy_document" {
    defaults = {
      id = "terraform-test-policy"
      json = jsonencode({
        Version   = "2012-10-17"
        Statement = []
      })
      minified_json = jsonencode({
        Version   = "2012-10-17"
        Statement = []
      })
    }
  }

  mock_resource "aws_cloudwatch_log_group" {
    defaults = {
      arn = "arn:aws:logs:us-west-2:123456789012:log-group:/generals-server/prod/server"
    }
  }

  mock_resource "aws_ebs_volume" {
    defaults = {
      arn = "arn:aws:ec2:us-west-2:123456789012:volume/vol-0123456789abcdef0"
      id  = "vol-0123456789abcdef0"
    }
  }

  mock_resource "aws_eip" {
    defaults = {
      allocation_id = "eipalloc-0123456789abcdef0"
      public_ip     = "192.0.2.10"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn  = "arn:aws:iam::123456789012:role/generals-server-prod"
      id   = "generals-server-prod"
      name = "generals-server-prod"
    }
  }

  mock_resource "aws_instance" {
    defaults = {
      arn = "arn:aws:ec2:us-west-2:123456789012:instance/i-0123456789abcdef0"
      id  = "i-0123456789abcdef0"
    }
  }

  mock_resource "aws_internet_gateway" {
    defaults = {
      arn = "arn:aws:ec2:us-west-2:123456789012:internet-gateway/igw-0123456789abcdef0"
      id  = "igw-0123456789abcdef0"
    }
  }

  mock_resource "aws_route53_record" {
    defaults = {
      fqdn = "online.example.com"
    }
  }

  mock_resource "aws_security_group" {
    defaults = {
      arn = "arn:aws:ec2:us-west-2:123456789012:security-group/sg-0123456789abcdef0"
      id  = "sg-0123456789abcdef0"
    }
  }

  mock_resource "aws_ssm_parameter" {
    defaults = {
      arn = "arn:aws:ssm:us-west-2:123456789012:parameter/generals-server/prod/value"
    }
  }

  mock_resource "aws_subnet" {
    defaults = {
      arn = "arn:aws:ec2:us-west-2:123456789012:subnet/subnet-0123456789abcdef0"
      id  = "subnet-0123456789abcdef0"
    }
  }

  mock_resource "aws_vpc" {
    defaults = {
      arn = "arn:aws:ec2:us-west-2:123456789012:vpc/vpc-0123456789abcdef0"
      id  = "vpc-0123456789abcdef0"
    }
  }
}

variables {
  acme_email              = "operator@example.com"
  container_image_digest  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  expected_aws_account_id = "123456789012"
  hostname                = "online.example.com"
  route53_zone_id         = "Z1234567890TEST"
}

run "cost_effective_single_host_topology" {
  command = plan

  assert {
    condition = (
      aws_instance.server.instance_type == "t4g.small" &&
      !aws_instance.server.associate_public_ip_address &&
      !aws_instance.server.monitoring
    )
    error_message = "The deployment must remain one cost-conscious EC2 host without detailed monitoring or a second public IP."
  }

  assert {
    condition = (
      aws_instance.server.metadata_options[0].http_endpoint == "enabled" &&
      aws_instance.server.metadata_options[0].http_tokens == "required" &&
      aws_instance.server.metadata_options[0].http_put_response_hop_limit == 1 &&
      aws_instance.server.metadata_options[0].instance_metadata_tags == "disabled"
    )
    error_message = "The host must require IMDSv2 and keep role credentials out of bridged application containers."
  }

  assert {
    condition = (
      aws_instance.server.root_block_device[0].encrypted &&
      aws_instance.server.root_block_device[0].volume_type == "gp3" &&
      aws_instance.server.root_block_device[0].delete_on_termination &&
      aws_instance.server.credit_specification[0].cpu_credits == "standard"
    )
    error_message = "The root disk must be encrypted gp3 and T4g must use bounded standard CPU credits."
  }

  assert {
    condition = (
      aws_ebs_volume.data.availability_zone == var.availability_zone &&
      aws_ebs_volume.data.encrypted &&
      aws_ebs_volume.data.type == "gp3" &&
      aws_volume_attachment.data.volume_id == aws_ebs_volume.data.id &&
      !aws_volume_attachment.data.force_detach &&
      !aws_volume_attachment.data.skip_destroy &&
      aws_volume_attachment.data.stop_instance_before_detaching
    )
    error_message = "The SQLite volume must remain encrypted, AZ-local, retained, and safely detached."
  }

  assert {
    condition = (
      !aws_subnet.public.map_public_ip_on_launch &&
      aws_route.internet.destination_cidr_block == "0.0.0.0/0" &&
      aws_route.internet.gateway_id == aws_internet_gateway.server.id &&
      aws_eip.server.domain == "vpc" &&
      aws_eip.server.instance == aws_instance.server.id
    )
    error_message = "The single public subnet must use one stable EIP through its Internet Gateway."
  }

  assert {
    condition = (
      length(aws_vpc_security_group_ingress_rule.control) == 1 &&
      one(values(aws_vpc_security_group_ingress_rule.control)).from_port == 29900 &&
      one(values(aws_vpc_security_group_ingress_rule.control)).to_port == 29900 &&
      one(values(aws_vpc_security_group_ingress_rule.control)).ip_protocol == "tcp" &&
      length(aws_vpc_security_group_ingress_rule.relay) == 1 &&
      one(values(aws_vpc_security_group_ingress_rule.relay)).from_port == 27901 &&
      one(values(aws_vpc_security_group_ingress_rule.relay)).to_port == 27901 &&
      one(values(aws_vpc_security_group_ingress_rule.relay)).ip_protocol == "udp" &&
      length(aws_vpc_security_group_ingress_rule.public_web) == 1 &&
      one(values(aws_vpc_security_group_ingress_rule.public_web)).from_port == 8082 &&
      one(values(aws_vpc_security_group_ingress_rule.public_web)).to_port == 8082 &&
      one(values(aws_vpc_security_group_ingress_rule.public_web)).ip_protocol == "tcp" &&
      length(regexall("from_port\\s*=\\s*808[01]", file("${path.module}/network.tf"))) == 0 &&
      length(regexall("to_port\\s*=\\s*808[01]", file("${path.module}/network.tf"))) == 0
    )
    error_message = "Only control, relay, and public web may receive ingress; health and admin must remain private."
  }

  assert {
    condition = (
      output.public_web_url == "http://online.example.com:8082/" &&
      length(regexall("127\\.0\\.0\\.1:8080:8080/tcp", file("${path.module}/../runtime/compose.yaml"))) == 1 &&
      length(regexall("127\\.0\\.0\\.1:8081:8081/tcp", file("${path.module}/../runtime/compose.yaml"))) == 1 &&
      length(regexall("8082:8082/tcp", file("${path.module}/../runtime/compose.yaml"))) == 1
    )
    error_message = "Runtime Compose must publish only public web broadly while keeping health and admin on loopback."
  }

  assert {
    condition = (
      aws_route53_record.server.type == "A" &&
      aws_route53_record.server.ttl == 300 &&
      length(aws_route53_record.server.records) == 1 &&
      contains(aws_route53_record.server.records, aws_eip.server.public_ip)
    )
    error_message = "Route 53 must point the player hostname directly at the stable EIP."
  }

  assert {
    condition = (
      aws_ssm_parameter.admin_token.type == "SecureString" &&
      aws_ssm_parameter.admin_token.tier == "Standard" &&
      aws_ssm_parameter.admin_token.value_wo_version == 1 &&
      aws_ssm_parameter.desired_image.type == "String" &&
      endswith(aws_ssm_parameter.desired_image.value, "@${var.container_image_digest}")
    )
    error_message = "The admin token must be state-free SecureString data and the desired image must be digest-pinned."
  }

  assert {
    condition     = aws_cloudwatch_log_group.server.retention_in_days == 7
    error_message = "Application logs must retain the short cost-conscious default."
  }

  assert {
    condition = (
      aws_cloudwatch_metric_alarm.system_recovery.metric_name == "StatusCheckFailed_System" &&
      aws_cloudwatch_metric_alarm.system_recovery.period == 60 &&
      aws_cloudwatch_metric_alarm.system_recovery.datapoints_to_alarm == 2 &&
      contains(aws_cloudwatch_metric_alarm.system_recovery.alarm_actions, "arn:aws:automate:us-west-2:ec2:recover")
    )
    error_message = "The single host must request low-cost EC2 recovery after consecutive system failures."
  }

  assert {
    condition = alltrue([
      for forbidden in ["aws_lb", "aws_nat_gateway", "aws_backup_", "aws_ebs_snapshot"] :
      length(regexall(forbidden, join("\n", [for filename in fileset(path.module, "*.tf") : file("${path.module}/${filename}")]))) == 0
    ])
    error_message = "The lean stack must not add a load balancer, NAT Gateway, or backup resources."
  }

  assert {
    condition = (
      length(regexall("resource\\s+\"aws_ebs_volume\"\\s+\"data\"[\\s\\S]*?prevent_destroy\\s*=\\s*true", file("${path.module}/compute.tf"))) == 1 &&
      length(regexall("replace_triggered_by\\s*=", file("${path.module}/compute.tf"))) == 1 &&
      length(regexall("value_wo\\s*=\\s*ephemeral\\.random_password\\.admin_token\\.result", file("${path.module}/parameters.tf"))) == 1 &&
      length(regexall("use_lockfile\\s*=\\s*true", file("${path.module}/versions.tf"))) == 1
    )
    error_message = "Retained-volume, host-replacement, state-free-secret, and S3-lockfile safeguards must remain explicit."
  }
}

run "reject_mutable_image_reference" {
  command = plan

  variables {
    container_image_digest = "latest"
  }

  expect_failures = [var.container_image_digest]
}

run "reject_hostname_outside_hosted_zone" {
  command = plan

  variables {
    hostname = "online.example.net"
  }

  expect_failures = [check.hostname_in_public_zone]
}

run "reject_cross_region_availability_zone" {
  command = plan

  variables {
    availability_zone = "us-east-1a"
  }

  expect_failures = [check.availability_zone_region]
}

run "reject_wrong_aws_account" {
  command = plan

  variables {
    expected_aws_account_id = "999999999999"
  }

  expect_failures = [check.account_guardrail]
}
