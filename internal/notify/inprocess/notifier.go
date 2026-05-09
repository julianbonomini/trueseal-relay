package inprocess

import (
	"context"
	"encoding/hex"
	"sync"
)

// Notifier is an in-process implementation of notify.Notifier.
// Used in single-node deployments (SQLite). For clustering, swap for a
// Postgres LISTEN/NOTIFY adapter — see ADR-0007.
type Notifier struct {
	mu   sync.Mutex
	subs map[string][]chan struct{}
}

// New returns a ready Notifier.
func New() *Notifier {
	return &Notifier{subs: make(map[string][]chan struct{})}
}

// Notify signals all active subscribers for recipientKey.
// Signals are non-blocking — a subscriber that hasn't drained its channel
// is skipped (the pending signal is sufficient).
func (n *Notifier) Notify(recipientKey []byte) error {
	k := hex.EncodeToString(recipientKey)
	n.mu.Lock()
	chs := n.subs[k]
	n.mu.Unlock()
	for _, ch := range chs {
		select {
		case ch <- struct{}{}:
		default: // already has a pending signal; one flush covers all
		}
	}
	return nil
}

// Subscribe returns a channel that receives a struct{} each time Notify is
// called for recipientKey. The channel is closed when ctx is cancelled.
// Callers must consume or discard signals promptly.
func (n *Notifier) Subscribe(ctx context.Context, recipientKey []byte) (<-chan struct{}, error) {
	ch := make(chan struct{}, 1)
	k := hex.EncodeToString(recipientKey)

	n.mu.Lock()
	n.subs[k] = append(n.subs[k], ch)
	n.mu.Unlock()

	go func() {
		<-ctx.Done()
		n.mu.Lock()
		chs := n.subs[k]
		for i, c := range chs {
			if c == ch {
				n.subs[k] = append(chs[:i], chs[i+1:]...)
				break
			}
		}
		if len(n.subs[k]) == 0 {
			delete(n.subs, k)
		}
		n.mu.Unlock()
		close(ch)
	}()

	return ch, nil
}
