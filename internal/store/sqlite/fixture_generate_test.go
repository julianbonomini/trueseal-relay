package sqlite_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

// TestGenerateStoreV1Fixture regenerates testdata/store-v1.db — the G6
// release-gate fixture: queued Blobs written at Store Version 1 (the floor).
// It only runs with RELAY_GENERATE_FIXTURE=1 and is otherwise skipped; the
// committed artifact is what TestFixture_StoreV1_BlobsDeliverable verifies.
//
//	go test ./internal/store/sqlite/ -run TestGenerateStoreV1Fixture -count=1
//	RELAY_GENERATE_FIXTURE=1 go test ./internal/store/sqlite/ -run TestGenerateStoreV1Fixture -count=1
func TestGenerateStoreV1Fixture(t *testing.T) {
	if os.Getenv("RELAY_GENERATE_FIXTURE") == "" {
		t.Skip("set RELAY_GENERATE_FIXTURE=1 to regenerate testdata/store-v1.db")
	}
	dir := filepath.Join("testdata")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	path := filepath.Join(dir, "store-v1.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove old fixture%s: %v", suffix, err)
		}
	}

	s, err := sqlitestore.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, fixtureRecipient, fixtureBlobOne, 30*24*time.Hour); err != nil {
		t.Fatalf("Put one: %v", err)
	}
	if err := s.Put(ctx, fixtureRecipient, fixtureBlobTwo, 30*24*time.Hour); err != nil {
		t.Fatalf("Put two: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Make the fixture a single self-contained file: checkpoint the WAL back
	// into the main database and drop the sidecar files.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatalf("journal_mode=DELETE: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove sidecar%s: %v", suffix, err)
		}
	}
	t.Logf("regenerated %s (Store Version 1, 2 queued blobs)", path)
}
