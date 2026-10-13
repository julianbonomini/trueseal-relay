package store

import (
	"context"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/logging"
)

// Reaper periodically calls InboxStore.Reap to delete envelopes whose TTL
// has elapsed. Reaping is policy, not data loss — see ADR-0003.
//
// Reap errors are logged but not fatal: the relay keeps running and the next
// interval will retry. An envelope that survives past its TTL due to a
// transient Reap error will be cleaned on the next successful cycle.
type Reaper struct {
	store    InboxStore
	interval time.Duration
	logger   *logging.Logger
}

// NewReaper returns a Reaper that calls store.Reap every interval.
func NewReaper(s InboxStore, interval time.Duration) *Reaper {
	return &Reaper{store: s, interval: interval, logger: logging.Nop()}
}

// SetLogger routes the Reaper's logging (RELAY-9). nil restores the
// default no-op logger.
func (r *Reaper) SetLogger(l *logging.Logger) {
	if l == nil {
		l = logging.Nop()
	}
	r.logger = l
}

// Run starts the reap loop. Blocks until ctx is cancelled.
// Intended to run in its own goroutine: go reaper.Run(ctx).
func (r *Reaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.store.Reap(ctx); err != nil {
				// A store error never wraps a peer address (ADR-0012).
				r.logger.Printf("reaper: Reap error: %v", err)
			} else {
				r.logger.Debugf("reaper: sweep complete")
			}
		}
	}
}
