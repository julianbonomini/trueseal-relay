# Hexagonal architecture with pluggable InboxStore and Notifier

The relay core is isolated from infrastructure via two ports (Go interfaces). Adapters implement the ports. The running configuration is selected at startup via config — the core never changes.

**Port 1 — InboxStore**
Pure durable storage. Responsible for: accepting blobs into an inbox, non-destructively reading blobs for delivery, deleting blobs by ID after DeliverAck, and reaping blobs whose TTL has elapsed.

Blobs are deleted only after the recipient Device sends a DeliverAck — not on read. This is ack-gated deletion (ADR-0009). `Peek` is non-destructive; `DeleteByIDs` is called by the router after each DeliverAck.

```go
type InboxStore interface {
    Put(ctx context.Context, recipientKey []byte, envelope []byte, ttl time.Duration) error
    Peek(ctx context.Context, recipientKey []byte) ([]InboxBlob, error) // non-destructive
    DeleteByIDs(ctx context.Context, ids []int64) error
    Reap(ctx context.Context) error
}
```

Note: an earlier version of this interface used `Flush` (atomic fetch+delete). That was superseded by ADR-0009, which introduced ack-gated deletion. `Flush` no longer exists in the port.

**Port 2 — Notifier**
Cross-node delivery notification. When a blob arrives at Node B for a recipient whose Receive Session is on Node A, Node B notifies Node A to deliver immediately. Single-node deployments use an in-process notifier with zero overhead.

```go
type Notifier interface {
    Notify(recipientKey []byte) error
    Subscribe(ctx context.Context, recipientKey []byte) (<-chan struct{}, error)
}
```

Subscriptions are ctx-driven: when the caller cancels the context (e.g. on TCP drop, clean session close, or relay shutdown), the Subscribe implementation closes the returned channel and releases all associated resources. There is no explicit Unsubscribe call — callers use `defer cancel()` in the session handler.

**Adapters shipped:**

| Adapter | Implements | Use case |
|---|---|---|
| SQLite | InboxStore | Single-node, zero deps, embedded |
| Postgres | InboxStore + Notifier | Clustered, shared store, LISTEN/NOTIFY |
| InProcess | Notifier | Single-node, in-memory channel |

**Deploy configurations:**

Single-node (default): SQLite InboxStore + InProcess Notifier. Zero external dependencies. Ships as a single binary or docker compose with no additional services.

Clustered: Postgres InboxStore + Postgres Notifier. All nodes share one Postgres instance. Any node handles any request. Node failure does not lose accepted blobs. Cross-node delivery notification uses Postgres LISTEN/NOTIFY — no additional pub/sub dependency.

The alternative — a single hardcoded implementation — was rejected because it forces operators to run Postgres even for simple self-hosted single-node deployments, conflicting with the operational simplicity principle. The alternative of sticky sessions at the load balancer was rejected because it undermines fault tolerance: node failure requires session reconstruction.

Both configurations are compiled into the single binary. The adapter is selected at startup via configuration.
