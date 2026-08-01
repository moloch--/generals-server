# GeneralsX Online Server Operations

## Network layout

Publish these two gameplay-facing listeners on the same public host:

- TCP 29900: long-lived control connections, preferably native TLS.
- UDP 27901: gameplay relay.

HTTP 8080 exposes health state and metrics. Keep it private. A TCP reverse proxy
can front the control listener, but it cannot replace the UDP port; preserve the
original long-lived TCP connection and route UDP directly to the same server
process.

Set `--public-host` to a DNS name resolvable by every player. This value is sent
in per-player `game.started` events. It may differ from the control listener's
bind address, but it must be a bare ASCII DNS name or IPv4 address without a
scheme, port, path, whitespace, or IPv6 syntax. Invalid values fail startup. If
the relay uses a non-default external port, publish that port directly because
the current server advertises its bound UDP port.

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

Place the TLS files in `tls/fullchain.pem` and `tls/privkey.pem`, then set the
advertised DNS name:

```bash
mkdir -p tls
export GENERALS_PUBLIC_HOST=online.example.net
docker compose up --build -d
```

The compose definition publishes operations HTTP on host loopback only, drops
Linux capabilities, uses a read-only container filesystem, and persists the
SQLite database in the Compose-managed `generals-data` volume. The image
creates `/data` for uid 65532 before the empty volume is initialized, so the
non-root service can write its database without weakening host-directory
permissions. Certificate files must remain readable by container uid 65532
without making the private key world-readable.

`docker compose down` preserves the profile volume. Do not use
`docker compose down -v` unless deleting all persistent account data is
intentional. Stop the service before making a file-level volume backup, or use a
SQLite-consistent online backup. Include the complete named volume so WAL and
shared-memory sidecars cannot be omitted; its concrete Docker name can be found
with `docker volume ls --filter label=com.docker.compose.project`.

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
