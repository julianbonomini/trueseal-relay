package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	pgstore "github.com/julianbonomini/trueseal-relay/internal/store/postgres"
)

// testDSN returns the Postgres DSN from TEST_POSTGRES_DSN env var.
// Tests are skipped if the var is not set.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping Postgres integration tests")
	}
	return dsn
}

func newTestStore(t *testing.T) *pgstore.Store {
	t.Helper()
	ctx := context.Background()
	s, err := pgstore.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() {
		s.Truncate(ctx) //nolint:errcheck
		s.Close()
	})
	s.Truncate(ctx) //nolint:errcheck — clear any leftover rows from a prior run
	return s
}

func make32(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

var (
	keyA = make32(0xAA)
	keyB = make32(0xBB)
)

// ── Put + Peek ────────────────────────────────────────────────────────────────

// Put stores an envelope; Peek retrieves it without deleting.
func TestPut_Peek_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	env := []byte("hello-envelope")
	if err := s.Put(ctx, keyA, env, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}

	blobs, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(blobs) != 1 || string(blobs[0].Envelope) != string(env) {
		t.Errorf("want [%q], got %v", env, blobs)
	}
}

// Peek is non-destructive — second call returns same blobs.
func TestPeek_NonDestructive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("one"), time.Hour) //nolint:errcheck
	s.Put(ctx, keyA, []byte("two"), time.Hour) //nolint:errcheck

	first, _ := s.Peek(ctx, keyA)
	second, _ := s.Peek(ctx, keyA)
	if len(first) != 2 || len(second) != 2 {
		t.Errorf("want 2 blobs both peeks, got %d and %d", len(first), len(second))
	}
}

// Peek returns blobs in FIFO insertion order.
func TestPeek_FIFOOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, env := range []string{"one", "two", "three"} {
		s.Put(ctx, keyA, []byte(env), time.Hour) //nolint:errcheck
	}

	blobs, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	for i, want := range []string{"one", "two", "three"} {
		if string(blobs[i].Envelope) != want {
			t.Errorf("blobs[%d] = %q, want %q", i, blobs[i].Envelope, want)
		}
	}
}

// PeekAfter returns only blobs with IDs strictly greater than afterID, in
// arrival order — the basis for once-per-Receive-Session delivery (RELAY-7).
func TestPeekAfter_ReturnsOnlyNewerInOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, env := range []string{"one", "two", "three"} {
		s.Put(ctx, keyA, []byte(env), time.Hour) //nolint:errcheck
	}

	all, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 blobs, got %d", len(all))
	}

	newer, err := s.PeekAfter(ctx, keyA, all[0].ID)
	if err != nil {
		t.Fatalf("PeekAfter: %v", err)
	}
	if len(newer) != 2 || string(newer[0].Envelope) != "two" || string(newer[1].Envelope) != "three" {
		t.Errorf("want [two three] after first ID, got %v", newer)
	}

	none, err := s.PeekAfter(ctx, keyA, all[2].ID)
	if err != nil {
		t.Fatalf("PeekAfter last ID: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("want no blobs after last ID, got %v", none)
	}

	fromZero, err := s.PeekAfter(ctx, keyA, 0)
	if err != nil {
		t.Fatalf("PeekAfter 0: %v", err)
	}
	if len(fromZero) != 3 {
		t.Errorf("want 3 blobs from afterID 0, got %d", len(fromZero))
	}
}

// Peek on empty inbox returns empty slice without error.
func TestPeek_EmptyInbox(t *testing.T) {
	s := newTestStore(t)
	blobs, err := s.Peek(context.Background(), keyA)
	if err != nil {
		t.Fatalf("Peek on empty inbox: %v", err)
	}
	if len(blobs) != 0 {
		t.Errorf("want 0 blobs, got %d", len(blobs))
	}
}

// Peek isolates recipients — keyA and keyB do not bleed into each other.
func TestPeek_IsolatesRecipients(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("for-a"), time.Hour) //nolint:errcheck
	s.Put(ctx, keyB, []byte("for-b"), time.Hour) //nolint:errcheck

	blobsA, _ := s.Peek(ctx, keyA)
	blobsB, _ := s.Peek(ctx, keyB)

	if len(blobsA) != 1 || string(blobsA[0].Envelope) != "for-a" {
		t.Errorf("keyA: want [for-a], got %v", blobsA)
	}
	if len(blobsB) != 1 || string(blobsB[0].Envelope) != "for-b" {
		t.Errorf("keyB: want [for-b], got %v", blobsB)
	}
}

// Put rejects keys that are not exactly 32 bytes.
func TestPut_RejectsWrongKeyLength(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(context.Background(), []byte("too-short"), []byte("env"), time.Hour)
	if err == nil {
		t.Error("want error for short recipient key, got nil")
	}
}

// ── DeleteForRecipient ───────────────────────────────────────────────────────

// DeleteForRecipient removes only the specified blobs from that recipient's inbox.
func TestDeleteForRecipient_RemovesSpecified(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("one"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("two"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("three"), time.Hour) //nolint:errcheck

	blobs, _ := s.Peek(ctx, keyA)
	if err := s.DeleteForRecipient(ctx, keyA, []int64{blobs[0].ID, blobs[1].ID}); err != nil {
		t.Fatalf("DeleteForRecipient: %v", err)
	}

	remaining, _ := s.Peek(ctx, keyA)
	if len(remaining) != 1 || string(remaining[0].Envelope) != "three" {
		t.Errorf("want [three], got %v", remaining)
	}
}

// DeleteForRecipient with non-existent IDs is a no-op.
func TestDeleteForRecipient_NonExistentIDs(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteForRecipient(context.Background(), keyA, []int64{9999999, 8888888}); err != nil {
		t.Errorf("want no error for non-existent IDs, got %v", err)
	}
}

// RELAY-1 regression (issue #38): deleting under the WRONG recipient key must
// be a silent no-op — the blob stays in its real owner's inbox.
func TestDeleteForRecipient_ForeignIDsAreNoOp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("for-A"), time.Hour) //nolint:errcheck

	blobs, err := s.Peek(ctx, keyA)
	if err != nil || len(blobs) != 1 {
		t.Fatalf("Peek: %v, %d blobs", err, len(blobs))
	}

	if err := s.DeleteForRecipient(ctx, keyB, []int64{blobs[0].ID}); err != nil {
		t.Fatalf("DeleteForRecipient: %v", err)
	}

	remaining, _ := s.Peek(ctx, keyA)
	if len(remaining) != 1 || string(remaining[0].Envelope) != "for-A" {
		t.Errorf("want A's blob intact after foreign delete, got %v", remaining)
	}
}

// DeleteForRecipient with empty slice is a no-op.
func TestDeleteForRecipient_Empty(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteForRecipient(context.Background(), keyA, nil); err != nil {
		t.Errorf("want no error for nil IDs, got %v", err)
	}
}

// ── Reap ─────────────────────────────────────────────────────────────────────

// Reap removes expired blobs and leaves unexpired ones.
func TestReap_DeletesExpiredOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("expired"), -time.Second) //nolint:errcheck
	s.Put(ctx, keyA, []byte("live"), time.Hour)       //nolint:errcheck

	if err := s.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	blobs, _ := s.Peek(ctx, keyA)
	if len(blobs) != 1 || string(blobs[0].Envelope) != "live" {
		t.Errorf("want [live] after reap, got %v", blobs)
	}
}

// ── Notify + Subscribe ───────────────────────────────────────────────────────

// Notify wakes up a Subscribe listener.
func TestNotify_WakesSubscriber(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()

	ch, err := s.Subscribe(subCtx, keyA)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := s.Notify(keyA); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for notification")
	}
}

// Subscribe ctx cancel closes the channel.
func TestSubscribe_CtxCancelClosesChannel(t *testing.T) {
	s := newTestStore(t)

	subCtx, subCancel := context.WithCancel(context.Background())
	ch, err := s.Subscribe(subCtx, keyA)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	subCancel()

	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed — expected
			}
			// drain any notifications that arrived before cancel
		case <-timeout:
			t.Fatal("timed out waiting for channel to close after ctx cancel")
		}
	}
}

// Multiple subscribers for the same key all receive the notification.
func TestNotify_MultipleSubscribersSameKey(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sub1Ctx, sub1Cancel := context.WithCancel(ctx)
	defer sub1Cancel()
	sub2Ctx, sub2Cancel := context.WithCancel(ctx)
	defer sub2Cancel()

	ch1, _ := s.Subscribe(sub1Ctx, keyA)
	ch2, _ := s.Subscribe(sub2Ctx, keyA)

	if err := s.Notify(keyA); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	for i, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Fatalf("subscriber %d: channel closed unexpectedly", i+1)
			}
		case <-ctx.Done():
			t.Fatalf("subscriber %d: timed out waiting for notification", i+1)
		}
	}
}

// Notify for keyB does not wake subscriber for keyA.
func TestNotify_DoesNotCrossKeys(t *testing.T) {
	s := newTestStore(t)

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()

	ch, _ := s.Subscribe(subCtx, keyA)

	// Notify keyB — should NOT wake keyA's subscriber
	if err := s.Notify(keyB); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	select {
	case <-ch:
		t.Error("keyA subscriber woken by keyB notification")
	case <-time.After(200 * time.Millisecond):
		// expected — no spurious wakeup
	}
}
