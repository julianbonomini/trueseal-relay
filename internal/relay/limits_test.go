package relay_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

func newRouterWithLimits(t *testing.T, limits relay.Limits) *relay.Router {
	t.Helper()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return relay.NewRouterWithLimits(store, inprocess.New(), relay.DefaultTTL, 0, limits)
}

func recipient(b byte) [32]byte {
	var k [32]byte
	k[0] = b
	return k
}

// TestRouter_InboxQuotaBoundary pushes through the full OnPush path with a
// 2-blob quota (RELAY-6; ADR-0012): at the limit accepted, one over refused
// with the relay's typed, temporary ErrInboxFull.
func TestRouter_InboxQuotaBoundary(t *testing.T) {
	r := newRouterWithLimits(t, relay.Limits{MaxBlobsPerInbox: 2})
	ctx := context.Background()
	key := recipient(0x51)

	for i := 0; i < 2; i++ {
		if err := r.OnPush(ctx, pushBody(key, []byte{byte(i)})); err != nil {
			t.Fatalf("push %d at/under quota refused: %v", i+1, err)
		}
	}
	err := r.OnPush(ctx, pushBody(key, []byte{0x03}))
	if !errors.Is(err, relay.ErrInboxFull) {
		t.Fatalf("one over quota: want ErrInboxFull, got %v", err)
	}
}

// TestRouter_InboxBytesQuotaBoundary: same boundary on the byte quota.
func TestRouter_InboxBytesQuotaBoundary(t *testing.T) {
	r := newRouterWithLimits(t, relay.Limits{MaxBytesPerInbox: 64})
	ctx := context.Background()
	key := recipient(0x52)

	// An Envelope of EXACTLY 64 bytes fills the quota to the boundary.
	if err := r.OnPush(ctx, pushBodySized(key, 64)); err != nil {
		t.Fatalf("push filling the byte quota refused: %v", err)
	}
	if err := r.OnPush(ctx, pushBody(key, []byte{0x01})); !errors.Is(err, relay.ErrInboxFull) {
		t.Fatalf("one byte over quota: want ErrInboxFull, got %v", err)
	}
}

// TestRouter_RateLimitBoundary: burst 2 with a negligible sustained rate —
// two pushes pass, the third is refused with temporary ErrRateLimited.
func TestRouter_RateLimitBoundary(t *testing.T) {
	r := newRouterWithLimits(t, relay.Limits{PushRatePerSecond: 0.000001, PushRateBurst: 2})
	ctx := context.Background()
	key := recipient(0x53)

	for i := 0; i < 2; i++ {
		if err := r.OnPush(ctx, pushBody(key, []byte{byte(i)})); err != nil {
			t.Fatalf("push %d within burst refused: %v", i+1, err)
		}
	}
	if err := r.OnPush(ctx, pushBody(key, []byte{0x03})); !errors.Is(err, relay.ErrRateLimited) {
		t.Fatalf("over the burst: want ErrRateLimited, got %v", err)
	}
}

// TestRouter_RateLimitKeyedPerRecipient proves the bucket is keyed on the
// recipient Inbox (ADR-0012: never on a peer address — there is none here):
// flooding A's Inbox leaves B's rate untouched.
func TestRouter_RateLimitKeyedPerRecipient(t *testing.T) {
	r := newRouterWithLimits(t, relay.Limits{PushRatePerSecond: 0.000001, PushRateBurst: 1})
	ctx := context.Background()
	a, b := recipient(0x54), recipient(0x55)

	if err := r.OnPush(ctx, pushBody(a, []byte{1})); err != nil {
		t.Fatalf("A first push: %v", err)
	}
	if err := r.OnPush(ctx, pushBody(a, []byte{2})); !errors.Is(err, relay.ErrRateLimited) {
		t.Fatalf("A second push: want ErrRateLimited, got %v", err)
	}
	if err := r.OnPush(ctx, pushBody(b, []byte{1})); err != nil {
		t.Fatalf("B's rate limit is independent of A's flood: %v", err)
	}
}

// TestRouter_ReceiveSessionCapBoundary: at the cap the new session is
// refused with ErrTooManyReceiveSessions; after a session ends (channel
// close = full teardown) a new connect succeeds (ADR-0012).
func TestRouter_ReceiveSessionCapBoundary(t *testing.T) {
	r := newRouterWithLimits(t, relay.Limits{MaxReceiveSessionsPerDevice: 2})
	key := recipient(0x56)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ch1, err := r.OnReceiveConnect(ctx1, key)
	if err != nil {
		t.Fatalf("session 1: %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if _, err := r.OnReceiveConnect(ctx2, key); err != nil {
		t.Fatalf("session 2 at the cap: %v", err)
	}
	if _, err := r.OnReceiveConnect(context.Background(), key); !errors.Is(err, relay.ErrTooManyReceiveSessions) {
		t.Fatalf("session 3 over the cap: want ErrTooManyReceiveSessions, got %v", err)
	}

	// End session 1; the delivery channel closing is the teardown signal
	// (the slot release happens before the close).
	cancel1()
	for range ch1 {
	}
	if _, err := r.OnReceiveConnect(context.Background(), key); err != nil {
		t.Fatalf("connect after a slot freed: %v", err)
	}
}

// TestRouter_ReceiveSessionCapKeyedPerDevice: the cap is per Device key —
// one Device at its cap does not block another Device's first session.
func TestRouter_ReceiveSessionCapKeyedPerDevice(t *testing.T) {
	r := newRouterWithLimits(t, relay.Limits{MaxReceiveSessionsPerDevice: 1})
	a, b := recipient(0x57), recipient(0x58)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := r.OnReceiveConnect(ctx, a); err != nil {
		t.Fatalf("A session 1: %v", err)
	}
	if _, err := r.OnReceiveConnect(context.Background(), a); !errors.Is(err, relay.ErrTooManyReceiveSessions) {
		t.Fatalf("A session 2 over cap: want ErrTooManyReceiveSessions, got %v", err)
	}
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	if _, err := r.OnReceiveConnect(ctxB, b); err != nil {
		t.Fatalf("B's first session while A is at cap: %v", err)
	}
}

// TestRouter_LimitsDisabledByDefault: NewRouter (no limits) keeps the
// pre-RELAY-6 behaviour for existing callers — pushes and sessions are
// uncapped.
func TestRouter_LimitsDisabledByDefault(t *testing.T) {
	r := newRouter(t)
	ctx := context.Background()
	key := recipient(0x59)
	for i := 0; i < 20; i++ {
		if err := r.OnPush(ctx, pushBody(key, []byte{byte(i)})); err != nil {
			t.Fatalf("push %d with no limits: %v", i, err)
		}
	}
}

// TestRateLimiter_Refill is an internal-mechanics check surfaced through
// the public constructor's semantics: with a real sustained rate, tokens
// refill over time and a refused push succeeds later (temporary contract).
func TestRateLimiter_Refill(t *testing.T) {
	r := newRouterWithLimits(t, relay.Limits{PushRatePerSecond: 50, PushRateBurst: 1})
	ctx := context.Background()
	key := recipient(0x5A)

	if err := r.OnPush(ctx, pushBody(key, []byte{1})); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if err := r.OnPush(ctx, pushBody(key, []byte{2})); !errors.Is(err, relay.ErrRateLimited) {
		t.Fatalf("second push: want ErrRateLimited, got %v", err)
	}
	// 50 tokens/s ⇒ one token per 20 ms; wait well past the refill.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := r.OnPush(ctx, pushBody(key, []byte{3})); err == nil {
			return
		} else if !errors.Is(err, relay.ErrRateLimited) {
			t.Fatalf("unexpected error while waiting for refill: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("bucket never refilled — a rate refusal must be temporary")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
