package relay_test

// Relay-level vector checks (G5): the routing loop's size refusal agrees
// with the numbers the vectors pin.

import (
	"context"
	"errors"
	"testing"

	"github.com/julianbonomini/trueseal-relay/internal/config"
	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
	"github.com/julianbonomini/trueseal-relay/internal/vectors"
)

// The relay's default envelope ceiling IS the vectors' maxEnvelopeBytes —
// the number the client's send() fits under (ADR-0025: the relay enforces
// the same ceiling and may only be configured lower).
func TestVectors_EnvelopeCeilingMatchesDefault(t *testing.T) {
	v, _, err := vectors.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.DefaultMaxEnvelopeBytes != int64(v.Protocol.MaxEnvelopeBytes) {
		t.Fatalf("DefaultMaxEnvelopeBytes = %d, vectors pin maxEnvelopeBytes %d — the two layers of ADR-0025 disagree",
			config.DefaultMaxEnvelopeBytes, v.Protocol.MaxEnvelopeBytes)
	}
}

// A push whose envelope exceeds the ceiling is refused with the sentinel the
// session layer maps to the permanent tooLarge reason code.
func TestVectors_OversizedPushRefusedTooLarge(t *testing.T) {
	v, rejections, err := vectors.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	store, err := sqlitestore.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	r := relay.NewRouter(store, inprocess.New(), relay.DefaultTTL, int64(v.Protocol.MaxEnvelopeBytes))

	// One byte over the ceiling: the size refusal fires before the
	// envelope is parsed, so a raw oversized envelope suffices.
	body := make([]byte, 32+v.Protocol.MaxEnvelopeBytes+1)
	err = r.OnPush(context.Background(), body)
	if !errors.Is(err, relay.ErrEnvelopeTooLarge) {
		t.Fatalf("OnPush over the ceiling: err = %v, want ErrEnvelopeTooLarge (maps to reason 0x01 tooLarge)", err)
	}

	// And exactly AT the ceiling a well-formed envelope is accepted
	// (boundary, both sides) — pushBodySized builds one of an exact size.
	if err := r.OnPush(context.Background(), pushBodySized(make32(0xCC), v.Protocol.MaxEnvelopeBytes)); err != nil {
		t.Fatalf("OnPush at the ceiling: %v", err)
	}

	// The vectors' size rejection names the client's typed error; the relay
	// speaks the same ceiling, so the case anchors both ends.
	var found bool
	for _, rj := range rejections {
		if rj.Layer == "size" {
			found = true
		}
	}
	if !found {
		t.Fatal("no size-layer rejection in the vectors")
	}
}
