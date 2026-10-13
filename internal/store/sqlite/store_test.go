package sqlite_test

import (
	"context"
	"path/filepath"
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

// Put stores an envelope retrievable via Peek.
func TestPut_StoresEnvelope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	env := []byte("envelope-bytes")
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

	blobs, err := s2.Peek(context.Background(), keyA)
	if err != nil {
		t.Fatalf("Peek after reopen: %v", err)
	}
	if len(blobs) != 1 || string(blobs[0].Envelope) != "survives" {
		t.Errorf("want ['survives'] after reopen, got %v", blobs)
	}
}

// ── PeekAfter ────────────────────────────────────────────────────────────────

// PeekAfter returns only blobs with IDs strictly greater than afterID, in
// arrival order — the basis for once-per-Receive-Session delivery (RELAY-7).
func TestPeekAfter_ReturnsOnlyNewerInOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("one"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("two"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("three"), time.Hour) //nolint:errcheck

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

	// afterID 0 equals Peek (whole inbox).
	fromZero, err := s.PeekAfter(ctx, keyA, 0)
	if err != nil {
		t.Fatalf("PeekAfter 0: %v", err)
	}
	if len(fromZero) != 3 {
		t.Errorf("want 3 blobs from afterID 0, got %d", len(fromZero))
	}

	// Non-destructive: a repeated PeekAfter returns the same blobs.
	again, err := s.PeekAfter(ctx, keyA, all[0].ID)
	if err != nil {
		t.Fatalf("second PeekAfter: %v", err)
	}
	if len(again) != 2 {
		t.Errorf("want 2 blobs on repeat PeekAfter, got %d", len(again))
	}
}

// PeekAfter scopes to the recipient — another inbox's IDs don't leak in.
func TestPeekAfter_IsolatesRecipients(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("a1"), time.Hour) //nolint:errcheck
	s.Put(ctx, keyB, []byte("b1"), time.Hour) //nolint:errcheck
	s.Put(ctx, keyA, []byte("a2"), time.Hour) //nolint:errcheck

	aBlobs, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek A: %v", err)
	}
	newer, err := s.PeekAfter(ctx, keyA, aBlobs[0].ID)
	if err != nil {
		t.Fatalf("PeekAfter: %v", err)
	}
	if len(newer) != 1 || string(newer[0].Envelope) != "a2" {
		t.Errorf("want [a2] only, got %v", newer)
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

// DeleteForRecipient deletes specific blobs from that recipient's inbox and
// leaves others intact.
func TestDeleteForRecipient_DeletesSpecificBlobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("one"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("two"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("three"), time.Hour) //nolint:errcheck

	blobs, _ := s.Peek(ctx, keyA)
	// delete only the first two
	ids := []int64{blobs[0].ID, blobs[1].ID}
	if err := s.DeleteForRecipient(ctx, keyA, ids); err != nil {
		t.Fatalf("DeleteForRecipient: %v", err)
	}

	remaining, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek after DeleteForRecipient: %v", err)
	}
	if len(remaining) != 1 || string(remaining[0].Envelope) != "three" {
		t.Errorf("want [three], got %v", remaining)
	}
}

// DeleteForRecipient with non-existent IDs is a no-op.
func TestDeleteForRecipient_NonExistentIDs(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteForRecipient(context.Background(), keyA, []int64{9999, 8888}); err != nil {
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

	// Key B tries to delete A's blob by ID.
	if err := s.DeleteForRecipient(ctx, keyB, []int64{blobs[0].ID}); err != nil {
		t.Fatalf("DeleteForRecipient: %v", err)
	}

	remaining, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek after foreign delete: %v", err)
	}
	if len(remaining) != 1 || string(remaining[0].Envelope) != "for-A" {
		t.Errorf("want A's blob intact after foreign delete, got %v", remaining)
	}
}

// Peek then DeleteForRecipient: acked blobs deleted, un-acked blobs remain.
func TestPeek_ThenDeleteForRecipient_LeavesRemainder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Put(ctx, keyA, []byte("acked"), time.Hour)   //nolint:errcheck
	s.Put(ctx, keyA, []byte("unacked"), time.Hour) //nolint:errcheck

	blobs, _ := s.Peek(ctx, keyA)
	// ack only the first
	if err := s.DeleteForRecipient(ctx, keyA, []int64{blobs[0].ID}); err != nil {
		t.Fatalf("DeleteForRecipient: %v", err)
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

	blobs, err := s.Peek(ctx, keyA)
	if err != nil {
		t.Fatalf("Peek after Reap: %v", err)
	}
	if len(blobs) != 1 || string(blobs[0].Envelope) != "live" {
		t.Errorf("want ['live'] after reap, got %v", blobs)
	}
}
