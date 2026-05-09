package relay

import (
	"context"
	"errors"
	"time"

	"github.com/julianbonomini/hush-relay/internal/notify"
	"github.com/julianbonomini/hush-relay/internal/store"
)

// Router implements session.Handler.
// It wires Push Sessions to the InboxStore and delivers blobs to active
// Receive Sessions via the Notifier.
//
// Push path:  body[0:32] = recipient routing key; body[32:] = opaque envelope.
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
	store    store.InboxStore
	notifier notify.Notifier
	ttl      time.Duration
}

// NewRouter returns a Router wired to store and notifier.
// ttl is applied to every Put — use relay.DefaultTTL for the standard 30-day window.
func NewRouter(s store.InboxStore, n notify.Notifier, ttl time.Duration) *Router {
	return &Router{store: s, notifier: n, ttl: ttl}
}

// OnPush implements session.Handler.
// body = [recipient_pub: 32 bytes][envelope: opaque bytes]
// Returns error if body < 32 bytes or Put fails (caller must not Ack).
func (r *Router) OnPush(ctx context.Context, body []byte) error {
	if len(body) < 32 {
		return errors.New("relay: push body too short (want ≥ 32 bytes for recipient key)")
	}
	var key RecipientKey
	copy(key[:], body[:32])
	envelope := body[32:]

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
func (r *Router) OnReceiveConnect(ctx context.Context, deviceKey RecipientKey) <-chan []byte {
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
