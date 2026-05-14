package store

import (
	"context"
	"time"
)

// InboxBlob is a blob fetched from the InboxStore via Peek.
// ID is an opaque store-assigned identifier used to delete the blob
// after the recipient acknowledges delivery. See ADR-0009.
type InboxBlob struct {
	ID       int64
	Envelope []byte
}

// InboxStore is the port for durable inbox storage.
// Implementations must be safe for concurrent use.
//
// The store operates on raw bytes — it has no knowledge of Envelope
// structure, recipient identity, or blob content. It routes by key only.
//
// See ADR-0003 (durable until delivery) and ADR-0007 (hexagonal architecture).
type InboxStore interface {
	// Put stores an envelope in the inbox for recipientKey.
	// The envelope is held durably until DeleteByIDs (after DeliverAck) or TTL expiry.
	// Must persist across crashes — returning nil guarantees the blob
	// survives a process restart.
	// Put does not deduplicate — if the same envelope is stored twice
	// (e.g. outbox replay after a crash), both copies are stored.
	// Deduplication is the recipient client's responsibility.
	Put(ctx context.Context, recipientKey []byte, envelope []byte, ttl time.Duration) error

	// Peek fetches all envelopes for recipientKey without deleting them.
	// Returns InboxBlob values carrying the store-assigned ID alongside content.
	// A subsequent call to DeleteByIDs with those IDs removes them.
	// Used to implement Ack-gated deletion — see ADR-0009.
	Peek(ctx context.Context, recipientKey []byte) ([]InboxBlob, error)

	// DeleteByIDs deletes blobs by their store-assigned IDs.
	// IDs that no longer exist (already reaped or deleted) are silently ignored.
	DeleteByIDs(ctx context.Context, ids []int64) error

	// Reap deletes all envelopes whose TTL has elapsed.
	// Reaping is policy, not data loss — see ADR-0003.
	// Called periodically by the TTL reaper goroutine.
	Reap(ctx context.Context) error
}
