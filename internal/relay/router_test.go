package relay_test

import (
	"context"
	"path/filepath"
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
	return relay.NewRouter(store, inprocess.New(), relay.DefaultTTL)
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

	// Give goroutine time to subscribe before pushing.
	time.Sleep(10 * time.Millisecond)

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
