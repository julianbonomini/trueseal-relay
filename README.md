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

Two compose files — pick one:

| File | Store | Nodes | Use when |
|------|-------|-------|----------|
| `docker-compose.yml` | SQLite | 1 | default, single VPS |
| `docker-compose.cluster.yml` | Postgres | 2+ | you need horizontal scale |

Both include Dozzle (public log viewer) and Caddy (HTTPS). Caddy only starts in the `production` profile — locally services are reachable directly on `localhost`.

---

### Local (no domain, no TLS)

**Single node:**
```sh
cp .env.example .env   # fill in keypair if you have one; domains don't matter locally
docker compose up
# relay:  localhost:7700 / 7701
# health: localhost:7702
# logs:   localhost:8080
```

**Cluster:**
```sh
docker compose -f docker-compose.cluster.yml build --no-cache
docker compose -f docker-compose.cluster.yml up
# relay (via HAProxy): localhost:7700 / 7701
# health (via HAProxy): localhost:7702
# logs:  localhost:8080
```

---

### Production (VPS + domain + TLS)

**Prerequisites:**
- VPS with Docker + Docker Compose
- Two DNS A records → VPS IP **(Cloudflare: grey cloud / DNS-only — raw TCP ports 7700/7701 cannot be proxied)**:
  - `relay.yourdomain.com` — health endpoint (HTTPS via Caddy)
  - `logs.yourdomain.com` — public log viewer (HTTPS via Caddy)
- Ports open: `80`, `443`, `7700`, `7701`

**Single node:**
```sh
# 1. Grab compose file + env template
curl -fsSL https://github.com/julianbonomini/trueseal-relay/releases/latest/download/docker-compose.yml -o docker-compose.yml
curl -fsSL https://github.com/julianbonomini/trueseal-relay/releases/latest/download/Caddyfile -o Caddyfile
curl -fsSL https://github.com/julianbonomini/trueseal-relay/releases/latest/download/.env.example -o .env

# 2. Fill in RELAY_DOMAIN and LOGS_DOMAIN
$EDITOR .env

# 3. Start with Caddy
docker compose --profile production up -d

# 4. Get the relay public key — distribute to clients
docker compose logs relay | grep "public key"
```

**Cluster:**
```sh
curl -fsSL https://github.com/julianbonomini/trueseal-relay/releases/latest/download/docker-compose.cluster.yml -o docker-compose.cluster.yml
curl -fsSL https://github.com/julianbonomini/trueseal-relay/releases/latest/download/Caddyfile -o Caddyfile
curl -fsSL https://github.com/julianbonomini/trueseal-relay/releases/latest/download/docker/haproxy.cluster.cfg -o docker/haproxy.cluster.cfg
curl -fsSL https://github.com/julianbonomini/trueseal-relay/releases/latest/download/.env.example -o .env

# Set HEALTH_UPSTREAM=lb:7702 and your domains in .env
$EDITOR .env

docker compose -f docker-compose.cluster.yml --profile production up -d
```

Caddy fetches a Let's Encrypt TLS certificate automatically on first boot.

---

### Keypair

Auto-generated on first run, stored in the `relay-data` Docker volume. To bring your own:

```sh
# Generate and print hex
docker compose run --rm relay /trueseal-relay -genkey

# Set in .env
TRUESEAL_RELAY_KEYPAIR_HEX=your_hex_key_here
```

---

### Public logs

All relay logs are publicly readable at `https://logs.yourdomain.com` (Dozzle). This is intentional — logs contain no IP addresses, no sender identities, no content. Anyone can audit that the relay is blind.

---

### Automatic deploys

New releases deploy automatically on each `v*.*.*` git tag via GitHub Actions. Three secrets required in your fork:

| Secret | Value |
|--------|-------|
| `VPS_HOST` | VPS IP or hostname |
| `VPS_USER` | SSH user |
| `DEPLOY_SSH_KEY` | Private half of a deploy SSH keypair |

Add the public half to `~/.ssh/authorized_keys` on the VPS.

---

### Clustered (Postgres)

Two relay Nodes sharing one Postgres inbox store. HAProxy load-balances raw TCP (ports 7700/7701) across Nodes. Caddy handles HTTPS for health and logs. Any Node handles any Push or Receive Session — no sticky sessions, no Node is load-bearing.

**Cross-node delivery:** when a Push Session lands on Node B for a device whose Receive Session is on Node A, Node B stores the blob in Postgres and sends a `NOTIFY`. Node A is `LISTEN`ing — it wakes up immediately and delivers. No polling, no delay.

**Node restart:** devices reconnect to any healthy Node; the initial inbox drain on reconnect recovers pending blobs. No data is lost.

Edit `docker-compose.cluster.yml` to change the Postgres password and add more Node entries as needed.

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

### Relay env vars

Env vars (used in Docker) or TOML file (pass with `-config`). Env vars override TOML.

| Variable | Default | |
|----------|---------|--|
| `TRUESEAL_RELAY_KEYPAIR_PATH` | — | **Required** |
| `TRUESEAL_RELAY_KEYPAIR_HEX` | — | Optional. Supply hex key directly instead of file |
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

### Compose env vars (`.env`)

| Variable | Default | |
|----------|---------|--|
| `RELAY_DOMAIN` | — | **Required in prod.** Subdomain for health endpoint |
| `LOGS_DOMAIN` | — | **Required in prod.** Subdomain for Dozzle log viewer |
| `HEALTH_UPSTREAM` | `relay:7702` | `relay:7702` (single node) or `lb:7702` (cluster) |
| `RELAY_IMAGE` | `ghcr.io/julianbonomini/trueseal-relay:latest` | Override to pin a version |

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

→ [MANIFESTO.md](MANIFESTO.md) · [Protocol overview](https://trueseal.dev/docs/protocol/overview) · [trueseal-relay docs](https://trueseal.dev/docs/components/trueseal-relay/overview)
