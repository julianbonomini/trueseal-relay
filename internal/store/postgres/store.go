package postgres

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/julianbonomini/trueseal-relay/internal/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS inbox (
	id         BIGSERIAL PRIMARY KEY,
	recipient  BYTEA     NOT NULL CHECK(length(recipient) = 32),
	envelope   BYTEA     NOT NULL,
	expires_at BIGINT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbox_recipient  ON inbox(recipient);
CREATE INDEX IF NOT EXISTS idx_inbox_expires_at ON inbox(expires_at);
`

// Store implements store.InboxStore and notify.Notifier backed by Postgres.
// pgxpool is used for all store operations; a single dedicated *pgx.Conn
// is used exclusively for LISTEN/NOTIFY. See ADR-0010.
type Store struct {
	pool     *pgxpool.Pool
	listener *pgx.Conn
	cancel   context.CancelFunc // cancels the listenLoop goroutine

	mu       sync.RWMutex
	subs     map[string][]chan struct{} // channelName → subscriber chans
	refcount map[string]int            // channelName → active subscriber count
}

// New opens a pool and a dedicated listener connection, applies the schema,
// and starts the background listen loop.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: apply schema: %w", err)
	}

	listener, err := pgx.Connect(ctx, dsn)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: open listener connection: %w", err)
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	s := &Store{
		pool:     pool,
		listener: listener,
		cancel:   cancel,
		subs:     make(map[string][]chan struct{}),
		refcount: make(map[string]int),
	}
	go s.listenLoop(loopCtx)
	return s, nil
}

// Close shuts down the store: cancels the listen loop, closes the listener
// connection, and closes the pool.
func (s *Store) Close() {
	s.cancel()
	_ = s.listener.Close(context.Background())
	s.pool.Close()
}

// Put stores an envelope in the inbox for recipientKey. See store.InboxStore.
func (s *Store) Put(ctx context.Context, recipientKey []byte, envelope []byte, ttl time.Duration) error {
	if len(recipientKey) != 32 {
		return fmt.Errorf("postgres: recipient key must be 32 bytes, got %d", len(recipientKey))
	}
	expiresAt := time.Now().Add(ttl).Unix()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO inbox (recipient, envelope, expires_at) VALUES ($1, $2, $3)`,
		recipientKey, envelope, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: put: %w", err)
	}
	return nil
}

// Peek fetches all envelopes for recipientKey without deleting them.
// Returns InboxBlob values in insertion order (FIFO). See store.InboxStore.
func (s *Store) Peek(ctx context.Context, recipientKey []byte) ([]store.InboxBlob, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, envelope FROM inbox WHERE recipient = $1 ORDER BY id ASC`,
		recipientKey,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: peek: %w", err)
	}
	defer rows.Close()

	var blobs []store.InboxBlob
	for rows.Next() {
		var b store.InboxBlob
		if err := rows.Scan(&b.ID, &b.Envelope); err != nil {
			return nil, fmt.Errorf("postgres: peek scan: %w", err)
		}
		blobs = append(blobs, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: peek rows: %w", err)
	}
	return blobs, nil
}

// DeleteByIDs deletes blobs by their store-assigned IDs. See store.InboxStore.
func (s *Store) DeleteByIDs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	// Build $1, $2, ... placeholders
	args := make([]any, len(ids))
	placeholders := make([]byte, 0, len(ids)*4)
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, []byte(fmt.Sprintf("$%d", i+1))...)
		args[i] = id
	}
	query := `DELETE FROM inbox WHERE id IN (` + string(placeholders) + `)`
	if _, err := s.pool.Exec(ctx, query, args...); err != nil {
		return fmt.Errorf("postgres: delete by ids: %w", err)
	}
	return nil
}

// Reap deletes all envelopes whose TTL has elapsed. See store.InboxStore.
func (s *Store) Reap(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM inbox WHERE expires_at <= $1`,
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("postgres: reap: %w", err)
	}
	return nil
}

// Notify sends a LISTEN/NOTIFY wakeup to all subscribers of recipientKey.
// See notify.Notifier.
func (s *Store) Notify(recipientKey []byte) error {
	name := channelName(recipientKey)
	_, err := s.pool.Exec(context.Background(), `SELECT pg_notify($1, '')`, name)
	if err != nil {
		return fmt.Errorf("postgres: notify: %w", err)
	}
	return nil
}

// Subscribe registers a delivery wakeup channel for recipientKey.
// The returned channel receives a signal whenever a blob arrives for this key.
// The channel is closed when ctx is cancelled. See notify.Notifier.
func (s *Store) Subscribe(ctx context.Context, recipientKey []byte) (<-chan struct{}, error) {
	ch := make(chan struct{}, 1)
	name := channelName(recipientKey)

	s.mu.Lock()
	s.refcount[name]++
	if s.refcount[name] == 1 {
		if _, err := s.listener.Exec(context.Background(), "LISTEN "+name); err != nil {
			s.refcount[name]--
			s.mu.Unlock()
			return nil, fmt.Errorf("postgres: listen %s: %w", name, err)
		}
	}
	s.subs[name] = append(s.subs[name], ch)
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		s.mu.Lock()
		defer s.mu.Unlock()

		// Remove ch from subs[name]
		subs := s.subs[name]
		for i, sub := range subs {
			if sub == ch {
				s.subs[name] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		close(ch)

		s.refcount[name]--
		if s.refcount[name] == 0 {
			delete(s.subs, name)
			delete(s.refcount, name)
			_, _ = s.listener.Exec(context.Background(), "UNLISTEN "+name)
		}
	}()

	return ch, nil
}

// listenLoop runs in a background goroutine, dispatching Postgres NOTIFY
// events to in-process subscribers. Calls log.Fatalf on connection failure
// (ADR-0010: crash and let the process manager restart the Node).
func (s *Store) listenLoop(ctx context.Context) {
	for {
		notif, err := s.listener.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // clean shutdown via Close()
			}
			log.Fatalf("postgres listener: connection lost: %v — restarting node", err)
		}

		s.mu.RLock()
		chans := s.subs[notif.Channel]
		snapshot := make([]chan struct{}, len(chans))
		copy(snapshot, chans)
		s.mu.RUnlock()

		for _, ch := range snapshot {
			select {
			case ch <- struct{}{}:
			default: // subscriber already has a pending notification
			}
		}
	}
}

// Truncate deletes all rows from the inbox table. For use in tests only.
func (s *Store) Truncate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `TRUNCATE inbox`)
	return err
}

// channelName derives a Postgres LISTEN/NOTIFY channel name from a 32-byte
// recipient key. Uses the first 31 bytes hex-encoded with an "r" prefix,
// giving exactly 63 characters — the maximum Postgres allows (NAMEDATALEN-1).
// The final byte is omitted to fit the limit; collision probability for
// random 32-byte keys is negligible.
func channelName(key []byte) string {
	return "r" + hex.EncodeToString(key[:31])
}
