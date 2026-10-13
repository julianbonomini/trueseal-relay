package relay_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

func newRouter(t *testing.T) *relay.Router {
	t.Helper()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return relay.NewRouter(store, inprocess.New(), relay.DefaultTTL, 0)
}

// protoEnvelope builds a well-formed proto Envelope addressed to
// recipient, carrying payload as the sealed payload field. The layout
// matches ADR-0031 / the sync vectors: recipient_pub is field 3 (32
// bytes), payload is field 6. Since RELAY-3 the router refuses envelopes
// whose recipient_pub is missing or doesn't match the routing prefix, so
// every test push goes through this helper.
func protoEnvelope(recipient [32]byte, payload []byte) []byte {
	env := []byte{0x1a, 0x20} // field 3, length 32
	env = append(env, recipient[:]...)
	env = append(env, 0x32) // field 6, length-delimited
	env = appendVarint(env, uint64(len(payload)))
	env = append(env, payload...)
	return env
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// pushBody builds a Push body [routing prefix: 32][proto Envelope] whose
// routing prefix matches the Envelope's recipient_pub — the only shape the
// router accepts since RELAY-3.
func pushBody(recipient [32]byte, payload []byte) []byte {
	envelope := protoEnvelope(recipient, payload)
	body := make([]byte, 32+len(envelope))
	copy(body[:32], recipient[:])
	copy(body[32:], envelope)
	return body
}


// pushBodySized builds a Push body whose proto Envelope is EXACTLY
// envelopeSize bytes — for the size-limit boundary tests, which measure
// the envelope check, not the payload.
func pushBodySized(recipient [32]byte, envelopeSize int) []byte {
	// Overhead: field 3 (tag 1 + len 1 + 32) + field 6 (tag 1) + payload
	// length varint. Solve for the payload length, accounting for the
	// varint's own size.
	payload := envelopeSize - 34 - 1
	// The payload length and its varint's own size must agree; normally a
	// fixed point exists within a couple of iterations, but cap the loop so
	// a pathological size (varint-boundary no-fixed-point) fails loudly
	// instead of spinning.
	for i := 0; i < 10; i++ {
		l := varintLen(uint64(payload))
		if payload+l+35 == envelopeSize {
			break
		}
		payload = envelopeSize - 35 - l
		if i == 9 {
			panic("pushBodySized: no fixed point for envelopeSize — not representable as a well-formed envelope")
		}
	}
	if payload < 0 {
		panic("envelopeSize too small for a well-formed envelope")
	}
	if got := 35 + varintLen(uint64(payload)) + payload; got != envelopeSize {
		panic("pushBodySized: no fixed point for envelopeSize — not representable as a well-formed envelope")
	}
	return pushBody(recipient, make([]byte, payload))
}

func varintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

var recipientA = relay.RecipientKey(make32(0xAA))
var recipientB = relay.RecipientKey(make32(0xBB))

func make32(b byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = b
	}
	return k
}

// OnPush with body shorter than 32 bytes returns an error.
func TestRouter_PushBodyTooShort(t *testing.T) {
	r := newRouter(t)
	err := r.OnPush(context.Background(), []byte("short"))
	if err == nil {
		t.Error("want error for short body, got nil")
	}
}

// OnPush stores body[32:] under body[0:32].
func TestRouter_PushStores(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	env := []byte("envelope-payload")
	if err := r.OnPush(ctx, pushBody(make32(0xAA), env)); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := r.OnReceiveConnect(devCtx, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	select {
	case got := <-ch:
		// The stored/forwarded bytes are exactly the proto Envelope pushed
		// (verbatim storage; pushBody wraps the payload in one).
		if want := string(protoEnvelope(make32(0xAA), env)); string(got.Envelope) != want {
			t.Errorf("want %q, got %q", want, got.Envelope)
		}
	case <-time.After(time.Second):
		t.Error("want envelope delivered, timed out")
	}
}

// Device connects with blobs already in store → all delivered immediately.
func TestRouter_ConnectPeeksExisting(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	r.OnPush(ctx, pushBody(make32(0xAA), []byte("one")))   //nolint:errcheck
	r.OnPush(ctx, pushBody(make32(0xAA), []byte("two")))   //nolint:errcheck
	r.OnPush(ctx, pushBody(make32(0xAA), []byte("three"))) //nolint:errcheck

	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := r.OnReceiveConnect(devCtx, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	got := collectN(t, ch, 3, time.Second)
	if len(got) != 3 {
		t.Fatalf("want 3 blobs, got %d", len(got))
	}
}

// Blob pushed while device is online → delivered via channel without reconnect.
func TestRouter_OnlineDelivery(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := r.OnReceiveConnect(devCtx, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	r.OnPush(ctx, pushBody(make32(0xAA), []byte("live-blob"))) //nolint:errcheck

	select {
	case got := <-ch:
		if want := string(protoEnvelope(make32(0xAA), []byte("live-blob"))); string(got.Envelope) != want {
			t.Errorf("want the wrapped live-blob envelope, got %q", got.Envelope)
		}
	case <-time.After(time.Second):
		t.Error("want delivery within 1s, timed out")
	}
}

// Blob pushed while device offline; device connects later → delivered on connect.
func TestRouter_OfflineThenConnect(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	r.OnPush(ctx, pushBody(make32(0xBB), []byte("offline-blob"))) //nolint:errcheck

	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := r.OnReceiveConnect(devCtx, recipientB)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	select {
	case got := <-ch:
		if want := string(protoEnvelope(make32(0xBB), []byte("offline-blob"))); string(got.Envelope) != want {
			t.Errorf("want the wrapped offline-blob envelope, got %q", got.Envelope)
		}
	case <-time.After(time.Second):
		t.Error("want delivery on connect, timed out")
	}
}

// Device disconnects (ctx cancel) → deliverCh closed.
func TestRouter_DisconnectClosesChannel(t *testing.T) {
	r := newRouter(t)

	devCtx, cancel := context.WithCancel(context.Background())
	ch, err := r.OnReceiveConnect(devCtx, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("want channel closed, got value")
		}
	case <-time.After(time.Second):
		t.Error("want channel closed within 1s")
	}
}

// ── T6: Concurrent sessions + ack-gated deletion ───────────────────────────

// Two concurrent Receive Sessions for the same device key may both deliver
// the blob — Peek does not delete, so both sessions see it. Deduplication is
// the client's responsibility (ADR-0009). Once either session acks the blob,
// subsequent Peeks return nothing.
func TestRouter_TwoConcurrentReceiveSessions_PeekBothSeeBlob(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("blob"))); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	ctx1, cancel1 := context.WithCancel(ctx)
	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel1()
	defer cancel2()

	ch1, err := r.OnReceiveConnect(ctx1, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}
	ch2, err := r.OnReceiveConnect(ctx2, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	var mu sync.Mutex
	var got []relay.DeliveryBlob
	var wg sync.WaitGroup

	drain := func(ch <-chan relay.DeliveryBlob, cancel context.CancelFunc) {
		defer wg.Done()
		for {
			select {
			case blob, ok := <-ch:
				if !ok {
					return
				}
				mu.Lock()
				got = append(got, blob)
				mu.Unlock()
				r.OnDeliverAck(ctx, recipientA, blob.BlobID) //nolint:errcheck
			case <-time.After(300 * time.Millisecond):
				cancel()
				return
			}
		}
	}

	wg.Add(2)
	go drain(ch1, cancel1)
	go drain(ch2, cancel2)
	wg.Wait()

	// At least one session delivered the blob; both may have (client deduplicates)
	if len(got) == 0 {
		t.Error("want at least 1 delivery, got 0")
	}
}

// RELAY-7: each Blob is delivered once per Receive Session. Two pushes after
// connect with no acks: the second notify must deliver ONLY the new Blob —
// un-acked Blobs re-deliver on a NEW Receive Session, not on every notify
// (ADR-0009; sync ADR-0026).
func TestRouter_NotifyDeliversEachBlobOncePerSession(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := r.OnReceiveConnect(devCtx, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	readBlob := func() string {
		t.Helper()
		select {
		case got := <-ch:
			return string(got.Envelope)
		case <-time.After(2 * time.Second):
			t.Fatal("want a delivered blob, timed out")
			return ""
		}
	}

	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("one"))); err != nil {
		t.Fatalf("OnPush one: %v", err)
	}
	if got := readBlob(); got != string(protoEnvelope(make32(0xAA), []byte("one"))) {
		t.Fatalf("want first delivery %q, got %q", "one", got)
	}

	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("two"))); err != nil {
		t.Fatalf("OnPush two: %v", err)
	}
	if got := readBlob(); got != string(protoEnvelope(make32(0xAA), []byte("two"))) {
		t.Fatalf("want second delivery %q, got %q — blob one must not be re-sent on this session", "two", got)
	}

	// Nothing more arrives on this session. The 300 ms negative window is
	// generous for slow CI: a duplicate from a whole-inbox re-flush is the
	// goroutine's immediate next action, so red stays deterministic.
	select {
	case extra := <-ch:
		t.Fatalf("want no further delivery on this session, got %q", extra.Envelope)
	case <-time.After(300 * time.Millisecond):
	}
}

// RELAY-7: arrival order is preserved across notifies (in-order delivery,
// sync ADR-0026).
func TestRouter_DeliveryOrderAcrossNotifies(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := r.OnReceiveConnect(devCtx, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	for _, payload := range []string{"first", "second", "third"} {
		if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte(payload))); err != nil {
			t.Fatalf("OnPush %s: %v", payload, err)
		}
		select {
		case got := <-ch:
			if want := string(protoEnvelope(make32(0xAA), []byte(payload))); string(got.Envelope) != want {
				t.Fatalf("want wrapped %q, got %q — arrival order broken", payload, got.Envelope)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("want %q delivered, timed out", payload)
		}
	}
}

// RELAY-1 regression: a DeliverAck from Device B for a Blob ID that lives in
// Device A's Inbox must be a silent no-op — the Blob stays in A's Inbox and is
// still delivered to A (issue #38; sync ADR-0026).
func TestRouter_DeliverAckFromForeignDevice_DoesNotDelete(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	// Device A is online and receives its blob, learning the store-assigned ID.
	ctxA, cancelA := context.WithCancel(ctx)
	chA, err := r.OnReceiveConnect(ctxA, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("for-A"))); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	var blobID int64
	select {
	case blob := <-chA:
		blobID = blob.BlobID
	case <-time.After(time.Second):
		t.Fatal("A did not receive its blob")
	}

	// Device B acks A's blob ID. Must not delete from A's Inbox.
	if err := r.OnDeliverAck(ctx, recipientB, blobID); err != nil {
		t.Fatalf("OnDeliverAck: %v", err)
	}

	// A disconnects without acking, then reconnects: the blob must be
	// re-delivered to A (proof it survived B's foreign ack).
	cancelA()
	ctxA2, cancelA2 := context.WithCancel(ctx)
	defer cancelA2()
	chA2, err := r.OnReceiveConnect(ctxA2, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	select {
	case blob := <-chA2:
		if string(blob.Envelope) != string(protoEnvelope(make32(0xAA), []byte("for-A"))) {
			t.Errorf("want 'for-A', got %q", blob.Envelope)
		}
	case <-time.After(time.Second):
		t.Fatal("blob missing from A's Inbox after a foreign DeliverAck — cross-inbox delete")
	}
}

// RELAY-1 companion: acking a blob ID that exists in NO inbox (guessed ID)
// is a silent no-op and returns no error — no oracle for blob existence.
func TestRouter_DeliverAckUnknownID_NoOp(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("for-A"))); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	if err := r.OnDeliverAck(ctx, recipientB, 424242); err != nil {
		t.Fatalf("OnDeliverAck with unknown ID returned error: %v", err)
	}
}

// Device connects, acks received blob, disconnects, then reconnects.
// The acked blob must not be re-delivered.
func TestRouter_ReconnectAfterAck_NoRedelivery(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	ctx1, cancel1 := context.WithCancel(ctx)
	ch1, err := r.OnReceiveConnect(ctx1, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("blob"))); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	var blobID int64
	select {
	case blob := <-ch1:
		if string(blob.Envelope) != string(protoEnvelope(make32(0xAA), []byte("blob"))) {
			t.Errorf("want 'blob', got %q", blob.Envelope)
		}
		blobID = blob.BlobID
	case <-time.After(time.Second):
		t.Fatal("blob not received on first session")
	}

	// Ack the blob — deletes it from store
	if err := r.OnDeliverAck(ctx, recipientA, blobID); err != nil {
		t.Fatalf("OnDeliverAck: %v", err)
	}
	cancel1()

	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	ch2, err := r.OnReceiveConnect(ctx2, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	select {
	case blob := <-ch2:
		t.Errorf("want no delivery on reconnect after ack, got: %q", blob.Envelope)
	case <-time.After(200 * time.Millisecond):
		// good — no re-delivery after ack
	}
}

// Device connects, receives blob, disconnects WITHOUT acking.
// On reconnect the blob must be re-delivered (ADR-0009).
func TestRouter_ReconnectWithoutAck_Redelivers(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("blob"))); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	// First session — receive but do NOT ack
	ctx1, cancel1 := context.WithCancel(ctx)
	ch1, err := r.OnReceiveConnect(ctx1, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}
	select {
	case <-ch1:
		// received but not acked
	case <-time.After(time.Second):
		t.Fatal("blob not received on first session")
	}
	cancel1() // disconnect without ack

	// Reconnect — blob must be re-delivered
	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	ch2, err := r.OnReceiveConnect(ctx2, recipientA)
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}
	select {
	case got := <-ch2:
		if want := string(protoEnvelope(make32(0xAA), []byte("blob"))); string(got.Envelope) != want {
			t.Errorf("want wrapped 'blob' on redelivery, got %q", got.Envelope)
		}
	case <-time.After(time.Second):
		t.Error("want blob re-delivered on reconnect, timed out")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func collectN(t *testing.T, ch <-chan relay.DeliveryBlob, n int, timeout time.Duration) []relay.DeliveryBlob {
	t.Helper()
	var out []relay.DeliveryBlob
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case v, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, v)
		case <-deadline:
			return out
		}
	}
	return out
}

// ── Envelope size limit (ADR-0008 / issue #16) ────────────────────────────────

func newRouterWithLimit(t *testing.T, maxBytes int64) *relay.Router {
	t.Helper()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "limit.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return relay.NewRouter(store, inprocess.New(), relay.DefaultTTL, maxBytes)
}

// Envelope exactly at the limit is accepted.
func TestRouter_EnvelopeAtLimitAccepted(t *testing.T) {
	const limit = 100
	r := newRouterWithLimit(t, limit)
	if err := r.OnPush(context.Background(), pushBodySized(make32(0xCC), limit)); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

// Envelope one byte over the limit is rejected — no Ack.
func TestRouter_EnvelopeOverLimitRejected(t *testing.T) {
	const limit = 100
	r := newRouterWithLimit(t, limit)
	if err := r.OnPush(context.Background(), pushBodySized(make32(0xCC), limit+1)); err == nil {
		t.Error("want error for oversized envelope, got nil")
	}
}

// Oversized blob is not persisted — a subsequent Receive Session gets nothing.
func TestRouter_OversizedEnvelopeNotStored(t *testing.T) {
	const limit = 100
	r := newRouterWithLimit(t, limit)
	_ = r.OnPush(context.Background(), pushBodySized(make32(0xCC), limit+1))

	devCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := r.OnReceiveConnect(devCtx, relay.RecipientKey(make32(0xCC)))
	if err != nil {
		t.Fatalf("OnReceiveConnect: %v", err)
	}

	select {
	case blob := <-ch:
		t.Errorf("want nothing stored, got blob len=%d", len(blob.Envelope))
	case <-time.After(200 * time.Millisecond):
		// good
	}
}

// RELAY-8: a zero limit no longer disables the cap — config validation owns
// operator intent now, and the Router defensively treats 0 (or negative) as
// the protocol ceiling rather than "no limit".
func TestRouter_ZeroLimitMeansProtocolCeiling(t *testing.T) {
	r := newRouterWithLimit(t, 0)
	if err := r.OnPush(context.Background(), pushBodySized(make32(0xDD), int(relay.ProtocolMaxEnvelopeBytes))); err != nil {
		t.Errorf("envelope exactly at the protocol ceiling must be accepted, got %v", err)
	}
	if err := r.OnPush(context.Background(), pushBodySized(make32(0xEE), int(relay.ProtocolMaxEnvelopeBytes)+1)); err == nil {
		t.Error("envelope one byte over the protocol ceiling must be refused with limit=0")
	}
}
