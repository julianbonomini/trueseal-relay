package store

import (
	"context"
	"time"
)

// InboxStore is the port for durable inbox storage.
// Implementations must be safe for concurrent use.
//
// The store operates on raw bytes — it has no knowledge of Envelope
// structure, recipient identity, or blob content. It routes by key only.
//
// See ADR-0003 (durable until delivery) and ADR-0007 (hexagonal architecture).
type InboxStore interface {
	// Put stores an envelope in the inbox for recipientKey.
	// The envelope is held durably until Flush or TTL expiry.
	// Must persist across crashes — returning nil guarantees the blob
	// survives a process restart.
	// Put does not deduplicate — if the same envelope is stored twice
	// (e.g. outbox replay after a crash), both copies are stored.
	// Deduplication is the recipient client's responsibility.
	Put(ctx context.Context, recipientKey []byte, envelope []byte, ttl time.Duration) error

	// Flush atomically fetches and deletes all envelopes for recipientKey.
	// The fetch and delete are a single transaction — no envelope is
	// returned twice, and no returned envelope remains in the store.
	Flush(ctx context.Context, recipientKey []byte) ([][]byte, error)

	// Reap deletes all envelopes whose TTL has elapsed.
	// Reaping is policy, not data loss — see ADR-0003.
	// Called periodically by the TTL reaper goroutine.
	Reap(ctx context.Context) error
}
