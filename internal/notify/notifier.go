package notify

import "context"

// Notifier is the port for cross-node delivery notification.
// It decouples the relay routing loop from the storage layer —
// when a blob arrives for a device with an active Receive Session
// on this node, the Notifier wakes the delivery goroutine immediately.
//
// For single-node deployments, use the in-process adapter (no-op for
// cross-node — delivery is handled inline by the session registry).
// For clustered deployments, use the Postgres LISTEN/NOTIFY adapter.
//
// See ADR-0007 (hexagonal architecture).
type Notifier interface {
	// Notify signals that a new envelope has arrived for recipientKey.
	// Called by the push path after a successful InboxStore.Put.
	Notify(recipientKey []byte) error

	// Subscribe returns a channel that receives a signal (struct{}) each
	// time a new envelope arrives for recipientKey.
	// The channel is closed when ctx is cancelled — callers must cancel
	// the context when the Receive Session ends (TCP drop, clean close,
	// or relay shutdown). Use defer cancel() in the session handler.
	Subscribe(ctx context.Context, recipientKey []byte) (<-chan struct{}, error)
}
