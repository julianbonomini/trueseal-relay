package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const schema = `
CREATE TABLE IF NOT EXISTS inbox (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	recipient   BLOB    NOT NULL CHECK(length(recipient) = 32),
	envelope    BLOB    NOT NULL,
	expires_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbox_recipient  ON inbox(recipient);
CREATE INDEX IF NOT EXISTS idx_inbox_expires_at ON inbox(expires_at);
`

// Store is a SQLite-backed InboxStore.
// Safe for concurrent use — all access serialised through a single connection.
// See ADR-0003 (durable until delivery) and ADR-0007 (hexagonal architecture).
type Store struct {
	db *sql.DB
}

// New opens (or creates) a SQLite database at path and initialises the schema.
// WAL mode and FULL synchronous are set to guarantee crash-safe persistence.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}

	// Single connection — serialises writes, avoids SQLITE_BUSY under concurrency.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: set WAL mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=FULL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: set synchronous=FULL: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: apply schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close releases the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Put stores an envelope in the inbox for recipientKey.
// Does not deduplicate — see InboxStore.Put contract.
func (s *Store) Put(ctx context.Context, recipientKey []byte, envelope []byte, ttl time.Duration) error {
	expiresAt := time.Now().Add(ttl).Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO inbox (recipient, envelope, expires_at) VALUES (?, ?, ?)`,
		recipientKey, envelope, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("sqlite: put: %w", err)
	}
	return nil
}

// Peek fetches all envelopes for recipientKey without deleting them.
// Returns InboxBlob values in insertion order (FIFO).
func (s *Store) Peek(ctx context.Context, recipientKey []byte) ([]store.InboxBlob, error) {
	return s.PeekAfter(ctx, recipientKey, 0)
}

// PeekAfter fetches envelopes for recipientKey with IDs strictly greater than
// afterID, in insertion order (FIFO), without deleting them.
// See store.InboxStore.PeekAfter.
func (s *Store) PeekAfter(ctx context.Context, recipientKey []byte, afterID int64) ([]store.InboxBlob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, envelope FROM inbox WHERE recipient = ? AND id > ? ORDER BY id ASC`,
		recipientKey, afterID,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: peek after: %w", err)
	}
	defer rows.Close()

	var blobs []store.InboxBlob
	for rows.Next() {
		var b store.InboxBlob
		if err := rows.Scan(&b.ID, &b.Envelope); err != nil {
			return nil, fmt.Errorf("sqlite: peek after scan: %w", err)
		}
		blobs = append(blobs, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: peek after rows: %w", err)
	}
	return blobs, nil
}

// DeleteForRecipient deletes blobs by their store-assigned IDs, but only from
// the inbox owned by recipientKey. IDs that are missing or belong to another
// inbox are silently ignored. See store.InboxStore.
func (s *Store) DeleteForRecipient(ctx context.Context, recipientKey []byte, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	// Build parameterised query:
	// DELETE FROM inbox WHERE recipient = ? AND id IN (?, ?, ...)
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, recipientKey)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `DELETE FROM inbox WHERE recipient = ? AND id IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("sqlite: delete for recipient: %w", err)
	}
	return nil
}

// Reap deletes all envelopes whose TTL has elapsed.
// Reaping is policy, not data loss — see ADR-0003.
func (s *Store) Reap(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM inbox WHERE expires_at <= ?`,
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("sqlite: reap: %w", err)
	}
	return nil
}
