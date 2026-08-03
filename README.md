# GeneralsX Online Server

`generals-server` is the standalone Go control and gameplay relay service for
the GeneralsX **MULTIPLAYER > ONLINE** path. It intentionally does not change
or participate in LAN discovery and local play.

The server provides:

- standalone guest, registration, login, resumable sessions, and profiles;
- public chat rooms, direct chat, buddy requests, and presence;
- staged game discovery, password joins, opaque retail option pass-through,
  product/INI compatibility partitioning, readiness, credential-confirmed
  launch, host coordination, and basic results/stats;
- basic two-player quickmatch keyed by mode and the exact compatibility tuple;
- a token-authenticated, slot-aware UDP relay for opaque game traffic;
- JSON health output and Prometheus metrics;
- a bearer-authenticated REST API and embedded HeroUI Pro admin dashboard.

The exact client contract is [docs/PROTOCOL.md](docs/PROTOCOL.md).
Production and service-manager guidance is in
[docs/OPERATIONS.md](docs/OPERATIONS.md).

## Run locally

Go 1.26 or newer is required.

```bash
go run ./cmd/generals-server \
  --public-host 127.0.0.1
```

Defaults are TCP `:29900`, UDP `:27901`, and HTTP `:8080`. Point the game at
the control service:

```text
-onlineServer 127.0.0.1:29900
```

Bare endpoints deliberately use ephemeral guest authentication and never send
the retail password over plaintext. Persistent account login requires a TLS
listener and a `tls://` client endpoint.

Check operations endpoints:

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/metrics
```

The admin service is disabled unless both `--admin-listen` and
`--admin-token-file` are set. The token file must contain at least 32 printable
ASCII bytes and be accessible only by its owner. In production, publish this
listener only on a private interface such as Tailscale; the supplied Compose
deployment binds it to the exact address in `GENERALS_ADMIN_HOST`.

## Internet deployment

Build a static server binary where supported:

```bash
CGO_ENABLED=0 go build -trimpath -o bin/generals-server ./cmd/generals-server
```

Configure the public DNS name separately, then advertise it to clients:

```bash
./bin/generals-server \
  --control-listen :29900 \
  --relay-listen :27901 \
  --public-host online.example.net \
  --tls-cert /run/secrets/fullchain.pem \
  --tls-key /run/secrets/privkey.pem \
  --data-file /var/lib/generals-server/profiles.db
```

`--public-host` accepts a bare ASCII DNS name or IPv4 address only. Do not add a
scheme, port, path, whitespace, or IPv6 literal; invalid values fail startup.
By default, relay credentials advertise the UDP port bound by `--relay-listen`.
If a firewall or NAT maps a different external UDP port, set that value with
`--public-relay-port`; zero keeps the bound-port default.

Allow inbound TCP 29900 and the advertised UDP relay port (27901 by default).
Firewall HTTP 8080 from the public Internet or bind it to a private monitoring
interface. The control protocol supports native TLS; do not enable insecure
password auth on an Internet-facing plaintext listener.

The admin dashboard and REST API use TCP 8081 in the Compose deployment, bound
only to the configured Tailscale IPv4 address. Do not open that port on the
public firewall.

Players connect with certificate and hostname verification enabled:

```text
-onlineServer tls://online.example.net:29900
```

Persistent profiles, password hashes, buddy relationships, and stats are stored
transactionally in SQLite at `--data-file`. File-backed databases use
write-ahead logging (WAL). Sessions, rooms, staged games, Quick Match queues,
guest profiles, and relay allocations remain process-local, so the database is
not a shared-state mechanism for horizontally scaled server replicas.

For Docker Compose, copy `.env.example` to `.env`, point
`GENERALS_DATA_DIR` at a private host directory such as
`/home/your-user/generals-data`, install the TLS files in
`GENERALS_TLS_DIR`, and run `docker compose up --build -d`. Subsequent starts
use `docker compose up -d`; the host directory remains intact across
`docker compose down`. See [docs/OPERATIONS.md](docs/OPERATIONS.md) for the
directory permissions and backup procedure.

## Admin web build

The authored React application is under `web/`. Its production output is
written to `internal/app/adminui/dist`, where the Go standard library's
`embed` package includes the complete asset tree in the server binary. The
scratch runtime image therefore contains no Node runtime, `node_modules`, or
loose web files. The dashboard uses HeroUI Pro, Font Awesome Free, and the same
GeneralsXZH icon source used by the packaged macOS application.

To regenerate the embedded assets, authenticate the HeroUI Pro CLI and run:

```bash
cd web
npm ci
npm run build
```

The generated application assets are checked in so normal Go and Docker builds
do not require HeroUI credentials. Never commit the local HeroUI cache,
`node_modules`, or an admin token.

## Verify

```bash
go test -race ./...
go vet ./...
```

The integration tests exercise two independent control clients through auth,
rooms, game staging, readiness/start coordination, and bidirectional UDP relay
traffic.

## Important limits

- At most eight participants per staged game.
- Defaults cap the service at 128 authenticated players, 64 staged/active
  games, 256 total control sockets, and 10,000 persistent profiles. Operators
  may lower these limits; `--max-profiles` is bounded to 100,000.
- A relay payload is at most 1,100 bytes, plus its 32-byte relay header.
- Up to 32 initial packets per recipient are buffered until that recipient
  binds its UDP endpoint; later overflow is dropped and counted.
- Relay credentials must be confirmed by every participant within 15 seconds
  (`--start-ready-timeout`) before `game.go` authorizes retail launch.
- Chat is ephemeral; it is not persisted.
- Chat and general control commands are rate limited per connection.
- Guest profiles cannot use persistent buddies or stats.
- Display names are case-insensitively unique and limited to 1-24 ASCII
  letters, digits, spaces, dots, dashes, or underscores so retail lobby/result
  delimiters cannot be injected. Persistent buddy lists are capped at 100
  entries.
- Stats/result submission is compatibility-oriented and trusts the client.
- Quickmatch is in-memory, same-mode pairing without geographic/rating logic;
  product, compatibility-version, and compatibility-relevant INI CRC must also
  match exactly. The server deliberately does not compare native executable
  CRCs across Windows PE and macOS Mach-O builds. Stats stay disabled until the
  retail client has a pre-launch profile-ID exchange.
- A departed custom-game participant's relay token and endpoint are removed
  without interrupting post-`game.go` survivor traffic. Explicit `game.end`
  releases the retained game and relay.
- An idle relay allocation expires after 15 minutes by default. Expiry also
  releases the corresponding control-plane game state and notifies remaining
  participants with `game.ended`.
- Relay tokens authenticate routing; they do not encrypt game packets.
