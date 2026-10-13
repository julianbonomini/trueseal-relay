package relay_test

// RELAY-3 regression tests (#40, threat-model decision trueseal-roadmap#16):
// a push whose 32-byte routing prefix does not match the Envelope's own
// recipient_pub field must be refused with the permanent malformed code and
// never stored. Today the relay routes on the prefix and stores the rest
// verbatim without looking inside it — a pusher can file a Blob in an Inbox
// its Envelope isn't addressed to.
//
// The Envelope layout is pinned by the sync test vectors (ADR-0031):
// recipient_pub is proto field 3, a 32-byte length-delimited field.

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
	"github.com/julianbonomini/trueseal-relay/internal/vectors"
)

// vectorEnvelope returns the sync test vectors' sealed Envelope bytes and
// the recipient key it is addressed to (G5 anchor — the field layout this
// test relies on is pinned by the vectors, not assumed).
func vectorEnvelope(t *testing.T) (envelope []byte, recipient [32]byte) {
	t.Helper()
	v, _, err := vectors.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	envelope, err = hex.DecodeString(v.SealedEnvelope.EnvelopeProtoHex)
	if err != nil {
		t.Fatalf("envelopeProtoHex: %v", err)
	}
	recip, err := hex.DecodeString(v.SealedEnvelope.Inputs.RecipientNoisePub)
	if err != nil {
		t.Fatalf("recipientNoisePub: %v", err)
	}
	if len(recip) != 32 {
		t.Fatalf("recipientNoisePub is %d bytes, want 32", len(recip))
	}
	copy(recipient[:], recip)
	return envelope, recipient
}

// newPrefixRouter returns a Router backed by a store the test can inspect
// directly, so "refused" also means "provably never stored".
func newPrefixRouter(t *testing.T) (*relay.Router, *sqlitestore.Store) {
	t.Helper()
	st, err := sqlitestore.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return relay.NewRouter(st, inprocess.New(), relay.DefaultTTL, 0), st
}

func storedCount(t *testing.T, st *sqlitestore.Store, key []byte) int {
	t.Helper()
	blobs, err := st.PeekAfter(context.Background(), key, 0)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	return len(blobs)
}

// The defect: prefix A, Envelope addressed to B. Today the relay stores it
// in A's Inbox. After the fix it must be refused (malformed) and NOT stored.
func TestRouter_PushRoutingPrefixMismatch_Refused(t *testing.T) {
	r, st := newPrefixRouter(t)
	ctx := context.Background()

	envelope, addressedTo := vectorEnvelope(t)
	wrongPrefix := make32(0xEE)
	if wrongPrefix == addressedTo {
		t.Fatal("test setup: wrong prefix accidentally matches")
	}

	// Raw body: the real vector Envelope under a prefix that isn't its
	// recipient_pub.
	body := make([]byte, 32+len(envelope))
	copy(body[:32], wrongPrefix[:])
	copy(body[32:], envelope)
	err := r.OnPush(ctx, body)
	if !errors.Is(err, relay.ErrRecipientMismatch) {
		t.Fatalf("OnPush with mismatched prefix: err = %v, want ErrRecipientMismatch (maps to permanent malformed)", err)
	}
	if n := storedCount(t, st, wrongPrefix[:]); n != 0 {
		t.Fatalf("mismatched push stored %d blob(s) in the wrong Inbox", n)
	}
}

// A push whose routing prefix matches the Envelope's recipient_pub is
// accepted and stored — the happy path is unchanged.
func TestRouter_PushRoutingPrefixMatch_Accepted(t *testing.T) {
	r, st := newPrefixRouter(t)
	ctx := context.Background()

	envelope, addressedTo := vectorEnvelope(t)
	body := make([]byte, 32+len(envelope))
	copy(body[:32], addressedTo[:])
	copy(body[32:], envelope)
	if err := r.OnPush(ctx, body); err != nil {
		t.Fatalf("OnPush with matching prefix: %v", err)
	}
	if n := storedCount(t, st, addressedTo[:]); n != 1 {
		t.Fatalf("stored %d blobs, want 1", n)
	}
}

// Envelopes whose recipient_pub field is missing, short, long, duplicated
// or unparseable are refused malformed — the relay can never store an
// Envelope whose addressee it cannot verify.
func TestRouter_PushEnvelopeRecipient_Unverifiable_Refused(t *testing.T) {
	r, st := newPrefixRouter(t)
	ctx := context.Background()
	prefix := make32(0xAA)

	short := append([]byte{0x1a, 0x1f}, prefix[:31]...)
	long := append(append([]byte{0x1a, 0x21}, prefix[:]...), 0x00)
	duplicated := append(append([]byte{0x1a, 0x20}, prefix[:]...), append([]byte{0x1a, 0x20}, prefix[:]...)...)

	cases := map[string][]byte{
		// Only a payload field (field 6), no recipient_pub at all.
		"missing recipient_pub": {0x32, 0x03, 'f', 'o', 'o'},
		// recipient_pub present but 31 bytes instead of 32.
		"short recipient_pub": short,
		// recipient_pub present but 33 bytes.
		"long recipient_pub": long,
		// Two recipient_pub fields — ambiguous addressee.
		"duplicated recipient_pub": duplicated,
		// Field claims 32 bytes but the envelope ends early.
		"truncated mid-field": {0x1a, 0x20, 0x01, 0x02},
		// Field number 0 is invalid in protobuf — a real decoder (prost on
		// the client) rejects the whole message, so the relay refuses too.
		"invalid field number 0": append(append([]byte{0x1a, 0x20}, prefix[:]...), 0x00, 0x01),
		// Empty envelope.
		"empty envelope": {},
	}
	// These envelopes are deliberately NOT well-formed, so they can't go
	// through pushBody (which wraps its payload in a valid envelope) —
	// build the raw [prefix][envelope] body directly.
	rawBody := func(envelope []byte) []byte {
		body := make([]byte, 32+len(envelope))
		copy(body[:32], prefix[:])
		copy(body[32:], envelope)
		return body
	}
	for name, envelope := range cases {
		err := r.OnPush(ctx, rawBody(envelope))
		if !errors.Is(err, relay.ErrPushRecipientMalformed) {
			t.Errorf("%s: err = %v, want ErrPushRecipientMalformed (maps to permanent malformed)", name, err)
		}
		if n := storedCount(t, st, prefix[:]); n != 0 {
			t.Errorf("%s: stored %d blob(s); an unverifiable envelope must never be stored", name, n)
		}
	}
}
