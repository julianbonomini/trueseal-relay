package session_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/hush-relay/internal/relay"
	"github.com/julianbonomini/hush-relay/internal/session"
)

// receiveHandler records OnReceiveConnect calls and delivers blobs via a channel.
type receiveHandler struct {
	connectedKey relay.RecipientKey
	deliverCh    chan []byte
}

func newReceiveHandler() *receiveHandler {
	return &receiveHandler{deliverCh: make(chan []byte, 8)}
}

func (h *receiveHandler) OnPush(_ context.Context, _ []byte) error { return nil }

func (h *receiveHandler) OnReceiveConnect(_ context.Context, key relay.RecipientKey) <-chan []byte {
	h.connectedKey = key
	return h.deliverCh
}

// AcceptReceive: relay completes XX handshake and extracts device public key.
func TestAcceptReceive_ExtractsDeviceKey(t *testing.T) {
	relayKey, _ := noise.DH25519.GenerateKeypair(nil)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)

	client, server := net.Pipe()
	h := newReceiveHandler()

	go session.AcceptReceive(server, relayKey, h) //nolint:errcheck

	// Client: XX handshake
	cs1, cs2 := doXXHandshake(t, client, relayKey.Public, deviceKey)

	// Handler received device's public key
	time.Sleep(20 * time.Millisecond)
	if h.connectedKey != relay.RecipientKey(deviceKey.Public) {
		t.Errorf("want device key %x, got %x", deviceKey.Public, h.connectedKey)
	}

	client.Close()
	_, _ = cs1, cs2
}

// AcceptReceive: relay delivers blobs sent via the deliver channel.
func TestAcceptReceive_DeliversBlobs(t *testing.T) {
	relayKey, _ := noise.DH25519.GenerateKeypair(nil)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)

	client, server := net.Pipe()
	h := newReceiveHandler()

	go session.AcceptReceive(server, relayKey, h) //nolint:errcheck

	_, cs2 := doXXHandshake(t, client, relayKey.Public, deviceKey)

	time.Sleep(20 * time.Millisecond)

	// Push a blob via the deliver channel
	h.deliverCh <- []byte("blob-for-device")

	// Expect a Deliver frame
	client.SetDeadline(time.Now().Add(2 * time.Second))
	raw := readMsg(t, client)
	plain, err := cs2.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt deliver: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeDeliver {
		t.Errorf("want Deliver, got type=%02x ok=%v", typ, ok)
	}
	if string(body) != "blob-for-device" {
		t.Errorf("want 'blob-for-device', got %q", body)
	}

	client.Close()
}

// AcceptReceive: relay echoes Heartbeat frames.
func TestAcceptReceive_EchoesHeartbeat(t *testing.T) {
	relayKey, _ := noise.DH25519.GenerateKeypair(nil)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)

	client, server := net.Pipe()
	h := newReceiveHandler()

	go session.AcceptReceive(server, relayKey, h) //nolint:errcheck

	cs1, cs2 := doXXHandshake(t, client, relayKey.Public, deviceKey)

	time.Sleep(20 * time.Millisecond)

	// Send Heartbeat
	hb, _ := cs1.Encrypt(nil, nil, session.Frame(session.MsgTypeHeartbeat, nil))
	writeMsg(t, client, hb)

	// Expect Heartbeat echo
	client.SetDeadline(time.Now().Add(2 * time.Second))
	raw := readMsg(t, client)
	plain, err := cs2.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt heartbeat echo: %v", err)
	}
	typ, _, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeHeartbeat {
		t.Errorf("want Heartbeat echo, got type=%02x ok=%v", typ, ok)
	}

	client.Close()
}

// ── helpers ───────────────────────────────────────────────────────────────────

// doXXHandshake performs a Noise XX handshake as the initiator (device side).
// Returns (cs_initiator_send, cs_initiator_receive).
func doXXHandshake(t *testing.T, conn net.Conn, relayPub []byte, deviceKey noise.DHKey) (*noise.CipherState, *noise.CipherState) {
	t.Helper()
	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		StaticKeypair: deviceKey,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("XX handshake state: %v", err)
	}

	// → e
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("XX write e: %v", err)
	}
	writeMsg(t, conn, msg)

	// ← e, ee, s, es
	resp := readMsg(t, conn)
	if _, _, _, err = hs.ReadMessage(nil, resp); err != nil {
		t.Fatalf("XX read e,ee,s,es: %v", err)
	}

	// → s, se
	msg, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("XX write s,se: %v", err)
	}
	writeMsg(t, conn, msg)

	return cs1, cs2
}

func writeMsg(t *testing.T, conn net.Conn, msg []byte) {
	t.Helper()
	out := make([]byte, 2+len(msg))
	out[0] = byte(len(msg) >> 8)
	out[1] = byte(len(msg))
	copy(out[2:], msg)
	if _, err := conn.Write(out); err != nil {
		t.Fatalf("writeMsg: %v", err)
	}
}
