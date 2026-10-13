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

One supported deployment: the shipped `docker-compose.yml` — a single Node on SQLite. It runs **only the relay**: no log viewer, no reverse proxy, no Docker socket mount, and an exact pinned image tag (never `:latest` — gate G8). Logs are private to you (ADR-0012); health stays on the container network, where Docker's own HEALTHCHECK uses it.

| File | Store | Nodes | Status |
|------|-------|-------|--------|
| `docker-compose.yml` | SQLite | 1 | **supported** — the preview deployment |
| `experimental/docker-compose.cluster.yml` | Postgres | 2+ | experimental (ADR-0012), not versioned, may change or go away |

**Health.** The image carries a Docker `HEALTHCHECK` against `/healthz` (port 7702, container-network only — never published). `/healthz` probes the inbox store: `200` with `{"status":"ok","dev_mode":false,"transport_version":1}` while the store answers, `503` when it is unreadable or stalls, so `docker ps` shows a Node with a broken `inbox.db` as unhealthy instead of green. `transport_version` is the Transport Version from the handshake prefix — the number the Compatibility Table lists.

---

### Local

**Single node:**
```sh
cp .env.example .env   # RELAY_IMAGE is already pinned to an exact version
mkdir -p /tmp/relay-dev && docker compose up --build   # local build; production pulls the pinned image
# relay:  localhost:7700 / 7701
# health: localhost:7702 (container network; add -p to publish for local poking)
```

**Cluster (experimental):**
```sh
docker compose -f experimental/docker-compose.cluster.yml up --build
# relay (via HAProxy): localhost:7700 / 7701
```

---

### Production (VPS)

**Prerequisites:** a VPS with Docker + the compose plugin, and ports `7700` and `7701` open. No domain, TLS or reverse proxy is required — devices speak Noise-over-TCP to the relay directly and authenticate it by its public key, not by a certificate.

```sh
# 1. Grab the compose file and env template at an exact release tag
V=0.2.0   # the version the Compatibility Table lists for your TrueSeal Release
curl -fsSL "https://raw.githubusercontent.com/julianbonomini/trueseal-relay/v${V}/docker-compose.yml" -o docker-compose.yml
curl -fsSL "https://raw.githubusercontent.com/julianbonomini/trueseal-relay/v${V}/.env.example" -o .env

# 2. RELAY_IMAGE is pre-pinned to that exact version in .env — verify it matches $V
$EDITOR .env

# 3. Start
docker compose up -d

# 4. Print what clients configure — distribute the Relay Address out-of-band
docker compose exec relay /trueseal-relay address --host yourdomain.com
```

Upgrading is: bump `RELAY_IMAGE` in `.env` to the next exact version, `docker compose pull relay && docker compose up -d`. See *Upgrading and the inbox store* below for what happens to `inbox.db`.

---

### Keypair

Auto-generated on first start (mode 0600, never overwritten), stored in the `relay-data` Docker volume. Print what clients configure:

```sh
# Public key
docker compose exec relay /trueseal-relay pubkey

# Relay Address
docker compose exec relay /trueseal-relay address --host yourdomain.com
```

To bring your own:

```sh
# Generate and print hex (create-exclusive — never overwrites an existing file).
# The entrypoint execs the binary, so 'genkey' is the binary's first argument.
docker compose run --rm relay genkey -keyout /data/keypair.hex

# Set in .env
TRUESEAL_RELAY_KEYPAIR_HEX=your_hex_key_here
```

---

### Upgrading and the inbox store

The relay's inbox store (`inbox.db`) carries a **Store Version** (ADR-0013), checked at every start. Upgrading is pull-and-restart:

- **Same or older store** — the relay starts. A store from before versioning is adopted as version 1 (data untouched); an older versioned store is migrated forward in one transaction, so a crash mid-migration leaves the old store intact and the next start retries.
- **Newer store** (you downgraded the image) — the relay refuses to start and leaves the store byte-for-byte unchanged:
  `inbox store version N is newer than this relay supports (store version M) — refusing to start, store left unchanged; upgrade the relay`.
  Run the newer image again; the store is fine.
- **Unmigratable store** — the relay refuses with a line naming both versions and leaves the store unchanged. Restore from the pre-upgrade state or wait for a Store Reset release.

**Store Reset releases.** Rarely, a release's notes declare a *relay Store Reset* — its format changed incompatibly. On such a release the relay refuses to start on an older store until the operator passes `-reset-store`, which **drops every inbox** (up to 30 days of undelivered blobs — senders will not resend) and keeps the keypair, so clients stay configured:

```sh
docker compose run --rm relay -reset-store
```

On any release that does *not* declare a Store Reset, `-reset-store` is refused before the store is touched. The startup log always names the running store version, e.g. `inbox store: sqlite, store version 1`.

The Postgres backend is experimental (ADR-0012); store versioning applies to the supported SQLite backend only.

---

### Logs are private

Relay logs are for you, the Operator, and nobody else (ADR-0012). Normal mode logs lifecycle events and field-free error classes — never an IP address, a key, a size or a per-message event. Verbose logging exists only as the explicit per-invocation dev mode (`-dev` / `TRUESEAL_RELAY_DEV=1`). There is no shipped log viewer, and the compose file exposes no log port. See *Logging modes* under Configuration.

---

### Releases and deploys

Relay releases are independent of the lockstep TrueSeal Release (their own version line, from `0.2.0`) and follow e2e ADR-0002: a candidate image is built from a commit by `candidate.yml` (multi-arch, provenance-attested, referenced by digest in the Release Manifest), the Release Conductor's gate tests exactly those bytes, and `promote.yml` then tags the **identical digest** `vX.Y.Z` with no rebuild.

**No release ever touches infrastructure.** Deploying is a separate, explicit, manual action — `deploy.yml` takes an exact published version (it refuses `latest`, verifies the image's build attestation, and pulls that tag on the VPS). Nothing triggers it from a tag. One-time provisioning, as repo secrets:

| Secret | Value |
|--------|-------|
| `VPS_HOST` | VPS IP or hostname |
| `VPS_USER` | SSH user |
| `DEPLOY_SSH_KEY` | Private half of a deploy SSH keypair |

Add the public half to `~/.ssh/authorized_keys` on the VPS, and keep `~/trueseal-relay/` on the VPS holding the shipped `docker-compose.yml` plus a `.env`.

---

### Clustered (Postgres, experimental)

Two relay Nodes sharing one Postgres inbox store, under `experimental/` per ADR-0012 (not a supported preview deployment; the Postgres store is unversioned). HAProxy load-balances raw TCP (ports 7700/7701) across Nodes with connection logging off (ADR-0011). Any Node handles any Push or Receive Session — no sticky sessions, no Node is load-bearing.

**Cross-node delivery:** when a Push Session lands on Node B for a device whose Receive Session is on Node A, Node B stores the blob in Postgres and sends a `NOTIFY`. Node A is `LISTEN`ing — it wakes up immediately and delivers. No polling, no delay.

**Node restart:** devices reconnect to any healthy Node; the initial inbox drain on reconnect recovers pending blobs. No data is lost.

Edit `experimental/docker-compose.cluster.yml` to change the Postgres password and add more Node entries as needed.

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
| `TRUESEAL_RELAY_LISTEN_HEALTH` | `:7702` | `GET /healthz → 200 {"status":"ok","dev_mode":false,"transport_version":1}` while the store answers; `503 {"status":"unavailable",...}` when it doesn't (ADR-0012) |
| `TRUESEAL_RELAY_TTL` | `720h` | undelivered blob retention; capped at 30 days — a higher value refuses startup (ADR-0034) |
| `TRUESEAL_RELAY_REAP_INTERVAL` | `1h` | |
| `TRUESEAL_RELAY_MAX_CONNECTIONS` | `1000` | per listener; excess connections rejected; explicit `0` = no limit |
| `TRUESEAL_RELAY_MAX_ENVELOPE_BYTES` | `65482` | Noise u16 framing ceiling — a higher value refuses startup (ADR-0025) |
| `TRUESEAL_RELAY_HANDSHAKE_DEADLINE` | `10s` | version-prefix read + Noise handshake (ADR-0012) |
| `TRUESEAL_RELAY_PUSH_SESSION_DEADLINE` | `30s` | total Push Session lifetime, accept to close |
| `TRUESEAL_RELAY_RECEIVE_IDLE_DEADLINE` | `75s` | Receive Session closed after this much silence; any inbound frame resets it |
| `TRUESEAL_RELAY_WRITE_DEADLINE` | `30s` | one outbound write; a stalled client is dropped |
| `TRUESEAL_RELAY_SHUTDOWN_DRAIN` | `10s` | graceful shutdown: in-flight pushes get at most this long to commit and Ack before being cut off (ADR-0012); raise `stop_grace_period` with it |
| `TRUESEAL_RELAY_MAX_BLOBS_PER_INBOX` | `10000` | per-Inbox blob quota; refusal `inboxFull` (temporary) |
| `TRUESEAL_RELAY_MAX_BYTES_PER_INBOX` | `268435456` | per-Inbox byte quota (256 MiB); refusal `inboxFull` (temporary) |
| `TRUESEAL_RELAY_PUSH_RATE_PER_SECOND` | `20` | per-recipient sustained push rate (token bucket) |
| `TRUESEAL_RELAY_PUSH_RATE_BURST` | `500` | per-recipient bucket capacity; refusal `rateLimited` (temporary) |
| `TRUESEAL_RELAY_MAX_RECEIVE_SESSIONS_PER_DEVICE` | `4` | concurrent Receive Sessions per Device key; excess refused |
| `TRUESEAL_RELAY_DEV` | off | dev logging mode (see below); `-dev` flag equivalent; never settable from TOML |

Abuse limits and deadlines follow ADR-0012: every limit is keyed on the recipient Inbox, the Device key or a global count — never on a peer address. All values accept short durations/counts so e2e can exercise them. No refusal is logged in normal mode.

### Logging modes

**Normal mode (default).** The relay logs lifecycle events (startup, listener addresses, its own public key, shutdown) and errors. A normal-mode log line never contains an IP address, a key or key prefix, a size, a Blob ID or any other per-client field (ADR-0012). Session errors are rate-limited to one field-free line per listener per minute, carrying a one-word class — `timeout`, `reset`, `closed` or `internal` — so a scanner cannot flood the log and an operator still gets signal. Refusals are silent.

**Dev mode (`-dev` or `TRUESEAL_RELAY_DEV=1`).** For debugging the protocol. It prints a prominent warning at startup, and `GET /healthz` reports `"dev_mode":true`. Dev mode adds per-message detail — device key prefixes, envelope sizes, Blob IDs, session lifecycle events — and surfaces the silent-refusal counters (rate limited, inbox full, too large, malformed, per-device session cap, connection limit) with running totals. Dev mode is never settable from the TOML config: verbose logging is always an explicit, per-invocation choice.

**In any mode, the relay never logs, stores or uses a client's IP address, and never logs payload bytes.** The promise has no conditions attached (ADR-0012 "IP addresses"), and it is verified by a test that runs a full session over real TCP in dev mode and fails if any peer address or payload byte reaches the log. Non-test code contains no `RemoteAddr()` call, enforced by a source-scanning test.

Every numeric knob is explicit: an unset knob takes its default, but an explicit zero or negative value refuses startup with a clear message (omit the setting to use the default). The two protocol ceilings — the 30-day TTL cap and the 65,482-byte envelope limit — may only be lowered; a higher value refuses startup (RELAY-8, sync ADR-0025/0034).

Full annotated example: [`config/example.toml`](config/example.toml) — every value is the default, pinned against the code by a test. Under docker compose, prefer the env-var path (`.env.example`); the image ships no config file.

### Hostile Relay (e2e test builds only)

The release gate (e2e ADR-0001, gate G3) needs a **Hostile Relay**: the real relay, plus injection hooks that make its delivery path misbehave on demand, so the harness can prove every SDK rejects hostile relay input with a typed error or delivery issue — and never acks or applies it.

Build it with the `hostile` Go build tag:

```sh
go build -tags hostile -o trueseal-relay-hostile ./cmd/trueseal-relay
./trueseal-relay-hostile -no-config -hostile-control 127.0.0.1:7703
```

`-hostile-control` (or `TRUESEAL_RELAY_HOSTILE_CONTROL`) starts a **loopback-only** control listener — the bind address must resolve to 127.0.0.1/::1 or startup is refused. The harness drives it with newline-delimited JSON, one command and one response (`{"ok":true}` or `{"ok":false,"error":"..."}`) per line:

| Command | Effect on the device's Receive Session |
|---|---|
| `{"cmd":"set","device":"*","drop":true}` | **drop** — withhold Deliver frames (blobs stay in the Inbox un-acked; clearing the rule and reconnecting re-delivers them) |
| `{"cmd":"set","device":"*","corrupt":true}` | **corrupt** — flip one byte in the envelope inside the Deliver frame (signature/AEAD must fail client-side) |
| `{"cmd":"set","device":"*","duplicate":true}` | **duplicate** — send every blob twice, same blob ID, same bytes |
| `{"cmd":"set","device":"*","reorder":2}` | **reorder** — hold a window of N blobs, emit in reverse arrival order |
| `{"cmd":"inject","device":"<64-hex>","envelope":"<hex>"}` | **inject** — deliver harness-supplied bytes (a fake Manifest or Sync from the Adversary Client; the relay cannot sign) under a synthetic blob ID (`≥ 2^62`); the client's ack of it is a silent store no-op |
| `{"cmd":"replay","device":"<64-hex>","count":1}` | **replay** — re-deliver the last N blobs of the session's history with their original blob IDs (`count:0` = whole history) |
| `{"cmd":"clear","device":"*"}` | resume honest delivery for the device |

`device` is a full device key (64 hex chars) or `"*"` for every device without an exact entry; an exact entry beats the wildcard. Rules apply in the fixed order drop → corrupt → duplicate → reorder. Inject and replay target one device's live sessions and fail cleanly if none is connected.

**A release binary cannot carry any of this.** The hook code and the control listener compile only under the tag; `SetDeliveryWrapper` exists only under the tag; release builds define no `-hostile-control` flag (passing it fails "flag provided but not defined"). CI proves it: it builds and tests both variants and asserts with `go tool nm` that no `internal/hostile` symbol reaches a normal build, and the Dockerfile fails its own build the same way. **The hostile binary is never published** — no release or candidate workflow builds with the tag, and no image ships it. It is a test tool for trueseal-e2e; even so, its logs follow the same privacy rules as every mode: no peer addresses, no payload bytes.

### Compose env vars (`.env`)

| Variable | Default | |
|----------|---------|--|
| `RELAY_IMAGE` | *(required, no default)* | Exact pinned image tag, e.g. `ghcr.io/julianbonomini/trueseal-relay:v0.2.0`. The compose file refuses to start without it; `:latest` is never a valid value (G8) |

---

## Keypair

The relay's X25519 keypair is used in both Noise handshakes. Devices verify it before exchanging any data — distribute the public key to clients out-of-band.

On first start the relay generates the keypair itself (mode 0600, create-exclusive — an existing key is never overwritten). Print what clients configure (inside Docker the binary lives at the absolute path `/trueseal-relay`):

```sh
# Docker
make pubkey                                    # public key
make address HOST=yourdomain.com               # Relay Address (sync ADR-0028)

# Local binary
./trueseal-relay pubkey
./trueseal-relay address --host yourdomain.com
```

To bring your own key instead:

```sh
# Generate once (refuses to overwrite an existing file)
./trueseal-relay genkey -keyout keypair.hex

# Then either point the relay at the file, or set TRUESEAL_RELAY_KEYPAIR_HEX
```

The public key is printed on first start and by `pubkey`. The private key lives in the Docker volume and is never baked into the image. Changing the key breaks every client configuration (ADR-0012), so back it up once.

Clustered deployments (`experimental/docker-compose.cluster.yml`, experimental per ADR-0012): the keypair auto-generates into the shared volume on first start; print it with `docker compose -f experimental/docker-compose.cluster.yml exec relay-1 /trueseal-relay pubkey`.

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
