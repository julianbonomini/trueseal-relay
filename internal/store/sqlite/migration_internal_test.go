package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// queryUserVersion reads PRAGMA user_version with a raw connection.
func queryUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func hasColumn(t *testing.T, path, table, column string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("table_info scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func countBlobs(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inbox`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

var errInjectedMigration = errors.New("injected migration failure")

// Criterion: a migration failure injected midway leaves the old store intact,
// and the next start completes the migration.
//
// The production chain is empty (v1 is the floor), so this drives the
// machinery with a synthetic chain: v1→v2 adds a column, v2→v3 fails.
// After the failure the store must be exactly version 1 — no column, blob
// intact — and a retry with a working chain must reach version 3.
func TestMigrationChain_FailureMidway_OldStoreIntact_RetryCompletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	recipient := make([]byte, 32)
	for i := range recipient {
		recipient[i] = 0x55
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(context.Background(), recipient, []byte("queued-blob"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s.Close()

	addNoteColumn := func(tx *sql.Tx) error {
		_, err := tx.Exec(`ALTER TABLE inbox ADD COLUMN note TEXT`)
		return err
	}

	broken := []migration{
		{from: 1, apply: addNoteColumn},
		{from: 2, apply: func(tx *sql.Tx) error { return errInjectedMigration }},
	}
	_, err = openStore(path, Options{}, broken, 3)
	if !errors.Is(err, errInjectedMigration) {
		t.Fatalf("err = %v, want the injected failure", err)
	}

	// The old store is intact (one transaction per ADR-0013): version 1,
	// no half-applied column, blob present.
	if v := queryUserVersion(t, path); v != 1 {
		t.Fatalf("version after failed migration = %d, want 1 (rolled back)", v)
	}
	if hasColumn(t, path, "inbox", "note") {
		t.Fatal("failed migration left its column behind — step 1 did not roll back atomically with step 2")
	}
	if n := countBlobs(t, path); n != 1 {
		t.Fatalf("blob count after failed migration = %d, want 1", n)
	}

	// Retry with a working chain: the migration completes.
	fixed := []migration{
		{from: 1, apply: addNoteColumn},
		{from: 2, apply: func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE inbox SET note = 'migrated' WHERE note IS NULL`)
			return err
		}},
	}
	s2, err := openStore(path, Options{}, fixed, 3)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	defer s2.Close()
	if got := s2.StoreVersion(); got != 3 {
		t.Fatalf("StoreVersion() = %d, want 3 after retry", got)
	}
	if n := countBlobs(t, path); n != 1 {
		t.Fatalf("blob count after retry = %d, want 1", n)
	}
}

// A store with no complete migration path is unmigratable — named versions,
// reset advertised only when the release declares one.
func TestMigrationChain_MissingStep_Unmigratable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Close()

	// Target 3 but only a 1→2 step exists.
	_, err = openStore(path, Options{}, []migration{
		{from: 1, apply: func(tx *sql.Tx) error { return nil }},
	}, 3)
	var unmig *UnmigratableError
	if !errors.As(err, &unmig) {
		t.Fatalf("err = %v, want UnmigratableError", err)
	}
	if unmig.Found != 2 || unmig.Supported != 3 {
		t.Fatalf("versions = %d→%d, want 2→3 (the step that has no path)", unmig.Found, unmig.Supported)
	}
	// The chain is one transaction (ADR-0013): a missing step leaves the
	// OLD store fully intact — never a half-migrated version.
	if v := queryUserVersion(t, path); v != 1 {
		t.Fatalf("version = %d, want 1 (whole chain rolled back)", v)
	}
}

// Production sanity: the real chain is empty at the v1 floor, so a current
// store opens through the migration path untouched.
func TestMigrationChain_ProductionFloor(t *testing.T) {
	if len(migrations) != 0 {
		t.Fatalf("production migrations = %d steps, want 0 at the v1 floor", len(migrations))
	}
	path := filepath.Join(t.TempDir(), "inbox.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if got := s.StoreVersion(); got != CurrentStoreVersion {
		t.Fatalf("StoreVersion() = %d, want %d", got, CurrentStoreVersion)
	}
}

// The reset path rebuilds the schema in one transaction: a failure leaves
// the old store readable.
func TestReset_KeepsKeypairUntouched(t *testing.T) {
	// The keypair is a separate file — the reset only ever touches the store
	// path. This test pins that the store path is the ONLY path the reset
	// opens for writing: create a keypair-looking file next to the store and
	// verify it survives byte-for-byte.
	dir := t.TempDir()
	storePath := filepath.Join(dir, "inbox.db")
	keyPath := filepath.Join(dir, "keypair.hex")
	keyBytes := []byte("000102030405060708090a0b0c0d0e0f")
	if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
		t.Fatalf("write keypair: %v", err)
	}

	s, err := New(storePath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recipient := make([]byte, 32)
	if err := s.Put(context.Background(), recipient, []byte("doomed"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s.Close()

	s2, err := NewWithOptions(storePath, Options{ResetDeclared: true, ResetRequested: true})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	s2.Close()

	got := readFile(t, keyPath)
	if string(got) != string(keyBytes) {
		t.Fatal("reset modified the keypair file")
	}
	if v := queryUserVersion(t, storePath); v != 1 {
		t.Fatalf("version after reset = %d, want 1", v)
	}
	if n := countBlobs(t, storePath); n != 0 {
		t.Fatalf("blobs after reset = %d, want 0", n)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
