# hush-relay

A zero-knowledge message relay for [hush-sync](https://github.com/julianbonomini/hush-sync). Stores and forwards encrypted blobs between devices without ever learning their contents, sender identity, or group membership.

The relay sees one thing: a recipient public key. Everything else is opaque ciphertext.

---

## How it works

Two session types over TCP, both Noise-encrypted:

| Port | Session | Protocol | Direction |
|------|---------|----------|-----------|
| `7701` | Push | Noise NK | hush-sync → relay (anonymous sender) |
| `7700` | Receive | Noise XX | device → relay (long-lived, mutual auth) |

**Push path**: hush-sync opens a fresh anonymous NK session, sends one or more blobs with a 32-byte recipient key prefix. Relay stores the blob, sends Ack. Session closes.

**Deliver path**: device holds a long-lived XX session. On connect, inbox is flushed immediately. While connected, new blobs are delivered as they arrive. Relay sends periodic heartbeats to keep NAT alive.

Delivered blobs are deleted immediately. Undelivered blobs are kept until TTL (default 30 days).

---

## Quick start (Docker)

```sh
# Clone and start — keypair is generated automatically on first run
git clone https://github.com/julianbonomini/hush-relay
cd hush-relay
docker compose up -d

# Check the relay public key (share this with your clients)
docker compose logs relay | grep "relay public key"
```

That's it. The relay is running on ports `7700` (receive) and `7701` (push).

Data is persisted in a named Docker volume (`relay-data`). The keypair is generated once and stored at `/data/keypair.hex` inside the volume.

---

## Configuration

Configuration is via TOML file or environment variables. Environment variables override the TOML file. If no config file is found, env vars are used exclusively (useful for Docker).

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `HUSH_RELAY_KEYPAIR_PATH` | — | **Required.** Path to hex-encoded X25519 private key |
| `HUSH_RELAY_STORE_SQLITE_PATH` | — | **Required** (SQLite). Path to SQLite database file |
| `HUSH_RELAY_LISTEN_RECEIVE` | `:7700` | XX receive session listener |
| `HUSH_RELAY_LISTEN_PUSH` | `:7701` | NK push session listener |
| `HUSH_RELAY_LISTEN_HEALTH` | `:7702` | HTTP health endpoint (`GET /healthz → 200 OK`) |
| `HUSH_RELAY_STORE_TYPE` | `sqlite` | Storage adapter: `sqlite` or `postgres` |
| `HUSH_RELAY_MAX_ENVELOPE_BYTES` | `65482` | Max envelope bytes per Push frame (Noise u16 framing ceiling) |

### TOML file

```toml
[relay]
keypair_path   = "/data/keypair.hex"
listen_push    = ":7701"
listen_receive = ":7700"
listen_health  = ":7702"             # GET /healthz → 200 OK
ttl            = "720h"        # envelope TTL (default: 30 days)
reap_interval  = "1h"          # how often expired envelopes are reaped

[store]
type        = "sqlite"
sqlite_path = "/data/inbox.db"
```

Run with a config file:

```sh
hush-relay -config /path/to/relay.toml
```

A sample config is provided at [`config/relay.toml`](config/relay.toml).

---

## Keypair management

The relay's X25519 keypair authenticates it to devices (Noise NK/XX). Generate once:

```sh
# Docker
docker compose run --rm relay /hush-relay -genkey -keyout /data/keypair.hex

# Local binary
./hush-relay -genkey -keyout keypair.hex
```

The public key is printed during generation — share it with your clients.

**Keep the private key secret.** It lives in the Docker volume and is never baked into the image.

---

## Building from source

Requires Go 1.26+. Pure Go — no CGO, no external C dependencies.

```sh
# Build binary
make build

# Run tests
make test

# Build Docker image
make docker-build
```

---

## Deployment

### Single node (SQLite)

```sh
docker compose up -d
```

### Cluster (Postgres) — coming in #9

Multiple relay nodes sharing a Postgres inbox. Any node handles any request. No sticky sessions required. Invisible to hush-sync and devices.

---

## Security model

- **Blind by design**: recipient public key is unavoidable (it is the address). Sender identity, group membership, and blob contents are structurally unknowable — not policy-withheld.
- **Durable until delivered**: an accepted blob is never lost before delivery. WAL + `synchronous=FULL` on SQLite. Ack is sent only after the blob is persisted.
- **No relay is irreplaceable**: any instance can fail or be replaced. The security model does not change.

See [MANIFESTO.md](MANIFESTO.md) for the full design intent.
