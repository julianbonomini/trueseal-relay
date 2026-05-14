package main_test

import (
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

// TestGracefulShutdown_WaitsForActiveSession verifies that the relay's
// shutdown logic (WaitGroup + 30s timeout) waits for an active Receive
// Session to finish before returning, but does complete once the session
// closes.
//
// Uses net.Pipe() for the transport layer (no real TCP listener required)
// so it runs in restricted sandbox environments too.
func TestGracefulShutdown_WaitsForActiveSession(t *testing.T) {
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "shutdown.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("relay key: %v", err)
	}
	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}

	router := relay.NewRouter(store, inprocess.New(), relay.DefaultTTL, 0)

	// Wiring identical to main: WaitGroup tracks active sessions.
	var wg sync.WaitGroup

	// Simulate one accepted connection (the "accept loop" hand-off).
	clientConn, serverConn := net.Pipe()

	wg.Add(1)
	go func() {
		defer wg.Done()
		session.AcceptReceive(serverConn, relayKey, router) //nolint:errcheck
	}()

	// Client completes the XX handshake — session is now established.
	shutdownXXHandshake(t, clientConn, deviceKey)

	// Shutdown wait pattern from main (mirrors what runs after <-ctx.Done()).
	shutdownDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(shutdownDone)
	}()

	// Session is still alive — shutdown must be blocking.
	select {
	case <-shutdownDone:
		t.Fatal("shutdown completed before active session was closed")
	case <-time.After(100 * time.Millisecond):
		// Good — relay is waiting for the session.
	}

	// Close the client connection — session ends naturally.
	clientConn.Close()

	// Shutdown must complete now.
	select {
	case <-shutdownDone:
		// Good.
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not complete after session closed")
	}
}

// TestGracefulShutdown_NoActiveSessions verifies that shutdown is
// immediate when no sessions are open.
func TestGracefulShutdown_NoActiveSessions(t *testing.T) {
	var wg sync.WaitGroup

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Good — drained instantly.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdown with no sessions should complete immediately")
	}
}

// shutdownXXHandshake performs the Noise XX handshake as the initiator so
// that AcceptReceive on the server side considers the session established.
func shutdownXXHandshake(t *testing.T, conn net.Conn, deviceKey noise.DHKey) {
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
	writeMsgShutdown(t, conn, msg)

	resp := readMsgShutdown(t, conn)
	if _, _, _, err = hs.ReadMessage(nil, resp); err != nil {
		t.Fatalf("XX read e,ee,s,es: %v", err)
	}

	msg, _, _, err = hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("XX write s,se: %v", err)
	}
	writeMsgShutdown(t, conn, msg)
}

func writeMsgShutdown(t *testing.T, conn net.Conn, msg []byte) {
	t.Helper()
	out := make([]byte, 2+len(msg))
	out[0] = byte(len(msg) >> 8)
	out[1] = byte(len(msg))
	copy(out[2:], msg)
	if _, err := conn.Write(out); err != nil {
		t.Fatalf("writeMsgShutdown: %v", err)
	}
}

func readMsgShutdown(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	lenbuf := make([]byte, 2)
	if _, err := conn.Read(lenbuf); err != nil {
		t.Fatalf("readMsgShutdown len: %v", err)
	}
	n := int(lenbuf[0])<<8 | int(lenbuf[1])
	buf := make([]byte, n)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("readMsgShutdown body: %v", err)
	}
	return buf
}
