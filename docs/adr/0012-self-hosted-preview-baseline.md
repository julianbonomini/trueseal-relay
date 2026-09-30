# Self-hosted preview baseline: abuse limits, private logs, no metrics

Status: accepted (decided 2026-09-29 in [trueseal-roadmap#11](https://github.com/julianbonomini/trueseal-roadmap/issues/11); the IP rule was tightened on 2026-09-30 in [trueseal-roadmap#26](https://github.com/julianbonomini/trueseal-roadmap/issues/26); not yet implemented; the TTL ceiling was lowered to 30 days on 2026-09-30 by trueseal-sync ADR-0034 in [trueseal-roadmap#30](https://github.com/julianbonomini/trueseal-roadmap/issues/30)). This supersedes the public-log rationale of ADR-0011. Its rule of never logging IPs stands. It also narrows ADR-0010 (Postgres/cluster) to experimental.

The developer preview supports exactly one self-hosted deployment: a single Node on SQLite, run from the repo's Docker compose file. That Node enforces abuse limits by default. In normal operation it emits nothing about clients, exposes no metrics, and keeps its logs private to the Operator.

We need this because the relay had no deadlines, quotas or rate limits. One idle socket could hold a connection slot forever. Its logs carried client IPs, key prefixes and per-push sizes, and they were published through an unauthenticated Dozzle viewer. A privacy stance that rests on "the logs prove the relay is blind" fails as soon as a log line leaks metadata. The simpler promise is that the relay emits no client metadata at all.

## Liveness and deadlines

- **The client starts heartbeats.** A Device sends a Heartbeat on its Receive Session every 25 s. The relay echoes it, and closes a Receive Session after 75 s with no frame received. This is a wire-behaviour change. The clean-break version policy (trueseal-sync ADR-0022) allows it.
- **Noise handshake deadline:** 10 s, for both listeners.
- **Push Session deadline:** 30 s in total, from accept to close.

## Abuse limits

Push Sessions are anonymous, so every limit is keyed on the recipient Inbox, the Device key, or a global count. **The relay never reads, stores or keys state on IP addresses.** Per-IP limiting belongs at the Operator's firewall or reverse proxy, and the docs say so.

| Limit | Default | Refusal when exceeded |
|---|---|---|
| Blobs per Inbox | 10,000 | `inbox full` (temporary) |
| Bytes per Inbox | 256 MiB | `inbox full` (temporary) |
| Push rate per recipient | token bucket: 20/s sustained, burst of 500 | `rate limited` (temporary) |
| Concurrent connections per listener | 1,000 | connection closed |
| Concurrent Receive Sessions per Device key | 4 | new session refused |
| Envelope size | Protocol Size Limit (trueseal-sync ADR-0025) | `too large` (permanent) |
| TTL | 30 days | Reaped |

Refusal codes and client backoff follow trueseal-sync ADR-0026. The Operator can change every value, but the envelope size limit and the TTL can only be lowered. **The relay refuses to start with a TTL longer than 30 days**, which is the client Replay Window (60 days) minus the client Outbox expiry (30 days). A message pushed on its last outbox day then still arrives inside the Replay Window, so an honest message is never rejected as too old while its sender believes it was delivered (trueseal-sync ADR-0034).

Accepted consequence: anyone who knows a Device's public key can use up that Device's quota and delay its delivery. This is a targeted availability attack. The preview threat model documents it; the relay does not prevent it.

## Logging

- **Normal mode** logs lifecycle events (startup, listener addresses, the relay public key, shutdown) and errors. A normal-mode log line never contains an IP address, a key or key prefix, a size, a Blob ID, a timestamp tied to an individual message, or any other per-client or per-message field.
- **Dev mode** is enabled with the `-dev` flag or `TRUESEAL_RELAY_DEV=1`. It prints a prominent warning at startup, and `/healthz` reports `dev_mode: true`. Dev mode may log key prefixes, sizes and Blob IDs. It never logs payload bytes or IP addresses.
- **No log viewer ships.** Dozzle, and the Caddy route in front of it, are removed. Logs are private to the Operator.

## IP addresses

**The relay never logs, stores or uses a client's IP address, in any mode.** No log line in normal or dev mode, no store row and no limit contains or depends on a peer address. The relay's own promise has no conditions attached, because a promise that only holds "in normal mode" breaks as soon as someone runs dev mode in production, and debugging the protocol never needs an IP.

What this does not cover: the host OS, the VPS provider and any firewall the Operator adds still see each TCP connection's source address. Hiding the IP from the host needs a network hop the Operator doesn't control (Tor or an independent proxy). That is out of scope for the preview; see the [IP-hiding research](https://github.com/julianbonomini/trueseal-roadmap/blob/research/relay-ip-hiding/research/relay-ip-hiding.md).

- **Release gate:** an e2e scenario runs the real relay in normal and dev mode through the receive, push, connection-limit, deadline and shutdown paths. It fails if any client address appears in the relay's log output or in `inbox.db`.
- **Public wording:** "The relay never logs, stores or uses your IP address. The server it runs on still sees the connection, as with any internet service. To hide your IP from the server too, use a VPN or Tor." Copy never says the relay "can't see" IPs.

## Health and metrics

- `/healthz` performs a cheap read against the store and returns 503 when the store is unusable.
- The health listener is used by a Docker `HEALTHCHECK` and is not published publicly. Caddy leaves the single-node compose file.
- **The preview has no metrics endpoint.** Aggregate metrics can be added later without breaking anything.

## Graceful shutdown

On SIGTERM:

1. Stop accepting connections on every listener.
2. Let pushes whose writes are already in progress commit and send their Ack, for at most 10 s.
3. Cancel every Receive Session. Un-acked Blobs stay in their Inbox and are re-delivered on reconnect. Client dedup absorbs the repeat.
4. Close the store.

A push cut off before its Ack is retried by the sender. That is ordinary at-least-once behaviour.

## Keypair setup

- On first start, the entrypoint generates `/data/keypair.hex` (mode 0600) and prints the public key. The doubled-binary-path bug that made `-genkey` a no-op under Docker is fixed.
- `trueseal-relay pubkey` prints the hex public key that clients configure, for example through `docker compose exec relay trueseal-relay pubkey`.
- `TRUESEAL_RELAY_KEYPAIR_HEX` remains for Operators who bring their own key.
- The preview does not support rotating the relay key. The docs state that changing the key breaks every client configuration. A combined relay address string is left to the SDK API shape decision.

## Backup and restore

- **The keypair is the critical asset.** Back it up once. Losing it means reconfiguring every client.
- **`inbox.db` is a buffer, not a record.** Losing it loses only undelivered messages, which senders will not resend because the relay already acked them. Backing it up is optional and uses SQLite's online `.backup`.
- **Restoring an older snapshot is safe.** Already-delivered Blobs come back and are delivered again. Clients drop them: a Blob still inside the Replay Window hits its dedup record, and an older one is rejected by the window (trueseal-sync ADR-0031, ADR-0034).

## Storage backends

SQLite is the only supported backend. The Postgres adapter, the cluster compose file and the HAProxy config stay in the tree as **experimental**. They are left out of getting-started docs, release gates and support claims, and the cluster files move under `experimental/`.

## Considered options

- **Per-IP rate limits inside the relay, held in memory.** Rejected. They would stop one abuser who uses many recipient keys, but the relay would then process client addresses, and "the relay never looks at IPs" is a simpler and more checkable promise.
- **Relay-initiated heartbeats.** Rejected. The client already owns reconnection, so it also owns liveness.
- **Aggregate Prometheus metrics.** Deferred. Adding them later breaks nothing.
- **Public, privacy-scrubbed logs as a trust signal (ADR-0011's rationale).** Rejected. Logs can't prove blindness, because a single leaky line undoes the claim and nobody can audit that a line was never written.
- **IP addresses in dev-mode logs.** Rejected on 2026-09-30. It would make the no-IP promise conditional, for no real debugging gain.
- **Hiding client IPs from the relay host in the preview (Tor, an SDK proxy option, or a third-party hop).** Deferred past the preview. The only hops that help are ones the Operator doesn't control. Tor is weak on iOS, a proxy option adds public SDK API and gate scope, and a default third-party hop is an external commitment.
- **Postgres as a supported single-node backend.** Rejected for the preview. It would double the gate matrix and adds nothing on a single Node.
