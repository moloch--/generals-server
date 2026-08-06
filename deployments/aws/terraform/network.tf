resource "aws_vpc" "server" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = {
    Name = local.name_prefix
  }
}

resource "aws_internet_gateway" "server" {
  vpc_id = aws_vpc.server.id

  tags = {
    Name = local.name_prefix
  }
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.server.id
  availability_zone       = var.availability_zone
  cidr_block              = var.public_subnet_cidr
  map_public_ip_on_launch = false

  tags = {
    Name = "${local.name_prefix}-public"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.server.id

  tags = {
    Name = "${local.name_prefix}-public"
  }
}

resource "aws_route" "internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.server.id
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_security_group" "server" {
  name        = local.name_prefix
  description = "Public Generals control, relay, and read-only web traffic only"
  vpc_id      = aws_vpc.server.id

  tags = {
    Name = local.name_prefix
  }
}

resource "aws_vpc_security_group_ingress_rule" "control" {
  for_each = var.allowed_gameplay_ipv4_cidrs

  security_group_id = aws_security_group.server.id
  description       = "Generals TLS control"
  cidr_ipv4         = each.value
  from_port         = 29900
  to_port           = 29900
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_ingress_rule" "relay" {
  for_each = var.allowed_gameplay_ipv4_cidrs

  security_group_id = aws_security_group.server.id
  description       = "Generals authenticated UDP relay"
  cidr_ipv4         = each.value
  from_port         = 27901
  to_port           = 27901
  ip_protocol       = "udp"
}

resource "aws_vpc_security_group_ingress_rule" "public_web" {
  for_each = var.allowed_public_web_ipv4_cidrs

  security_group_id = aws_security_group.server.id
  description       = "Generals public web interface"
  cidr_ipv4         = each.value
  from_port         = 8082
  to_port           = 8082
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_egress_rule" "internet" {
  security_group_id = aws_security_group.server.id
  description       = "Host updates, ECR, SSM, logs, and ACME"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}
