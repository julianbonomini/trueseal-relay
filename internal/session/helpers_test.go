package session_test

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/session"
)

// testClientPrefix is the pinned Transport Version prefix a v1 client sends
// (ADR-0022; internal/session/version.go; the sync test vectors pin the same
// bytes — internal/vectors).
var testClientPrefix = []byte{0x54, 0x53, 0x50, 0x00, 0x01}

// clientVersionExchange performs the client half of the ADR-0022 version
// exchange and returns the prologue the Noise handshake must bind.
func clientVersionExchange(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	if _, err := conn.Write(testClientPrefix); err != nil {
		t.Fatalf("version exchange write: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("version exchange read: %v", err)
	}
	if reply[0] != 0x01 {
		t.Fatalf("version exchange: relay refused (reply 0x%02x)", reply[0])
	}
	return testClientPrefix
}

// pushClientHandshake runs the version exchange + NK handshake as the push
// client and returns (send, recv) cipher states.
func pushClientHandshake(t *testing.T, conn net.Conn, relayPub []byte) (send, recv *noise.CipherState) {
	t.Helper()
	prologue := clientVersionExchange(t, conn)
	cfg := noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  relayPub,
		Prologue:    prologue,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("NK handshake state: %v", err)
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("NK write e: %v", err)
	}
	if _, err := conn.Write(encodeMsg(msg)); err != nil {
		t.Fatalf("write e: %v", err)
	}
	resp := readHandshakeMsg(t, conn)
	_, cs1, cs2, err := hs.ReadMessage(nil, resp)
	if err != nil {
		t.Fatalf("NK read resp: %v", err)
	}
	return cs1, cs2
}

// sendPushFrame encrypts and writes one Push frame body.
func sendPushFrame(t *testing.T, conn net.Conn, cs *noise.CipherState, body []byte) {
	t.Helper()
	enc, err := cs.Encrypt(nil, nil, session.Frame(session.MsgTypePush, body))
	if err != nil {
		t.Fatalf("encrypt push: %v", err)
	}
	if _, err := conn.Write(encodeMsg(enc)); err != nil {
		t.Fatalf("write push: %v", err)
	}
}

// readErrorFrame reads one relay→client message and returns the parsed Error
// frame body, failing unless it is an Error frame.
func readErrorFrame(t *testing.T, conn net.Conn, cs *noise.CipherState) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	raw := readHandshakeMsg(t, conn)
	plain, err := cs.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt response: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok {
		t.Fatal("want parseable frame")
	}
	if typ != session.MsgTypeError {
		t.Fatalf("want Error frame (0x05), got type 0x%02x", typ)
	}
	return body
}
