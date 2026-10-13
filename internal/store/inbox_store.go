package store

import (
	"context"
	"errors"
	"time"
)

// ErrInboxFull is returned by PutWithLimits when the recipient's Inbox has
// reached a configured occupancy limit (blob count or total bytes). It is a
// TEMPORARY refusal (RELAY-6; ADR-0012): space frees up when blobs are
// delivered-and-acked or reaped, so the pusher may retry later.
var ErrInboxFull = errors.New("store: recipient inbox full")

// InboxLimits caps per-Inbox occupancy (ADR-0012 abuse limits). The zero
// value means no limit: MaxBlobs and MaxBytes of 0 disable their respective
// checks. Keyed on the recipient Inbox only — never on a peer address.
type InboxLimits struct {
	// MaxBlobs is the maximum number of undelivered blobs one Inbox holds.
	MaxBlobs int64
	// MaxBytes is the maximum total envelope bytes one Inbox holds.
	MaxBytes int64
}

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
	// The envelope is held durably until DeleteForRecipient (after DeliverAck)
	// or TTL expiry.
	// Must persist across crashes — returning nil guarantees the blob
	// survives a process restart.
	// Put does not deduplicate — if the same envelope is stored twice
	// (e.g. outbox replay after a crash), both copies are stored.
	// Transport deduplication is the recipient client's responsibility. Relay blob
	// IDs are not application message IDs and are unstable across duplicate puts.
	Put(ctx context.Context, recipientKey []byte, envelope []byte, ttl time.Duration) error

	// PutWithLimits is Put with an atomic occupancy check (RELAY-6;
	// ADR-0012): if storing envelope would exceed limits for recipientKey's
	// Inbox, nothing is stored and the error wraps ErrInboxFull. The check
	// and the insert happen in one transaction, so concurrent pushes cannot
	// both pass the check and overshoot the quota. A zero-value limits
	// disables the check (equivalent to Put).
	PutWithLimits(ctx context.Context, recipientKey []byte, envelope []byte, ttl time.Duration, limits InboxLimits) error

	// Peek fetches all envelopes for recipientKey without deleting them.
	// Returns InboxBlob values carrying the store-assigned ID alongside content.
	// A subsequent call to DeleteForRecipient with the same recipientKey and
	// those IDs removes them.
	// Used to implement Ack-gated deletion — see ADR-0009.
	Peek(ctx context.Context, recipientKey []byte) ([]InboxBlob, error)

	// PeekAfter fetches envelopes for recipientKey with store-assigned IDs
	// strictly greater than afterID, in arrival order, without deleting them.
	// A Receive Session passes the ID of the last Blob it delivered so that
	// each Blob is delivered once per session — un-acked Blobs are re-sent
	// only on a new Receive Session (RELAY-7; sync ADR-0026). afterID 0
	// returns the whole Inbox, equivalent to Peek.
	//
	// Implementations must return IDs in the same monotonic order they are
	// assigned: a Blob visible to one call must never be skipped by a later
	// call with a smaller afterID. SQLite's rowid ordering satisfies this.
	// The experimental Postgres backend allocates BIGSERIAL IDs out of
	// commit order, so two concurrent same-recipient Pushes can commit an
	// older-ID row after a newer one — that row is skipped for the rest of
	// the session's lifetime. No Blob is lost (it is delivered on the next
	// Receive Session), and SQLite (the supported backend, ADR-0012) is
	// unaffected.
	PeekAfter(ctx context.Context, recipientKey []byte, afterID int64) ([]InboxBlob, error)

	// DeleteForRecipient deletes blobs by their store-assigned IDs, but only
	// from the inbox owned by recipientKey. IDs that are missing, already
	// reaped or deleted, or belong to another inbox are silently ignored —
	// a DeliverAck can never delete a Blob from another Device's Inbox
	// (RELAY-1; sync ADR-0026). The caller learns nothing about whether an
	// ID exists.
	DeleteForRecipient(ctx context.Context, recipientKey []byte, ids []int64) error

	// Reap deletes all envelopes whose TTL has elapsed.
	// Reaping is policy, not data loss — see ADR-0003.
	// Called periodically by the TTL reaper goroutine.
	Reap(ctx context.Context) error

	// Ping is a cheap liveness read for the health check (RELAY-10;
	// ADR-0012 "Health and metrics"): it must prove the store can answer a
	// read right now, and nothing more. Callers bound it with their own
	// context timeout. Ping never reports client metadata.
	Ping(ctx context.Context) error
}
