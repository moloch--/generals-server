# GeneralsX Online Server Operations

## Network layout

Publish these two gameplay-facing listeners on the same public host:

- TCP 29900: long-lived control connections, preferably native TLS.
- UDP 27901: gameplay relay.

HTTP 8080 exposes health state and metrics. Keep it private. A TCP reverse proxy
can front the control listener, but it cannot replace the UDP port; preserve the
original long-lived TCP connection and route UDP directly to the same server
process.

The optional admin listener exposes an embedded web dashboard and REST API.
Keep it on a private management interface. The Compose deployment publishes
TCP 8081 only on `GENERALS_ADMIN_HOST`, which should be the exact IPv4 address
reported by `tailscale ip -4`. A plain `8081:8081` mapping is unsafe because it
binds every host interface.

Set `--public-host` to a DNS name resolvable by every player. This value is sent
in per-player `game.started` events. It may differ from the control listener's
bind address, but it must be a bare ASCII DNS name or IPv4 address without a
scheme, port, path, whitespace, or IPv6 syntax. Invalid values fail startup. If
the public UDP port differs from the port bound by `--relay-listen`, pass the
external port with `--public-relay-port`. Its default value of zero advertises
the bound UDP port, preserving direct-listener deployments.

For example, when NAT forwards public UDP 32001 to local UDP 27901:

```bash
generals-server \
  --relay-listen :27901 \
  --public-host online.example.net \
  --public-relay-port 32001
```

## TLS

Pass a PEM certificate chain and matching private key:

```bash
generals-server \
  --public-host online.example.net \
  --tls-cert /etc/generals-server/tls/fullchain.pem \
  --tls-key /etc/generals-server/tls/privkey.pem
```

Both files are required. The server accepts TLS 1.2 or newer. Certificate
renewal currently requires a process restart. Limit private-key read access to
the service account.

The initial raw-TCP game adapter can use guest profiles on a plaintext local
server. For a trusted development network only, opt in to password auth with
`--allow-insecure-password-auth`. Never use that flag on an Internet-facing
listener: it exposes passwords and bearer tokens.

## Persistence and backups

Profiles, password hashes, buddy relationships, pending buddy requests, and
stats are stored transactionally in the SQLite database selected by
`--data-file`. The default path is `data/profiles.db`. The database and its
parent directory are created with private permissions.

File-backed databases use SQLite write-ahead logging (WAL), foreign-key
enforcement, and a bounded busy timeout. WAL permits concurrent readers while a
write is in progress, but it can place recently committed transactions in a
`profiles.db-wal` sidecar. SQLite may also create `profiles.db-shm`; both files
belong to the database while the service is running.

The service stores at most 10,000 profiles by default. Set `--max-profiles` to a
value from 1 through 100,000.
Registration checks and persists this ceiling atomically, including concurrent
attempts; lowering it below the number already stored prevents startup instead
of silently discarding profiles.

Never copy only `profiles.db` while the service is running: committed data may
still reside in its WAL file. Use a SQLite-consistent online backup mechanism,
such as the SQLite backup API or `VACUUM INTO`, or stop the service cleanly and
copy the entire database directory or container volume. A live filesystem
snapshot is safe only when it captures the database, WAL, and shared-memory
sidecars at the same point in time. Test restoration by starting one isolated
process against the restored copy.

The SQLite store removes whole-database JSON rewrites, but the service retains
deliberate single-node limits:

- run exactly one writer process per database file;
- do not place the database on storage with unreliable file locking, including
  network filesystems that do not provide SQLite-compatible locks;
- do not modify the database directly while the server runs;
- do not treat the database as shared coordination between replicas.

Control, game, quickmatch, and UDP relay state are process-local. Horizontal
scaling therefore requires a shared coordination design in addition to a
multi-node datastore; pointing replicas at one SQLite file is unsupported.

## Container deployment

The Compose deployment keeps the database in a normal host directory instead
of a Docker-managed volume. Create private data and TLS directories, generate
a private admin bearer token, copy the example environment, and set the
advertised host, Tailscale IPv4 address, plus your numeric host user and group
IDs:

```bash
install -d -m 0700 "$HOME/generals-data" "$PWD/tls"
umask 077
openssl rand -base64 48 > "$PWD/admin-token"
cp .env.example .env
# Edit .env: set GENERALS_PUBLIC_HOST, GENERALS_DATA_DIR, GENERALS_TLS_DIR,
# GENERALS_ADMIN_HOST from `tailscale ip -4`, GENERALS_ADMIN_TOKEN_FILE,
# GENERALS_UID from `id -u`, GENERALS_GID from `id -g`, and the certificate
# lineage name used by Certbot.
install -m 0600 /secure/source/fullchain.pem "$PWD/tls/fullchain.pem"
install -m 0600 /secure/source/privkey.pem "$PWD/tls/privkey.pem"
docker compose up --build -d
```

The TLS copy commands are mandatory: Compose intentionally refuses to create
or populate the configured TLS directory. To issue a Let's Encrypt IP
certificate instead of copying an existing certificate, first create the ACME
state volume and request the short-lived certificate while public TCP port 80
is free:

```bash
docker volume create generals-server-letsencrypt
docker run --rm \
  --publish 0.0.0.0:80:80/tcp \
  --volume generals-server-letsencrypt:/etc/letsencrypt \
  certbot/certbot:v5.4.0 certonly \
  --standalone --non-interactive --agree-tos \
  --email operator@example.net \
  --preferred-profile shortlived \
  --cert-name online-example-net \
  --ip-address 203.0.113.10
./deployments/renew-certificate.sh
docker compose up --build -d
```

Set the same certificate name in `.env`. Replace the example email and IP; for
a DNS certificate, use Certbot's `--domains` option instead. The renewal script
uses the existing Certbot lineage, atomically refreshes the private host TLS
directory, preserves mode `0600` and the configured host ownership, and
restarts the Compose container only when the certificate changed.

Install the script in the service user's crontab. Logging through `logger`
keeps output under the host journal's rotation policy instead of appending to
an unbounded file:

```cron
17 3,15 * * * /absolute/path/generals-server/deployments/renew-certificate.sh 2>&1 | /usr/bin/logger -t generals-server-cert-renew
```

After the first build, routine lifecycle commands need no extra flags:

```bash
docker compose up -d
docker compose logs -f
docker compose restart
docker compose down
```

The compose definition publishes operations HTTP on host loopback only and the
admin service on the exact Tailscale IPv4 address only. It drops Linux
capabilities, uses a read-only container filesystem, and bind-mounts
`GENERALS_DATA_DIR` at `/data`. It runs as `GENERALS_UID:GENERALS_GID`, so
`profiles.db` and any WAL sidecars remain owned and directly accessible by the
host service account. `create_host_path: false` deliberately makes startup
fail if a configured path is missing instead of silently creating a root-owned
path. Keep both directories mode `0700`; certificate files and the admin token
should be mode `0600`.

`docker compose down` removes containers and networks but does not touch the
host database directory. Stop the service before making a file-level backup,
then copy the complete `GENERALS_DATA_DIR`; do not omit WAL or shared-memory
sidecars. Keep backups outside that directory and test restoration with an
isolated server process.

## Admin dashboard and REST API

Start the admin listener only with both flags:

```bash
generals-server \
  --admin-listen 100.64.0.1:8081 \
  --admin-token-file /etc/generals-server/admin-token
```

The web interface is available at `/admin/`. Its JavaScript, CSS, and HTML are
compiled into `internal/app/adminui/dist` and embedded directly in the Go
binary with `embed.FS`; there is no runtime web directory to mount or manage.
The login screen retains the bearer token only in the current browser tab's
`sessionStorage`.

Every REST endpoint below requires `Authorization: Bearer <token>` and returns
a JSON `data` envelope except successful profile/session/game mutations, which
return 204:

- `GET /api/admin/v1/overview`: process, hub, and relay counters.
- `GET /api/admin/v1/profiles?query=&limit=&offset=`: searchable profile page.
- `PUT /api/admin/v1/profiles/{userID}/password`: reset an account password.
- `DELETE /api/admin/v1/profiles/{userID}`: permanently delete an account.
- `GET /api/admin/v1/sessions`: active control sessions.
- `DELETE /api/admin/v1/sessions/{userID}`: disconnect one player.
- `GET /api/admin/v1/games`: staged and active games.
- `DELETE /api/admin/v1/games/{gameID}`: close a game and remove its members.
- `POST /api/admin/v1/events/ticket`: issue a 30-second, single-use realtime
  connection ticket.

The dashboard exchanges its bearer token for the short-lived ticket and opens
`GET /api/admin/v1/events?ticket=...` as a same-origin WebSocket. The stream
pushes overview, session, game, relay, and profile-revision snapshots once per
second; the browser falls back to periodic REST refreshes while disconnected.
The long-lived admin token is never placed in a URL.

Resetting a password or deleting a profile revokes saved resume tokens,
pending admissions, and any active control connection. Profile deletion also
cascades through the account's buddy relationships and pending requests.

Profile, player, game, snapshot, and counter identifiers that could exceed
JavaScript's safe integer range are serialized as strings. The server does not
enable CORS, the API never returns credentials, and disruptive actions are
logged by ID.

From another device on the same tailnet, open:

```text
http://<tailscale-name-or-ip>:8081/admin/
```

No public firewall port is required for this listener. Verify the host binding
after deployment with `docker compose ps` and `ss -ltn`; it must show the
Tailscale address, not `0.0.0.0:8081` or `[::]:8081`.

## systemd deployment

Install the binary and supplied unit, then provide the real DNS name in the
environment file:

```bash
sudo install -m 0755 generals-server /usr/local/bin/generals-server
sudo install -m 0644 deployments/generals-server.service /etc/systemd/system/
sudo install -d -m 0750 -o generals-server -g generals-server /etc/generals-server/tls
sudo sh -c 'printf "%s\n" "GENERALS_PUBLIC_HOST=online.example.net" > /etc/generals-server/environment'
sudo systemctl daemon-reload
sudo systemctl enable --now generals-server
```

Create the locked-down `generals-server` system user and install readable TLS
files before starting the unit. The service uses systemd's `StateDirectory` for
the SQLite database and its WAL sidecars, and binds health checks to loopback.

## Monitoring

Use `GET /healthz` or `GET /readyz` for process health. Both return live control
and relay addresses, player/game gauges, and UDP counters. `GET /metrics`
returns Prometheus text.

Important signals:

- `generals_online_players` and game gauges show control-plane load.
- `generals_relay_dropped_auth_total` detects stale or forged credentials.
- `generals_relay_dropped_rate_limit_total` indicates abusive clients or limits
  that are too low for real matches.
- `generals_relay_buffered_until_bind_total` shows gameplay packets arriving
  before every participant completed UDP Bind.
- `generals_relay_dropped_no_endpoint_total` means a recipient's bounded
  pre-Bind queue overflowed.
- sustained relay bytes/packets establish required egress capacity.

The service logs structured key/value text to stdout. Collect it through the
container runtime or journal.

## Graceful updates

Send SIGTERM and allow up to ten seconds for control and HTTP shutdown. Active
matches are not migrated; a restart invalidates in-memory sessions, rooms,
games, quickmatch entries, guest profiles, and relay tokens. Persistent account
data remains.

Schedule updates outside active matches. A future multi-node design needs
draining, shared state, and explicit relay migration before zero-downtime
updates are possible.

## Game compatibility partitioning

Every custom game and quickmatch queue entry carries an immutable product,
compatibility-version, and unsigned 32-bit INI CRC tuple. Custom joins reject a
different tuple with `incompatible_game`; quickmatch pairs only an exact tuple
and mode match. This keeps Generals separate from Zero Hour and prevents known
gameplay-data differences from entering the same relay allocation.

The INI CRC is the client-computed checksum of compatibility-relevant game
data. Do not replace it with a native executable checksum or introduce an
operator-side PE/Mach-O comparison: Windows and macOS executables necessarily
differ while remaining eligible for the same cross-platform game.

## Capacity and abuse controls

Each game supports at most eight participants. UDP payloads are capped at 1,100
bytes before the 32-byte relay header. Per-participant packet and byte limits
are configurable; defaults are 600 packets and 2 MiB per second. Idle relay
allocations expire after 15 minutes. Before launch, every participant has 15
seconds by default to confirm that it parsed its relay credentials; configure
this with `--start-ready-timeout`. A timeout deletes the pending game and relay
instead of leaving a host stuck in `starting`.

Relay idle expiry is terminal for the corresponding game. The server removes
the control-plane game and every user-to-game mapping, restores connected
participants to Online status, and sends `game.ended` with reason
`relay_idle_timeout`. This cleanup is coordinated after releasing the relay
lock so concurrent control commands and disconnects cannot invert Hub/relay
lock ordering.

The default control-plane ceilings are 256 total TCP/TLS sockets, 128
authenticated players, 10,000 persistent profiles, and 64 staged or active
games. Per connection, the server accepts at most 60 commands per second and
10 chat messages per ten seconds. These are configured with
`--max-control-connections`, `--max-online-players`, `--max-profiles`,
`--max-staged-games`, `--max-commands-per-second`, and
`--max-chat-messages-per-10s`; pre-launch confirmation uses
`--start-ready-timeout`. The UDP relay retains up to 32 early packets per
recipient until that player binds; queue overflow drops the oldest packet.

After `game.go`, a participant control disconnect removes only that player's
relay token, endpoint, and slot. Survivor endpoints stay allocated so a
three-to-eight-player match does not collapse because one player left. The
remaining host—or any survivor if the host departed—must eventually send
`game.end` to release the game and relay cleanly.

Firewall all unused ports, monitor rejected traffic, and apply upstream
connection limits or denial-of-service protection suitable for long-lived TCP
and UDP. The application limits message sizes and relay rates, but it is not a
general-purpose DDoS mitigation layer.
