# trueseal-relay

Encrypted blob relay for [trueseal-sync](https://github.com/julianbonomini/trueseal-sync). Accepts ciphertext from senders, holds it for offline recipients, delivers on reconnect. Routes by recipient public key only — content, sender identity, and group membership are structurally unknowable.

Implements [trueseal-protocol](https://github.com/julianbonomini/trueseal-protocol) on the server side.

---

## Sessions

Two TCP listeners, both Noise-encrypted:

| Port | Noise pattern | Direction | Lifetime |
|------|--------------|-----------|----------|
| `7700` | XX — mutual auth | device ↔ relay | long-lived, one per device |
| `7701` | NK — relay-only auth | trueseal-sync → relay | short-lived, one per push batch |

NK means the sender is anonymous — the relay cannot link a push session to any device or receive session. XX means both sides authenticate; the relay registers the device's stable public key and delivers its inbox immediately on connect.

---

## Deploying

### Prerequisites

- A VPS with Docker + Docker Compose installed
- Two DNS A records pointing to the VPS IP **(Cloudflare: grey cloud / DNS-only — raw TCP ports 7700/7701 cannot be proxied)**:
  - `relay.yourdomain.com` — health endpoint (HTTPS via Caddy)
  - `logs.yourdomain.com` — public log viewer (Dozzle)
- Ports open on VPS: `80`, `443`, `7700`, `7701`

### First run

```sh
# 1. Get the compose file and example env
curl -fsSL https://github.com/julianbonomini/trueseal-relay/releases/latest/download/docker-compose.yml -o docker-compose.yml
curl -fsSL https://github.com/julianbonomini/trueseal-relay/releases/latest/download/.env.example -o .env

# 2. Fill in your two domain names
#    RELAY_DOMAIN=relay.yourdomain.com
#    LOGS_DOMAIN=logs.yourdomain.com
$EDITOR .env

# 3. Start — keypair auto-generates on first boot
docker compose up -d

# 4. Get the relay public key — distribute this to your clients
docker compose logs relay | grep "public key"
```

Caddy fetches a Let's Encrypt TLS certificate automatically on first boot.

### Keypair

A keypair is generated on first run and stored in the `relay-data` Docker volume. To bring your own (e.g. to restore a known public key after redeployment):

```sh
# Generate a keypair and print the hex
docker compose run --rm relay /trueseal-relay -genkey

# Set it in .env
TRUESEAL_RELAY_KEYPAIR_HEX=your_hex_key_here
```

### Public logs

The relay exposes all logs publicly at `https://logs.yourdomain.com` (Dozzle). This is intentional — logs contain no IP addresses, no sender identities, no content. Anyone can audit that the relay is behaving as promised.

### Automatic deploys

New releases are deployed automatically via GitHub Actions on each `v*.*.*` tag. The VPS needs three GitHub secrets:

| Secret | Value |
|--------|-------|
| `VPS_HOST` | VPS IP or hostname |
| `VPS_USER` | SSH user |
| `DEPLOY_SSH_KEY` | Private half of a deploy SSH keypair |

Add the public half to `~/.ssh/authorized_keys` on the VPS.

### Clustered (Postgres)

Multiple Nodes sharing one Postgres inbox store. Any Node handles any Push or Receive Session — no sticky sessions, no Node is load-bearing. A Caddy load balancer distributes connections.

```sh
# Generate a keypair once (shared across all Nodes via the relay-data volume)
docker compose -f docker-compose.cluster.yml run --rm relay-1 \
  /trueseal-relay -genkey -keyout /data/keypair.hex

# Start: two relay Nodes + Postgres + Caddy load balancer
docker compose -f docker-compose.cluster.yml up -d
```

Edit `docker-compose.cluster.yml` to change the Postgres password and add more Node entries as needed.

**Cross-node delivery:** when a Push Session lands on Node B for a device whose Receive Session is on Node A, Node B stores the blob in Postgres and sends a `NOTIFY`. Node A is `LISTEN`ing on the recipient's channel — it wakes up immediately and delivers the blob. No polling, no delay.

**Node restart:** if a Node crashes (including listener connection loss — see ADR-0010), it restarts automatically. Devices reconnect to any healthy Node; the initial Inbox drain on reconnect recovers any pending blobs. No data is lost.

---

## Migrating from single-node to clustered

There is no automated migration tool. Migrating is a planned operator action:

1. Keep the existing single-node relay running until it drains.
2. Stand up the clustered deployment pointing at the new Postgres store.
3. Cut over DNS / load balancer to point clients at the new cluster.
4. Devices reconnect. Blobs still in the old SQLite inbox will be delivered if the old relay remains reachable, or reaped by TTL if it is shut down.

Inbox blobs are ephemeral by design — TTL is 30 days by default, but in practice devices reconnect within minutes. A brief delivery gap during cutover is acceptable.

---

## Configuration

Env vars (used in Docker) or TOML file (pass with `-config`). Env vars override TOML.

| Variable | Default | |
|----------|---------|--|
| `TRUESEAL_RELAY_KEYPAIR_PATH` | — | **Required** |
| `TRUESEAL_RELAY_STORE_TYPE` | `sqlite` | `sqlite` or `postgres` |
| `TRUESEAL_RELAY_STORE_SQLITE_PATH` | — | **Required** when `store.type = sqlite` |
| `TRUESEAL_RELAY_STORE_POSTGRES_DSN` | — | **Required** when `store.type = postgres` |
| `TRUESEAL_RELAY_LISTEN_RECEIVE` | `:7700` | |
| `TRUESEAL_RELAY_LISTEN_PUSH` | `:7701` | |
| `TRUESEAL_RELAY_LISTEN_HEALTH` | `:7702` | `GET /healthz → 200 ok` |
| `TRUESEAL_RELAY_TTL` | `720h` | undelivered blob retention |
| `TRUESEAL_RELAY_REAP_INTERVAL` | `1h` | |
| `TRUESEAL_RELAY_MAX_CONNECTIONS` | `1000` | per listener; excess connections rejected |
| `TRUESEAL_RELAY_MAX_ENVELOPE_BYTES` | `65482` | Noise u16 framing ceiling |

Full annotated example: [`config/relay.toml`](config/relay.toml).

To use a config file instead of env vars:

```sh
trueseal-relay -config /path/to/relay.toml
```

---

## Keypair

The relay's X25519 keypair is used in both Noise handshakes. Devices verify it before exchanging any data — distribute the public key to clients out-of-band. Generate once at deployment setup:

```sh
# Docker (single-node)
docker compose run --rm relay /trueseal-relay -genkey -keyout /data/keypair.hex

# Docker (clustered) — generates into the shared relay-data volume
docker compose -f docker-compose.cluster.yml run --rm relay-1 \
  /trueseal-relay -genkey -keyout /data/keypair.hex

# Local binary
./trueseal-relay -genkey -keyout keypair.hex
```

The public key is printed on generation. The private key lives in the Docker volume and is never baked into the image. In a clustered deployment all Nodes share the same keypair file via a common volume.

---

## Building

Go 1.26+. Pure Go — no CGo, no external C dependencies.

```sh
make build        # → ./trueseal-relay
make test
make docker-build
```

Integration tests for the Postgres adapter require a reachable Postgres instance:

```sh
TEST_POSTGRES_DSN="postgres://relay:relay@localhost:5432/trueseal_test?sslmode=disable" \
  go test ./internal/store/postgres/
```

Tests are skipped automatically when `TEST_POSTGRES_DSN` is not set.

---

## Design

- **Blind** — content, sender identity, group membership are structurally unknowable, not policy-withheld
- **Durable until delivered** — accepted blobs survive crashes; Ack sent only after persistence (ADR-0009)
- **Replaceable** — no specific Node is load-bearing; swap or scale without changing the security model (ADR-0010)

→ [MANIFESTO.md](MANIFESTO.md) · [trueseal-protocol wire spec](https://github.com/julianbonomini/trueseal-protocol)
