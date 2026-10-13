package session_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
)

// receiveHandler records OnReceiveConnect calls and delivers blobs via a channel.
// connectedCh is signalled (with the device key) when OnReceiveConnect fires,
// allowing tests to synchronise without time.Sleep.
type receiveHandler struct {
	connectedCh chan relay.RecipientKey
	deliverCh   chan relay.DeliveryBlob
}

func newReceiveHandler() *receiveHandler {
	return &receiveHandler{
		connectedCh: make(chan relay.RecipientKey, 1),
		deliverCh:   make(chan relay.DeliveryBlob, 8),
	}
}

func (h *receiveHandler) OnPush(_ context.Context, _ []byte) error { return nil }

func (h *receiveHandler) OnReceiveConnect(_ context.Context, key relay.RecipientKey) (<-chan relay.DeliveryBlob, error) {
	// Signal the key before returning so tests can synchronise via connectedCh.
	select {
	case h.connectedCh <- key:
	default:
	}
	return h.deliverCh, nil
}

func (h *receiveHandler) OnDeliverAck(_ context.Context, _ relay.RecipientKey, _ int64) error {
	return nil
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

	// Wait for OnReceiveConnect via channel — no time.Sleep, no data race.
	select {
	case got := <-h.connectedCh:
		if got != relay.RecipientKey(deviceKey.Public) {
			t.Errorf("want device key %x, got %x", deviceKey.Public, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnReceiveConnect was not called within deadline")
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

	// Wait for session to be established before delivering.
	select {
	case <-h.connectedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReceiveConnect not called within deadline")
	}

	// Push a blob via the deliver channel
	h.deliverCh <- relay.DeliveryBlob{BlobID: 42, Envelope: []byte("blob-for-device")}

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
	_, envelope, ok := session.DecodeDeliverBody(body)
	if !ok {
		t.Fatal("DecodeDeliverBody returned ok=false")
	}
	if string(envelope) != "blob-for-device" {
		t.Errorf("want 'blob-for-device', got %q", envelope)
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

	// Wait for session to be established before sending heartbeat.
	select {
	case <-h.connectedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReceiveConnect not called within deadline")
	}

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

// RELAY-7 regression: a DeliveryBlob already buffered on the deliver channel
// when the session opens must be DELIVERED to the device — not consumed and
// discarded by the subscribe-failure probe (issue #44; the discard breaks
// at-least-once delivery for that session, ADR-0009).
func TestAcceptReceive_EarlyBufferedBlobDelivered(t *testing.T) {
	relayKey, _ := noise.DH25519.GenerateKeypair(nil)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)

	client, server := net.Pipe()
	h := newReceiveHandler()

	// A blob is queued for delivery before the session finishes opening
	// (e.g. the router's initial inbox flush raced session setup).
	h.deliverCh <- relay.DeliveryBlob{BlobID: 7, Envelope: []byte("early-blob")}

	go session.AcceptReceive(server, relayKey, h) //nolint:errcheck

	_, cs2 := doXXHandshake(t, client, relayKey.Public, deviceKey)

	client.SetDeadline(time.Now().Add(2 * time.Second))
	raw := readMsg(t, client)
	plain, err := cs2.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt deliver: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeDeliver {
		t.Fatalf("want Deliver, got type=%02x ok=%v — the early blob was discarded", typ, ok)
	}
	id, envelope, ok := session.DecodeDeliverBody(body)
	if !ok {
		t.Fatal("DecodeDeliverBody returned ok=false")
	}
	if id != 7 || string(envelope) != "early-blob" {
		t.Errorf("want blob 7 'early-blob', got id=%d %q", id, envelope)
	}

	client.Close()
}

// AcceptReceive: if OnReceiveConnect reports a registration error (Subscribe
// failure), AcceptReceive must close the connection rather than silently
// continue unable to deliver (RELAY-7: explicit error, no channel probing).
func TestAcceptReceive_SubscribeFailureClosesConnection(t *testing.T) {
	relayKey, _ := noise.DH25519.GenerateKeypair(nil)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)

	client, server := net.Pipe()

	// failHandler returns an error, simulating Subscribe failure.
	failHandler := &subscribeFailHandler{}

	acceptDone := make(chan error, 1)
	go func() {
		acceptDone <- session.AcceptReceive(server, relayKey, failHandler)
	}()

	// Complete the XX handshake as the device
	_, _ = doXXHandshake(t, client, relayKey.Public, deviceKey)

	// AcceptReceive should detect the closed channel and return an error,
	// which closes the server-side conn. The client should see EOF.
	client.SetDeadline(time.Now().Add(2 * time.Second))
	var readErr error
	buf := make([]byte, 1)
	_, readErr = client.Read(buf)
	if readErr == nil {
		t.Error("want connection closed by relay on subscribe failure, got successful read")
	}

	// AcceptReceive should return (an error, not hang)
	select {
	case err := <-acceptDone:
		if err == nil {
			t.Error("want non-nil error from AcceptReceive on subscribe failure")
		}
	case <-time.After(2 * time.Second):
		t.Error("AcceptReceive did not return after subscribe failure")
	}
}

// subscribeFailHandler returns a pre-closed deliver channel to simulate Subscribe failure.
type subscribeFailHandler struct{}

func (h *subscribeFailHandler) OnPush(_ context.Context, _ []byte) error { return nil }
func (h *subscribeFailHandler) OnReceiveConnect(_ context.Context, _ relay.RecipientKey) (<-chan relay.DeliveryBlob, error) {
	return nil, errors.New("subscribe failed")
}
func (h *subscribeFailHandler) OnDeliverAck(_ context.Context, _ relay.RecipientKey, _ int64) error {
	return nil
}

// ── T4: Security tests ────────────────────────────────────────────────────────

// Noise XX does not reject unknown device keys — the relay accepts any device
// (any-to-any auth). What the relay DOES reject is a tampered handshake
// message: corrupted bytes cause AEAD authentication to fail.
//
// This test verifies that AcceptReceive returns an error when the third
// handshake message (s, se) is corrupted.
func TestAcceptReceive_TamperedHandshakeRejected(t *testing.T) {
	relayKey, _ := noise.DH25519.GenerateKeypair(nil)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)

	client, server := net.Pipe()

	acceptDone := make(chan error, 1)
	go func() {
		acceptDone <- session.AcceptReceive(server, relayKey, newReceiveHandler())
	}()

	// Perform the version exchange, then the first two XX messages normally
	prologue := clientVersionExchange(t, client)
	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		StaticKeypair: deviceKey,
		Prologue:      prologue,
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
	writeMsg(t, client, msg)

	// ← e, ee, s, es
	resp := readMsg(t, client)
	if _, _, _, err = hs.ReadMessage(nil, resp); err != nil {
		t.Fatalf("XX read e,ee,s,es: %v", err)
	}

	// Build the third message (s, se) but corrupt it before sending
	msg3, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("XX write s,se: %v", err)
	}
	// Flip the last byte to corrupt the AEAD authentication tag
	msg3[len(msg3)-1] ^= 0xFF
	writeMsg(t, client, msg3)

	// AcceptReceive must return an error (not hang, not accept the session)
	select {
	case err := <-acceptDone:
		if err == nil {
			t.Error("want error on tampered handshake, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Error("AcceptReceive did not return after tampered handshake")
	}
	client.Close()
}

// ── T5: Error path coverage ───────────────────────────────────────────────────

// AcceptReceive returns nil (not an error) when the connection is closed by
// the peer — this exercises isClosedErr.
func TestAcceptReceive_ClosedConnReturnsNil(t *testing.T) {
	relayKey, _ := noise.DH25519.GenerateKeypair(nil)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)

	client, server := net.Pipe()
	h := newReceiveHandler()

	acceptDone := make(chan error, 1)
	go func() {
		acceptDone <- session.AcceptReceive(server, relayKey, h)
	}()

	doXXHandshake(t, client, relayKey.Public, deviceKey)

	// Wait for the session to be established
	select {
	case <-h.connectedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReceiveConnect not called within deadline")
	}

	// Close the client — server reads return "use of closed network connection"
	client.Close()

	select {
	case err := <-acceptDone:
		if err != nil {
			t.Errorf("want nil for closed-conn error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("AcceptReceive did not return after connection closed")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────
// Returns (cs_initiator_send, cs_initiator_receive).
func doXXHandshake(t *testing.T, conn net.Conn, relayPub []byte, deviceKey noise.DHKey) (*noise.CipherState, *noise.CipherState) {
	t.Helper()
	prologue := clientVersionExchange(t, conn)
	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		StaticKeypair: deviceKey,
		Prologue:      prologue,
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

// ── RELAY-2: concurrent writers on one Receive Session ───────────────────────

// Regression test for RELAY-2 (issue #39): the delivery goroutine and the
// Heartbeat echo path must not corrupt the Receive Session stream when they
// write concurrently. flynn/noise CipherState is not safe for concurrent use
// (racing nonce counters produce frames the client cannot decrypt), and a
// two-Write length+body framing lets frames interleave on the wire.
//
// The test runs heavy Deliver traffic and Heartbeats together on one session
// and asserts the client decrypts and parses EVERY frame it receives. Deliver
// frames must all arrive (delivery guarantee); Heartbeat echoes may be dropped
// under load (a dropped echo is harmless — the client heartbeats every 25 s,
// ADR-0012), so only a minimum echo count is asserted. Under `-race`, the
// pre-fix code is also caught by the race detector on the shared CipherState.
func TestAcceptReceive_ConcurrentDeliverAndHeartbeat_StreamIntegrity(t *testing.T) {
	const delivers = 200
	const heartbeats = 200

	relayKey, _ := noise.DH25519.GenerateKeypair(nil)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)

	client, server := net.Pipe()
	h := newReceiveHandler()

	go session.AcceptReceive(server, relayKey, h) //nolint:errcheck

	cs1, cs2 := doXXHandshake(t, client, relayKey.Public, deviceKey)

	select {
	case <-h.connectedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReceiveConnect not called within deadline")
	}

	// Deliver traffic from the routing loop.
	go func() {
		for i := 0; i < delivers; i++ {
			h.deliverCh <- relay.DeliveryBlob{
				BlobID:   int64(1000 + i),
				Envelope: []byte(fmt.Sprintf("blob-%d", i)),
			}
		}
	}()

	// Concurrent Heartbeats from the client.
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		for i := 0; i < heartbeats; i++ {
			hb, err := cs1.Encrypt(nil, nil, session.Frame(session.MsgTypeHeartbeat, nil))
			if err != nil {
				return
			}
			out := make([]byte, 2+len(hb))
			out[0] = byte(len(hb) >> 8)
			out[1] = byte(len(hb))
			copy(out[2:], hb)
			if _, err := client.Write(out); err != nil {
				return // session torn down
			}
		}
	}()

	// Read until every Deliver has arrived (plus at least one echo, proving
	// the heartbeat path was exercised). Every received frame must decrypt
	// and parse — a single failure means the stream was corrupted.
	gotDelivers, gotEchoes := 0, 0
	client.SetDeadline(time.Now().Add(30 * time.Second))
	for gotDelivers < delivers || gotEchoes == 0 {
		raw := readMsg(t, client)
		plain, err := cs2.Decrypt(nil, nil, raw)
		if err != nil {
			t.Fatalf("frame %d: decrypt failed (stream corrupted): %v", gotDelivers+gotEchoes, err)
		}
		typ, body, ok := session.Parse(plain)
		if !ok {
			t.Fatalf("frame %d: malformed frame (boundaries interleaved)", gotDelivers+gotEchoes)
		}
		switch typ {
		case session.MsgTypeDeliver:
			_, envelope, ok := session.DecodeDeliverBody(body)
			if !ok {
				t.Fatalf("frame %d: undecodable Deliver body", gotDelivers)
			}
			gotDelivers++
			_ = envelope
		case session.MsgTypeHeartbeat:
			gotEchoes++
		default:
			t.Fatalf("frame %d: unexpected type %02x", gotDelivers+gotEchoes, typ)
		}
	}
	<-hbDone
	client.Close()
}
