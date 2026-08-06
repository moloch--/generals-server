resource "aws_eip" "server" {
  domain   = "vpc"
  instance = aws_instance.server.id

  tags = {
    Name = local.name_prefix
  }

  depends_on = [aws_internet_gateway.server]
}

resource "aws_route53_record" "server" {
  zone_id = data.aws_route53_zone.server.zone_id
  name    = var.hostname
  type    = "A"
  ttl     = 300
  records = [aws_eip.server.public_ip]
}

resource "aws_route53_record" "public_web" {
  zone_id = data.aws_route53_zone.server.zone_id
  name    = var.public_hostname
  type    = "A"
  ttl     = 300
  records = [aws_eip.server.public_ip]
}
