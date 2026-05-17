# Postgres adapter for clustered inbox store and cross-node delivery notification

A single-node deployment with SQLite and an in-process notifier cannot scale horizontally: inbox blobs live on one Node, and delivery notifications never cross process boundaries. To support multi-Node clusters, both ports (InboxStore and Notifier) need shared, external backing. Postgres covers both with a single dependency.

## Decision

Add a `postgres` package implementing both `store.InboxStore` and `notify.Notifier` in one struct. All Nodes in a cluster share one Postgres instance. Store ops use `pgxpool`; cross-node delivery notification uses Postgres `LISTEN/NOTIFY` on a single dedicated `*pgx.Conn` per Node, demultiplexed in-process by recipient key.

**Channel naming:** `LISTEN/NOTIFY` channel name = `hex(recipientKey)` — a 64-character lowercase hex string, one channel per recipient key.

**Ref-counted LISTEN/UNLISTEN:** the notifier tracks active subscriber count per key. `LISTEN` is issued when count goes 0→1; `UNLISTEN` when count goes 1→0. Multiple concurrent Receive Sessions for the same key (allowed per ADR decision, issue #18) share a single `LISTEN`.

**Listener failure → crash:** if the dedicated listener connection drops, the Node calls `log.Fatalf`. The process manager (systemd, Docker) restarts the Node. Devices reconnect to any healthy Node; the initial Peek on reconnect drains any pending inbox blobs. Reconnect logic lives in the client library (trueseal-sync), not the relay.

**All Nodes reap:** every Node runs the TTL reaper independently. Duplicate `DELETE` on already-reaped rows is a no-op. No leader election required.

**Config:** `store.type = "postgres"` selects the adapter. `store.postgres_dsn` (or env `TRUESEAL_RELAY_STORE_POSTGRES_DSN`) provides the connection string. SQLite remains the default for single-node deployments — no external dependencies required unless the operator opts in.

**Schema:** identical to the SQLite schema in structure. `BIGSERIAL` primary key, `BYTEA` for recipient and envelope, `BIGINT` for `expires_at`. No additional columns — the relay retains only what routing requires.

## Considered options

**Redis** — covers the Notifier port cleanly (pub/sub built-in) but fights the InboxStore model. The inbox requires `Peek` (non-destructive read) and ack-gated deletion via `DeleteByIDs` — Redis has no native non-destructive queue semantics. Using Redis for notification only still requires a separate shared store, adding a second external dependency. Rejected.

**Message broker (RabbitMQ, NATS, Kafka)** — good at fan-out notification, wrong shape for durable keyed storage. Broker semantics assume destructive reads; the inbox requires re-delivery on reconnect without re-enqueue. NATS JetStream (KV + pub/sub) is the closest fit but adds more operational complexity than Postgres for most self-hosters. Rejected.

**Sticky sessions at the load balancer** — routes each device to a fixed Node, keeping session and inbox co-located. Rejected: node failure requires session reconstruction and the fixed routing makes no relay irreplaceable — violating the manifesto principle.

**Reconnect-in-place on listener failure** — Node re-establishes the listener connection, re-issues LISTEN for all active keys, then Peeks all active inboxes. Correct but complex: state machine, re-LISTEN loop, race window during recovery. Rejected in favour of crash-restart: client reconnect resilience is already built into trueseal-sync, and with multiple Nodes a restart is a non-event.

## Consequences

- SQLite stays the default. Operators who never need clustering run zero extra services.
- Migration from SQLite to Postgres is a planned operator action. Inbox blobs in the SQLite store are not migrated — they remain until TTL reap or delivery if the old Node is kept running during the transition. This is documented in the README.
- Node restart on listener failure is an operational expectation, not a bug. Process managers must be configured to restart the binary.
