package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/store"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
	_ "modernc.org/sqlite"
)

// The committed testdata/store-v1.db fixture holds exactly these two blobs,
// queued for this recipient, written at Store Version 1 (the floor).
var (
	fixtureRecipient = repeatByte(0xAB, 32)
	fixtureBlobOne   = []byte("fixture-blob-one-v1")
	fixtureBlobTwo   = []byte("fixture-blob-two-v1")
)

func repeatByte(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// rawExec runs statements against a store file with no versioning logic —
// used to build unversioned and foreign-version stores.
func rawExec(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("raw exec %q: %v", q, err)
		}
	}
}

func rawUserVersion(t *testing.T, path string) int {
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

func rawQueryBlobs(t *testing.T, path string, recipient []byte) [][]byte {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT envelope FROM inbox WHERE recipient = ? ORDER BY id ASC`, recipient)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatalf("raw scan: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// todaySchema is the schema a current binary writes — used to build the
// "today's unversioned store" case without going through the versioned open.
const todaySchema = `
CREATE TABLE IF NOT EXISTS inbox (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	recipient   BLOB    NOT NULL CHECK(length(recipient) = 32),
	envelope    BLOB    NOT NULL,
	expires_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbox_recipient  ON inbox(recipient);
CREATE INDEX IF NOT EXISTS idx_inbox_expires_at ON inbox(expires_at);
`

func checksum(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open for checksum: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("checksum: %v", err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// Criterion: a fresh store records Store Version 1.
func TestNew_FreshStoreRecordsVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	s, err := sqlitestore.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := s.StoreVersion(); got != 1 {
		t.Fatalf("StoreVersion() = %d, want 1", got)
	}
	s.Close()
	if got := rawUserVersion(t, path); got != 1 {
		t.Fatalf("PRAGMA user_version = %d, want 1", got)
	}
}

// Criterion: today's unversioned store (schema matches) is adopted as
// version 1 with its queued Blobs intact.
func TestUnversionedStore_AdoptedAsVersion1_BlobsIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	recipient := repeatByte(0x11, 32)
	rawExec(t, path, todaySchema,
		`INSERT INTO inbox (recipient, envelope, expires_at) VALUES (x'1111111111111111111111111111111111111111111111111111111111111111', x'AAAA', 9999999999)`,
		`INSERT INTO inbox (recipient, envelope, expires_at) VALUES (x'1111111111111111111111111111111111111111111111111111111111111111', x'BBBB', 9999999999)`,
	)
	if v := rawUserVersion(t, path); v != 0 {
		t.Fatalf("setup: expected unversioned store, got version %d", v)
	}

	s, err := sqlitestore.New(path)
	if err != nil {
		t.Fatalf("New on unversioned store: %v", err)
	}
	defer s.Close()
	if got := s.StoreVersion(); got != 1 {
		t.Fatalf("StoreVersion() = %d, want 1 (adopted)", got)
	}
	blobs, err := s.Peek(context.Background(), recipient)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(blobs) != 2 || string(blobs[0].Envelope) != "\xaa\xaa" || string(blobs[1].Envelope) != "\xbb\xbb" {
		t.Fatalf("adoption lost blobs: %+v", blobs)
	}
}

// An unversioned store whose schema does not match is unmigratable: refused
// without a declared reset; with a declared reset the refusal names the flag;
// with the flag it drops the inboxes and starts at version 1.
func TestUnversionedStore_SchemaMismatch(t *testing.T) {
	build := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "inbox.db")
		rawExec(t, path, `CREATE TABLE inbox (nonsense TEXT)`)
		return path
	}

	t.Run("refused on a non-declared release", func(t *testing.T) {
		_, err := sqlitestore.New(build(t))
		var unmig *sqlitestore.UnmigratableError
		if !errors.As(err, &unmig) {
			t.Fatalf("err = %v, want UnmigratableError", err)
		}
		if unmig.Found != 0 || unmig.Supported != 1 {
			t.Fatalf("versions = %d→%d, want 0→1", unmig.Found, unmig.Supported)
		}
		if strings.Contains(err.Error(), "-reset-store") {
			t.Fatalf("non-declared release must not advertise the reset flag: %v", err)
		}
	})

	t.Run("declared release without the flag names the flag", func(t *testing.T) {
		_, err := sqlitestore.NewWithOptions(build(t), sqlitestore.Options{ResetDeclared: true})
		var unmig *sqlitestore.UnmigratableError
		if !errors.As(err, &unmig) {
			t.Fatalf("err = %v, want UnmigratableError", err)
		}
		if !strings.Contains(err.Error(), "-reset-store") {
			t.Fatalf("declared release must name the flag: %v", err)
		}
	})

	t.Run("declared release with the flag resets to version 1", func(t *testing.T) {
		path := build(t)
		s, err := sqlitestore.NewWithOptions(path, sqlitestore.Options{ResetDeclared: true, ResetRequested: true})
		if err != nil {
			t.Fatalf("reset open: %v", err)
		}
		defer s.Close()
		if got := s.StoreVersion(); got != 1 {
			t.Fatalf("StoreVersion() = %d, want 1 after reset", got)
		}
		blobs, err := s.Peek(context.Background(), repeatByte(0x22, 32))
		if err != nil {
			t.Fatalf("Peek after reset: %v", err)
		}
		if len(blobs) != 0 {
			t.Fatalf("reset left %d blobs, want 0", len(blobs))
		}
		// Reset rebuilds the real schema — a normal Put must work.
		if err := s.Put(context.Background(), repeatByte(0x22, 32), []byte("after-reset"), time.Hour); err != nil {
			t.Fatalf("Put after reset: %v", err)
		}
	})
}

// Criterion: a newer store is refused byte-for-byte unchanged, and the
// message names both versions.
func TestNewerStore_RefusedByteForByteUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	s, err := sqlitestore.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(context.Background(), repeatByte(0x33, 32), []byte("queued"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s.Close()

	// Simulate a store written by a newer release.
	rawExec(t, path, `PRAGMA user_version = 5`)
	before := checksum(t, path)

	_, err = sqlitestore.New(path)
	var tooNew *sqlitestore.TooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("err = %v, want TooNewError", err)
	}
	if tooNew.Found != 5 || tooNew.Supported != 1 {
		t.Fatalf("versions = %d→%d, want 5→1", tooNew.Found, tooNew.Supported)
	}
	msg := err.Error()
	if !strings.Contains(msg, "5") || !strings.Contains(msg, "1") {
		t.Fatalf("refusal must name both versions: %v", err)
	}

	if after := checksum(t, path); after != before {
		t.Fatal("refusal modified the store file")
	}
}

// Criterion: the reset flag is refused on a release that isn't a declared
// Store Reset — even against a healthy store.
func TestResetFlag_RefusedOnNonDeclaredRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	s, err := sqlitestore.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(context.Background(), repeatByte(0x44, 32), []byte("keep-me"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s.Close()

	_, err = sqlitestore.NewWithOptions(path, sqlitestore.Options{ResetRequested: true})
	var refused *sqlitestore.ResetNotDeclaredError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want ResetNotDeclaredError", err)
	}

	// The store is untouched: a plain open sees the blob.
	s2, err := sqlitestore.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	blobs, err := s2.Peek(context.Background(), repeatByte(0x44, 32))
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(blobs) != 1 || string(blobs[0].Envelope) != "keep-me" {
		t.Fatalf("refused reset touched the store: %+v", blobs)
	}
}

// Criterion: the committed v1 fixture (queued Blobs at the version floor)
// opens and those Blobs are deliverable — the G6 gate's unit-level form.
func TestFixture_StoreV1_BlobsDeliverable(t *testing.T) {
	src := filepath.Join("testdata", "store-v1.db")
	dst := filepath.Join(t.TempDir(), "inbox.db")
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create copy: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	out.Close()

	s, err := sqlitestore.New(dst)
	if err != nil {
		t.Fatalf("New on fixture: %v", err)
	}
	defer s.Close()
	if got := s.StoreVersion(); got != 1 {
		t.Fatalf("fixture StoreVersion() = %d, want 1", got)
	}

	blobs, err := s.Peek(context.Background(), fixtureRecipient)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(blobs) != 2 ||
		string(blobs[0].Envelope) != string(fixtureBlobOne) ||
		string(blobs[1].Envelope) != string(fixtureBlobTwo) {
		t.Fatalf("fixture blobs = %+v", blobs)
	}

	// Deliver them: ack-gated deletion, then the inbox is empty.
	ids := []int64{blobs[0].ID, blobs[1].ID}
	if err := s.DeleteForRecipient(context.Background(), fixtureRecipient, ids); err != nil {
		t.Fatalf("DeleteForRecipient: %v", err)
	}
	remaining, err := s.Peek(context.Background(), fixtureRecipient)
	if err != nil {
		t.Fatalf("Peek after delivery: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("%d blobs remain after delivery", len(remaining))
	}
}

// Guard: the InboxStore contract still holds through the versioned open.
var _ store.InboxStore = (*sqlitestore.Store)(nil)

// Criterion (orchestrator decision 2026-10-13): refuse-newer wins over the
// reset flag, even on a declared Store Reset release. TooNew protects data
// written by a future release; a downgrade-with-reset must never wipe it.
// Before RELAY-16 the reset branch ran first and would have dropped the
// newer store.
func TestNewerStore_ResetFlagStillRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	s, err := sqlitestore.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(context.Background(), repeatByte(0x44, 32), []byte("future-data"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s.Close()
	rawExec(t, path, `PRAGMA user_version = 5`)
	before := checksum(t, path)

	_, err = sqlitestore.NewWithOptions(path, sqlitestore.Options{ResetDeclared: true, ResetRequested: true})
	var tooNew *sqlitestore.TooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("err = %v, want TooNewError even with the reset flag on a declared release", err)
	}
	if after := checksum(t, path); after != before {
		t.Fatal("the reset flag touched a newer store — refuse-newer must win")
	}
}

// WasReset reports an actual reset only: the flag on a fresh store is not
// a reset, and the log must not claim one (RELAY-16 parked note).
func TestWasReset(t *testing.T) {
	t.Run("false on a fresh store even with the flag", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "inbox.db")
		s, err := sqlitestore.NewWithOptions(path, sqlitestore.Options{ResetDeclared: true, ResetRequested: true})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer s.Close()
		if s.WasReset() {
			t.Fatal("WasReset = true on a fresh store — nothing was dropped")
		}
	})
	t.Run("true after a real reset", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "inbox.db")
		s, err := sqlitestore.New(path)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := s.Put(context.Background(), repeatByte(0x55, 32), []byte("x"), time.Hour); err != nil {
			t.Fatalf("Put: %v", err)
		}
		s.Close()
		s2, err := sqlitestore.NewWithOptions(path, sqlitestore.Options{ResetDeclared: true, ResetRequested: true})
		if err != nil {
			t.Fatalf("reset open: %v", err)
		}
		defer s2.Close()
		if !s2.WasReset() {
			t.Fatal("WasReset = false after a real reset")
		}
	})
}
