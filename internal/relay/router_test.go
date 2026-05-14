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

func pushBody(recipient [32]byte, envelope []byte) []byte {
	body := make([]byte, 32+len(envelope))
	copy(body[:32], recipient[:])
	copy(body[32:], envelope)
	return body
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
	ch := r.OnReceiveConnect(devCtx, recipientA)

	select {
	case got := <-ch:
		if string(got.Envelope) != string(env) {
			t.Errorf("want %q, got %q", env, got.Envelope)
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
	ch := r.OnReceiveConnect(devCtx, recipientA)

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
	ch := r.OnReceiveConnect(devCtx, recipientA)

	r.OnPush(ctx, pushBody(make32(0xAA), []byte("live-blob"))) //nolint:errcheck

	select {
	case got := <-ch:
		if string(got.Envelope) != "live-blob" {
			t.Errorf("want 'live-blob', got %q", got.Envelope)
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
	ch := r.OnReceiveConnect(devCtx, recipientB)

	select {
	case got := <-ch:
		if string(got.Envelope) != "offline-blob" {
			t.Errorf("want 'offline-blob', got %q", got.Envelope)
		}
	case <-time.After(time.Second):
		t.Error("want delivery on connect, timed out")
	}
}

// Device disconnects (ctx cancel) → deliverCh closed.
func TestRouter_DisconnectClosesChannel(t *testing.T) {
	r := newRouter(t)

	devCtx, cancel := context.WithCancel(context.Background())
	ch := r.OnReceiveConnect(devCtx, recipientA)

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

	ch1 := r.OnReceiveConnect(ctx1, recipientA)
	ch2 := r.OnReceiveConnect(ctx2, recipientA)

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

// Device connects, acks received blob, disconnects, then reconnects.
// The acked blob must not be re-delivered.
func TestRouter_ReconnectAfterAck_NoRedelivery(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	ctx1, cancel1 := context.WithCancel(ctx)
	ch1 := r.OnReceiveConnect(ctx1, recipientA)

	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("blob"))); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	var blobID int64
	select {
	case blob := <-ch1:
		if string(blob.Envelope) != "blob" {
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
	ch2 := r.OnReceiveConnect(ctx2, recipientA)

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
	ch1 := r.OnReceiveConnect(ctx1, recipientA)
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
	ch2 := r.OnReceiveConnect(ctx2, recipientA)
	select {
	case got := <-ch2:
		if string(got.Envelope) != "blob" {
			t.Errorf("want 'blob' on redelivery, got %q", got.Envelope)
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
	env := make([]byte, limit)
	if err := r.OnPush(context.Background(), pushBody(make32(0xCC), env)); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

// Envelope one byte over the limit is rejected — no Ack.
func TestRouter_EnvelopeOverLimitRejected(t *testing.T) {
	const limit = 100
	r := newRouterWithLimit(t, limit)
	env := make([]byte, limit+1)
	if err := r.OnPush(context.Background(), pushBody(make32(0xCC), env)); err == nil {
		t.Error("want error for oversized envelope, got nil")
	}
}

// Oversized blob is not persisted — a subsequent Receive Session gets nothing.
func TestRouter_OversizedEnvelopeNotStored(t *testing.T) {
	const limit = 100
	r := newRouterWithLimit(t, limit)
	env := make([]byte, limit+1)
	_ = r.OnPush(context.Background(), pushBody(make32(0xCC), env))

	devCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := r.OnReceiveConnect(devCtx, relay.RecipientKey(make32(0xCC)))

	select {
	case blob := <-ch:
		t.Errorf("want nothing stored, got blob len=%d", len(blob.Envelope))
	case <-time.After(200 * time.Millisecond):
		// good
	}
}

// Zero limit (disabled) — arbitrarily large envelope is accepted.
func TestRouter_ZeroLimitDisabled(t *testing.T) {
	r := newRouterWithLimit(t, 0)
	env := make([]byte, 10*1024*1024) // 10 MiB
	if err := r.OnPush(context.Background(), pushBody(make32(0xDD), env)); err != nil {
		t.Errorf("want nil with limit=0, got %v", err)
	}
}
