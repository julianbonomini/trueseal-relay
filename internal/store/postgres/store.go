package postgres

import (
	"context"
	"encoding/hex"
	"errors"
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

// listenCmd is a serialised LISTEN or UNLISTEN command sent to the listenLoop.
type listenCmd struct {
	sql  string
	done chan<- error
}

// Store implements store.InboxStore and notify.Notifier backed by Postgres.
// pgxpool is used for all store operations; a single dedicated *pgx.Conn
// is used exclusively for LISTEN/NOTIFY. See ADR-0010.
//
// The listener connection is owned entirely by the listenLoop goroutine.
// LISTEN/UNLISTEN commands are serialised through cmdCh so that Subscribe
// and the loop never access the connection concurrently.
type Store struct {
	pool     *pgxpool.Pool
	listener *pgx.Conn
	cancel   context.CancelFunc
	cmdCh    chan listenCmd // LISTEN/UNLISTEN commands → listenLoop

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
		cmdCh:    make(chan listenCmd, 64),
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
//
// LISTEN is sent to the listenLoop via cmdCh — never called directly on the
// listener connection to avoid concurrent access with WaitForNotification.
func (s *Store) Subscribe(ctx context.Context, recipientKey []byte) (<-chan struct{}, error) {
	ch := make(chan struct{}, 1)
	name := channelName(recipientKey)

	s.mu.Lock()
	s.refcount[name]++
	needListen := s.refcount[name] == 1
	s.subs[name] = append(s.subs[name], ch)
	s.mu.Unlock()

	if needListen {
		done := make(chan error, 1)
		s.cmdCh <- listenCmd{sql: "LISTEN " + name, done: done}
		if err := <-done; err != nil {
			s.mu.Lock()
			s.removeSub(name, ch)
			s.refcount[name]--
			if s.refcount[name] == 0 {
				delete(s.subs, name)
				delete(s.refcount, name)
			}
			s.mu.Unlock()
			close(ch)
			return nil, fmt.Errorf("postgres: listen %s: %w", name, err)
		}
	}

	go func() {
		<-ctx.Done()
		s.mu.Lock()
		s.removeSub(name, ch)
		close(ch)
		s.refcount[name]--
		shouldUnlisten := s.refcount[name] == 0
		if shouldUnlisten {
			delete(s.subs, name)
			delete(s.refcount, name)
		}
		s.mu.Unlock()

		if shouldUnlisten {
			done := make(chan error, 1)
			select {
			case s.cmdCh <- listenCmd{sql: "UNLISTEN " + name, done: done}:
				<-done // wait; ignore error (best effort on teardown)
			default:
				// cmdCh full or store closing — skip, harmless
			}
		}
	}()

	return ch, nil
}

// removeSub removes ch from subs[name]. Caller must hold s.mu.
func (s *Store) removeSub(name string, ch chan struct{}) {
	subs := s.subs[name]
	for i, sub := range subs {
		if sub == ch {
			s.subs[name] = append(subs[:i], subs[i+1:]...)
			return
		}
	}
}

// listenLoop runs in a background goroutine and is the sole owner of the
// listener connection — no other goroutine may call methods on it.
//
// The loop alternates between:
//  1. Draining all pending LISTEN/UNLISTEN commands from cmdCh.
//  2. Waiting for a Postgres notification with a short timeout so that
//     new commands are picked up promptly.
//
// On connection failure the Node calls log.Fatalf (ADR-0010).
func (s *Store) listenLoop(ctx context.Context) {
	for {
		// Drain all pending LISTEN/UNLISTEN commands before blocking.
	drain:
		for {
			select {
			case cmd := <-s.cmdCh:
				_, err := s.listener.Exec(context.Background(), cmd.sql)
				cmd.done <- err
			default:
				break drain
			}
		}

		// Wait for a notification, timing out quickly to re-check for commands.
		waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		notif, err := s.listener.WaitForNotification(waitCtx)
		cancel()

		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				if ctx.Err() != nil {
					return // clean shutdown via Close()
				}
				continue // timeout — loop back to drain commands
			}
			if ctx.Err() != nil {
				return
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
			default:
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
