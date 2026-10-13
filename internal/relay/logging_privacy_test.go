package relay_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

// lockedBuffer is a goroutine-safe log sink: session and router goroutines
// log concurrently while the test reads at the end.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLogs_NoClientMetadata is the RELAY-4 regression test (issue #41,
// ADR-0012 "Logging"/"IP addresses", release gate G7). It captures the
// standard logger across a full push → receive → deliver → ack cycle plus a
// failed handshake, and asserts no client metadata appears: no IP or port,
// no key or key prefix, no Blob ID, no envelope size, no per-session timing.
//
// RED on the defect: the pre-fix relay logs the device key prefix, the Blob
// ID and the envelope size during exactly this cycle.
func TestLogs_NoClientMetadata(t *testing.T) {
	var lb lockedBuffer
	prev := log.Writer()
	log.SetOutput(&lb)
	t.Cleanup(func() { log.SetOutput(prev) })

	st, err := sqlitestore.New(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("relay key: %v", err)
	}
	router := relay.NewRouter(st, inprocess.New(), relay.DefaultTTL, 0)
	rr := newReadyRouter(router)

	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}

	// Receive Session for device B.
	clientReceive, serverReceive := net.Pipe()
	recvDone := make(chan struct{})
	go func() {
		session.AcceptReceive(serverReceive, relayKey, rr) //nolint:errcheck
		close(recvDone)
	}()
	csSend, csRecv := doXXHandshakeBoth(t, clientReceive, deviceKey)
	select {
	case <-rr.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Receive Session not registered within deadline")
	}

	// Anonymous Push Session from device A, one blob to B.
	envelope := []byte("hello-device-b")
	clientPush, serverPush := net.Pipe()
	pushDone := make(chan struct{})
	go func() {
		session.AcceptPush(serverPush, relayKey, router) //nolint:errcheck
		close(pushDone)
	}()
	doPushSession(t, clientPush, relayKey.Public, deviceKey.Public, envelope)
	clientPush.Close()
	select {
	case <-pushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("push session did not close within deadline")
	}

	// B receives the Deliver frame and learns the Blob ID.
	clientReceive.SetDeadline(time.Now().Add(2 * time.Second))
	raw := readMsg(t, clientReceive)
	plain, err := csRecv.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt deliver: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeDeliver {
		t.Fatalf("want Deliver, got type=%02x ok=%v", typ, ok)
	}
	blobID, _, ok := session.DecodeDeliverBody(body)
	if !ok {
		t.Fatal("DecodeDeliverBody failed")
	}

	// B acks the Blob.
	ackFrame, err := csSend.Encrypt(nil, nil, session.Frame(session.MsgTypeDeliverAck, session.EncodeDeliverAckBody(blobID)))
	if err != nil {
		t.Fatalf("encrypt ack: %v", err)
	}
	writeMsg(t, clientReceive, ackFrame)

	// Wait until the ack has been processed (the Blob is gone from the store).
	deadline := time.Now().Add(2 * time.Second)
	for {
		blobs, err := st.Peek(context.Background(), deviceKey.Public)
		if err != nil {
			t.Fatalf("peek: %v", err)
		}
		if len(blobs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("blob still in inbox 2s after DeliverAck")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Error path: a connection that fails the XX handshake (garbage bytes).
	badClient, badServer := net.Pipe()
	badDone := make(chan struct{})
	go func() {
		session.AcceptReceive(badServer, relayKey, router) //nolint:errcheck
		close(badDone)
	}()
	badClient.Write([]byte{0x00, 0x02, 0xff, 0xff}) // u16 len 2, garbage body
	badClient.Close()
	select {
	case <-badDone:
	case <-time.After(2 * time.Second):
		t.Fatal("failed-handshake session did not return within deadline")
	}

	// Close the Receive Session and wait for AcceptReceive to return, so any
	// deferred disconnect logging has fired before we assert.
	clientReceive.Close()
	select {
	case <-recvDone:
	case <-time.After(2 * time.Second):
		t.Fatal("receive session did not return within deadline")
	}

	out := lb.String()
	banned := map[string]string{
		"loopback IPv4":       "127.0.0.1",
		"loopback IPv6":       "[::1]",
		"blob ID field":       "blob_id",
		"device field":        "device=",
		"recipient field":     "recipient=",
		"address field":       "addr=",
		"duration field":      "duration=",
		"envelope size field": "envelope=",
		"device key prefix":   fmt.Sprintf("%x", deviceKey.Public[:4]),
		"device key full":     fmt.Sprintf("%x", deviceKey.Public),
	}
	for name, s := range banned {
		if strings.Contains(out, s) {
			t.Errorf("log output contains %s %q\nfull output:\n%s", name, s, out)
		}
	}
}

// doXXHandshakeBoth is doXXHandshake but returns both CipherStates so the
// client can send (DeliverAck) as well as receive.
func doXXHandshakeBoth(t *testing.T, conn net.Conn, deviceKey noise.DHKey) (send, recv *noise.CipherState) {
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

	msg, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("XX write s,se: %v", err)
	}
	writeMsg(t, conn, msg)
	return cs1, cs2
}
