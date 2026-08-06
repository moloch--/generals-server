# Cost-effective AWS deployment

This deployment runs the existing `generals-server` container on one Amazon
Linux 2023 EC2 instance. It uses one public subnet, an Elastic IP, a retained
encrypted EBS data volume, Route 53, ECR, Parameter Store, and short-retention
CloudWatch Logs.

It intentionally creates no load balancer, NAT Gateway, Auto Scaling group,
backup policy, snapshot schedule, SSH listener, bastion, or multi-AZ replica.
TCP `29900`, UDP `27901`, and the read-only website on TCP `8082` are public
inbound ports. Health TCP `8080` and admin TCP `8081` bind to host loopback and
are reachable through Session Manager port forwarding.

## Availability and data boundary

This is one process in one Availability Zone. The external EBS volume survives
normal instance replacement, but it is not a backup. Volume corruption,
accidental deletion outside Terraform, account compromise, or loss of the
selected Availability Zone can permanently destroy profiles. Live sessions,
lobbies, matches, queues, guest profiles, and relay allocations also disappear
whenever the container restarts.

Terraform sets `prevent_destroy` on the data volume and never force-detaches it.
These controls reduce operator mistakes but do not provide recovery copies.
One CloudWatch alarm requests EC2 system-status recovery in place; it cannot
recover an unavailable Availability Zone or damaged EBS data.

## Layout

- `bootstrap/` creates the versioned S3 Terraform-state bucket and immutable
  ECR repository. It starts with local state because a stack cannot create its
  own backend before initialization.
- `terraform/` creates the single-AZ network, EC2 instance, Elastic IP, DNS,
  retained EBS volume, IAM, state-free admin token, and log group.
- `runtime/` contains the hardened Compose definition, host scripts, and
  systemd units installed through compressed EC2 user data.

The server image is pinned by digest. Updating the digest changes a non-secret
Parameter Store value; the host reconciles it within five minutes without
replacing EC2. Host-configuration inputs such as hostname, repository,
architecture, ACME email, or an explicitly pinned AMI produce a visible EC2
replacement in the plan. Edits only to embedded bootstrap scripts deliberately
require `-replace=aws_instance.server`; Terraform never performs a useless
user-data stop/start that cloud-init would not replay.

## Prerequisites

- Terraform `1.11` or newer;
- AWS CLI v2 authenticated to the intended account;
- Docker with Buildx;
- the Session Manager plugin for admin tunnels;
- an existing public Route 53 hosted zone;
- permissions to create the resources in both stacks.

The default runtime is ARM64 on `t4g.small`. Publish a `linux/arm64` image or
change both `architecture` and `instance_type` consistently.

## 1. Bootstrap state and ECR

```bash
cd deployments/aws/bootstrap
cp terraform.tfvars.example terraform.tfvars
# Set expected_aws_account_id and a globally unique state_bucket_name.
terraform init
terraform test
terraform plan -out bootstrap.tfplan
terraform apply bootstrap.tfplan
```

Record the outputs:

```bash
terraform output -raw state_bucket_name
terraform output -raw aws_region
terraform output -raw ecr_repository_url
terraform output -raw ecr_repository_name
```

The bootstrap state remains local and contains no application secret. Protect
it until the bootstrap resources are intentionally migrated or imported into
another state.

## 2. Build and publish an immutable image

From `deployments/aws/bootstrap`:

```bash
repository_url=$(terraform output -raw ecr_repository_url)
repository_name=$(terraform output -raw ecr_repository_name)
region=$(terraform output -raw aws_region)
registry=${repository_url%%/*}
release_tag=$(git -C ../../.. rev-parse --short=12 HEAD)

aws ecr get-login-password --region "$region" |
  docker login --username AWS --password-stdin "$registry"

docker buildx build \
  --platform linux/arm64 \
  --tag "$repository_url:$release_tag" \
  --push \
  ../../..

aws ecr describe-images \
  --region "$region" \
  --repository-name "$repository_name" \
  --image-ids "imageTag=$release_tag" \
  --query 'imageDetails[0].imageDigest' \
  --output text
```

Copy the returned `sha256:...` digest into the deployment variables. Never use
`latest`; ECR tag immutability is enabled.

## 3. Initialize and apply the deployment

```bash
cd ../terraform
cp terraform.tfvars.example terraform.tfvars
```

Set at least:

- the expected AWS account ID;
- Region and one Availability Zone;
- public hostname and hosted-zone ID;
- ACME contact email;
- ECR repository name and published image digest.
- optional gameplay and public-web IPv4 CIDR allowlists; both default to the
  public Internet.

Initialize the backend using the bucket created above:

```bash
state_bucket=$(terraform -chdir=../bootstrap output -raw state_bucket_name)
region=$(terraform -chdir=../bootstrap output -raw aws_region)

terraform init \
  -backend-config="bucket=$state_bucket" \
  -backend-config="key=generals-server/prod.tfstate" \
  -backend-config="region=$region"

terraform fmt -check -recursive ..
terraform validate
terraform test
terraform plan -out deployment.tfplan
terraform apply deployment.tfplan
```

Terraform generates the 64-character admin token with an ephemeral random
value and writes it through Parameter Store's write-only attribute. The token
does not enter the Terraform plan or state. Increment `admin_token_version` and
apply to rotate it.

The host obtains a free Let's Encrypt certificate with the Route 53 DNS-01
plugin, so public TCP `80` is never opened. Complete Certbot state and the
exported PEM files reside on the retained EBS volume. A systemd timer checks for
renewal twice daily. Hostname, expiry, and key pairing are validated, and the
container is restarted only when a new pair has passed readiness; failed loads
remain pending for the next reconciliation.

## 4. Verify the host

The initial certificate and image pull can take several minutes. The host
retries failed reconciliation automatically.

```bash
instance_id=$(terraform output -raw instance_id)
region=$(terraform output -raw aws_region)

aws ssm send-command \
  --region "$region" \
  --instance-ids "$instance_id" \
  --document-name AWS-RunShellScript \
  --parameters 'commands=["cloud-init status --wait","systemctl status --no-pager generals-server.service generals-deploy.service","findmnt /srv/generals","docker inspect generals-server --format {{.Image}}"]'
```

Verify the public TLS control endpoint:

```bash
openssl s_client \
  -connect "$(terraform output -raw public_hostname):29900" \
  -servername "$(terraform output -raw public_hostname)" \
  -verify_return_error </dev/null
```

An arbitrary UDP probe cannot validate the authenticated relay. Use the
repository's real client/integration flow for UDP verification.

Verify the independently routed public website and snapshot API:

```bash
public_web_url=$(terraform output -raw public_web_url)
curl --fail --show-error --silent "$public_web_url" >/dev/null
curl --fail --show-error --silent "${public_web_url}api/public/v1/snapshot"
```

The supplied single-host stack exposes TCP `8082` as plaintext origin HTTP. For
a browser-facing production site, terminate HTTPS separately on TCP `443` and
forward only public-site requests to this origin; the stack intentionally does
not claim built-in web TLS or send private admin TCP `8081` through that path.

TCP `8082` exposes only read-only public data. The private admin UI and every
`/api/admin/v1/*` route remain unavailable there even if a request supplies an
admin bearer token.

## Admin access

Start the tunnel printed by Terraform:

```bash
terraform output -raw admin_tunnel_command
```

Run that command, then open <http://127.0.0.1:8081/admin/>. Retrieve the bearer
token only when needed:

```bash
aws ssm get-parameter \
  --region "$(terraform output -raw aws_region)" \
  --name "$(terraform output -raw admin_token_parameter_name)" \
  --with-decryption \
  --query 'Parameter.Value' \
  --output text
```

The command prints the token to the current terminal. Do not save it in shell
history, Terraform variables, repository files, or durable logs.

For health and metrics, run the `health_tunnel_command` output and query
`http://127.0.0.1:8080/readyz` or `/metrics` locally.

## Release and rollback

Build and push another immutable tag, update `container_image_digest`, and
apply Terraform. The host notices the desired-image Parameter Store change,
pulls and platform-verifies the digest through the ECR credential helper while
the old container remains live, then restarts exactly one container. To
reconcile immediately instead of waiting for the timer:

```bash
aws ssm send-command \
  --region "$(terraform output -raw aws_region)" \
  --instance-ids "$(terraform output -raw instance_id)" \
  --document-name AWS-RunShellScript \
  --parameters 'commands=["systemctl start generals-deploy.service"]'
```

Rollback is the same operation with the previous digest. Each deployment is a
brief outage because two SQLite writers and two independent relay processes
must never overlap.

## Planned host replacement

Changes to the moving public AMI and embedded user-data implementation are
ignored deliberately. Host-configuration inputs and an explicit `ami_id`
change do plan a replacement. For any controlled OS/bootstrap replacement,
stop the application first through SSM, stop the instance, and then apply the
reviewed replacement plan (adding `-replace` when Terraform did not infer it):

```bash
region=$(terraform output -raw aws_region)
instance_id=$(terraform output -raw instance_id)

command_id=$(aws ssm send-command \
  --region "$region" \
  --instance-ids "$instance_id" \
  --document-name AWS-RunShellScript \
  --parameters 'commands=["systemctl stop generals-server.service"]' \
  --query 'Command.CommandId' \
  --output text)

aws ssm wait command-executed \
  --region "$region" \
  --command-id "$command_id" \
  --instance-id "$instance_id"

aws ec2 stop-instances \
  --region "$region" \
  --instance-ids "$instance_id"

aws ec2 wait instance-stopped \
  --region "$region" \
  --instance-ids "$instance_id"

terraform apply -replace=aws_instance.server
```

The attachment resource stops the old instance before detaching and does not
force-detach. The replacement must stay in the configured Availability Zone so
it can mount the existing volume. Because this deployment has no backups, test
replacement procedures with disposable data before using them on production.

## Destruction

`terraform destroy` intentionally stops at the retained EBS volume. Deleting
that volume requires a conscious source change removing `prevent_destroy`, and
permanently erases the only profile database. The bootstrap state bucket is
also protected, while a non-empty ECR repository refuses deletion.
