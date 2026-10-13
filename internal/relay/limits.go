package relay

import (
	"errors"
	"sync"
	"time"
)

// Refusal sentinels for the ADR-0012 abuse limits (RELAY-6). The session
// layer maps them onto temporary Error frame reason codes with errors.Is:
// ErrInboxFull → inboxFull (0x81), ErrRateLimited → rateLimited (0x82),
// ErrTooManyReceiveSessions → unavailable (0x83). All three are temporary:
// space frees up, buckets refill, sessions close.
var (
	// ErrInboxFull: the recipient's Inbox quota (blobs or bytes) is
	// exhausted. Wraps the store's refusal.
	ErrInboxFull = errors.New("relay: recipient inbox full")
	// ErrRateLimited: the per-recipient push rate limit fired.
	ErrRateLimited = errors.New("relay: push rate limited")
	// ErrTooManyReceiveSessions: the Device already has the configured
	// number of concurrent Receive Sessions; the new one is refused and
	// its connection closed (ADR-0012).
	ErrTooManyReceiveSessions = errors.New("relay: too many concurrent receive sessions for this device")
)

// rateLimitSoftCap bounds the bucket map between sweeps. Far above any
// realistic Inbox count on a self-hosted relay; sweeps evict idle buckets,
// so the map tracks active recipients, not every key ever seen.
const rateLimitSoftCap = 4096

// bucketIdleTTL is how long an untouched token bucket survives a sweep.
// Buckets refill fully within burst/rate seconds anyway, so an evicted
// bucket's state is indistinguishable from a fresh one moments later.
const bucketIdleTTL = 10 * time.Minute

// rateLimiter is a per-recipient token bucket (ADR-0012: sustained rate +
// burst, keyed on the recipient Inbox — never on a peer address, which the
// relay never reads). It is an in-memory, node-local limiter: clustered
// Nodes (experimental) each hold their own buckets, so the effective
// cluster-wide rate scales with the Node count — the same documented
// caveat the experimental Postgres PeekAfter carries.
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64 // bucket capacity
	now     func() time.Time
	buckets map[RecipientKey]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(ratePerSecond, burst float64, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{
		rate:    ratePerSecond,
		burst:   burst,
		now:     now,
		buckets: make(map[RecipientKey]*bucket),
	}
}

// allow reports whether one push to key's Inbox may proceed, consuming a
// token if so. A fresh bucket starts full (burst pushes arrive at once
// before the sustained rate binds).
func (l *rateLimiter) allow(key RecipientKey) bool {
	if l.rate <= 0 && l.burst <= 0 {
		return true // limiter disabled
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// Bound the map: sweep idle buckets once growth passes the soft cap.
	if len(l.buckets) > rateLimitSoftCap {
		cutoff := l.now().Add(-bucketIdleTTL)
		for k, b := range l.buckets {
			if b.last.Before(cutoff) {
				delete(l.buckets, k)
			}
		}
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: l.now()}
		l.buckets[key] = b
	}
	now := l.now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sessionCap tracks concurrent Receive Sessions per Device key
// (ADR-0012: at most N per key; a new one is refused and closed).
type sessionCap struct {
	mu     sync.Mutex
	max    int
	active map[RecipientKey]int
}

func newSessionCap(max int) *sessionCap {
	return &sessionCap{max: max, active: make(map[RecipientKey]int)}
}

// acquire takes a session slot for key. false means the cap is reached —
// check and increment happen under one lock, so concurrent connects cannot
// split-brain the count. A max of 0 disables the cap.
func (c *sessionCap) acquire(key RecipientKey) bool {
	if c.max <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[key] >= c.max {
		return false
	}
	c.active[key]++
	return true
}

// release returns a session slot. Every successful acquire pairs with
// exactly one release (the delivery goroutine's defer).
func (c *sessionCap) release(key RecipientKey) {
	if c.max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.active[key]; n <= 1 {
		delete(c.active, key)
	} else {
		c.active[key] = n - 1
	}
}
