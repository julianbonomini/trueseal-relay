# Hexagonal architecture with pluggable InboxStore and Notifier

The relay core is isolated from infrastructure via two ports (Go interfaces). Adapters implement the ports. The running configuration is selected at startup via config — the core never changes.

**Port 1 — InboxStore**
Pure durable storage. Responsible for: accepting blobs into an inbox, atomically flushing and deleting all blobs for a recipient on delivery, and reaping blobs whose TTL has elapsed.

```go
type InboxStore interface {
    Put(ctx context.Context, recipientKey []byte, env []byte, ttl time.Duration) error
    Flush(ctx context.Context, recipientKey []byte) ([][]byte, error) // atomic fetch+delete
    Reap(ctx context.Context) error
}
```

**Port 2 — Notifier**
Cross-node delivery notification. When a blob arrives at Node B for a recipient whose Receive Session is on Node A, Node B notifies Node A to deliver immediately. Single-node deployments use an in-process notifier with zero overhead.

```go
type Notifier interface {
    Notify(ctx context.Context, recipientKey []byte) error
    Subscribe(ctx context.Context, recipientKey []byte) (<-chan struct{}, error)
    Unsubscribe(ctx context.Context, recipientKey []byte) error
}
```

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
