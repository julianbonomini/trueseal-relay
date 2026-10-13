//go:build hostile

package hostile

import (
	"context"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
)

// These tests prove each RELAY-14 hostile action's effect on the Deliver
// frames a test client receives (issue #34 completion criteria). The
// wrapped channel IS the Deliver-frame payload source: the session layer
// frames each DeliveryBlob with session.EncodeDeliverBody, so asserting
// on (BlobID, Envelope) here is asserting on the bytes inside the client's
// Deliver frames. The corrupt test additionally frames the output to prove
// the wire-level effect.

var testKey = relay.RecipientKey{0xAA}

func blob(id int64, envelope string) relay.DeliveryBlob {
	return relay.DeliveryBlob{BlobID: id, Envelope: []byte(envelope)}
}

// feed wraps a fresh input channel and returns it with the wrapped output.
func feed(t *testing.T, e *Engine, rules Rules) (ctx context.Context, cancel context.CancelFunc, in chan<- relay.DeliveryBlob, out <-chan relay.DeliveryBlob) {
	t.Helper()
	if err := e.SetRules("*", rules); err != nil {
		t.Fatalf("SetRules: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	rawIn := make(chan relay.DeliveryBlob, 16)
	return ctx, cancel, rawIn, e.WrapSession(ctx, testKey, rawIn)
}

// recv reads one blob with a deadline.
func recv(t *testing.T, out <-chan relay.DeliveryBlob) relay.DeliveryBlob {
	t.Helper()
	select {
	case b, ok := <-out:
		if !ok {
			t.Fatalf("output channel closed unexpectedly")
		}
		return b
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for a delivered blob")
		return relay.DeliveryBlob{}
	}
}

// recvNone asserts no blob arrives within the window.
func recvNone(t *testing.T, out <-chan relay.DeliveryBlob) {
	t.Helper()
	select {
	case b, ok := <-out:
		if ok {
			t.Fatalf("expected no delivery, got blob %d", b.BlobID)
		}
		t.Fatalf("output channel closed unexpectedly")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestDropWithholdsDelivery(t *testing.T) {
	e := NewEngine(nil)
	_, cancel, in, out := feed(t, e, Rules{Drop: true})
	defer cancel()

	in <- blob(1, "withheld")
	recvNone(t, out)

	// Clearing the rule restores honest delivery for new blobs.
	e.ClearRules("*")
	in <- blob(2, "through")
	if got := recv(t, out); got.BlobID != 2 || string(got.Envelope) != "through" {
		t.Fatalf("after clear: got %d %q", got.BlobID, got.Envelope)
	}
}

func TestDuplicateEmitsSameBlobTwice(t *testing.T) {
	e := NewEngine(nil)
	_, cancel, in, out := feed(t, e, Rules{Duplicate: true})
	defer cancel()

	in <- blob(7, "dup-me")
	first := recv(t, out)
	second := recv(t, out)
	if first.BlobID != 7 || second.BlobID != 7 {
		t.Fatalf("duplicate must keep the blob ID: got %d and %d", first.BlobID, second.BlobID)
	}
	if string(first.Envelope) != "dup-me" || string(second.Envelope) != "dup-me" {
		t.Fatalf("duplicate must keep the bytes: %q / %q", first.Envelope, second.Envelope)
	}
	// The duplicate owns its backing array — mutating one must not
	// disturb the other (a shared slice would corrupt Noise encryption).
	first.Envelope[0] ^= 0xFF
	if string(second.Envelope) != "dup-me" {
		t.Fatalf("duplicate shares its backing array with the first copy")
	}
	recvNone(t, out)
}

func TestReorderSwapsWithinWindow(t *testing.T) {
	e := NewEngine(nil)
	_, cancel, in, out := feed(t, e, Rules{ReorderWindow: 2})
	defer cancel()

	in <- blob(1, "first")
	in <- blob(2, "second")
	// Window full → emitted in reverse arrival order.
	if got := recv(t, out); got.BlobID != 2 {
		t.Fatalf("reorder: expected blob 2 first, got %d", got.BlobID)
	}
	if got := recv(t, out); got.BlobID != 1 {
		t.Fatalf("reorder: expected blob 1 second, got %d", got.BlobID)
	}
}

func TestReorderFlushesHeldBlobOnClose(t *testing.T) {
	e := NewEngine(nil)
	_, cancel, in, out := feed(t, e, Rules{ReorderWindow: 3})
	defer cancel()

	in <- blob(1, "held")
	recvNone(t, out) // held: window not full
	close(in)
	// The routing loop's close flushes held blobs (in reverse order) and
	// then propagates the close — a held blob is never silently lost.
	if got := recv(t, out); got.BlobID != 1 {
		t.Fatalf("flush: expected held blob 1, got %d", got.BlobID)
	}
	select {
	case _, ok := <-out:
		if ok {
			t.Fatalf("expected channel close after flush")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("channel not closed after input close")
	}
}

func TestCorruptMutatesTheDeliverFrameBody(t *testing.T) {
	e := NewEngine(nil)
	_, cancel, in, out := feed(t, e, Rules{Corrupt: true})
	defer cancel()

	original := "an-envelope-the-client-signed"
	in <- blob(9, original)
	got := recv(t, out)
	if got.BlobID != 9 {
		t.Fatalf("corrupt must keep the blob ID: got %d", got.BlobID)
	}
	if string(got.Envelope) == original {
		t.Fatalf("corrupt did not change the envelope bytes")
	}
	if len(got.Envelope) != len(original) {
		t.Fatalf("corrupt must not resize the envelope: %d != %d", len(got.Envelope), len(original))
	}
	// The store's bytes are immutable: exactly one byte differs.
	diff := 0
	for i := range got.Envelope {
		if got.Envelope[i] != original[i] {
			diff++
		}
	}
	if diff != 1 {
		t.Fatalf("expected exactly one flipped byte, got %d", diff)
	}

	// Wire-level proof: framed exactly as the session layer frames it,
	// the Deliver body the client decrypts carries the corrupted bytes.
	body := session.EncodeDeliverBody(uint64(got.BlobID), got.Envelope)
	if string(body[8:]) == original {
		t.Fatalf("framed Deliver body is uncorrupted")
	}
	if string(body[8:]) != string(got.Envelope) {
		t.Fatalf("framed body mismatch")
	}
}

func TestInjectDeliversHarnessBytesUnderSyntheticID(t *testing.T) {
	e := NewEngine(nil)
	_, cancel, _, out := feed(t, e, Rules{})
	defer cancel()

	harnessBytes := []byte("manifest-bytes-from-the-adversary-client")
	if err := e.Inject(testKey, harnessBytes); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	got := recv(t, out)
	if got.BlobID < syntheticBlobIDBase {
		t.Fatalf("injected blob ID %d is not in the synthetic range (>= %d)", got.BlobID, syntheticBlobIDBase)
	}
	if string(got.Envelope) != string(harnessBytes) {
		t.Fatalf("injected envelope mismatch: %q", got.Envelope)
	}

	// Inject into a device with no live session is a typed error, not a
	// silent drop.
	var noSession relay.RecipientKey
	if err := e.Inject(noSession, harnessBytes); err == nil {
		t.Fatalf("Inject without a live session must fail")
	}
}

func TestReplayRedeliversWithOriginalID(t *testing.T) {
	e := NewEngine(nil)
	_, cancel, in, out := feed(t, e, Rules{})
	defer cancel()

	in <- blob(11, "once")
	in <- blob(12, "twice")
	if got := recv(t, out); got.BlobID != 11 {
		t.Fatalf("setup: got %d", got.BlobID)
	}
	if got := recv(t, out); got.BlobID != 12 {
		t.Fatalf("setup: got %d", got.BlobID)
	}

	// Replay the last one: same blob ID, same bytes — the client's dedup
	// must absorb it.
	if err := e.Replay(testKey, 1); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got := recv(t, out)
	if got.BlobID != 12 || string(got.Envelope) != "twice" {
		t.Fatalf("replay: got %d %q, want 12 \"twice\"", got.BlobID, got.Envelope)
	}

	// Replay the whole history (count 0): both come again, oldest first.
	if err := e.Replay(testKey, 0); err != nil {
		t.Fatalf("Replay all: %v", err)
	}
	if got := recv(t, out); got.BlobID != 11 {
		t.Fatalf("replay all: got %d, want 11", got.BlobID)
	}
	if got := recv(t, out); got.BlobID != 12 {
		t.Fatalf("replay all: got %d, want 12", got.BlobID)
	}
}

func TestExactKeyRulesBeatWildcard(t *testing.T) {
	e := NewEngine(nil)
	if err := e.SetRules("*", Rules{Drop: true}); err != nil {
		t.Fatal(err)
	}
	exact := relay.RecipientKey{0xBB}
	if err := e.SetRules(hexKey(exact), Rules{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan relay.DeliveryBlob, 4)
	out := e.WrapSession(ctx, exact, in)
	in <- blob(1, "honest-for-this-device")
	if got := recv(t, out); got.BlobID != 1 {
		t.Fatalf("exact rules must override the wildcard: got %d", got.BlobID)
	}
}

func TestSetRulesRejectsBadSelector(t *testing.T) {
	e := NewEngine(nil)
	if err := e.SetRules("not-hex", Rules{}); err == nil {
		t.Fatalf("a non-hex, non-wildcard selector must be refused")
	}
}

func hexKey(k relay.RecipientKey) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range k {
		out[2*i] = digits[b>>4]
		out[2*i+1] = digits[b&0xF]
	}
	return string(out)
}
