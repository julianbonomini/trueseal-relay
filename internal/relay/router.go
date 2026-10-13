package relay

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/logging"
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

	// RELAY-6 (ADR-0012) abuse limits. nil limiter / cap disables them.
	inboxLimits store.InboxLimits
	rateLimiter *rateLimiter
	sessionCap  *sessionCap

	// RELAY-9 (ADR-0012 "Logging"): the single logging seam. Lifecycle and
	// field-free errors in every mode; per-message detail (key prefixes,
	// sizes, Blob IDs) in dev mode only. Never an address, never payload
	// bytes, in any mode.
	logger *logging.Logger

	// refusals counts silent refusals per class (RELAY-6 made refusals
	// log-free in normal mode; RELAY-9's dev mode surfaces them, with the
	// running total attached).
	refusals refusalCounters

	// wrapper, when non-nil, wraps each Receive Session's delivery channel
	// (RELAY-14, the Hostile Relay). It is set ONLY in builds with the
	// `hostile` build tag — release builds have no way to install one: the
	// setter exists only in wrap_hostile.go, and wrapDelivery (paired
	// files) is an inlined passthrough without the tag.
	//
	// STANDING RULE: never add an untagged setter for this field, and never
	// widen the tag on wrap_hostile.go. The release-safety property
	// (a shipped binary cannot be enrolled into hostile behaviour) rests
	// entirely on the setter's absence from !hostile builds — CI's nm
	// check and the Dockerfile's unstripped-intermediate check both assume
	// it. A future legitimate knob belongs behind its own build tag, with
	// the same paired-file pattern.
	wrapper DeliveryWrapper
}

// DeliveryWrapper transforms one Receive Session's stream of DeliveryBlobs
// between the routing loop and the Deliver frame writer (RELAY-14). The
// only implementation is the hostile engine, compiled behind the `hostile`
// build tag; release builds never contain one.
type DeliveryWrapper interface {
	// WrapSession returns the channel the session layer reads. in is
	// closed by the routing loop at teardown; implementations must
	// propagate that close (after flushing any held blobs) so the
	// session's teardown signal is preserved.
	WrapSession(ctx context.Context, key RecipientKey, in <-chan DeliveryBlob) <-chan DeliveryBlob
}

// refusalCounters are the Router's per-class silent-refusal totals.
type refusalCounters struct {
	tooLarge    atomic.Int64
	malformed   atomic.Int64
	rateLimited atomic.Int64
	inboxFull   atomic.Int64
	sessionCap  atomic.Int64
}

// Limits carries the ADR-0012 abuse limits a Router enforces (RELAY-6).
// Every value is operator-configurable; the zero value disables its limit.
type Limits struct {
	// MaxBlobsPerInbox and MaxBytesPerInbox cap per-Inbox occupancy
	// (refusal: inboxFull, temporary).
	MaxBlobsPerInbox int64
	MaxBytesPerInbox int64
	// PushRatePerSecond and PushRateBurst are the per-recipient token
	// bucket (refusal: rateLimited, temporary).
	PushRatePerSecond float64
	PushRateBurst     float64
	// MaxReceiveSessionsPerDevice caps concurrent Receive Sessions per
	// Device key (refusal: the new session is closed).
	MaxReceiveSessionsPerDevice int
}

// NewRouter returns a Router wired to store and notifier.
// ttl is applied to every Put — use relay.DefaultTTL for the standard 30-day window.
// maxEnvelopeBytes caps the envelope size (body[32:] of Push body). RELAY-8:
// there is no "0 means no limit" — operator intent is validated in config
// before this constructor runs, and a non-positive value here defensively
// means the protocol ceiling, never an uncapped router.
// It enforces no abuse limits; production wiring uses NewRouterWithLimits.
func NewRouter(s store.InboxStore, n notify.Notifier, ttl time.Duration, maxEnvelopeBytes int64) *Router {
	return NewRouterWithLimits(s, n, ttl, maxEnvelopeBytes, Limits{})
}

// NewRouterWithLimits is NewRouter plus the ADR-0012 abuse limits
// (RELAY-6). Every limit is keyed on the recipient Inbox, the Device key,
// or a global count — never on a peer address, which the relay never
// reads, stores or uses (ADR-0012; trueseal-roadmap#26).
func NewRouterWithLimits(s store.InboxStore, n notify.Notifier, ttl time.Duration, maxEnvelopeBytes int64, limits Limits) *Router {
	if maxEnvelopeBytes <= 0 {
		maxEnvelopeBytes = ProtocolMaxEnvelopeBytes
	}
	return &Router{
		store:            s,
		notifier:         n,
		ttl:              ttl,
		maxEnvelopeBytes: maxEnvelopeBytes,
		inboxLimits: store.InboxLimits{
			MaxBlobs: limits.MaxBlobsPerInbox,
			MaxBytes: limits.MaxBytesPerInbox,
		},
		rateLimiter: newRateLimiter(limits.PushRatePerSecond, limits.PushRateBurst, nil),
		sessionCap:  newSessionCap(limits.MaxReceiveSessionsPerDevice),
		logger:      logging.Nop(),
	}
}

// SetLogger routes the Router's logging (RELAY-9). nil restores the
// default no-op logger. Called once from main before listeners start.
func (r *Router) SetLogger(l *logging.Logger) {
	if l == nil {
		l = logging.Nop()
	}
	r.logger = l
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
		n := r.refusals.malformed.Add(1)
		r.logger.Debugf("push refused: malformed (body too short; total %d)", n)
		return ErrPushBodyTooShort
	}
	var key RecipientKey
	copy(key[:], body[:32])
	envelope := body[32:]

	if int64(len(envelope)) > r.maxEnvelopeBytes {
		n := r.refusals.tooLarge.Add(1)
		r.logger.Debugf("push refused: too large recipient=%x envelope=%d bytes (limit %d; total %d)", key[:4], len(envelope), r.maxEnvelopeBytes, n)
		return fmt.Errorf("%w: %d bytes (limit %d)", ErrEnvelopeTooLarge, len(envelope), r.maxEnvelopeBytes)
	}

	// RELAY-3 (#40, threat-model decision): the routing prefix must BE the
	// Envelope's own recipient_pub — the one field sync ADR-0031 leaves
	// Relay-readable. A pusher that labels a blob with one Inbox's prefix
	// while the Envelope addresses another is refused (permanent malformed)
	// and nothing is stored. The payload is never inspected.
	recipient, err := extractEnvelopeRecipient(envelope)
	if err != nil {
		n := r.refusals.malformed.Add(1)
		r.logger.Debugf("push refused: malformed recipient field recipient=%x (total %d)", key[:4], n)
		return fmt.Errorf("%w: %v", ErrPushRecipientMalformed, err)
	}
	if recipient != key {
		n := r.refusals.malformed.Add(1)
		r.logger.Debugf("push refused: routing prefix != envelope recipient recipient=%x (total %d)", key[:4], n)
		return ErrRecipientMismatch
	}

	// RELAY-6: the per-recipient token bucket is checked BEFORE the store —
	// a flood must not reach the store at all. Refusal is temporary
	// (rateLimited): the bucket refills.
	if !r.rateLimiter.allow(key) {
		n := r.refusals.rateLimited.Add(1)
		r.logger.Debugf("push refused: rate limited recipient=%x (total %d)", key[:4], n)
		return ErrRateLimited
	}

	// RELAY-6: the quota check is atomic with the insert inside the store —
	// concurrent pushes cannot overshoot. ErrInboxFull (temporary) is
	// translated to the relay's refusal sentinel for the session layer.
	if err := r.store.PutWithLimits(ctx, key[:], envelope, r.ttl, r.inboxLimits); err != nil {
		if errors.Is(err, store.ErrInboxFull) {
			n := r.refusals.inboxFull.Add(1)
			r.logger.Debugf("push refused: inbox full recipient=%x (total %d)", key[:4], n)
			return fmt.Errorf("%w: %v", ErrInboxFull, err)
		}
		return err
	}
	_ = r.notifier.Notify(key[:]) // best-effort; delivery goroutine handles the flush
	r.logger.Debugf("push: recipient=%x envelope=%d bytes", key[:4], len(envelope))
	return nil
}

// OnDeliverAck implements session.Handler.
// Deletes the blob identified by blobID from the acking Device's own Inbox
// only — an ack naming an ID that is missing or belongs to another Device's
// Inbox is a silent no-op (RELAY-1; sync ADR-0026). See ADR-0009.
func (r *Router) OnDeliverAck(ctx context.Context, deviceKey RecipientKey, blobID int64) error {
	r.logger.Debugf("ack: device=%x blob_id=%d", deviceKey[:4], blobID)
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
	// unbounded. If the session write loop falls behind, deliverNew will
	// block on the channel send (ctx.Done() provides the escape hatch).
	deliverCh := make(chan DeliveryBlob, 256)

	// RELAY-6: concurrent Receive Sessions per Device key are capped
	// (ADR-0012). The slot is taken under one lock (no split-brain count)
	// and released exactly once when the delivery goroutine exits — the
	// channel close afterwards is the test-visible teardown signal. A
	// refused session returns an error, and the session layer closes the
	// connection so the client reconnects (RELAY-7).
	if !r.sessionCap.acquire(deviceKey) {
		close(deliverCh)
		n := r.refusals.sessionCap.Add(1)
		r.logger.Debugf("recv: session refused, per-device cap reached key=%x (total %d)", deviceKey[:4], n)
		return nil, ErrTooManyReceiveSessions
	}
	r.logger.Debugf("recv: device connected key=%x", deviceKey[:4])

	subCh, err := r.notifier.Subscribe(ctx, deviceKey[:])
	if err != nil {
		r.sessionCap.release(deviceKey)
		close(deliverCh)
		return nil, fmt.Errorf("relay: subscribe device inbox: %w", err)
	}

	go func() {
		defer func() {
			r.sessionCap.release(deviceKey)
			close(deliverCh) // close LAST: signals full teardown (slot released)
			r.logger.Debugf("recv: device disconnected key=%x", deviceKey[:4])
		}()

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

	// RELAY-14: in `hostile` tag builds an installed wrapper transforms the
	// stream (drop/duplicate/reorder/corrupt and the inject/replay
	// surface). Without the tag this is an inlined passthrough.
	return r.wrapDelivery(ctx, deviceKey, deliverCh), nil
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
