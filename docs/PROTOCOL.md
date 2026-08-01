# GeneralsX Online Protocol v1

This document is the wire contract between the GeneralsX Online client and
`generals-server`. Protocol version 1 uses one long-lived TCP control
connection and a separate authenticated UDP gameplay relay.

Default ports:

| Service | Transport | Default |
|---|---:|---:|
| Control | TCP or TLS-over-TCP | `29900` |
| Gameplay relay | UDP | `27901` |
| Health and metrics | HTTP | `8080` |

The game command-line flag `-onlineServer <endpoint>` selects the control
endpoint. `tls://host[:control-port]` requires verified TLS and enables
persistent account authentication. A bare `host[:control-port]` is explicit
plaintext guest mode for local development. The server advertises the public
relay host and actual relay port in `game.started`; the client must not derive
the relay endpoint from the control endpoint.

## Control framing and envelopes

Control messages are UTF-8 JSON objects delimited by one LF byte (`0x0a`). A
message may not contain an unescaped newline inside its JSON encoding. The
default maximum encoded line is 65,536 bytes. CRLF input works because JSON
whitespace is permitted, but implementations should send LF only.

Every client request has this envelope:

```json
{"v":1,"type":"game.list","id":"42","data":{}}
```

- `v` is the integer `1`.
- `type` is one command name from this document.
- `id` is a non-empty, client-selected string. It only correlates the response.
- `data` is a command-specific object. Use `{}` when a command has no fields.
- Unknown fields inside `data` are rejected. Clients should omit optional
  fields instead of sending `null`.

A successful response is:

```json
{"v":1,"type":"response","id":"42","ok":true,"data":{"games":[]}}
```

An error response is:

```json
{"v":1,"type":"response","id":"42","ok":false,"code":"game_not_found","error":"game not found"}
```

Server events have no `id` or `ok`:

```json
{"v":1,"type":"game.list","data":{"games":[]}}
```

Responses and events can be interleaved. Clients must dispatch by both `type`
and `id`, rather than assuming the next line is the response to the last
request.

## Common objects

### Profile

```json
{
  "user_id": 1,
  "username": "example",
  "display_name": "Example",
  "guest": false,
  "created_at": "2026-08-01T12:34:56.000000000Z"
}
```

`user_id` is an unsigned 64-bit integer. Persistent profile IDs have the high
bit clear. Ephemeral guest IDs have the high bit set. JSON parsers that cannot
represent all uint64 values exactly must retain guest IDs as their original
decimal token or use an unsigned 64-bit representation.

### Member

```json
{"user_id":1,"display_name":"Example","host":true,"ready":false,"slot":0}
```

Gameplay slots are integers `0` through `7`. Chat-room members use slot `-1`.

### Game options

```json
{
  "map":"Maps/Example/Example.map",
  "starting_cash":10000,
  "use_stats":true,
  "allow_observers":false,
  "opaque":"legacy game option string",
  "slot_list":"legacy slot list string",
  "ready_key":"gxrk1:0123456789abcdef"
}
```

`opaque` and `slot_list` are passed through without interpretation so the
existing GameSpy staging-room adapter can retain its retail option encoding.
Each is limited to 4,096 bytes. `map` is limited to 128 bytes.
`ready_key` is an adapter-generated semantic fingerprint that excludes only a
human slot's accepted bit. A changed key clears non-host readiness; an
acceptance-only slot-list echo with the same key does not.

### Game summary and snapshot

```json
{
  "product":"zerohour",
  "compatibility_version":1,
  "ini_crc":1865069505,
  "game_id":"0123456789abcdef",
  "name":"No Rush",
  "map":"Maps/Example/Example.map",
  "host_name":"Example",
  "players":2,
  "max_players":8,
  "has_password":false,
  "state":"open"
}
```

The top-level compatibility tuple is immutable for the lifetime of the game:

- `product` is exactly `generals` or `zerohour`;
- `compatibility_version` is currently `1`;
- `ini_crc` is the client's unsigned 32-bit checksum of compatibility-relevant
  game INI data.

This tuple separates clients whose gameplay data or product differs. It does
not contain or compare the native executable CRC: Windows PE and macOS Mach-O
binaries are expected to differ even when their gameplay data is compatible.

`game_id` is exactly 16 lowercase hexadecimal characters. State is `open`,
`starting` (relay credentials issued; confirmations pending), or `started`
(every participant confirmed credentials and received `game.go`). A game
snapshot adds `members` and the complete `options` object:

```json
{
  "product":"zerohour",
  "compatibility_version":1,
  "ini_crc":1865069505,
  "game_id":"0123456789abcdef",
  "name":"No Rush",
  "host_name":"Example",
  "players":2,
  "max_players":8,
  "has_password":false,
  "state":"open",
  "members":[
    {"user_id":1,"display_name":"Example","host":true,"ready":false,"slot":0},
    {"user_id":2,"display_name":"Peer","host":false,"ready":true,"slot":1}
  ],
  "options":{"use_stats":true,"allow_observers":false}
}
```

Passwords are never included in summaries, snapshots, or events.

Default service-wide limits are 128 authenticated players, 10,000 persistent
profiles, 64 staged or active games, and 256 total control sockets. Each
connection may issue 60 commands per second and 10 chat messages per ten-second
window. Operators may lower or raise these bounded settings with the
corresponding command-line flags. Commands rejected by either per-connection
limiter return `rate_limited`.

## Connection and authentication

Immediately after accepting TCP, the server sends:

```json
{"v":1,"type":"server.hello","data":{"protocol":1,"server_time":"2026-08-01T12:34:56Z","password_auth_requires_tls":true}}
```

The first client request must be one of:

| Command | Request `data` | Successful response `data` |
|---|---|---|
| `auth.guest` | `{"display_name":"Player"}` | Auth result |
| `auth.register` | `{"username":"player1","password":"at least 8 bytes","display_name":"Player"}` | Auth result |
| `auth.login` | `{"username":"player1","password":"at least 8 bytes"}` | Auth result |
| `auth.resume` | `{"token":"base64url token"}` | Auth result with a rotated token |

An auth result is:

```json
{
  "profile":{"user_id":1,"username":"player1","display_name":"Player","guest":false,"created_at":"2026-08-01T12:34:56Z"},
  "token":"43-byte-base64url-token",
  "expires_at":"2026-08-02T12:34:56Z"
}
```

Guest auth results contain only `profile`; guests do not receive resumable
tokens. Persistent profiles receive at most one live resume token, so issuing
or rotating a token invalidates the previous token for that profile.
Display names must contain 1-24 ASCII letters, digits, spaces, dots, dashes, or
underscores. Commas, colons, semicolons, and backslashes are rejected because
the retail lobby and results formats use them as unescaped delimiters.

Registration reserves both player capacity and the display name before it
persists a profile. A rejected registration therefore creates no account.
Registrations return `server_full` when either the online-player admission
limit or persistent-profile limit is reached. A resume token is consumed only
after admission succeeds, so a `server_full` resume can retry the same token.

After the auth response, the server joins the connection to `global` and sends
`session.ready` with the current room snapshot and public game list:

```json
{"v":1,"type":"session.ready","data":{"room":{...},"games":[]}}
```

Only one live connection per `user_id` is retained. Replacing a connection
sends `session.replaced` to the older connection before closing it.

Password auth is rejected on plaintext TCP by default. Local development may
start the server with `--allow-insecure-password-auth`; Internet deployments
should configure `--tls-cert` and `--tls-key`. Persistent token resumption is
also rejected on plaintext unless that unsafe development override is set.
The standalone game uses only `auth.guest` for a bare endpoint and never
serializes its password there. Plaintext exposes chat and control metadata.

Authenticated session commands:

| Command | Request `data` | Successful response `data` |
|---|---|---|
| `ping` | `{}` | `{"type":"pong","server_time":"..."}` |
| `session.close` | `{}` | `{"closed":true}`; server then closes TCP |
| `profile.get` | `{}` | `{"profile": Profile}` |
| `profile.update` | `{"display_name":"New Name"}` | `{"profile": Profile}` |

## Network rooms

Rooms are public chat/discovery channels, distinct from individual staged
games. The built-in room IDs are `global`, `2v2`, `3v3`, and `4v4`.

| Command | Request `data` | Successful response `data` |
|---|---|---|
| `room.list` | `{}` | `{"rooms":[{"room_id":"global","name":"Global","players":3}]}` |
| `room.join` | `{"room_id":"2v2"}` | `{"room": RoomSnapshot}` |
| `room.leave` | `{}` | `{"left":true}` |
| `room.chat` | `{"message":"hello","action":false}` | `{"sent":true}` |

Joining a room automatically leaves the previous room. Membership changes send
this event to all current members:

```json
{"v":1,"type":"room.updated","data":{"room":{"room_id":"global","name":"Global","members":[...]}}}
```

Chat sends:

```json
{
  "v":1,
  "type":"room.chat",
  "data":{"room_id":"global","user_id":1,"display_name":"Player","message":"hello","action":false,"sent_at":"2026-08-01T12:34:56Z"}
}
```

## Staged games

| Command | Request `data` | Successful response `data` |
|---|---|---|
| `game.list` | `{}` | `{"games":[GameSummary,...]}` |
| `game.create` | `{"product":"zerohour","compatibility_version":1,"ini_crc":1865069505,"name":"Game","password":"","max_players":8,"options":GameOptions}` | `{"game":GameSnapshot}` |
| `game.join` | `{"product":"zerohour","compatibility_version":1,"ini_crc":1865069505,"game_id":"0123456789abcdef","password":""}` | `{"game":GameSnapshot}` |
| `game.leave` | `{}` | `{"left":true}` |
| `game.options` | Any subset of `name`, `password`, `max_players`, `options` | `{"game":GameSnapshot}` |
| `game.ready` | `{"ready":true}` | `{"ready":true}` |
| `game.chat` | `{"message":"hello","action":false}` | `{"sent":true}` |
| `game.kick` | `{"user_id":2}` | `{"kicked":true,"user_id":2}` |
| `game.start` | `{}` | `{"starting":true,"game_id":"..."}` |
| `game.start_ready` | `{"game_id":"0123456789abcdef"}` | `{"ready":true,"go":false}` (or `go:true` for the final confirmation) |
| `game.end` | `{}` | `{"ended":true}` |

`game.options`, `game.kick`, and `game.start` require the host. `game.end`
requires the host while it remains present; after a started host disconnects,
any survivor may use it for cleanup. A kick targets a non-host member before
start and emits `game.kicked` to that player with
`{"game_id":"...","reason":"host_kicked"}`. A start requires at least two members
and every non-host member to be ready. Create and join are rejected while the
user is in another game or the quickmatch queue; cancel quickmatch first.
`starting` and `started` games are omitted from the public joinable list.
Create requires a valid compatibility tuple. Join requires an exact match with
the tuple advertised by the game summary; any tuple mismatch returns the stable
error code `incompatible_game`. Game option updates cannot change this tuple.

Public-list changes send `game.list` to every authenticated client:

```json
{"v":1,"type":"game.list","data":{"games":[...]}}
```

Any staging change sends `game.updated` to members:

```json
{"v":1,"type":"game.updated","data":{"game":GameSnapshot}}
```

For `game.join`, existing members receive that update, while the joining client
first receives its successful response containing the authoritative snapshot.
This establishes the retail staging room before subsequent staging events.

Game chat sends `game.chat` with the same sender/message/action/timestamp fields
as room chat plus `game_id`.

`game.start` allocates the relay and moves the game to `starting`. Each
participant receives an individual `game.started` credential event. Tokens are
different for every participant:

```json
{
  "v":1,
  "type":"game.started",
  "data":{
    "game_id":"0123456789abcdef",
    "relay_host":"games.example.net",
    "relay_port":27901,
    "slot":1,
    "relay_token":"32-lowercase-hex-characters",
    "relay_protocol":1,
    "peers":[
      {"user_id":1,"display_name":"Host","host":true,"ready":false,"slot":0},
      {"user_id":2,"display_name":"Peer","host":false,"ready":true,"slot":1}
    ]
  }
}
```

After parsing and storing those credentials, each client sends
`game.start_ready` for the same game ID. This confirmation means credentials
were accepted; it does not wait for UDP BindAck because the retail transport is
created only after GAMESTART. The final confirmation changes state to
`started`, and every current member receives:

```json
{"v":1,"type":"game.go","data":{"game_id":"0123456789abcdef"}}
```

If every member does not confirm within `--start-ready-timeout` (15 seconds by
default), the relay and game are deleted and members receive `game.ended` with
reason `start_timeout`. Any departure while `starting` similarly cancels with
reason `player_left`. Timer generation and state checks prevent a stale timeout
from cancelling a game after `game.go`.

After `game.go`, a listed custom game survives individual control departures.
The relay deletes only that participant's token, endpoint, and slot, while
survivor endpoints remain active. Survivors receive this nonterminal event:

```json
{"v":1,"type":"game.peer_left","data":{"game_id":"0123456789abcdef","departed_user_id":2,"departed_display_name":"Peer","departed_slot":1,"departed_host":false}}
```

Slots and host flags are not reassigned mid-match. If the departed player was
the host, any survivor may send `game.end`. Explicit `game.end` deletes the
game and relay, releases all members, and emits `game.ended` with reason
`host_ended`; it never reopens stale staging state. The immutable roster
captured at `game.go` remains the authority for `stats.results`, so a host can
still report a departed launch player as `disconnect`.

Before relay allocation, a staged host departure dissolves the open lobby.
Remaining members first receive a final `game.updated` without the host and
with host-authored `opaque`, `slot_list`, and `ready_key` cleared, followed by
`game.ended` with reason `host_left`. Empty games are deleted.

## Player chat, buddies, and status

| Command | Request `data` | Successful response `data` |
|---|---|---|
| `player.chat` | `{"user_id":2,"message":"hello"}` | `{"sent":true}` |
| `buddy.list` | `{}` | `{"buddies":[Buddy,...],"pending":[Buddy,...]}` |
| `buddy.request` | `{"user_id":2}` or `{"display_name":"Peer"}` | `{"requested":true,"user_id":2}` |
| `buddy.accept` | `{"user_id":2}` | `{"accepted":true}` |
| `buddy.remove` | `{"user_id":2}` | `{"removed":true}` |
| `buddy.status` | `{"status":"online"}` | `{"status":"online"}` |

Valid status values are `online`, `away`, and `in_game`. Buddy operations
require persistent profiles. `Buddy` has `user_id`, `display_name`, and
`online` fields.

Incoming events are:

- `player.chat` with sender `user_id`, `display_name`, `message`, and `sent_at`.
- `buddy.requested` with requester identity.
- `buddy.accepted` with accepting identity.
- `buddy.status` with identity, `online`, and `status`.

## Stats and game results

Stats objects contain unsigned `wins`, `losses`, `disconnects`, and `games`,
plus signed `rating`. Persistent profiles start at rating zero.

| Command | Request `data` | Successful response `data` |
|---|---|---|
| `stats.get` | `{}` or `{"user_id":2}` | `{"user_id":2,"stats":PlayerStats}` |
| `stats.update` | `{"delta":PlayerStats}` | `{"stats":PlayerStats}` |
| `stats.results` | See below | `{"recorded":true}` |

`stats.update` updates the authenticated persistent profile only and limits
each counter delta to one and rating to `-100..100` per request. It exists for
legacy persistent-storage compatibility; operators should treat client-sent
stats as untrusted.

The original host—or a survivor after that host leaves—may report a started
game's results once:

```json
{
  "v":1,
  "type":"stats.results",
  "id":"r1",
  "data":{
    "game_id":"0123456789abcdef",
    "results":[
      {"user_id":1,"outcome":"win"},
      {"user_id":2,"outcome":"loss"}
    ]
  }
}
```

Outcomes are `win`, `loss`, or `disconnect`. The current implementation uses a
simple `+10/-10/-15` rating delta and is not an anti-cheat authority. Results
are accepted only when the game was created with `use_stats:true` and the
request contains every member from the immutable `game.go` launch roster
exactly once, including departed players as `disconnect`. The original host
reports while present; after that host departs, any survivor may report. The
update is persisted atomically before the game is marked reported, so a
storage error can be retried.

## Quickmatch

| Command | Request `data` | Successful response `data` |
|---|---|---|
| `quickmatch.enqueue` | `{"product":"zerohour","compatibility_version":1,"ini_crc":1865069505,"mode":"1v1"}` | Queued or matched result |
| `quickmatch.cancel` | `{}` | `{"cancelled":true}` |

The first player waits. A queued response echoes the immutable matching key:

```json
{"queued":true,"mode":"1v1","product":"zerohour","compatibility_version":1,"ini_crc":1865069505}
```

Only a second waiting player with the identical mode, product, compatibility
version, and INI CRC is paired into an unlisted two-player game. Re-enqueue is
idempotent and retains the original key until `quickmatch.cancel`. The server
immediately allocates and starts its relay because the retail Quick Match UI
does not send a separate start command. Both receive, in order:

```json
{"v":1,"type":"quickmatch.matched","data":{"game":GameSnapshot}}
```

and their individual `game.started` relay credential event.

No separate `game.start` command is sent for Quick Match. Its matched snapshot
starts in `starting`; both players still confirm `game.started` credentials
with `game.start_ready` and wait for `game.go`. Quickmatch currently matches
FIFO-style within one server process and does not rank by latency or rating.
Its `GameOptions.use_stats` is always false because the retail Quick Match path
does not exchange authoritative profile IDs before launch. If either
participant leaves, the unlisted match is deleted, the survivor receives
`game.ended`, and both are free to enqueue again.

## UDP relay protocol

The relay carries opaque Generals transport datagrams. It does not parse,
rewrite, acknowledge, reorder, or retransmit the game payload. Every UDP frame
has a fixed 32-byte header followed by at most 1,100 payload bytes:

| Offset | Size | Encoding | Field |
|---:|---:|---|---|
| 0 | 4 | bytes | ASCII magic `GXRL` |
| 4 | 1 | uint8 | protocol version `1` |
| 5 | 1 | uint8 | kind |
| 6 | 1 | uint8 | source slot |
| 7 | 1 | uint8 | destination slot; `255` means broadcast |
| 8 | 8 | big-endian uint64 | game ID (the numeric value of `game_id`) |
| 16 | 16 | bytes | participant relay token decoded from hex |
| 32 | 0..1100 | bytes | opaque game datagram |

Kinds:

| Value | Name | Direction | Payload |
|---:|---|---|---|
| 1 | Bind | client to server | empty |
| 2 | Data | client to server | opaque game datagram |
| 3 | Keepalive | client to server | empty |
| 4 | BindAck | server to client | empty |
| 5 | DataOut | server to client | opaque game datagram |
| 6 | Error | reserved | not currently emitted |

Client procedure:

1. Decode the 16-byte token and 8-byte game ID from `game.started`.
2. Send Bind with its assigned source slot and token to the advertised relay.
3. Wait for BindAck. The server returns the receiver's token in the header, so
   the client can authenticate the relay endpoint and allocation.
4. Wrap each complete native Generals UDP transport datagram in Data. Use a
   peer's slot for unicast or `255` to fan out to all other participants.
5. On DataOut, require the configured relay source address, kind 5, matching
   game ID, matching local token, and matching destination slot. Strip exactly
   32 bytes and inject the remaining datagram into the Online transport receive
   path with source identity set from the source slot.
6. Send Keepalive during idle periods. Any valid Bind, Keepalive, or Data frame
   refreshes the observed NAT endpoint; this supports NAT port rebinding.

The relay rejects an unknown game/token pair, a token used with the wrong
source slot, malformed/oversized packets, and participants exceeding the
configured packet or byte rate. Broadcast never echoes to the sender. Packets
for a peer that has not yet bound are held in a 32-packet per-recipient queue.
If that queue fills, the oldest packet is dropped and the new packet is kept;
the queue is flushed after a valid Bind or Keepalive. Idle game allocations
expire after 15 minutes by default.

The token authenticates routing but does not encrypt gameplay. Internet
deployments should firewall UDP to the relay port, monitor `/metrics`, and use
short idle timeouts appropriate to their player population.

## HTTP operations endpoints

- `GET /healthz` and `GET /readyz` return JSON with control/relay addresses,
  player/game counts, and relay counters.
- `GET /metrics` returns Prometheus text metrics.

The default HTTP listener is `:8080`; it is not part of the game client
protocol and should be firewalled from the public Internet or explicitly bound
to a private monitoring interface.
