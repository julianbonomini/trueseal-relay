package relay_test

// Cluster end-to-end tests.
//
// These tests simulate a two-node clustered deployment by creating two Router
// instances backed by the same Postgres store. Each Router acts as an
// independent Node. Cross-node delivery is the critical path: a Push arrives
// at Node B while the recipient's Receive Session is on Node A. Postgres
// LISTEN/NOTIFY wakes Node A and it delivers the blob.
//
// Tests are skipped when TEST_POSTGRES_DSN is not set.

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
	pgstore "github.com/julianbonomini/trueseal-relay/internal/store/postgres"
)

// testClusterDSN returns the Postgres DSN for cluster tests, skipping if unset.
func testClusterDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping cluster integration tests")
	}
	return dsn
}

// newClusterNode creates an independent Router + Store instance backed by
// the shared Postgres at dsn. Each call simulates a separate Node.
func newClusterNode(t *testing.T, dsn string) (*relay.Router, *pgstore.Store, noise.DHKey) {
	t.Helper()
	ctx := context.Background()
	s, err := pgstore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgstore.New (cluster node): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("relay key: %v", err)
	}
	router := relay.NewRouter(s, s, relay.DefaultTTL, 0)
	return router, s, relayKey
}

// truncateInbox wipes the inbox table before each cluster test for isolation.
func truncateInbox(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	s, err := pgstore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("truncateInbox: %v", err)
	}
	defer s.Close()
	if err := s.Truncate(ctx); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
}

// Cluster E2E: Push arrives at Node B while recipient's Receive Session is on
// Node A. Postgres LISTEN/NOTIFY crosses the node boundary and delivers the blob.
func TestClusterE2E_CrossNodeDelivery(t *testing.T) {
	dsn := testClusterDSN(t)
	truncateInbox(t, dsn)

	// Both nodes share the same relay keypair (same deployment).
	sharedRelayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("relay key: %v", err)
	}
	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}

	nodeA, _, _ := newClusterNode(t, dsn)
	nodeB, _, _ := newClusterNode(t, dsn)

	rrA := newReadyRouter(nodeA)

	// Device opens a Receive Session on Node A.
	clientReceive, serverReceive := net.Pipe()
	defer clientReceive.Close()
	go func() {
		session.AcceptReceive(serverReceive, sharedRelayKey, rrA) //nolint:errcheck
	}()
	cs2 := doXXHandshake(t, clientReceive, deviceKey)

	// Wait for Node A's OnReceiveConnect — subscription registered.
	select {
	case <-rrA.readyCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Node A: Receive Session not registered within deadline")
	}

	// Push arrives at Node B — different router, different store instance.
	envelope := []byte("cross-node-delivery")
	clientPush, serverPush := net.Pipe()
	go func() {
		session.AcceptPush(serverPush, sharedRelayKey, nodeB) //nolint:errcheck
	}()
	doPushSession(t, clientPush, sharedRelayKey.Public, deviceKey.Public, envelope)
	clientPush.Close()

	// Node A should receive the Postgres NOTIFY and deliver to the device.
	clientReceive.SetDeadline(time.Now().Add(4 * time.Second))
	raw := readMsg(t, clientReceive)
	plain, err := cs2.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt deliver: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeDeliver {
		t.Fatalf("want Deliver frame, got type=%02x ok=%v", typ, ok)
	}
	_, deliveredEnv, ok := session.DecodeDeliverBody(body)
	if !ok {
		t.Fatal("DecodeDeliverBody failed")
	}
	if string(deliveredEnv) != string(envelope) {
		t.Errorf("want %q, got %q", envelope, deliveredEnv)
	}
}

// Cluster E2E: blob pushed to Node B while device is offline.
// Device connects to Node A later — initial Peek drains the inbox
// without any NOTIFY needed.
func TestClusterE2E_OfflinePush_ConnectToOtherNode(t *testing.T) {
	dsn := testClusterDSN(t)
	truncateInbox(t, dsn)

	sharedRelayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("relay key: %v", err)
	}
	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}

	nodeA, _, _ := newClusterNode(t, dsn)
	nodeB, _, _ := newClusterNode(t, dsn)

	// Push to Node B — device offline.
	envelope := []byte("offline-cross-node")
	clientPush, serverPush := net.Pipe()
	go func() {
		session.AcceptPush(serverPush, sharedRelayKey, nodeB) //nolint:errcheck
	}()
	// Ack received → blob committed to shared Postgres.
	doPushSession(t, clientPush, sharedRelayKey.Public, deviceKey.Public, envelope)
	clientPush.Close()

	// Device connects to Node A (different from where push arrived).
	clientReceive, serverReceive := net.Pipe()
	defer clientReceive.Close()
	go func() {
		session.AcceptReceive(serverReceive, sharedRelayKey, nodeA) //nolint:errcheck
	}()
	cs2 := doXXHandshake(t, clientReceive, deviceKey)

	// Initial Peek on Node A drains the blob from shared Postgres.
	clientReceive.SetDeadline(time.Now().Add(3 * time.Second))
	raw := readMsg(t, clientReceive)
	plain, err := cs2.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt deliver: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeDeliver {
		t.Fatalf("want Deliver frame, got type=%02x ok=%v", typ, ok)
	}
	_, deliveredEnv, ok := session.DecodeDeliverBody(body)
	if !ok {
		t.Fatal("DecodeDeliverBody failed")
	}
	if string(deliveredEnv) != string(envelope) {
		t.Errorf("want %q, got %q", envelope, deliveredEnv)
	}
}

// Cluster E2E: DeliverAck processed by Node A deletes the blob from the shared
// Postgres inbox. Node B connects for the same device later and sees empty inbox.
func TestClusterE2E_AckDeletesFromSharedInbox(t *testing.T) {
	dsn := testClusterDSN(t)
	truncateInbox(t, dsn)

	sharedRelayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("relay key: %v", err)
	}
	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}

	var recipientKey relay.RecipientKey
	copy(recipientKey[:], deviceKey.Public)

	nodeA, storeA, _ := newClusterNode(t, dsn)
	_, storeB, _ := newClusterNode(t, dsn)

	// Push blob via Node A.
	envelope := []byte("ack-deletes-shared")
	clientPush, serverPush := net.Pipe()
	go func() {
		session.AcceptPush(serverPush, sharedRelayKey, nodeA) //nolint:errcheck
	}()
	doPushSession(t, clientPush, sharedRelayKey.Public, deviceKey.Public, envelope)
	clientPush.Close()

	// Peek from Node A's store to get the blob ID.
	ctx := context.Background()
	blobs, err := storeA.Peek(ctx, deviceKey.Public)
	if err != nil || len(blobs) != 1 {
		t.Fatalf("want 1 blob after push, got %d err=%v", len(blobs), err)
	}
	blobID := blobs[0].ID

	// Simulate device sending DeliverAck via Node A's router directly.
	if err := nodeA.OnDeliverAck(ctx, recipientKey, blobID); err != nil {
		t.Fatalf("OnDeliverAck: %v", err)
	}

	// Node B's store should now see an empty inbox for this device.
	remaining, err := storeB.Peek(ctx, deviceKey.Public)
	if err != nil {
		t.Fatalf("Peek on Node B after ack: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("want empty inbox on Node B after ack, got %d blobs", len(remaining))
	}

	_ = storeB // confirmed empty above
}
