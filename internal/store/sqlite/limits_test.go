package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/store"
)

// newTestStore opens a fresh in-memory-file-backed store for quota tests.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir() + "/inbox.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestPutWithLimits_BlobBoundary pins the blob-count quota (RELAY-6;
// ADR-0012): at the limit the put succeeds, one over is refused with
// ErrInboxFull and stores nothing.
func TestPutWithLimits_BlobBoundary(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	key := make([]byte, 32)
	key[0] = 0xAA
	limits := store.InboxLimits{MaxBlobs: 2}

	for i := 0; i < 2; i++ {
		if err := s.PutWithLimits(ctx, key, []byte{byte(i)}, time.Hour, limits); err != nil {
			t.Fatalf("put %d at/under limit refused: %v", i+1, err)
		}
	}
	err := s.PutWithLimits(ctx, key, []byte{3}, time.Hour, limits)
	if !errors.Is(err, store.ErrInboxFull) {
		t.Fatalf("one over the blob limit: want ErrInboxFull, got %v", err)
	}
	blobs, err := s.Peek(ctx, key)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(blobs) != 2 {
		t.Fatalf("refused put stored something: inbox holds %d blobs, want 2", len(blobs))
	}
}

// TestPutWithLimits_BytesBoundary pins the byte quota: a put that fits
// exactly succeeds, one that crosses the limit is refused.
func TestPutWithLimits_BytesBoundary(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	key := make([]byte, 32)
	key[0] = 0xBB
	limits := store.InboxLimits{MaxBytes: 10}

	if err := s.PutWithLimits(ctx, key, []byte("1234567890"), time.Hour, limits); err != nil {
		t.Fatalf("put filling the byte limit exactly refused: %v", err)
	}
	err := s.PutWithLimits(ctx, key, []byte("x"), time.Hour, limits)
	if !errors.Is(err, store.ErrInboxFull) {
		t.Fatalf("one byte over the limit: want ErrInboxFull, got %v", err)
	}
}

// TestPutWithLimits_SpaceFreedByAckDelete is the temporary-refusal contract:
// once blobs leave the Inbox (DeliverAck delete or TTL reap), puts succeed
// again — a quota refusal is never permanent.
func TestPutWithLimits_SpaceFreedByAckDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	key := make([]byte, 32)
	key[0] = 0xCC
	limits := store.InboxLimits{MaxBlobs: 1}

	if err := s.PutWithLimits(ctx, key, []byte("a"), time.Hour, limits); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := s.PutWithLimits(ctx, key, []byte("b"), time.Hour, limits); !errors.Is(err, store.ErrInboxFull) {
		t.Fatalf("second put over limit: want ErrInboxFull, got %v", err)
	}
	blobs, _ := s.Peek(ctx, key)
	if err := s.DeleteForRecipient(ctx, key, []int64{blobs[0].ID}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.PutWithLimits(ctx, key, []byte("b"), time.Hour, limits); err != nil {
		t.Fatalf("put after space freed refused (quota must be temporary): %v", err)
	}
}

// TestPutWithLimits_KeyedOnRecipientOnly proves the quota is keyed on the
// recipient Inbox and nothing else (ADR-0012: never on a peer address —
// the quota cannot see one): two recipients have fully independent quotas.
func TestPutWithLimits_KeyedOnRecipientOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	keyA := make([]byte, 32)
	keyA[0] = 0x01
	keyB := make([]byte, 32)
	keyB[0] = 0x02
	limits := store.InboxLimits{MaxBlobs: 1}

	if err := s.PutWithLimits(ctx, keyA, []byte("a"), time.Hour, limits); err != nil {
		t.Fatalf("A first put: %v", err)
	}
	if err := s.PutWithLimits(ctx, keyA, []byte("a2"), time.Hour, limits); !errors.Is(err, store.ErrInboxFull) {
		t.Fatalf("A second put: want ErrInboxFull, got %v", err)
	}
	if err := s.PutWithLimits(ctx, keyB, []byte("b"), time.Hour, limits); err != nil {
		t.Fatalf("B's quota is independent of A's: %v", err)
	}
}

// TestPutWithLimits_ZeroLimitsIsPlainPut: the zero value disables the check
// (keeps Put's contract and the no-limits configuration working).
func TestPutWithLimits_ZeroLimitsIsPlainPut(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	key := make([]byte, 32)
	for i := 0; i < 5; i++ {
		if err := s.PutWithLimits(ctx, key, []byte{byte(i)}, time.Hour, store.InboxLimits{}); err != nil {
			t.Fatalf("put %d with zero limits: %v", i, err)
		}
	}
}
