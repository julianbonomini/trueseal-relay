package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/julianbonomini/hush-relay/internal/store"
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

// Flush atomically fetches and deletes all envelopes for recipientKey.
// Returns envelopes in insertion order (FIFO). The fetch and delete are
// a single IMMEDIATE transaction — no envelope is returned twice.
func (s *Store) Flush(ctx context.Context, recipientKey []byte) ([][]byte, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("sqlite: flush begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx,
		`SELECT envelope FROM inbox WHERE recipient = ? ORDER BY id ASC`,
		recipientKey,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: flush select: %w", err)
	}

	var envelopes [][]byte
	for rows.Next() {
		var env []byte
		if err := rows.Scan(&env); err != nil {
			rows.Close()
			return nil, fmt.Errorf("sqlite: flush scan: %w", err)
		}
		envelopes = append(envelopes, env)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: flush rows: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM inbox WHERE recipient = ?`,
		recipientKey,
	); err != nil {
		return nil, fmt.Errorf("sqlite: flush delete: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: flush commit: %w", err)
	}

	return envelopes, nil
}

// Peek fetches all envelopes for recipientKey without deleting them.
// Returns InboxBlob values in insertion order (FIFO).
func (s *Store) Peek(ctx context.Context, recipientKey []byte) ([]store.InboxBlob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, envelope FROM inbox WHERE recipient = ? ORDER BY id ASC`,
		recipientKey,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: peek: %w", err)
	}
	defer rows.Close()

	var blobs []store.InboxBlob
	for rows.Next() {
		var b store.InboxBlob
		if err := rows.Scan(&b.ID, &b.Envelope); err != nil {
			return nil, fmt.Errorf("sqlite: peek scan: %w", err)
		}
		blobs = append(blobs, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: peek rows: %w", err)
	}
	return blobs, nil
}

// DeleteByIDs deletes blobs by their store-assigned IDs.
// IDs that no longer exist are silently ignored.
func (s *Store) DeleteByIDs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	// Build parameterised query: DELETE FROM inbox WHERE id IN (?, ?, ...)
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `DELETE FROM inbox WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("sqlite: delete by ids: %w", err)
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
