package relay_test

// RELAY-9 (issue #29, ADR-0012 "Logging"): dev-mode logging at the
// routing layer. Dev mode may log key PREFIXES, sizes and Blob IDs with
// the running refusal totals; it never logs a full key, payload bytes or
// a peer address. Normal mode stays silent on every one of these paths
// (RELAY-4's guarantee, now enforced at the seam).

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/logging"
	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

func devTestRouter(t *testing.T, buf *bytes.Buffer, dev bool, limits relay.Limits) *relay.Router {
	t.Helper()
	st, err := sqlitestore.New(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := relay.NewRouterWithLimits(st, inprocess.New(), 30*24*time.Hour, 1024, limits)
	r.SetLogger(logging.New(buf, dev))
	return r
}

func devPushBody(t *testing.T, recipient [32]byte, payload []byte) []byte {
	t.Helper()
	env := protoEnvelope(recipient, payload)
	body := make([]byte, 32+len(env))
	copy(body[:32], recipient[:])
	copy(body[32:], env)
	return body
}

// Dev mode logs the accepted push's key prefix and size — and never the
// full 32-byte key or the payload bytes.
func TestDevLogs_AcceptedPush(t *testing.T) {
	var buf bytes.Buffer
	r := devTestRouter(t, &buf, true, relay.Limits{})

	var recipient [32]byte
	for i := range recipient {
		recipient[i] = 0xab
	}
	payload := []byte("payload-marker-should-never-be-logged")
	if err := r.OnPush(context.Background(), devPushBody(t, recipient, payload)); err != nil {
		t.Fatalf("OnPush: %v", err)
	}

	out := buf.String()
	prefix := hex.EncodeToString(recipient[:4])
	full := hex.EncodeToString(recipient[:])
	if !strings.Contains(out, prefix) {
		t.Errorf("dev log should name the key prefix %q: %q", prefix, out)
	}
	// The full key is the prefix repeated — count occurrences: the prefix
	// appears in `recipient=%x` with exactly 8 hex chars, so the full
	// 64-char key must never appear as a contiguous string.
	if strings.Contains(out, full) {
		t.Errorf("dev log contains the FULL device key: %q", out)
	}
	if !strings.Contains(out, "envelope=") {
		t.Errorf("dev log should name the envelope size: %q", out)
	}
	if strings.Contains(out, string(payload)) {
		t.Errorf("dev log contains payload bytes: %q", out)
	}
}

// Every refusal class logs its reason with a running total in dev mode —
// the C9 silent-refusal counters surfaced.
func TestDevLogs_RefusalsCounted(t *testing.T) {
	var buf bytes.Buffer
	// Rate limiter that refuses everything: positive rate, zero burst.
	r := devTestRouter(t, &buf, true, relay.Limits{PushRatePerSecond: 0.0001, PushRateBurst: 0})

	var recipient [32]byte
	recipient[0] = 0x01
	body := devPushBody(t, recipient, []byte("x"))

	for i := 0; i < 3; i++ {
		if err := r.OnPush(context.Background(), body); err == nil {
			t.Fatal("push should be rate limited")
		}
	}
	out := buf.String()
	if !strings.Contains(out, "rate limited") {
		t.Errorf("dev log should name the refusal reason: %q", out)
	}
	if !strings.Contains(out, "total 3") {
		t.Errorf("dev log should carry the running refusal total: %q", out)
	}
}

// The oversized refusal names the size and the limit.
func TestDevLogs_TooLarge(t *testing.T) {
	var buf bytes.Buffer
	r := devTestRouter(t, &buf, true, relay.Limits{})

	var recipient [32]byte
	recipient[0] = 0x02
	big := make([]byte, 2048) // router max is 1024 in devTestRouter
	if err := r.OnPush(context.Background(), devPushBody(t, recipient, big)); err == nil {
		t.Fatal("push should be refused as too large")
	}
	out := buf.String()
	if !strings.Contains(out, "too large") || !strings.Contains(out, "limit 1024") {
		t.Errorf("dev log should name size and limit: %q", out)
	}
}

// Normal mode: none of these lines exist at all (RELAY-4's guarantee
// holds through the new seam).
func TestNormalMode_NoPerMessageLines(t *testing.T) {
	var buf bytes.Buffer
	r := devTestRouter(t, &buf, false, relay.Limits{PushRatePerSecond: 0.0001, PushRateBurst: 0})

	var recipient [32]byte
	recipient[0] = 0x03
	_ = r.OnPush(context.Background(), devPushBody(t, recipient, []byte("x"))) // refused
	if out := buf.String(); out != "" {
		t.Errorf("normal mode emitted a per-message line: %q", out)
	}
}

// SetLogger(nil) restores the no-op default.
func TestSetLogger_NilRestoresNop(t *testing.T) {
	var buf bytes.Buffer
	r := devTestRouter(t, &buf, true, relay.Limits{})
	r.SetLogger(nil)
	var recipient [32]byte
	_ = r.OnPush(context.Background(), devPushBody(t, recipient, []byte("x")))
	if out := buf.String(); out != "" {
		t.Errorf("SetLogger(nil) should restore the no-op logger, got: %q", out)
	}
}
