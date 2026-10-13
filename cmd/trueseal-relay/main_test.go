package main

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
)

type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLogConnectionRefused_NoAddress is the RELAY-4 regression test for the
// refused-connection path (issue #41, ADR-0012): the relay never logs a
// client address, even when rejecting a connection at the limit.
//
// RED on the defect: the pre-fix refusal line included conn.RemoteAddr().
func TestLogConnectionRefused_NoAddress(t *testing.T) {
	var lb lockedBuf
	prev := log.Writer()
	log.SetOutput(&lb)
	defer log.SetOutput(prev)

	// The helper takes no peer address at all (ADR-0012: the relay never
	// logs, stores or uses IPs). The banned strings mirror what a refusal
	// line would contain if a RemoteAddr leaked back in.
	logConnectionRefused("push", 1000)

	out := lb.String()
	if !strings.Contains(out, "push") || !strings.Contains(out, "1000") {
		t.Errorf("refusal log lost its operator-useful content: %q", out)
	}
	for _, banned := range []string{"203.0.113.7", "5555", "addr="} {
		if strings.Contains(out, banned) {
			t.Errorf("refusal log contains client address %q: %q", banned, out)
		}
	}
}
