package session

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/logging"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

// logger is the session package's logging seam (RELAY-9, ADR-0012).
// Package-level because the Accept* entry points are called from main and
// many tests; it is set ONCE at process start, before any listener
// goroutine runs, and never mutated afterwards. The zero value is a no-op
// logger, so tests that never call SetLogger log nothing.
var logger atomic.Pointer[logging.Logger]

// SetLogger routes the session package's logging. nil restores the no-op
// default. Call once from main before listeners start.
func SetLogger(l *logging.Logger) {
	if l == nil {
		l = logging.Nop()
	}
	logger.Store(l)
}

// log returns the package logger, never nil.
func logg() *logging.Logger {
	if l := logger.Load(); l != nil {
		return l
	}
	return logging.Nop()
}

// AcceptReceive handles an inbound Noise XX Receive Session.
// Mutual authentication — relay and device verify each other's static keys.
// Calls handler.OnReceiveConnect with the device's public key, then:
//   - ranges the returned deliver channel, sending Deliver frames to the device
//   - echoes Heartbeat frames
//
// All outbound frames go through one sessionWriter, the sole owner of the
// outbound CipherState and the conn's write side (RELAY-2: concurrent
// encryption corrupts the Noise nonce sequence; interleaved writes corrupt
// framing).
//
// Returns when the connection closes or an unrecoverable error occurs.
// Callers must use defer cancel() on the context passed to OnReceiveConnect.
// No deadlines are applied; production wiring uses
// AcceptReceiveWithDeadlines.
func AcceptReceive(conn net.Conn, relayKey noise.DHKey, handler Handler) error {
	return AcceptReceiveWithDeadlines(conn, relayKey, handler, Deadlines{})
}

// AcceptReceiveWithDeadlines is AcceptReceive with the ADR-0012 deadlines
// (RELAY-6): the version-prefix read and Noise handshake must complete
// within d.Handshake; afterwards the session closes after d.ReceiveIdle of
// silence, where ANY inbound frame (Heartbeat, DeliverAck, even a
// malformed one) resets the timer — mirroring the client's any-frame
// liveness (SYNC-16). An idle close is ordinary (nil error), not a session
// error. The writer also gets d.Write as a per-write deadline so a stalled
// client cannot pin the writer goroutine (the RELAY-2 review follow-up).
func AcceptReceiveWithDeadlines(conn net.Conn, relayKey noise.DHKey, handler Handler, d Deadlines) error {
	err := acceptReceiveSession(conn, relayKey, handler, d)
	if isTimeoutErr(err) {
		// A deadline close is policy (ADR-0012), not a failure: an idle
		// connector must not surface as a per-connection error log.
		return nil
	}
	return err
}

func acceptReceiveSession(conn net.Conn, relayKey noise.DHKey, handler Handler, d Deadlines) error {
	defer conn.Close()
	if d.Handshake > 0 {
		// One absolute deadline covers the prefix read and all three XX
		// flights; the prefix read can no longer hang on its own (RELAY-6
		// follow-up to RELAY-5).
		_ = conn.SetDeadline(time.Now().Add(d.Handshake))
	}

	// ADR-0022: the Transport Version prefix precedes the handshake and is
	// bound into it as the Noise prologue.
	prologue, err := exchangeTransportVersion(conn)
	if err != nil {
		return err
	}

	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeXX,
		Initiator:     false,
		StaticKeypair: relayKey,
		Prologue:      prologue,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return formatErr("receive: handshake state", err)
	}

	// XX: ← e
	msg, err := readNoiseMsg(conn)
	if err != nil {
		return formatErr("receive: read e", err)
	}
	if _, _, _, err = hs.ReadMessage(nil, msg); err != nil {
		return formatErr("receive: read e msg", err)
	}

	// XX: → e, ee, s, es
	resp, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return formatErr("receive: write e,ee,s,es", err)
	}
	if err := writeNoiseMsg(conn, resp); err != nil {
		return formatErr("receive: send e,ee,s,es", err)
	}

	// XX: ← s, se — handshake complete
	msg, err = readNoiseMsg(conn)
	if err != nil {
		return formatErr("receive: read s,se", err)
	}
	// cs1 = initiator→responder (device sends, relay receives)
	// cs2 = responder→initiator (relay sends, device receives)
	_, cs1, cs2, err := hs.ReadMessage(nil, msg)
	if err != nil {
		return formatErr("receive: read s,se msg", err)
	}

	// Handshake done — disarm the handshake deadline before registration
	// and the session loop (the idle deadline is armed per-read below).
	// Registration runs without a conn deadline: it does store work whose
	// budget is its own, not the handshake's.
	if d.Handshake > 0 {
		_ = conn.SetDeadline(time.Time{})
	}

	// Extract device's static public key
	var deviceKey relay.RecipientKey
	copy(deviceKey[:], hs.PeerStatic())

	// RELAY-9: dev-mode session lifecycle detail — a key PREFIX only
	// (ADR-0012), never an address.
	logg().Debugf("recv: XX handshake complete key=%x", deviceKey[:4])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// stop shuts down the writer and the deliver goroutine when AcceptReceive
	// returns. Deferred before any return path below.
	stop := make(chan struct{})
	defer close(stop)

	// Registration failure is reported explicitly by the handler (RELAY-7):
	// close the connection so the client reconnects, rather than hanging
	// silently unable to receive. Nothing is consumed from the delivery
	// channel — an early Blob buffered before the writer starts is delivered,
	// never discarded (ADR-0009 at-least-once).
	deliverCh, err := handler.OnReceiveConnect(ctx, deviceKey)
	if err != nil {
		return formatErr("receive: register device", err)
	}

	// One writer per Receive Session: the sole owner of cs2 and the conn's
	// write side. Deliver and Heartbeat-echo frames both go through it.
	writer := newSessionWriter(conn, cs2, stop, d.Write)

	// Deliver goroutine: send blobs from routing loop to device.
	// send blocks while the writer is behind — that backpressure is intended;
	// unwritten blobs are never DeliverAck'd, so the Inbox keeps them.
	go func() {
		for blob := range deliverCh {
			body := EncodeDeliverBody(uint64(blob.BlobID), blob.Envelope)
			if !writer.send(Frame(MsgTypeDeliver, body), stop) {
				return
			}
		}
	}()

	// Receive loop: handle Heartbeat, drop unknown frames
	for {
		// ADR-0012: close after 75 s (default) with no frame received. ANY
		// inbound frame resets the timer — a DeliverAck or even a malformed
		// frame proves liveness exactly as a Heartbeat does.
		if d.ReceiveIdle > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(d.ReceiveIdle))
		}
		raw, err := readNoiseMsg(conn)
		if err != nil {
			if err == io.EOF || isClosedErr(err) || isTimeoutErr(err) {
				// Client close, writer-torn-down conn, or the idle deadline
				// expired — all ordinary session ends (RELAY-6).
				return nil
			}
			return formatErr("receive: read frame", err)
		}

		plain, err := cs1.Decrypt(nil, nil, raw)
		if err != nil {
			return formatErr("receive: decrypt", err)
		}

		typ, body, ok := Parse(plain)
		if !ok {
			// Unknown frame type or malformed framing: generic Error,
			// malformed code, no echoed bytes (ADR-0022). Non-blocking like
			// the heartbeat echo — the read loop must never stall.
			writer.trySend(Frame(MsgTypeError, []byte{byte(ReasonMalformed)}))
			continue
		}

		switch typ {
		case MsgTypeHeartbeat:
			// Non-blocking: a dropped echo is harmless (the client heartbeats
			// every 25 s, ADR-0012) and the read loop must never stall behind
			// a slow conn — it also serves DeliverAck.
			writer.trySend(Frame(MsgTypeHeartbeat, nil))
		case MsgTypeDeliverAck:
			blobID, ok := DecodeDeliverAckBody(body)
			if !ok {
				writer.trySend(Frame(MsgTypeError, []byte{byte(ReasonMalformed)}))
				continue
			}
			if err := handler.OnDeliverAck(ctx, deviceKey, int64(blobID)); err != nil {
				// Field-free apart from the store error (ADR-0012): the
				// device key and Blob ID are client metadata. A store error
				// never wraps a peer address.
				logg().Printf("recv: failed to delete acknowledged blob: %v", err)
				logg().Debugf("recv: delete failed device=%x blob_id=%d", deviceKey[:4], blobID)
			}
		default:
			// A known frame type that does not belong on a Receive Session
			// (Push, Deliver, Ack, Error): protocol violation — generic Error,
			// no echoed bytes (ADR-0022).
			writer.trySend(Frame(MsgTypeError, []byte{byte(ReasonMalformed)}))
		}
	}
}
