package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
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

// ── path validation ───────────────────────────────────────────────────────────

// New fails fast when the directory does not exist.
func TestNew_InvalidPath(t *testing.T) {
	_, err := sqlite.New("/nonexistent/path/that/cannot/exist/db.sqlite")
	if err == nil {
		t.Error("want error for invalid path, got nil")
	}
}

// ── Put ───────────────────────────────────────────────────────────────────────

// Put stores an envelope retrievable via Flush.
func TestPut_StoresEnvelope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	env := []byte("envelope-bytes")
	if err := s.Put(ctx, keyA, env, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Flush(ctx, keyA)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(got) != 1 || string(got[0]) != string(env) {
		t.Errorf("want [%q], got %v", env, got)
	}
}

// Put with a cancelled context returns an error.
func TestPut_CancelledContext(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Put(ctx, keyA, []byte("env"), time.Hour)
	if err == nil {
		t.Error("want error for cancelled context, got nil")
	}
}

// Put rejects a recipient key that is not exactly 32 bytes.
func TestPut_RejectsWrongKeyLength(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(context.Background(), []byte("too-short"), []byte("env"), time.Hour)
	if err == nil {
		t.Error("want error for short recipient key, got nil")
	}
}

// Put survives close and reopen — WAL+FULL guarantees persistence.
func TestPut_SurvivesClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.db")

	func() {
		s, err := sqlite.New(path)
		if err != nil {
			t.Fatalf("sqlite.New (first open): %v", err)
		}
		defer s.Close()
		if err := s.Put(context.Background(), keyA, []byte("survives"), time.Hour); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}()

	s2, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("sqlite.New (second open): %v", err)
	}
	defer s2.Close()

	got, err := s2.Flush(context.Background(), keyA)
	if err != nil {
		t.Fatalf("Flush after reopen: %v", err)
	}
	if len(got) != 1 || string(got[0]) != "survives" {
		t.Errorf("want ['survives'] after reopen, got %v", got)
	}
}

// ── Flush ─────────────────────────────────────────────────────────────────────

// Flush is atomic: returns all envelopes in FIFO order and deletes them.
func TestFlush_AtomicFetchAndDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, env := range []string{"one", "two", "three"} {
		s.Put(ctx, keyA, []byte(env), time.Hour) //nolint:errcheck
	}

	got, err := s.Flush(ctx, keyA)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 envelopes, got %d", len(got))
	}
	for i, want := range []string{"one", "two", "three"} {
		if string(got[i]) != want {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want)
		}
	}

	second, err := s.Flush(ctx, keyA)
	if err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("want empty second flush, got %d envelopes", len(second))
	}
}

// Flush on an empty inbox returns an empty slice without error.
func TestFlush_EmptyInbox(t *testing.T) {
	s := newTestStore(t)
	got, err := s.Flush(context.Background(), keyA)
	if err != nil {
		t.Fatalf("Flush on empty inbox: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 envelopes, got %d", len(got))
	}
}

// Flush only affects the given recipient — other inboxes are untouched.
func TestFlush_IsolatesRecipients(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("for-a"), time.Hour) //nolint:errcheck
	s.Put(ctx, keyB, []byte("for-b"), time.Hour) //nolint:errcheck

	got, err := s.Flush(ctx, keyA)
	if err != nil || len(got) != 1 || string(got[0]) != "for-a" {
		t.Errorf("Flush(keyA): want [for-a], got %v err=%v", got, err)
	}

	gotB, err := s.Flush(ctx, keyB)
	if err != nil || len(gotB) != 1 || string(gotB[0]) != "for-b" {
		t.Errorf("Flush(keyB): want [for-b], got %v err=%v", gotB, err)
	}
}

// Flush with a cancelled context returns an error.
func TestFlush_CancelledContext(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("env"), time.Hour) //nolint:errcheck

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Flush(cancelled, keyA)
	if err == nil {
		t.Error("want error for cancelled context, got nil")
	}
}

// ── Peek ─────────────────────────────────────────────────────────────────────

// Peek returns envelopes without deleting them.
func TestPeek_ReturnsWithoutDeleting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("one"), time.Hour) //nolint:errcheck
	s.Put(ctx, keyA, []byte("two"), time.Hour) //nolint:errcheck

	blobs, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(blobs) != 2 {
		t.Fatalf("want 2 blobs, got %d", len(blobs))
	}

	// blobs still in store after Peek
	again, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("second Peek: %v", err)
	}
	if len(again) != 2 {
		t.Errorf("want 2 blobs after second Peek, got %d", len(again))
	}
}

// DeleteByIDs deletes specific blobs and leaves others intact.
func TestDeleteByIDs_DeletesSpecificBlobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("one"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("two"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("three"), time.Hour) //nolint:errcheck

	blobs, _ := s.Peek(ctx, keyA)
	// delete only the first two
	ids := []int64{blobs[0].ID, blobs[1].ID}
	if err := s.DeleteByIDs(ctx, ids); err != nil {
		t.Fatalf("DeleteByIDs: %v", err)
	}

	remaining, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek after DeleteByIDs: %v", err)
	}
	if len(remaining) != 1 || string(remaining[0].Envelope) != "three" {
		t.Errorf("want [three], got %v", remaining)
	}
}

// DeleteByIDs with non-existent IDs is a no-op.
func TestDeleteByIDs_NonExistentIDs(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteByIDs(context.Background(), []int64{9999, 8888}); err != nil {
		t.Errorf("want no error for non-existent IDs, got %v", err)
	}
}

// Peek then DeleteByIDs: acked blobs deleted, un-acked blobs remain.
func TestPeek_ThenDeleteByIDs_LeavesRemainder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("acked"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("unacked"), time.Hour) //nolint:errcheck

	blobs, _ := s.Peek(ctx, keyA)
	// ack only the first
	if err := s.DeleteByIDs(ctx, []int64{blobs[0].ID}); err != nil {
		t.Fatalf("DeleteByIDs: %v", err)
	}

	// second Peek shows only the un-acked blob
	remaining, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(remaining) != 1 || string(remaining[0].Envelope) != "unacked" {
		t.Errorf("want [unacked], got %v", remaining)
	}
}

// ── Reap ──────────────────────────────────────────────────────────────────────

// Reap deletes expired envelopes and leaves unexpired ones intact.
func TestReap_DeletesExpiredOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("expired"), -time.Second) //nolint:errcheck
	s.Put(ctx, keyA, []byte("live"), time.Hour)       //nolint:errcheck

	if err := s.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	got, err := s.Flush(ctx, keyA)
	if err != nil {
		t.Fatalf("Flush after Reap: %v", err)
	}
	if len(got) != 1 || string(got[0]) != "live" {
		t.Errorf("want ['live'] after reap, got %v", got)
	}
}

// ── T7: Concurrent Flush ───────────────────────────────────────────────────

// Two goroutines calling Flush for the same key simultaneously must together
// receive exactly N envelopes — no double delivery (atomicity) and no loss
// (serialised via single DB connection).
func TestFlush_ConcurrentSameKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const N = 5
	for i := 0; i < N; i++ {
		env := []byte(fmt.Sprintf("env-%d", i))
		if err := s.Put(ctx, keyA, env, time.Hour); err != nil {
			t.Fatalf("Put[%d]: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var allGot [][]byte

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Flush(ctx, keyA)
			if err != nil {
				// Serialization conflict: the other goroutine already holds the
				// transaction lock. This is acceptable — those envelopes were
				// or will be delivered by the winning goroutine.
				return
			}
			mu.Lock()
			allGot = append(allGot, got...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(allGot) != N {
		t.Errorf("want %d total envelopes (no double delivery, no loss), got %d", N, len(allGot))
	}
}
