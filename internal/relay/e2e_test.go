package relay_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

func newE2ERouter(t *testing.T) (*relay.Router, noise.DHKey) {
	t.Helper()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("relay key: %v", err)
	}

	router := relay.NewRouter(store, inprocess.New(), relay.DefaultTTL, 0)
	return router, relayKey
}

// readyRouter wraps a session.Handler and signals readyCh when
// OnReceiveConnect is called. This replaces time.Sleep synchronisation
// with a proper channel signal — the test waits on readyCh before pushing.
type readyRouter struct {
	inner   session.Handler
	readyCh chan struct{}
}

func newReadyRouter(inner session.Handler) *readyRouter {
	return &readyRouter{inner: inner, readyCh: make(chan struct{}, 1)}
}

func (rr *readyRouter) OnPush(ctx context.Context, body []byte) error {
	return rr.inner.OnPush(ctx, body)
}

func (rr *readyRouter) OnReceiveConnect(ctx context.Context, key relay.RecipientKey) <-chan relay.DeliveryBlob {
	ch := rr.inner.OnReceiveConnect(ctx, key)
	select {
	case rr.readyCh <- struct{}{}:
	default:
	}
	return ch
}

func (rr *readyRouter) OnDeliverAck(ctx context.Context, key relay.RecipientKey, blobID int64) error {
	return rr.inner.OnDeliverAck(ctx, key, blobID)
}

// E2E: device A pushes, device B (online) receives immediately.
func TestE2E_PushDeliverOnline(t *testing.T) {
	router, relayKey := newE2ERouter(t)

	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}

	// Wrap the router so we know when the Receive Session is registered.
	rr := newReadyRouter(router)

	// Device B opens a Receive Session
	clientReceive, serverReceive := net.Pipe()
	defer clientReceive.Close()
	receiveCtx, receiveCancel := context.WithCancel(context.Background())
	defer receiveCancel()
	go func() {
		session.AcceptReceive(serverReceive, relayKey, rr) //nolint:errcheck
	}()
	cs2 := doXXHandshake(t, clientReceive, deviceKey)

	// Wait for OnReceiveConnect to fire — no time.Sleep.
	select {
	case <-rr.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Receive Session not registered in router within deadline")
	}

	// Device A opens a Push Session and sends a blob to device B
	envelope := []byte("hello-device-b")
	clientPush, serverPush := net.Pipe()
	go func() {
		session.AcceptPush(serverPush, relayKey, router) //nolint:errcheck
	}()
	doPushSession(t, clientPush, relayKey.Public, deviceKey.Public, envelope)
	clientPush.Close()

	// Device B receives the Deliver frame
	clientReceive.SetDeadline(time.Now().Add(2 * time.Second))
	raw := readMsg(t, clientReceive)
	plain, err := cs2.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt deliver: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeDeliver {
		t.Fatalf("want Deliver, got type=%02x ok=%v", typ, ok)
	}
	_, deliveredEnv, ok := session.DecodeDeliverBody(body)
	if !ok {
		t.Fatal("DecodeDeliverBody failed")
	}
	if string(deliveredEnv) != string(envelope) {
		t.Errorf("want %q, got %q", envelope, deliveredEnv)
	}
	_ = receiveCtx
}

// E2E: blob pushed while device offline; device connects later and receives on connect.
func TestE2E_PushDeliverOffline(t *testing.T) {
	router, relayKey := newE2ERouter(t)

	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}

	// Push first — no Receive Session open yet
	envelope := []byte("offline-blob")
	clientPush, serverPush := net.Pipe()
	go func() {
		session.AcceptPush(serverPush, relayKey, router) //nolint:errcheck
	}()
	// doPushSession returns after the Ack is received, meaning the blob is
	// already committed to the store. No sleep needed after this point.
	doPushSession(t, clientPush, relayKey.Public, deviceKey.Public, envelope)
	clientPush.Close()

	// Device connects after the push
	clientReceive, serverReceive := net.Pipe()
	defer clientReceive.Close()
	go func() {
		session.AcceptReceive(serverReceive, relayKey, router) //nolint:errcheck
	}()
	cs2 := doXXHandshake(t, clientReceive, deviceKey)

	// Expect immediate flush on connect
	clientReceive.SetDeadline(time.Now().Add(2 * time.Second))
	raw := readMsg(t, clientReceive)
	plain, err := cs2.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt deliver: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeDeliver {
		t.Fatalf("want Deliver, got type=%02x ok=%v", typ, ok)
	}
	_, deliveredEnv, ok := session.DecodeDeliverBody(body)
	if !ok {
		t.Fatal("DecodeDeliverBody failed")
	}
	if string(deliveredEnv) != string(envelope) {
		t.Errorf("want %q, got %q", envelope, deliveredEnv)
	}
}

// E2E: TCP drops after Deliver frame written but before DeliverAck.
// Blob must be re-delivered on reconnect (ADR-0009).
func TestE2E_TCPDropBeforeAck_Redelivers(t *testing.T) {
	router, relayKey := newE2ERouter(t)

	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}

	// Push a blob
	envelope := []byte("drop-me")
	clientPush, serverPush := net.Pipe()
	go func() {
		session.AcceptPush(serverPush, relayKey, router) //nolint:errcheck
	}()
	doPushSession(t, clientPush, relayKey.Public, deviceKey.Public, envelope)
	clientPush.Close()

	// First Receive Session — receive Deliver but drop TCP (no DeliverAck)
	clientReceive1, serverReceive1 := net.Pipe()
	go func() {
		session.AcceptReceive(serverReceive1, relayKey, router) //nolint:errcheck
	}()
	cs2First := doXXHandshake(t, clientReceive1, deviceKey)

	clientReceive1.SetDeadline(time.Now().Add(2 * time.Second))
	raw := readMsg(t, clientReceive1)
	plain, err := cs2First.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt deliver (first session): %v", err)
	}
	typ, _, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeDeliver {
		t.Fatalf("want Deliver, got type=%02x", typ)
	}

	// Drop the TCP connection without sending DeliverAck
	clientReceive1.Close()
	time.Sleep(50 * time.Millisecond) // allow AcceptReceive to observe the close

	// Second Receive Session — blob must be re-delivered
	clientReceive2, serverReceive2 := net.Pipe()
	defer clientReceive2.Close()
	go func() {
		session.AcceptReceive(serverReceive2, relayKey, router) //nolint:errcheck
	}()
	cs2Second := doXXHandshake(t, clientReceive2, deviceKey)

	clientReceive2.SetDeadline(time.Now().Add(2 * time.Second))
	raw2 := readMsg(t, clientReceive2)
	plain2, err := cs2Second.Decrypt(nil, nil, raw2)
	if err != nil {
		t.Fatalf("decrypt deliver (second session): %v", err)
	}
	typ2, body2, ok := session.Parse(plain2)
	if !ok || typ2 != session.MsgTypeDeliver {
		t.Fatalf("want Deliver on reconnect, got type=%02x", typ2)
	}
	_, deliveredEnv, ok := session.DecodeDeliverBody(body2)
	if !ok {
		t.Fatal("DecodeDeliverBody failed on reconnect")
	}
	if string(deliveredEnv) != string(envelope) {
		t.Errorf("want %q re-delivered, got %q", envelope, deliveredEnv)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// doPushSession performs a full NK push: handshake + Push frame + receive Ack.
func doPushSession(t *testing.T, conn net.Conn, relayPub []byte, recipientPub []byte, envelope []byte) {
	t.Helper()
	cfg := noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  relayPub,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("NK hs: %v", err)
	}

	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("NK write e: %v", err)
	}
	writeMsg(t, conn, msg)

	resp := readMsg(t, conn)
	_, cs1, _, err := hs.ReadMessage(nil, resp)
	if err != nil {
		t.Fatalf("NK read resp: %v", err)
	}

	body := make([]byte, 32+len(envelope))
	copy(body[:32], recipientPub)
	copy(body[32:], envelope)

	frame, err := cs1.Encrypt(nil, nil, session.Frame(session.MsgTypePush, body))
	if err != nil {
		t.Fatalf("encrypt push: %v", err)
	}
	writeMsg(t, conn, frame)

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	readMsg(t, conn) // Ack
}

// doXXHandshake performs a Noise XX handshake as the device (initiator).
// Returns the responder→initiator CipherState for decrypting Deliver frames.
func doXXHandshake(t *testing.T, conn net.Conn, deviceKey noise.DHKey) *noise.CipherState {
	t.Helper()
	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		StaticKeypair: deviceKey,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("XX hs: %v", err)
	}

	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("XX write e: %v", err)
	}
	writeMsg(t, conn, msg)

	resp := readMsg(t, conn)
	if _, _, _, err = hs.ReadMessage(nil, resp); err != nil {
		t.Fatalf("XX read e,ee,s,es: %v", err)
	}

	msg, _, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("XX write s,se: %v", err)
	}
	writeMsg(t, conn, msg)
	return cs2
}
