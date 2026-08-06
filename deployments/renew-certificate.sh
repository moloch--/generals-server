#!/bin/sh
set -eu

PATH=/usr/local/bin:/usr/bin:/bin
export PATH
umask 077

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -f "$script_dir/.env" ]; then
  deployment_dir=$script_dir
  environment_file=$script_dir/.env
elif [ -f "$script_dir/../.env" ]; then
  deployment_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
  environment_file=$deployment_dir/.env
else
  echo "renew-certificate: cannot find .env beside the script or in its parent" >&2
  exit 1
fi

if [ -f "$deployment_dir/compose.yaml" ]; then
  compose_file=$deployment_dir/compose.yaml
elif [ -f "$script_dir/../compose.yaml" ]; then
  compose_file=$(CDPATH= cd -- "$script_dir/.." && pwd)/compose.yaml
else
  echo "renew-certificate: cannot find compose.yaml for the deployment" >&2
  exit 1
fi

set -a
# The deployment operator owns this private file; it contains shell-compatible
# KEY=value entries shared with Docker Compose.
. "$deployment_dir/.env"
set +a

: "${GENERALS_TLS_DIR:?set GENERALS_TLS_DIR in .env}"
: "${GENERALS_UID:?set GENERALS_UID in .env}"
: "${GENERALS_GID:?set GENERALS_GID in .env}"
: "${GENERALS_CERTIFICATE_NAME:?set GENERALS_CERTIFICATE_NAME in .env}"

case "$GENERALS_TLS_DIR" in
  /*) ;;
  *) echo "renew-certificate: GENERALS_TLS_DIR must be absolute" >&2; exit 1 ;;
esac
case "$GENERALS_UID" in
  *[!0-9]*) echo "renew-certificate: UID must be numeric" >&2; exit 1 ;;
esac
case "$GENERALS_GID" in
  *[!0-9]*) echo "renew-certificate: GID must be numeric" >&2; exit 1 ;;
esac

certbot_image=${GENERALS_CERTBOT_IMAGE:-certbot/certbot:v5.4.0}
acme_volume=${GENERALS_ACME_VOLUME:-generals-server-letsencrypt}

compose() {
  docker compose \
    --env-file "$environment_file" \
    --file "$compose_file" \
    "$@"
}

test -d "$GENERALS_TLS_DIR"
actual_uid=$(stat -c %u "$GENERALS_TLS_DIR")
actual_gid=$(stat -c %g "$GENERALS_TLS_DIR")
if [ "$actual_uid:$actual_gid" != "$GENERALS_UID:$GENERALS_GID" ]; then
  echo "renew-certificate: TLS directory owner is $actual_uid:$actual_gid, expected $GENERALS_UID:$GENERALS_GID" >&2
  exit 1
fi
chmod 0700 "$GENERALS_TLS_DIR"

lock_file=$deployment_dir/renew-certificate.lock
exec 9>"$lock_file"
chmod 0600 "$lock_file"
flock -n 9 || exit 0

echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) checking certificate renewal"

old_hash=
if [ -f "$GENERALS_TLS_DIR/fullchain.pem" ]; then
  old_hash=$(sha256sum "$GENERALS_TLS_DIR/fullchain.pem" | cut -d " " -f 1)
fi

server_was_running=false
restore_server() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$server_was_running" = true ]; then
    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) restoring generals-server service"
    if ! compose start generals-server >/dev/null; then
      echo "renew-certificate: failed to restore generals-server service" >&2
      status=1
    fi
  fi
  exit "$status"
}
trap restore_server EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if compose ps --status running --services generals-server | grep -qx generals-server; then
  server_was_running=true
  compose stop --timeout 15 generals-server
fi

docker run --rm \
  --publish 0.0.0.0:80:80/tcp \
  --volume "$acme_volume":/etc/letsencrypt \
  "$certbot_image" renew \
  --non-interactive \
  --no-random-sleep-on-renew

docker run --rm \
  --network none \
  --volume "$acme_volume":/letsencrypt:ro \
  --volume "$GENERALS_TLS_DIR":/tls \
  --env CERTIFICATE_NAME="$GENERALS_CERTIFICATE_NAME" \
  --env RUNTIME_UID="$GENERALS_UID" \
  --env RUNTIME_GID="$GENERALS_GID" \
  alpine:3.23 sh -eu -c '
    certificate_dir=/letsencrypt/live/$CERTIFICATE_NAME
    test -s "$certificate_dir/fullchain.pem"
    test -s "$certificate_dir/privkey.pem"
    cp -L "$certificate_dir/fullchain.pem" /tls/.fullchain.pem.new
    cp -L "$certificate_dir/privkey.pem" /tls/.privkey.pem.new
    chown "$RUNTIME_UID:$RUNTIME_GID" /tls/.fullchain.pem.new /tls/.privkey.pem.new
    chmod 0600 /tls/.fullchain.pem.new /tls/.privkey.pem.new
    mv /tls/.fullchain.pem.new /tls/fullchain.pem
    mv /tls/.privkey.pem.new /tls/privkey.pem
  '

new_hash=$(sha256sum "$GENERALS_TLS_DIR/fullchain.pem" | cut -d " " -f 1)

if [ "$new_hash" != "$old_hash" ]; then
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) certificate changed"
else
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) certificate unchanged"
fi
