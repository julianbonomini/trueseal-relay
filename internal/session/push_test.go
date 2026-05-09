package session_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/hush-relay/internal/relay"
	"github.com/julianbonomini/hush-relay/internal/session"
)

// mockHandler records OnPush calls. OnReceiveConnect is not used in push tests.
type mockHandler struct {
	pushed [][]byte
	err    error
}

func (m *mockHandler) OnPush(_ context.Context, envelope []byte) error {
	m.pushed = append(m.pushed, envelope)
	return m.err
}

func (m *mockHandler) OnReceiveConnect(_ context.Context, _ relay.RecipientKey) <-chan []byte {
	ch := make(chan []byte)
	close(ch)
	return ch
}

// noiseConfig returns a standard Noise config for tests.
func noiseCfg(staticKey noise.DHKey) noise.Config {
	return noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Random:      nil, // use crypto/rand
		Pattern:     noise.HandshakeXX,
		StaticKeypair: staticKey,
	}
}

// dialNK opens a Noise NK session to the relay (client side of push).
func dialNK(conn net.Conn, relayPub []byte) (*noise.HandshakeState, error) {
	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeNK,
		Initiator:     true,
		PeerStatic:    relayPub,
	}
	hs, err := noise.NewHandshakeState(cfg)
	return hs, err
}

// AcceptPush: relay receives a Push frame, calls OnPush, sends Ack.
func TestAcceptPush_ReceivesEnvelopeAndAcks(t *testing.T) {
	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("generate relay key: %v", err)
	}

	client, server := net.Pipe()
	handler := &mockHandler{}

	go func() {
		if err := session.AcceptPush(server, relayKey, handler); err != nil {
			t.Logf("AcceptPush: %v", err) // expected after client closes
		}
	}()

	// Client: NK handshake → send Push frame → expect Ack
	cfg := noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  relayKey.Public,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("NK handshake state: %v", err)
	}

	// NK: initiator sends first, then reads
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("NK write e: %v", err)
	}
	if _, err := client.Write(encodeHandshakeMsg(msg)); err != nil {
		t.Fatalf("write e: %v", err)
	}

	resp := readHandshakeMsg(t, client)
	var cs1, cs2 *noise.CipherState
	_, cs1, cs2, err = hs.ReadMessage(nil, resp)
	if err != nil {
		t.Fatalf("NK read: %v", err)
	}

	// Send Push frame
	env := []byte("raw-envelope-bytes")
	push := session.Frame(session.MsgTypePush, env)
	encrypted, err := cs1.Encrypt(nil, nil, push)
	if err != nil {
		t.Fatalf("encrypt push: %v", err)
	}
	if _, err := client.Write(encodeMsg(encrypted)); err != nil {
		t.Fatalf("write push: %v", err)
	}

	// Expect Ack
	client.SetDeadline(time.Now().Add(2 * time.Second))
	ackRaw := readMsg(t, client)
	ackDecrypted, err := cs2.Decrypt(nil, nil, ackRaw)
	if err != nil {
		t.Fatalf("decrypt ack: %v", err)
	}
	typ, _, ok := session.Parse(ackDecrypted)
	if !ok || typ != session.MsgTypeAck {
		t.Errorf("want Ack, got type=%02x ok=%v", typ, ok)
	}

	client.Close()

	// Handler received the envelope
	time.Sleep(10 * time.Millisecond)
	if len(handler.pushed) != 1 || string(handler.pushed[0]) != string(env) {
		t.Errorf("want pushed=[%q], got %v", env, handler.pushed)
	}
	_ = cs2
}

// AcceptPush: ctx passed to OnPush is connection-scoped and cancelled when AcceptPush returns.
func TestAcceptPush_CtxCancelledOnConnClose(t *testing.T) {
	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("generate relay key: %v", err)
	}

	client, server := net.Pipe()

	ctxCh := make(chan context.Context, 1)
	// recordCtxHandler captures the ctx from the first OnPush call.
	recordCtxHandler := &recordOnPushCtxHandler{ctxCh: ctxCh}

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		if err := session.AcceptPush(server, relayKey, recordCtxHandler); err != nil {
			t.Logf("AcceptPush: %v", err)
		}
	}()

	// Perform NK handshake
	cfg := noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  relayKey.Public,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("NK handshake state: %v", err)
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("NK write e: %v", err)
	}
	if _, err := client.Write(encodeHandshakeMsg(msg)); err != nil {
		t.Fatalf("write e: %v", err)
	}
	resp := readHandshakeMsg(t, client)
	_, cs1, _, err := hs.ReadMessage(nil, resp)
	if err != nil {
		t.Fatalf("NK read: %v", err)
	}

	// Send a Push frame
	env := make([]byte, 33) // 32-byte recipient key + 1 byte payload
	push := session.Frame(session.MsgTypePush, env)
	encrypted, err := cs1.Encrypt(nil, nil, push)
	if err != nil {
		t.Fatalf("encrypt push: %v", err)
	}
	if _, err := client.Write(encodeMsg(encrypted)); err != nil {
		t.Fatalf("write push: %v", err)
	}

	// Wait for handler to capture the ctx
	var pushedCtx context.Context
	select {
	case pushedCtx = <-ctxCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never received ctx")
	}

	// Verify ctx has a Done channel (i.e. it is cancellable, not context.Background())
	if pushedCtx.Done() == nil {
		t.Error("want cancellable ctx (Done != nil), got context.Background() or equivalent")
	}

	// Close connection — AcceptPush should return and cancel the ctx via defer.
	client.Close()

	// Wait for AcceptPush to fully return
	select {
	case <-acceptDone:
	case <-time.After(2 * time.Second):
		t.Fatal("AcceptPush did not return after connection closed")
	}

	// After AcceptPush returns, the connection-scoped ctx must be Done.
	select {
	case <-pushedCtx.Done():
		// good — ctx was cancelled when AcceptPush returned
	case <-time.After(100 * time.Millisecond):
		t.Error("want ctx cancelled after AcceptPush returns, timed out")
	}
}

// recordOnPushCtxHandler records the first ctx passed to OnPush.
type recordOnPushCtxHandler struct {
	ctxCh chan context.Context
}

func (h *recordOnPushCtxHandler) OnPush(ctx context.Context, _ []byte) error {
	select {
	case h.ctxCh <- ctx:
	default:
	}
	return nil
}

func (h *recordOnPushCtxHandler) OnReceiveConnect(_ context.Context, _ relay.RecipientKey) <-chan []byte {
	ch := make(chan []byte)
	close(ch)
	return ch
}

// ── T4: Security tests ───────────────────────────────────────────────────────

// AcceptPush: client uses the wrong relay public key in the NK handshake.
// The NK pattern authenticates message 1 with DH(e_client, s_relay). If the
// client has the wrong static key, the server rejects the AEAD tag in msg1
// and closes the connection — the client sees EOF when reading the response.
func TestAcceptPush_WrongRelayKeyRejected(t *testing.T) {
	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("generate relay key: %v", err)
	}
	wrongKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("generate wrong key: %v", err)
	}

	client, server := net.Pipe()
	handler := &mockHandler{}

	acceptDone := make(chan error, 1)
	go func() {
		acceptDone <- session.AcceptPush(server, relayKey, handler)
	}()

	// Client initiates NK using the wrong relay public key
	cfg := noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  wrongKey.Public, // wrong key
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("NK handshake state: %v", err)
	}

	// NK msg1: e, es (AEAD tag computed with wrong DH)
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("NK write e: %v", err)
	}
	client.Write(encodeMsg(msg)) //nolint:errcheck

	// The server rejects msg1 (AEAD auth fail with wrong DH) and closes
	// the connection. The client must NOT be able to complete the handshake.
	client.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2)
	_, readErr := io.ReadFull(client, buf)
	if readErr == nil {
		// Got server response — verify the client cannot authenticate it
		respLen := int(buf[0])<<8 | int(buf[1])
		respBody := make([]byte, respLen)
		io.ReadFull(client, respBody) //nolint:errcheck
		_, _, _, authErr := hs.ReadMessage(nil, respBody)
		if authErr == nil {
			t.Error("want handshake authentication failure with wrong relay key, got success")
		}
	}
	// readErr != nil means server closed before responding — also a rejection.

	client.Close()
	// AcceptPush should have returned an error (not nil) due to rejection
	select {
	case err := <-acceptDone:
		if err == nil {
			t.Error("want AcceptPush to return error when rejecting handshake, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Error("AcceptPush did not return after rejected handshake")
	}
}

// ── T5: Error path coverage ───────────────────────────────────────────────────

// AcceptPush returns nil (not an error) when the connection is closed by the
// peer — this exercises isClosedErr.
func TestAcceptPush_ClosedConnReturnsNil(t *testing.T) {
	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("generate relay key: %v", err)
	}

	client, server := net.Pipe()
	handler := &mockHandler{}

	acceptDone := make(chan error, 1)
	go func() {
		acceptDone <- session.AcceptPush(server, relayKey, handler)
	}()

	// Complete the NK handshake
	cfg := noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  relayKey.Public,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("NK handshake state: %v", err)
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("NK write e: %v", err)
	}
	client.Write(encodeMsg(msg)) //nolint:errcheck
	readMsg(t, client)           // server response

	// Close without sending any Push frame
	client.Close()

	select {
	case err := <-acceptDone:
		if err != nil {
			t.Errorf("want nil for closed-conn error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("AcceptPush did not return after connection closed")
	}
}

// AcceptPush does NOT send Ack when OnPush returns an error.
// This verifies that the store-fail path in push.go skips the Ack.
func TestAcceptPush_NoAckOnOnPushError(t *testing.T) {
	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("generate relay key: %v", err)
	}

	client, server := net.Pipe()
	handler := &mockHandler{err: errors.New("store failed")}

	go func() {
		session.AcceptPush(server, relayKey, handler) //nolint:errcheck
	}()

	// NK handshake
	cfg := noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  relayKey.Public,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("NK handshake state: %v", err)
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("NK write e: %v", err)
	}
	client.Write(encodeMsg(msg)) //nolint:errcheck
	resp := readMsg(t, client)
	_, cs1, _, err := hs.ReadMessage(nil, resp)
	if err != nil {
		t.Fatalf("NK read resp: %v", err)
	}

	// Send Push frame
	env := []byte("raw-envelope-bytes")
	push := session.Frame(session.MsgTypePush, env)
	encrypted, err := cs1.Encrypt(nil, nil, push)
	if err != nil {
		t.Fatalf("encrypt push: %v", err)
	}
	client.Write(encodeMsg(encrypted)) //nolint:errcheck

	// No Ack should arrive — handler returned an error so Ack is withheld
	client.SetDeadline(time.Now().Add(100 * time.Millisecond))
	_, readErr := io.ReadFull(client, make([]byte, 2))
	if readErr == nil {
		t.Error("want no Ack when OnPush returns error, but received data")
	}

	client.Close()
}

// AcceptPush handles multiple Push frames in a single session,
// sending one Ack per Push in order.
func TestAcceptPush_MultiplePushFrames(t *testing.T) {
	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("generate relay key: %v", err)
	}

	client, server := net.Pipe()
	handler := &mockHandler{}

	go func() {
		session.AcceptPush(server, relayKey, handler) //nolint:errcheck
	}()

	// NK handshake
	cfg := noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  relayKey.Public,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("NK handshake state: %v", err)
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("NK write e: %v", err)
	}
	client.Write(encodeMsg(msg)) //nolint:errcheck
	resp := readMsg(t, client)
	_, cs1, cs2, err := hs.ReadMessage(nil, resp)
	if err != nil {
		t.Fatalf("NK read resp: %v", err)
	}

	const nFrames = 3
	envs := [][]byte{[]byte("env-1"), []byte("env-2"), []byte("env-3")}

	client.SetDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < nFrames; i++ {
		// Send Push frame
		push := session.Frame(session.MsgTypePush, envs[i])
		encrypted, err := cs1.Encrypt(nil, nil, push)
		if err != nil {
			t.Fatalf("encrypt push[%d]: %v", i, err)
		}
		client.Write(encodeMsg(encrypted)) //nolint:errcheck

		// Expect Ack
		ackRaw := readMsg(t, client)
		ackDecrypted, err := cs2.Decrypt(nil, nil, ackRaw)
		if err != nil {
			t.Fatalf("decrypt ack[%d]: %v", i, err)
		}
		typ, _, ok := session.Parse(ackDecrypted)
		if !ok || typ != session.MsgTypeAck {
			t.Errorf("push[%d]: want Ack, got type=%02x ok=%v", i, typ, ok)
		}
	}

	// All three envelopes delivered to handler
	if len(handler.pushed) != nFrames {
		t.Errorf("want %d pushed, got %d", nFrames, len(handler.pushed))
	}
	for i, env := range envs {
		if string(handler.pushed[i]) != string(env) {
			t.Errorf("handler.pushed[%d]: want %q, got %q", i, env, handler.pushed[i])
		}
	}

	client.Close()
}

// ── helpers ───────────────────────────────────────────────────────────────────

func encodeHandshakeMsg(msg []byte) []byte {
	return encodeMsg(msg)
}

func encodeMsg(msg []byte) []byte {
	out := make([]byte, 2+len(msg))
	out[0] = byte(len(msg) >> 8)
	out[1] = byte(len(msg))
	copy(out[2:], msg)
	return out
}

func readHandshakeMsg(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	return readMsg(t, conn)
}

func readMsg(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	lenbuf := make([]byte, 2)
	if _, err := conn.Read(lenbuf); err != nil {
		t.Fatalf("read length: %v", err)
	}
	n := int(lenbuf[0])<<8 | int(lenbuf[1])
	buf := make([]byte, n)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return buf
}
