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

---

## Configuration

Env vars (used in Docker) or TOML file (pass with `-config`). Env vars override TOML.

| Variable | Default | |
|----------|---------|--|
| `TRUESEAL_RELAY_KEYPAIR_PATH` | — | **Required** |
| `TRUESEAL_RELAY_STORE_SQLITE_PATH` | — | **Required** (SQLite) |
| `TRUESEAL_RELAY_LISTEN_RECEIVE` | `:7700` | |
| `TRUESEAL_RELAY_LISTEN_PUSH` | `:7701` | |
| `TRUESEAL_RELAY_LISTEN_HEALTH` | `:7702` | `GET /healthz → 200 ok` |
| `TRUESEAL_RELAY_STORE_TYPE` | `sqlite` | `sqlite` or `postgres` |
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
# Docker
docker compose run --rm relay /trueseal-relay -genkey -keyout /data/keypair.hex

# Local binary
./trueseal-relay -genkey -keyout keypair.hex
```

The public key is printed on generation. The private key lives in the Docker volume and is never baked into the image.

---

## Building

Go 1.26+. Pure Go — no CGo, no external C dependencies.

```sh
make build        # → ./trueseal-relay
make test
make docker-build
```

---

## Clustering

Default deployment is single-node SQLite. A Postgres backend for multi-node clustering (shared inbox, no sticky sessions) is tracked in [#9](https://github.com/julianbonomini/trueseal-relay/issues/9).

---

## Design

- **Blind** — content, sender identity, group membership are structurally unknowable, not policy-withheld
- **Durable until delivered** — accepted blobs survive crashes (WAL + `synchronous=FULL`); Ack sent only after persistence
- **Replaceable** — no specific instance is load-bearing; swap or scale without changing the security model

→ [MANIFESTO.md](MANIFESTO.md) · [trueseal-protocol wire spec](https://github.com/julianbonomini/trueseal-protocol)
