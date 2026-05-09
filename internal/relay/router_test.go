package relay_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/julianbonomini/hush-relay/internal/notify/inprocess"
	"github.com/julianbonomini/hush-relay/internal/relay"
	sqlitestore "github.com/julianbonomini/hush-relay/internal/store/sqlite"
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

	// Verify via a receive connect — flush delivers the envelope.
	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := r.OnReceiveConnect(devCtx, recipientA)

	select {
	case got := <-ch:
		if string(got) != string(env) {
			t.Errorf("want %q, got %q", env, got)
		}
	case <-time.After(time.Second):
		t.Error("want envelope delivered, timed out")
	}
}

// Device connects with blobs already in store → all delivered immediately.
func TestRouter_ConnectFlushesExisting(t *testing.T) {
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
		t.Fatalf("want 3 envelopes, got %d", len(got))
	}
}

// Blob pushed while device is online → delivered via channel without reconnect.
func TestRouter_OnlineDelivery(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := r.OnReceiveConnect(devCtx, recipientA)

	// OnReceiveConnect registers the subscription synchronously before returning,
	// so no sleep is needed here — the notifier will buffer the signal if the
	// delivery goroutine hasn't started yet.

	r.OnPush(ctx, pushBody(make32(0xAA), []byte("live-blob"))) //nolint:errcheck

	select {
	case got := <-ch:
		if string(got) != "live-blob" {
			t.Errorf("want 'live-blob', got %q", got)
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
		if string(got) != "offline-blob" {
			t.Errorf("want 'offline-blob', got %q", got)
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

// ── T6: Concurrent sessions + reconnect ────────────────────────────────────

// Two concurrent Receive Sessions for the same device key must not
// double-deliver a blob. Flush is atomic (serialisable SQLite transaction),
// so only one session wins the Flush; the other sees an empty result.
func TestRouter_TwoConcurrentReceiveSessions_NoDoubleDelivery(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	// Store a blob before any session opens
	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("blob"))); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	ctx1, cancel1 := context.WithCancel(ctx)
	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel1()
	defer cancel2()

	ch1 := r.OnReceiveConnect(ctx1, recipientA)
	ch2 := r.OnReceiveConnect(ctx2, recipientA)

	// Drain both channels concurrently and collect all delivered blobs
	var mu sync.Mutex
	var got [][]byte
	var wg sync.WaitGroup

	drain := func(ch <-chan []byte, cancel context.CancelFunc) {
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
			case <-time.After(300 * time.Millisecond):
				// No more blobs within window — close this session
				cancel()
				return
			}
		}
	}

	wg.Add(2)
	go drain(ch1, cancel1)
	go drain(ch2, cancel2)
	wg.Wait()

	if len(got) != 1 {
		t.Errorf("want exactly 1 delivery across both sessions, got %d: %v", len(got), got)
	}
}

// Device connects, receives a blob, disconnects, then reconnects.
// The blob must not be delivered again (it was deleted by the first Flush).
func TestRouter_ReconnectNoDoubleDelivery(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()

	// First session
	ctx1, cancel1 := context.WithCancel(ctx)
	ch1 := r.OnReceiveConnect(ctx1, recipientA)

	// Push a blob
	if err := r.OnPush(ctx, pushBody(make32(0xAA), []byte("blob"))); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	// Receive on first session
	select {
	case blob := <-ch1:
		if string(blob) != "blob" {
			t.Errorf("want 'blob', got %q", blob)
		}
	case <-time.After(time.Second):
		t.Fatal("blob not received on first session")
	}

	// Disconnect first session
	cancel1()

	// Reconnect
	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	ch2 := r.OnReceiveConnect(ctx2, recipientA)

	// Must not receive the blob again — it was deleted by the first Flush
	select {
	case blob := <-ch2:
		t.Errorf("want no delivery on reconnect, got: %q", blob)
	case <-time.After(200 * time.Millisecond):
		// good — no double delivery
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func collectN(t *testing.T, ch <-chan []byte, n int, timeout time.Duration) [][]byte {
	t.Helper()
	var out [][]byte
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
	env := make([]byte, limit) // exactly at limit
	if err := r.OnPush(context.Background(), pushBody(make32(0xCC), env)); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

// Envelope one byte over the limit is rejected — no Ack.
func TestRouter_EnvelopeOverLimitRejected(t *testing.T) {
	const limit = 100
	r := newRouterWithLimit(t, limit)
	env := make([]byte, limit+1) // one over
	if err := r.OnPush(context.Background(), pushBody(make32(0xCC), env)); err == nil {
		t.Error("want error for oversized envelope, got nil")
	}
}

// Oversized blob is not persisted — a subsequent Receive Session gets nothing.
func TestRouter_OversizedEnvelopeNotStored(t *testing.T) {
	const limit = 100
	r := newRouterWithLimit(t, limit)
	env := make([]byte, limit+1)
	_ = r.OnPush(context.Background(), pushBody(make32(0xCC), env)) // expect error

	devCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := r.OnReceiveConnect(devCtx, relay.RecipientKey(make32(0xCC)))

	select {
	case blob := <-ch:
		t.Errorf("want nothing stored, got blob len=%d", len(blob))
	case <-time.After(200 * time.Millisecond):
		// good
	}
}

// Zero limit (disabled) — arbitrarily large envelope is accepted.
func TestRouter_ZeroLimitDisabled(t *testing.T) {
	r := newRouterWithLimit(t, 0) // 0 = no limit
	env := make([]byte, 10*1024*1024) // 10 MiB
	if err := r.OnPush(context.Background(), pushBody(make32(0xDD), env)); err != nil {
		t.Errorf("want nil with limit=0, got %v", err)
	}
}
