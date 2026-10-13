package session_test

import (
	"net"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
)

// acceptResult carries an Accept*WithDeadlines return for assertions.
type acceptResult struct {
	err     error
	elapsed time.Duration
}

// awaitAcceptClose asserts the accept call returns within maxWait with a
// nil error — a deadline close is an ordinary session end (RELAY-6;
// ADR-0012), not a logged session error.
func awaitAcceptClose(t *testing.T, done <-chan acceptResult, maxWait time.Duration, what string) {
	t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("%s: deadline close returned an error (idle closes are ordinary): %v", what, r.err)
		}
	case <-time.After(maxWait):
		t.Fatalf("%s: connection was not closed within %v of the deadline expiring", what, maxWait)
	}
}

// TestAcceptPush_IdleHandshakeClosed (RELAY-6; ADR-0012): a client that
// connects and sends nothing — the slow-loris case the RELAY-5 review
// parked, since the version-prefix read had no deadline of its own — is
// closed by the handshake deadline.
func TestAcceptPush_IdleHandshakeClosed(t *testing.T) {
	relayKey := testRelayKey(t)
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan acceptResult, 1)
	start := time.Now()
	go func() {
		done <- acceptResult{err: session.AcceptPushWithDeadlines(server, relayKey, &mockHandler{}, session.Deadlines{Handshake: 100 * time.Millisecond}), elapsed: time.Since(start)}
	}()

	// The client sends nothing: the prefix read alone must hit the deadline.
	awaitAcceptClose(t, done, 3*time.Second, "idle push handshake")
}

// TestAcceptReceive_IdleHandshakeClosed: same on the XX Receive listener.
func TestAcceptReceive_IdleHandshakeClosed(t *testing.T) {
	relayKey := testRelayKey(t)
	client, server := net.Pipe()
	defer client.Close()
	h := newReceiveHandler()

	done := make(chan acceptResult, 1)
	go func() {
		done <- acceptResult{err: session.AcceptReceiveWithDeadlines(server, relayKey, h, session.Deadlines{Handshake: 100 * time.Millisecond})}
	}()

	awaitAcceptClose(t, done, 3*time.Second, "idle receive handshake")
}

// TestAcceptPush_SessionDeadlineTotal (RELAY-6; ADR-0012: 30 s in total,
// from accept): a Push Session whose handshake completes but which then
// goes idle is closed by the total session deadline — an idle Push Session
// cannot hold a slot forever.
func TestAcceptPush_SessionDeadlineTotal(t *testing.T) {
	relayKey := testRelayKey(t)
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan acceptResult, 1)
	go func() {
		done <- acceptResult{err: session.AcceptPushWithDeadlines(server, relayKey, &mockHandler{}, session.Deadlines{
			Handshake:   5 * time.Second,
			PushSession: 200 * time.Millisecond,
		})}
	}()

	// The client completes the handshake well inside the session budget,
	// then goes silent.
	pushClientHandshake(t, client, relayKey.Public)
	awaitAcceptClose(t, done, 3*time.Second, "idle push session")
}

// TestAcceptReceive_IdleDeadlineClosesSilentClient (RELAY-6; ADR-0012: 75 s
// with no frame received): a Receive Session with no inbound frames is
// closed after the configured idle deadline.
func TestAcceptReceive_IdleDeadlineClosesSilentClient(t *testing.T) {
	relayKey := testRelayKey(t)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)
	client, server := net.Pipe()
	defer client.Close()
	h := newReceiveHandler()

	done := make(chan acceptResult, 1)
	go func() {
		done <- acceptResult{err: session.AcceptReceiveWithDeadlines(server, relayKey, h, session.Deadlines{
			Handshake:   5 * time.Second,
			ReceiveIdle: 150 * time.Millisecond,
		})}
	}()

	doXXHandshake(t, client, relayKey.Public, deviceKey)
	<-h.connectedCh
	// The client goes silent — no heartbeats.
	awaitAcceptClose(t, done, 3*time.Second, "silent receive session")
}

// TestAcceptReceive_AnyInboundFrameResetsIdleDeadline (RELAY-6; ADR-0012
// with the SYNC-16 any-frame liveness note): a client heartbeating faster
// than the idle deadline stays connected well past it; once the heartbeats
// stop, the session closes. The heartbeat ECHO is the synchronisation
// primitive — each received echo proves the session was still alive.
func TestAcceptReceive_AnyInboundFrameResetsIdleDeadline(t *testing.T) {
	relayKey := testRelayKey(t)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)
	client, server := net.Pipe()
	defer client.Close()
	h := newReceiveHandler()

	done := make(chan acceptResult, 1)
	go func() {
		done <- acceptResult{err: session.AcceptReceiveWithDeadlines(server, relayKey, h, session.Deadlines{
			Handshake:   5 * time.Second,
			ReceiveIdle: 200 * time.Millisecond,
		})}
	}()

	cs1, cs2 := doXXHandshake(t, client, relayKey.Public, deviceKey)
	<-h.connectedCh

	// Heartbeat every 50 ms for 500 ms — 2.5× the idle deadline. Every
	// received echo proves the session survived that far.
	for i := 0; i < 10; i++ {
		enc, err := cs1.Encrypt(nil, nil, session.Frame(session.MsgTypeHeartbeat, nil))
		if err != nil {
			t.Fatalf("encrypt heartbeat: %v", err)
		}
		writeMsg(t, client, enc)
		client.SetReadDeadline(time.Now().Add(2 * time.Second))
		raw := readMsg(t, client)
		plain, err := cs2.Decrypt(nil, nil, raw)
		if err != nil {
			t.Fatalf("decrypt echo %d (session died mid-heartbeat — the idle deadline ignored live frames): %v", i, err)
		}
		typ, _, ok := session.Parse(plain)
		if !ok || typ != session.MsgTypeHeartbeat {
			t.Fatalf("echo %d: want Heartbeat frame, got type 0x%02x ok=%v", i, typ, ok)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Heartbeats stop → the session closes on the idle deadline.
	awaitAcceptClose(t, done, 3*time.Second, "receive session after heartbeats stop")
}

// TestAcceptReceive_WriteDeadlineStalledClient (RELAY-6; the RELAY-2 review
// follow-up): a client that never reads stalls the writer; the per-write
// deadline fires, the writer closes the conn, and the whole session tears
// down — no goroutine or queue is pinned. The read deadline is set far
// away so the WRITE deadline is provably what ends the session.
func TestAcceptReceive_WriteDeadlineStalledClient(t *testing.T) {
	relayKey := testRelayKey(t)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)
	client, server := net.Pipe()
	defer client.Close()
	h := newReceiveHandler()

	done := make(chan acceptResult, 1)
	go func() {
		done <- acceptResult{err: session.AcceptReceiveWithDeadlines(server, relayKey, h, session.Deadlines{
			Handshake:   5 * time.Second,
			ReceiveIdle: 30 * time.Second, // far away: not what fires
			Write:       100 * time.Millisecond,
		})}
	}()

	doXXHandshake(t, client, relayKey.Public, deviceKey)
	<-h.connectedCh

	// Fill the delivery path; the client never reads again. net.Pipe
	// writes block until read, so the writer blocks on its first write and
	// only the write deadline can free it.
	h.deliverCh <- relay.DeliveryBlob{BlobID: 1, Envelope: []byte("stalled")}

	awaitAcceptClose(t, done, 5*time.Second, "stalled-writer receive session")
}
