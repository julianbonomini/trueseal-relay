package relay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/notify"
	"github.com/julianbonomini/trueseal-relay/internal/store"
)

// Router implements session.Handler.
// It wires Push Sessions to the InboxStore and delivers blobs to active
// Receive Sessions via the Notifier.
//
// Push path:  body[0:32] = recipient routing key; body[32:] = opaque envelope.
//
//	Envelope size is checked against maxEnvelopeBytes (ADR-0008).
//	Put to store → Notify → Ack.
//	Ack is sent only after Put succeeds (ADR-0008).
//
// Deliver path: on Receive Session open, deliver the existing Inbox (without
//
//	deleting), then on each Notifier signal deliver only Blobs not yet
//	delivered to THIS session (PeekAfter on the last delivered ID —
//	RELAY-7). Blobs are deleted only after the Device sends a DeliverAck
//	frame. If the session closes before the Ack arrives, the blob remains
//	in the store and is re-delivered on the next Receive Session.
//	See ADR-0009.
type Router struct {
	store            store.InboxStore
	notifier         notify.Notifier
	ttl              time.Duration
	maxEnvelopeBytes int64
}

// NewRouter returns a Router wired to store and notifier.
// ttl is applied to every Put — use relay.DefaultTTL for the standard 30-day window.
// maxEnvelopeBytes caps the envelope size (body[32:] of Push body); 0 means no limit.
func NewRouter(s store.InboxStore, n notify.Notifier, ttl time.Duration, maxEnvelopeBytes int64) *Router {
	return &Router{store: s, notifier: n, ttl: ttl, maxEnvelopeBytes: maxEnvelopeBytes}
}

// Push refusal sentinels. The session layer maps them onto Error frame
// reason codes with errors.Is (ADR-0008 amended; the ADR-0026 relay
// contract): ErrEnvelopeTooLarge → tooLarge (permanent),
// ErrPushBodyTooShort → malformed (permanent); anything else → internal
// (temporary).
var (
	ErrPushBodyTooShort = errors.New("relay: push body too short (want ≥ 32 bytes for recipient key)")
	ErrEnvelopeTooLarge = errors.New("relay: envelope too large")
	// ErrPushRecipientMalformed: the envelope's recipient_pub field is
	// missing, not exactly 32 bytes, duplicated or unparseable — the relay
	// cannot verify the addressee (RELAY-3; ADR-0031).
	ErrPushRecipientMalformed = errors.New("relay: envelope recipient_pub missing or malformed")
	// ErrRecipientMismatch: the push routing prefix and the envelope's own
	// recipient_pub disagree — the pusher is trying to file the blob in an
	// Inbox it isn't addressed to (RELAY-3; threat-model decision).
	ErrRecipientMismatch = errors.New("relay: push routing prefix does not match the envelope recipient")
)

// OnPush implements session.Handler.
// body = [recipient_pub: 32 bytes][envelope: protobuf bytes] (ADR-0008).
// Returns error if body < 32 bytes, envelope exceeds maxEnvelopeBytes, or Put fails (caller must not Ack).
func (r *Router) OnPush(ctx context.Context, body []byte) error {
	if len(body) < 32 {
		return ErrPushBodyTooShort
	}
	var key RecipientKey
	copy(key[:], body[:32])
	envelope := body[32:]

	if r.maxEnvelopeBytes > 0 && int64(len(envelope)) > r.maxEnvelopeBytes {
		return fmt.Errorf("%w: %d bytes (limit %d)", ErrEnvelopeTooLarge, len(envelope), r.maxEnvelopeBytes)
	}

	// RELAY-3 (#40, threat-model decision): the routing prefix must BE the
	// Envelope's own recipient_pub — the one field sync ADR-0031 leaves
	// Relay-readable. A pusher that labels a blob with one Inbox's prefix
	// while the Envelope addresses another is refused (permanent malformed)
	// and nothing is stored. The payload is never inspected.
	recipient, err := extractEnvelopeRecipient(envelope)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPushRecipientMalformed, err)
	}
	if recipient != key {
		return ErrRecipientMismatch
	}

	if err := r.store.Put(ctx, key[:], envelope, r.ttl); err != nil {
		return err
	}
	_ = r.notifier.Notify(key[:]) // best-effort; delivery goroutine handles the flush
	return nil
}

// OnDeliverAck implements session.Handler.
// Deletes the blob identified by blobID from the acking Device's own Inbox
// only — an ack naming an ID that is missing or belongs to another Device's
// Inbox is a silent no-op (RELAY-1; sync ADR-0026). See ADR-0009.
func (r *Router) OnDeliverAck(ctx context.Context, deviceKey RecipientKey, blobID int64) error {
	return r.store.DeleteForRecipient(ctx, deviceKey[:], []int64{blobID})
}

// OnReceiveConnect implements session.Handler.
// Registers the device, flushes any pending blobs, and starts a delivery
// goroutine. Returns a channel the session layer reads to write Deliver frames.
// Channel is closed when ctx is cancelled (device disconnects).
//
// Concurrent Receive Sessions for the same device key (issue #18):
// The relay allows multiple concurrent Receive Sessions for the same key.
// trueseal-sync opens exactly one Receive Session per TruesealSession and replaces
// it atomically on reconnect, so genuine concurrency is rare (brief overlap
// during reconnect only). Delivery is non-destructive (PeekAfter); blobs are
// deleted only after DeliverAck. With concurrent sessions both may deliver the
// same blob — deduplication is the client's responsibility. This behaviour is
// tested in TestRouter_TwoConcurrentReceiveSessions_PeekBothSeeBlob.
//
// A non-nil error means the device could not be registered — the session
// layer closes the connection so the client reconnects (RELAY-7).
func (r *Router) OnReceiveConnect(ctx context.Context, deviceKey RecipientKey) (<-chan DeliveryBlob, error) {
	// deliverCh buffers up to 256 blobs between the delivery goroutine and
	// the session write loop. 256 is a practical upper bound for a single peek
	// burst — large enough to absorb a full inbox drain without blocking the
	// goroutine, small enough that backpressure kicks in before memory grows
	// unbounded. If the session write loop falls behind, peekTo will block on
	// the channel send (ctx.Done() provides the escape hatch).
	deliverCh := make(chan DeliveryBlob, 256)

	subCh, err := r.notifier.Subscribe(ctx, deviceKey[:])
	if err != nil {
		close(deliverCh)
		return nil, fmt.Errorf("relay: subscribe device inbox: %w", err)
	}

	go func() {
		defer close(deliverCh)

		// lastID tracks the last Blob delivered to THIS session. The initial
		// flush covers the whole Inbox (afterID 0); each notify then delivers
		// only newer Blobs, so a Blob is sent once per Receive Session and
		// un-acked Blobs are re-sent only on a NEW session (RELAY-7; ADR-0009;
		// sync ADR-0026).
		lastID := r.deliverNew(ctx, deviceKey, 0, deliverCh)

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-subCh:
				if !ok {
					return
				}
				lastID = r.deliverNew(ctx, deviceKey, lastID, deliverCh)
			}
		}
	}()

	return deliverCh, nil
}

// deliverNew sends every Inbox blob for key with an ID greater than afterID to
// ch, in arrival order, and returns the ID of the last Blob sent (afterID if
// none). Blobs remain in the store until the device sends a DeliverAck.
// See ADR-0009.
func (r *Router) deliverNew(ctx context.Context, key RecipientKey, afterID int64, ch chan<- DeliveryBlob) int64 {
	blobs, err := r.store.PeekAfter(ctx, key[:], afterID)
	if err != nil {
		return afterID
	}
	for _, b := range blobs {
		select {
		case ch <- DeliveryBlob{BlobID: b.ID, Envelope: b.Envelope}:
			afterID = b.ID
		case <-ctx.Done():
			return afterID
		}
	}
	return afterID
}
