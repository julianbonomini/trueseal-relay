package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/julianbonomini/hush-relay/internal/store/sqlite"
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
