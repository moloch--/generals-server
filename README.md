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
- JSON health output and Prometheus metrics.

The exact client contract is [docs/PROTOCOL.md](docs/PROTOCOL.md).
Production and service-manager guidance is in
[docs/OPERATIONS.md](docs/OPERATIONS.md).

## Run locally

Go 1.22 or newer is required.

```bash
go run ./cmd/generals-server \
  -public-host 127.0.0.1
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

## Internet deployment

Build a static server binary where supported:

```bash
CGO_ENABLED=0 go build -trimpath -o bin/generals-server ./cmd/generals-server
```

Configure the public DNS name separately, then advertise it to clients:

```bash
./bin/generals-server \
  -control-listen :29900 \
  -relay-listen :27901 \
  -public-host online.example.net \
  -tls-cert /run/secrets/fullchain.pem \
  -tls-key /run/secrets/privkey.pem \
  -data-file /var/lib/generals-server/profiles.json
```

`-public-host` accepts a bare ASCII DNS name or IPv4 address only. Do not add a
scheme, port, path, whitespace, or IPv6 literal; invalid values fail startup.

Allow inbound TCP 29900 and UDP 27901. Firewall HTTP 8080 from the public
Internet or bind it to a private monitoring interface. The control protocol supports native TLS; do
not enable insecure password auth on an Internet-facing plaintext listener.

Players connect with certificate and hostname verification enabled:

```text
-onlineServer tls://online.example.net:29900
```

The profile database is one atomically replaced JSON file, held in memory and
serialized under a process-local lock. This is intentionally simple for a
single-server MVP. It is not suitable for multiple replicas sharing one file,
large user populations, audit-grade results, or concurrent external writers.

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
  may lower these limits; `-max-profiles` is bounded to 100,000 for the JSON
  persistence backend.
- A relay payload is at most 1,100 bytes, plus its 32-byte relay header.
- Up to 32 initial packets per recipient are buffered until that recipient
  binds its UDP endpoint; later overflow is dropped and counted.
- Relay credentials must be confirmed by every participant within 15 seconds
  (`-start-ready-timeout`) before `game.go` authorizes retail launch.
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
- Relay tokens authenticate routing; they do not encrypt game packets.
