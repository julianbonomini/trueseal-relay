package relay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/julianbonomini/hush-relay/internal/notify"
	"github.com/julianbonomini/hush-relay/internal/store"
)

// Router implements session.Handler.
// It wires Push Sessions to the InboxStore and delivers blobs to active
// Receive Sessions via the Notifier.
//
// Push path:  body[0:32] = recipient routing key; body[32:] = opaque envelope.
//             Envelope size is checked against maxEnvelopeBytes (ADR-0008).
//             Put to store → Notify → Ack.
//             Ack is sent only after Put succeeds (ADR-0008).
//
// Deliver path: on Receive Session open, Flush existing blobs, then range
//               Notifier signals → Flush → deliver. Always goes through the
//               store — delivery is decoupled from the Ack.
//
// Known limitation: if TCP drops after Flush but before the Deliver frame
// is written, those blobs are lost. Fixing this properly requires a
// Peek+delete-on-ack InboxStore operation (future work).
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

// OnPush implements session.Handler.
// body = [recipient_pub: 32 bytes][envelope: protobuf bytes] (ADR-0008).
// Returns error if body < 32 bytes, envelope exceeds maxEnvelopeBytes, or Put fails (caller must not Ack).
func (r *Router) OnPush(ctx context.Context, body []byte) error {
	if len(body) < 32 {
		return errors.New("relay: push body too short (want ≥ 32 bytes for recipient key)")
	}
	var key RecipientKey
	copy(key[:], body[:32])
	envelope := body[32:]

	if r.maxEnvelopeBytes > 0 && int64(len(envelope)) > r.maxEnvelopeBytes {
		return fmt.Errorf("relay: envelope too large: %d bytes (limit %d)", len(envelope), r.maxEnvelopeBytes)
	}

	if err := r.store.Put(ctx, key[:], envelope, r.ttl); err != nil {
		return err
	}
	_ = r.notifier.Notify(key[:]) // best-effort; delivery goroutine handles the flush
	return nil
}

// OnReceiveConnect implements session.Handler.
// Registers the device, flushes any pending blobs, and starts a delivery
// goroutine. Returns a channel the session layer reads to write Deliver frames.
// Channel is closed when ctx is cancelled (device disconnects).
//
// Concurrent Receive Sessions for the same device key (issue #18):
// The relay allows multiple concurrent Receive Sessions for the same key.
// hush-sync opens exactly one Receive Session per HushSession and replaces
// it atomically on reconnect, so genuine concurrency is rare (brief overlap
// during reconnect only). The relay relies on atomic Flush (fetch+delete in
// a single serialisable transaction) to prevent double-delivery: only one
// session wins the race; the other gets an empty result. This behaviour is
// tested in TestRouter_TwoConcurrentReceiveSessions_NoDoubleDelivery.
func (r *Router) OnReceiveConnect(ctx context.Context, deviceKey RecipientKey) <-chan []byte {
	// deliverCh buffers up to 256 envelopes between the delivery goroutine and
	// the session write loop. 256 is a practical upper bound for a single flush
	// burst — large enough to absorb a full inbox drain without blocking the
	// goroutine, small enough that backpressure kicks in before memory grows
	// unbounded. If the session write loop falls behind, flushTo will block on
	// the channel send (ctx.Done() provides the escape hatch).
	deliverCh := make(chan []byte, 256)

	subCh, err := r.notifier.Subscribe(ctx, deviceKey[:])
	if err != nil {
		close(deliverCh)
		return deliverCh
	}

	go func() {
		defer close(deliverCh)

		// Flush any blobs that arrived before this session opened.
		r.flushTo(ctx, deviceKey, deliverCh)

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-subCh:
				if !ok {
					return
				}
				r.flushTo(ctx, deviceKey, deliverCh)
			}
		}
	}()

	return deliverCh
}

// flushTo drains the inbox for deviceKey and sends each envelope to ch.
// Stops early if ctx is cancelled.
func (r *Router) flushTo(ctx context.Context, key RecipientKey, ch chan<- []byte) {
	envelopes, err := r.store.Flush(ctx, key[:])
	if err != nil {
		return
	}
	for _, env := range envelopes {
		select {
		case ch <- env:
		case <-ctx.Done():
			return
		}
	}
}
