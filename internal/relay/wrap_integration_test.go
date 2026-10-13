//go:build hostile

package relay_test

// Router-level integration for RELAY-14: the hostile engine, installed via
// SetDeliveryWrapper (a symbol that exists only in `hostile` tag builds),
// transforms the DeliveryBlobs a Receive Session receives — and the
// peek-and-ack Inbox survives the hostility (a dropped blob re-delivers on
// the next session once the rule is cleared).

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/hostile"
	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

func recvBlob(t *testing.T, ch <-chan relay.DeliveryBlob) relay.DeliveryBlob {
	t.Helper()
	select {
	case b, ok := <-ch:
		if !ok {
			t.Fatalf("delivery channel closed unexpectedly")
		}
		return b
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for a blob")
		return relay.DeliveryBlob{}
	}
}

func recvNoBlob(t *testing.T, ch <-chan relay.DeliveryBlob) {
	t.Helper()
	select {
	case b, ok := <-ch:
		if ok {
			t.Fatalf("expected no delivery, got blob %d", b.BlobID)
		}
		t.Fatalf("delivery channel closed unexpectedly")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRouter_HostileEngineTransformsDelivery(t *testing.T) {
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	router := relay.NewRouter(store, inprocess.New(), relay.DefaultTTL, relay.ProtocolMaxEnvelopeBytes)

	engine := hostile.NewEngine(nil)
	defer engine.Close()
	router.SetDeliveryWrapper(engine)

	key := relay.RecipientKey{0x42}
	ctx, cancel := context.WithCancel(context.Background())

	// Rule on, session open, push arrives over the notify path: the drop
	// rule withholds it.
	if err := engine.SetRules("*", hostile.Rules{Drop: true}); err != nil {
		t.Fatal(err)
	}
	ch, err := router.OnReceiveConnect(ctx, key)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := router.OnPush(ctx, pushBody(key, []byte("withheld-payload"))); err != nil {
		t.Fatalf("push: %v", err)
	}
	recvNoBlob(t, ch)

	// Clear the rule and reconnect: the withheld blob was never acked, so
	// the Inbox re-delivers it on the new session (ADR-0009 survives
	// hostility — the client-side guarantee G3 builds on).
	engine.ClearRules("*")
	cancel()
	// Drain the torn-down session's channel close.
	select {
	case _, ok := <-ch:
		if ok {
			// A blob may have been in flight; the store still holds it.
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("first session did not tear down")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ch2, err := router.OnReceiveConnect(ctx2, key)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	got := recvBlob(t, ch2)
	if string(got.Envelope) != string(protoEnvelope(key, []byte("withheld-payload"))) {
		t.Fatalf("re-delivered envelope mismatch: %q", got.Envelope)
	}
}

func TestRouter_HostileInjectReachesTheSession(t *testing.T) {
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	router := relay.NewRouter(store, inprocess.New(), relay.DefaultTTL, relay.ProtocolMaxEnvelopeBytes)

	engine := hostile.NewEngine(nil)
	defer engine.Close()
	router.SetDeliveryWrapper(engine)

	key := relay.RecipientKey{0x43}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := router.OnReceiveConnect(ctx, key)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// The relay cannot sign: the harness (Adversary Client) supplies the
	// bytes of a fake Manifest or Sync envelope, and the hostile relay
	// files them straight into the live session.
	harnessBytes := protoEnvelope(key, []byte("injected-by-the-harness"))
	if err := engine.Inject(key, harnessBytes); err != nil {
		t.Fatalf("inject: %v", err)
	}
	got := recvBlob(t, ch)
	if string(got.Envelope) != string(harnessBytes) {
		t.Fatalf("injected envelope mismatch")
	}

	// The client's DeliverAck of the synthetic ID is a silent store no-op
	// (RELAY-1): nothing errors, nothing else is deleted.
	if err := router.OnDeliverAck(ctx, key, got.BlobID); err != nil {
		t.Fatalf("ack of synthetic ID must be a no-op, got: %v", err)
	}
}
