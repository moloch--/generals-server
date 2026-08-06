locals {
  compose_version = "5.4.0"
  admin_is_public = length(var.allowed_admin_ipv4_cidrs) > 0
  admin_bind_host = local.admin_is_public ? "0.0.0.0" : "127.0.0.1"
  admin_tls_cert  = local.admin_is_public ? "/tls/fullchain.pem" : ""
  admin_tls_key   = local.admin_is_public ? "/tls/privkey.pem" : ""
  compose_sha256 = {
    aarch64 = "fc5d1371f1ec7987e703da94ede49af3fbfb240b83f22991a98511de7bc4b93b"
    x86_64  = "837fd1d35bf6a494f41b5b5988269a7be79de337cf1a1a6ff0e45ab51bb4e9be"
  }

  host_config = templatefile("${path.module}/templates/host.conf.tftpl", {
    aws_region_b64              = base64encode(var.aws_region)
    data_volume_id_b64          = base64encode(aws_ebs_volume.data.id)
    admin_token_parameter_b64   = base64encode(aws_ssm_parameter.admin_token.name)
    desired_image_parameter_b64 = base64encode(aws_ssm_parameter.desired_image.name)
    ecr_repository_url_b64      = base64encode(data.aws_ecr_repository.server.repository_url)
    hostname_b64                = base64encode(var.hostname)
    public_hostname_b64         = base64encode(var.public_hostname)
    acme_email_b64              = base64encode(var.acme_email)
    admin_bind_host_b64         = base64encode(local.admin_bind_host)
    admin_tls_cert_b64          = base64encode(local.admin_tls_cert)
    admin_tls_key_b64           = base64encode(local.admin_tls_key)
    generals_platform_b64       = base64encode(local.deployment_platform)
    cloudwatch_log_group_b64    = base64encode(aws_cloudwatch_log_group.server.name)
  })

  docker_config = jsonencode({
    credHelpers = {
      (local.ecr_registry) = "ecr-login"
    }
  })

  user_data = templatefile("${path.module}/templates/user-data.sh.tftpl", {
    host_config_b64        = base64encode(local.host_config)
    compose_b64            = base64encode(file("${path.module}/../runtime/compose.yaml"))
    find_device_b64        = base64encode(file("${path.module}/../runtime/generals-find-data-device"))
    verify_mount_b64       = base64encode(file("${path.module}/../runtime/generals-verify-data-mount"))
    block_imds_b64         = base64encode(file("${path.module}/../runtime/generals-block-container-imds"))
    materialize_token_b64  = base64encode(file("${path.module}/../runtime/generals-materialize-token"))
    copy_certificate_b64   = base64encode(file("${path.module}/../runtime/generals-copy-certificate"))
    ensure_certificate_b64 = base64encode(file("${path.module}/../runtime/generals-ensure-certificate"))
    readiness_b64          = base64encode(file("${path.module}/../runtime/generals-readiness"))
    deploy_b64             = base64encode(file("${path.module}/../runtime/generals-deploy"))
    server_service_b64     = base64encode(file("${path.module}/../runtime/generals-server.service"))
    deploy_service_b64     = base64encode(file("${path.module}/../runtime/generals-deploy.service"))
    deploy_timer_b64       = base64encode(file("${path.module}/../runtime/generals-deploy.timer"))
    renew_service_b64      = base64encode(file("${path.module}/../runtime/generals-cert-renew.service"))
    renew_timer_b64        = base64encode(file("${path.module}/../runtime/generals-cert-renew.timer"))
    docker_dropin_b64      = base64encode(file("${path.module}/../runtime/docker-generals-data.conf"))
    docker_config_b64      = base64encode(local.docker_config)
    compose_download_url   = "https://github.com/docker/compose/releases/download/v${local.compose_version}/docker-compose-linux-${local.compose_architecture_name}"
    compose_sha256         = local.compose_sha256[local.compose_architecture_name]
  })

  compressed_user_data = base64gzip(local.user_data)
}

resource "terraform_data" "host_configuration" {
  input = {
    # Host configuration changes must not drift independently from DNS, IAM,
    # or Parameter Store. They trigger a visible replacement, while edits to
    # embedded bootstrap implementation still require an explicit -replace.
    host_config_sha256 = sha256(local.host_config)
    pinned_ami_id      = var.ami_id
  }
}

resource "aws_ebs_volume" "data" {
  availability_zone = var.availability_zone
  encrypted         = true
  size              = var.data_volume_size_gib
  type              = "gp3"

  tags = {
    Name = "${local.name_prefix}-data"
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_instance" "server" {
  ami                         = local.selected_ami_id
  instance_type               = var.instance_type
  subnet_id                   = aws_subnet.public.id
  vpc_security_group_ids      = [aws_security_group.server.id]
  iam_instance_profile        = aws_iam_instance_profile.server.name
  associate_public_ip_address = false
  monitoring                  = false
  source_dest_check           = true

  user_data_base64            = local.compressed_user_data
  user_data_replace_on_change = false

  metadata_options {
    http_endpoint               = "enabled"
    http_protocol_ipv6          = "disabled"
    http_put_response_hop_limit = 1
    http_tokens                 = "required"
    instance_metadata_tags      = "disabled"
  }

  root_block_device {
    delete_on_termination = true
    encrypted             = true
    volume_size           = var.root_volume_size_gib
    volume_type           = "gp3"
  }

  dynamic "credit_specification" {
    for_each = data.aws_ec2_instance_type.selected.burstable_performance_supported ? [1] : []
    content {
      cpu_credits = "standard"
    }
  }

  tags = {
    Name = local.name_prefix
  }

  lifecycle {
    # A new public AMI should be adopted only during a deliberate host replacement.
    # Likewise, changing embedded bootstrap files must not stop an active host
    # for an EC2 user-data update that cloud-init would not execute again.
    # The provider reports public-IP association after the separately managed EIP
    # is attached, even though launch-time and subnet auto-assignment stay disabled.
    ignore_changes = [ami, associate_public_ip_address, user_data_base64]
    replace_triggered_by = [
      terraform_data.host_configuration,
    ]

    precondition {
      condition     = contains(data.aws_ec2_instance_type.selected.supported_architectures, var.architecture)
      error_message = "instance_type does not support architecture."
    }
  }

  depends_on = [
    aws_cloudwatch_log_group.server,
    aws_iam_role_policy.server,
    aws_route_table_association.public,
    aws_ssm_parameter.admin_token,
    aws_ssm_parameter.desired_image,
  ]
}

resource "aws_volume_attachment" "data" {
  device_name                    = "/dev/sdf"
  instance_id                    = aws_instance.server.id
  volume_id                      = aws_ebs_volume.data.id
  force_detach                   = false
  skip_destroy                   = false
  stop_instance_before_detaching = true
}

check "user_data_size" {
  assert {
    # AWS decodes this Base64 payload before enforcing its 16 KiB raw user-data limit.
    condition     = length(local.compressed_user_data) <= 21848
    error_message = "Compressed EC2 user data exceeds the AWS 16 KiB decoded limit."
  }
}
