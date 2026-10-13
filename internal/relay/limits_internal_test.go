package relay

import (
	"testing"
	"time"
)

// TestRateLimiter_FakeClock pins the bucket mechanics deterministically:
// burst tokens are consumed immediately, the sustained rate refills by
// elapsed time, and tokens never accumulate past the burst cap.
func TestRateLimiter_FakeClock(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	l := newRateLimiter(10, 3, clock) // 10 tokens/s, burst 3
	var key RecipientKey
	key[0] = 0x42

	for i := 0; i < 3; i++ {
		if !l.allow(key) {
			t.Fatalf("token %d of the burst refused", i+1)
		}
	}
	if l.allow(key) {
		t.Fatal("4th token with an empty bucket allowed")
	}

	now = now.Add(100 * time.Millisecond) // +1 token at 10/s
	if !l.allow(key) {
		t.Fatal("refilled token refused")
	}
	if l.allow(key) {
		t.Fatal("second token after only 100ms allowed")
	}

	// A long idle must not accumulate past the burst.
	now = now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if !l.allow(key) {
			t.Fatalf("burst token %d after long idle refused", i+1)
		}
	}
	if l.allow(key) {
		t.Fatal("tokens accumulated past the burst cap")
	}
}

// TestRateLimiter_SweepBoundsTheMap: past the soft cap, buckets idle longer
// than bucketIdleTTL are evicted; recently-used buckets survive.
func TestRateLimiter_SweepBoundsTheMap(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	l := newRateLimiter(1, 1, clock)

	// Grow past the soft cap with distinct keys.
	var key RecipientKey
	for i := 0; i < rateLimitSoftCap+2; i++ {
		key[0] = byte(i)
		key[1] = byte(i >> 8)
		l.allow(key)
	}
	grown := len(l.buckets)
	if grown < rateLimitSoftCap {
		t.Fatalf("setup: map holds %d buckets, want ≥ soft cap %d", grown, rateLimitSoftCap)
	}

	// One fresh key seen now survives; everything else is idle. Advance
	// past the idle TTL and insert once more to trigger the sweep.
	var fresh RecipientKey
	fresh[0] = 0xFF
	l.allow(fresh)
	now = now.Add(bucketIdleTTL + time.Minute)
	var trigger RecipientKey
	trigger[0] = 0xFE
	l.allow(trigger)

	if len(l.buckets) > 4 {
		t.Fatalf("sweep left %d buckets, want the idle ones evicted", len(l.buckets))
	}
	if _, ok := l.buckets[fresh]; ok {
		t.Fatal("the fresh bucket was itself idle past the TTL — it should have been evicted too after the advance")
	}
}

// TestRateLimiter_Disabled: a zero rate AND burst disables the limiter.
func TestRateLimiter_Disabled(t *testing.T) {
	l := newRateLimiter(0, 0, nil)
	var key RecipientKey
	for i := 0; i < 100; i++ {
		if !l.allow(key) {
			t.Fatal("disabled limiter refused a push")
		}
	}
}

// TestSessionCap_ConcurrentAcquire: exactly `max` of N racing acquires win
// — the check-and-increment cannot split-brain under a mutex.
func TestSessionCap_ConcurrentAcquire(t *testing.T) {
	c := newSessionCap(4)
	var key RecipientKey
	const racers = 32
	won := make(chan bool, racers)
	for i := 0; i < racers; i++ {
		go func() { won <- c.acquire(key) }()
	}
	wins := 0
	for i := 0; i < racers; i++ {
		if <-won {
			wins++
		}
	}
	if wins != 4 {
		t.Fatalf("%d racing acquires won, want exactly 4", wins)
	}
	// Releases pair with acquires; after all 4 release, a new acquire wins.
	for i := 0; i < 4; i++ {
		c.release(key)
	}
	if !c.acquire(key) {
		t.Fatal("acquire after all releases refused")
	}
}

// TestSessionCap_Disabled: max 0 means uncapped.
func TestSessionCap_Disabled(t *testing.T) {
	c := newSessionCap(0)
	var key RecipientKey
	for i := 0; i < 100; i++ {
		if !c.acquire(key) {
			t.Fatal("disabled cap refused a session")
		}
	}
}
