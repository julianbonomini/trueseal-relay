package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/store"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

func newStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	s, err := sqlitestore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func key(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

// Reaper leaves unexpired envelopes intact.
func TestReaper_SurvivesUnexpired(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	s.Put(ctx, key(0xAA), []byte("live"), time.Hour) //nolint:errcheck

	r := store.NewReaper(s, 10*time.Millisecond)
	rCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(rCtx); close(done) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	got, _ := s.Peek(ctx, key(0xAA))
	if len(got) != 1 {
		t.Errorf("want 1 unexpired envelope, got %d", len(got))
	}
}

// Reaper stops cleanly when ctx is cancelled.
func TestReaper_StopsOnCtxCancel(t *testing.T) {
	s := newStore(t)
	r := store.NewReaper(s, time.Hour) // long interval — never fires

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("want Run to return within 1s of ctx cancel")
	}
}

// Reap error does not crash the relay — logged and retried next interval.
func TestReaper_ErrorDoesNotCrash(t *testing.T) {
	errStore := &errorStore{}
	r := store.NewReaper(errStore, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("want Run to return after cancel even with Reap errors")
	}
}

// errorStore is a minimal InboxStore stub that always returns an error from Reap.
type errorStore struct{}

func (e *errorStore) Put(_ context.Context, _ []byte, _ []byte, _ time.Duration) error {
	return nil
}
func (e *errorStore) Peek(_ context.Context, _ []byte) ([]store.InboxBlob, error) {
	return nil, nil
}
func (e *errorStore) DeleteByIDs(_ context.Context, _ []int64) error { return nil }
func (e *errorStore) Reap(_ context.Context) error {
	return fmt.Errorf("reap: simulated error")
}

// Reaper removes expired envelopes after a cycle.
func TestReaper_ReapsExpired(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	s.Put(ctx, key(0xAA), []byte("expired"), -time.Second) //nolint:errcheck

	r := store.NewReaper(s, 10*time.Millisecond)
	rCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(rCtx); close(done) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	got, _ := s.Peek(ctx, key(0xAA))
	if len(got) != 0 {
		t.Errorf("want 0 envelopes after reap, got %d", len(got))
	}
}
