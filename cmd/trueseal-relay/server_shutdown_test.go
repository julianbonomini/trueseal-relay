package main

// RELAY-11 graceful-shutdown tests (issue #31; ADR-0012 "Graceful
// shutdown"). They drive the testable server type against a real SQLite
// store and loopback listeners, with real Noise clients on the other end:
//
//   - an idle Receive Session must NOT hold shutdown — it is cancelled,
//     its connection closed, and shutdown finishes well under the drain
//     budget;
//   - a push whose write is in progress when shutdown starts commits and
//     the client receives its Ack;
//   - a push stalled past the drain budget is cut off with no Ack, so the
//     sender retries (ADR-0012 step 2, "at most 10 s");
//   - Blobs delivered but not acked stay in the Inbox (ADR-0009
//     peek-and-ack), so a restart re-delivers them;
//   - no session goroutines survive shutdown (the store closes after every
//     session has stopped).

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/config"
	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
	"github.com/julianbonomini/trueseal-relay/internal/store"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

// testServer builds a started server on loopback with a short drain and
// generous deadlines, returning it with the pieces a test needs.
func testServer(t *testing.T, handler session.Handler, drain time.Duration) (*server, *sqlitestore.Store, noise.DHKey) {
	t.Helper()
	st, err := sqlitestore.New(filepath.Join(t.TempDir(), "shutdown.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("relay key: %v", err)
	}

	cfg := config.RelayConfig{
		ListenPush:    "127.0.0.1:0",
		ListenReceive: "127.0.0.1:0",
		ListenHealth:  "127.0.0.1:0",
		TTL:           config.DefaultTTL,
		ReapInterval:  time.Hour,
		// Deadlines far above the test's runtime so only the shutdown
		// path itself can end a session.
		HandshakeDeadline:   10 * time.Second,
		PushSessionDeadline: 30 * time.Second,
		ReceiveIdleDeadline: 75 * time.Second,
		WriteDeadline:       30 * time.Second,
		ShutdownDrain:       drain,
		MaxEnvelopeBytes:    config.DefaultMaxEnvelopeBytes,
	}
	if handler == nil {
		handler = relay.NewRouter(st, inprocess.New(), cfg.TTL, cfg.MaxEnvelopeBytes)
	}
	reaper := storeReaperForTest(st)
	srv := newServer(cfg, relayKey, st, handler, reaper, nil)
	if err := srv.start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	return srv, st, relayKey
}

func storeReaperForTest(st *sqlitestore.Store) *store.Reaper {
	return store.NewReaper(st, time.Hour)
}

// shutdownDuration runs srv.shutdown and reports how long it took.
func shutdownDuration(srv *server) time.Duration {
	start := time.Now()
	srv.shutdown()
	return time.Since(start)
}

// TestShutdown_CancelsIdleReceiveSession is the core RELAY-11 regression:
// an idle Receive Session (client connected, saying nothing) must not hold
// shutdown. Shutdown cancels the session, closes the Device's connection
// and finishes well under the drain budget — Docker would SIGKILL at the
// drain boundary, so "well under" is the assertion, not "at".
func TestShutdown_CancelsIdleReceiveSession(t *testing.T) {
	const drain = 2 * time.Second
	srv, _, _ := testServer(t, nil, drain)

	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	conn, _, _ := dialXX(t, srv.recvLn.Addr().String(), deviceKey)

	elapsed := shutdownDuration(srv)

	if elapsed >= drain {
		t.Fatalf("shutdown with an idle Receive Session took %v — the session was not cancelled (drain budget %v)", elapsed, drain)
	}

	// The Device's connection must be closed: a read fails promptly.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("device connection still open after shutdown — session was not cancelled")
	} else {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("device connection still open after shutdown (read timed out): %v", err)
		}
	}
}

// blockingPushHandler wraps a Handler and holds one OnPush call until
// released (or the call's context is cancelled), so a test can put a push
// mid-commit exactly when shutdown starts.
type blockingPushHandler struct {
	session.Handler
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingPushHandler(inner session.Handler) *blockingPushHandler {
	return &blockingPushHandler{Handler: inner, started: make(chan struct{}), release: make(chan struct{})}
}

func (h *blockingPushHandler) OnPush(ctx context.Context, body []byte) error {
	h.once.Do(func() { close(h.started) })
	select {
	case <-h.release:
		return h.Handler.OnPush(ctx, body)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestShutdown_PushInFlightCommitsAndAcks: a push whose write is already in
// progress when shutdown starts commits to the store and the client
// receives its Ack (ADR-0012 step 2). Shutdown waits for it, inside the
// drain budget.
func TestShutdown_PushInFlightCommitsAndAcks(t *testing.T) {
	const drain = 2 * time.Second
	st, err := sqlitestore.New(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	router := relay.NewRouter(st, inprocess.New(), config.DefaultTTL, config.DefaultMaxEnvelopeBytes)
	blocking := newBlockingPushHandler(router)
	srv, _, relayKey := testServerWithStore(t, st, blocking, drain)

	var recipient [32]byte
	for i := range recipient {
		recipient[i] = byte(i + 1)
	}
	conn, cs1, cs2 := dialNK(t, srv.pushLn.Addr().String(), relayKey)

	// Send the Push frame; the handler holds it mid-commit.
	pushFrame(t, conn, cs1, recipient, []byte("mid-shutdown-envelope"))
	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		t.Fatal("push never reached the handler")
	}

	// Shutdown starts while the push is in flight.
	done := make(chan time.Duration, 1)
	go func() { done <- shutdownDuration(srv) }()

	// While the push is held, shutdown must NOT complete.
	select {
	case d := <-done:
		t.Fatalf("shutdown finished in %v while a push was mid-commit — it must wait up to the drain", d)
	case <-time.After(200 * time.Millisecond):
	}

	// Release the commit: the client must receive the Ack, the Blob must
	// be in the store, and shutdown finishes inside the budget. The
	// client then closes the Push Session, as trueseal-sync does once its
	// envelopes are sent (CONTEXT.md) — an idle open session is not
	// "in-flight work" the drain waits on.
	close(blocking.release)
	expectAck(t, conn, cs2)
	conn.Close()
	if n := inboxCount(t, st, recipient); n != 1 {
		t.Fatalf("committed Blob missing from the store: inbox has %d", n)
	}
	select {
	case d := <-done:
		if d >= drain {
			t.Fatalf("shutdown took %v despite the push committing promptly (drain %v)", d, drain)
		}
	case <-time.After(drain + 2*time.Second):
		t.Fatal("shutdown did not finish after the in-flight push committed")
	}
}

// TestShutdown_StalledPushIsCutOff: a push still in progress when the
// drain budget expires is cut off — the connection is closed with no Ack,
// so the sender retries (ADR-0012 step 2, "for at most 10 s"). The
// process must not wait for the stalled push beyond the budget.
func TestShutdown_StalledPushIsCutOff(t *testing.T) {
	const drain = 500 * time.Millisecond
	st, err := sqlitestore.New(filepath.Join(t.TempDir(), "stalled.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	router := relay.NewRouter(st, inprocess.New(), config.DefaultTTL, config.DefaultMaxEnvelopeBytes)
	blocking := newBlockingPushHandler(router) // never released
	srv, _, relayKey := testServerWithStore(t, st, blocking, drain)

	var recipient [32]byte
	for i := range recipient {
		recipient[i] = byte(i + 7)
	}
	conn, cs1, _ := dialNK(t, srv.pushLn.Addr().String(), relayKey)
	pushFrame(t, conn, cs1, recipient, []byte("stalled-envelope"))
	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		t.Fatal("push never reached the handler")
	}

	elapsed := shutdownDuration(srv)
	if elapsed < drain {
		t.Fatalf("shutdown finished in %v, before the drain budget %v — a mid-commit push should get the full budget to finish", elapsed, drain)
	}
	if elapsed >= drain+3*time.Second {
		t.Fatalf("shutdown took %v — a stalled push must be cut off at the drain budget %v, not waited on", elapsed, drain)
	}

	// No Ack: the connection is closed (a read fails with a close, not a
	// timeout), so the sender knows to retry.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := readNoiseFrame(conn); err == nil {
		t.Fatal("stalled push connection still open after shutdown — it must be cut off with no Ack")
	} else {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("stalled push connection still open after shutdown (read timed out): %v", err)
		}
	}
	if n := inboxCount(t, st, recipient); n != 0 {
		t.Fatalf("cut-off push must not be committed — inbox has %d blobs", n)
	}
}

// TestShutdown_UnackedBlobsStayInTheInbox: a Blob delivered to a Receive
// Session but not yet DeliverAck'd when shutdown cancels the session
// remains in the Inbox, so the next Receive Session after a restart
// re-delivers it (ADR-0009 peek-and-ack; ADR-0012 step 3).
func TestShutdown_UnackedBlobsStayInTheInbox(t *testing.T) {
	const drain = 2 * time.Second
	srv, st, _ := testServer(t, nil, drain)

	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	var recipient [32]byte
	copy(recipient[:], deviceKey.Public)

	// A Blob is waiting in the Device's Inbox.
	putBlob(t, st, recipient, []byte("deliver-me"))

	// The Device connects and the Blob is delivered (initial inbox
	// flush), but no DeliverAck is sent.
	conn, _, cs2 := dialXX(t, srv.recvLn.Addr().String(), deviceKey)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, body := readFrame(t, conn, cs2)
	if typ != session.MsgTypeDeliver {
		t.Fatalf("expected a Deliver frame, got type 0x%02x", typ)
	}
	if len(body) < 8 {
		t.Fatalf("Deliver body too short: %d", len(body))
	}

	shutdownDuration(srv)

	// The un-acked Blob survived: it is still in the Inbox for the next
	// Receive Session.
	if n := inboxCount(t, st, recipient); n != 1 {
		t.Fatalf("un-acked delivered Blob lost on shutdown: inbox has %d blobs, want 1", n)
	}
}

// TestShutdown_NoSessionsIsImmediate: with nothing connected, shutdown is
// prompt.
func TestShutdown_NoSessionsIsImmediate(t *testing.T) {
	srv, _, _ := testServer(t, nil, 2*time.Second)
	if d := shutdownDuration(srv); d >= time.Second {
		t.Fatalf("shutdown with no sessions took %v", d)
	}
}

// TestShutdown_NoGoroutineLeak: after shutdown returns, no session or
// accept-loop goroutines survive — the store may be closed safely.
func TestShutdown_NoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	srv, _, _ := testServer(t, nil, time.Second)
	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	conn, _, _ := dialXX(t, srv.recvLn.Addr().String(), deviceKey)
	defer conn.Close()
	shutdownDuration(srv)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked after shutdown: before=%d after=%d", before, runtime.NumGoroutine())
}

// --- test clients -------------------------------------------------------

// testServerWithStore is testServer with a caller-supplied store (the push
// tests wrap the router around the same store they inspect).
func testServerWithStore(t *testing.T, st *sqlitestore.Store, handler session.Handler, drain time.Duration) (*server, *sqlitestore.Store, noise.DHKey) {
	t.Helper()
	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("relay key: %v", err)
	}
	cfg := config.RelayConfig{
		ListenPush:          "127.0.0.1:0",
		ListenReceive:       "127.0.0.1:0",
		ListenHealth:        "127.0.0.1:0",
		TTL:                 config.DefaultTTL,
		ReapInterval:        time.Hour,
		HandshakeDeadline:   10 * time.Second,
		PushSessionDeadline: 30 * time.Second,
		ReceiveIdleDeadline: 75 * time.Second,
		WriteDeadline:       30 * time.Second,
		ShutdownDrain:       drain,
		MaxEnvelopeBytes:    config.DefaultMaxEnvelopeBytes,
	}
	srv := newServer(cfg, relayKey, st, handler, storeReaperForTest(st), nil)
	if err := srv.start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	return srv, st, relayKey
}

// versionPrefix is the pinned Transport Version 1 exchange (ADR-0022;
// internal/session/version.go).
var versionPrefix = []byte{0x54, 0x53, 0x50, 0x00, 0x01}

// dialXX connects and completes the version exchange + Noise XX handshake
// as a Device, returning the transport cipher states (cs1 client→relay,
// cs2 relay→client).
func dialXX(t *testing.T, addr string, deviceKey noise.DHKey) (net.Conn, *noise.CipherState, *noise.CipherState) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write(versionPrefix); err != nil {
		t.Fatalf("version write: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("version read: %v", err)
	}
	if reply[0] != 0x01 {
		t.Fatalf("relay refused version (reply 0x%02x)", reply[0])
	}
	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		StaticKeypair: deviceKey,
		Prologue:      versionPrefix,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("hs: %v", err)
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("xx e: %v", err)
	}
	writeNoiseFrame(t, conn, msg)
	resp, err := readNoiseFrame(conn)
	if err != nil {
		t.Fatalf("xx read resp: %v", err)
	}
	if _, _, _, err = hs.ReadMessage(nil, resp); err != nil {
		t.Fatalf("xx resp: %v", err)
	}
	msg, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("xx fin: %v", err)
	}
	writeNoiseFrame(t, conn, msg)
	return conn, cs1, cs2
}

// dialNK connects and completes the version exchange + Noise NK handshake
// as an anonymous pusher, returning the transport cipher states (cs1
// client→relay, cs2 relay→client).
func dialNK(t *testing.T, addr string, relayKey noise.DHKey) (net.Conn, *noise.CipherState, *noise.CipherState) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write(versionPrefix); err != nil {
		t.Fatalf("version write: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("version read: %v", err)
	}
	if reply[0] != 0x01 {
		t.Fatalf("relay refused version (reply 0x%02x)", reply[0])
	}
	var relayPub [32]byte
	copy(relayPub[:], relayKey.Public)
	cfg := noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  relayPub[:],
		Prologue:    versionPrefix,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("hs: %v", err)
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("nk e: %v", err)
	}
	writeNoiseFrame(t, conn, msg)
	resp, err := readNoiseFrame(conn)
	if err != nil {
		t.Fatalf("nk read resp: %v", err)
	}
	var cs1, cs2 *noise.CipherState
	if _, cs1, cs2, err = hs.ReadMessage(nil, resp); err != nil {
		t.Fatalf("nk resp: %v", err)
	}
	return conn, cs1, cs2
}

// pushFrame sends one Push frame: body = recipient (32 bytes) || envelope.
// The envelope carries recipient_pub as proto field 3 (the one field the
// relay reads, RELAY-3) and the payload as field 7 (ADR-0031's sealed
// payload) — the relay's minimal wire reader walks the WHOLE envelope, so
// every byte must be valid protobuf wire format.
func pushFrame(t *testing.T, conn net.Conn, cs *noise.CipherState, recipient [32]byte, envelopePayload []byte) {
	t.Helper()
	envelope := make([]byte, 0, 4+32+len(envelopePayload))
	envelope = append(envelope, 0x1A, 0x20) // field 3, LEN, 32 bytes
	envelope = append(envelope, recipient[:]...)
	envelope = append(envelope, 0x3A) // field 7, LEN
	envelope = appendVarint(envelope, uint64(len(envelopePayload)))
	envelope = append(envelope, envelopePayload...)
	body := append(recipient[:], envelope...)
	enc, err := cs.Encrypt(nil, nil, session.Frame(session.MsgTypePush, body))
	if err != nil {
		t.Fatalf("encrypt push: %v", err)
	}
	writeNoiseFrame(t, conn, enc)
}

// expectAck reads one frame and requires it to be an Ack.
func expectAck(t *testing.T, conn net.Conn, cs *noise.CipherState) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, _ := readFrame(t, conn, cs)
	if typ != session.MsgTypeAck {
		t.Fatalf("expected Ack, got frame type 0x%02x", typ)
	}
}

// readFrame reads one Noise transport message and decrypts it.
func readFrame(t *testing.T, conn net.Conn, cs *noise.CipherState) (session.MsgType, []byte) {
	t.Helper()
	raw, err := readNoiseFrame(conn)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	plain, err := cs.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt frame: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok {
		t.Fatalf("parse frame: not a frame (%d bytes)", len(plain))
	}
	return typ, body
}

// appendVarint encodes v as a protobuf varint.
func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func writeNoiseFrame(t *testing.T, conn net.Conn, msg []byte) {
	t.Helper()
	out := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(out, uint16(len(msg)))
	copy(out[2:], msg)
	if _, err := conn.Write(out); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readNoiseFrame reads one length-prefixed Noise message (no deadline
// manipulation — callers set read deadlines as needed).
func readNoiseFrame(conn net.Conn) ([]byte, error) {
	lenbuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, lenbuf); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(lenbuf)
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// putBlob stores one Blob directly in the Inbox.
func putBlob(t *testing.T, st *sqlitestore.Store, recipient [32]byte, envelope []byte) {
	t.Helper()
	if err := st.Put(context.Background(), recipient[:], envelope, time.Hour); err != nil {
		t.Fatalf("put: %v", err)
	}
}

// inboxCount reports how many Blobs are waiting for a recipient.
func inboxCount(t *testing.T, st *sqlitestore.Store, recipient [32]byte) int {
	t.Helper()
	blobs, err := st.PeekAfter(context.Background(), recipient[:], 0)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	return len(blobs)
}
