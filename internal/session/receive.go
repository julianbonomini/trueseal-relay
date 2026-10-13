package session

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

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
func AcceptReceive(conn net.Conn, relayKey noise.DHKey, handler Handler) error {
	defer conn.Close()

	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeXX,
		Initiator:     false,
		StaticKeypair: relayKey,
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

	// Extract device's static public key
	var deviceKey relay.RecipientKey
	copy(deviceKey[:], hs.PeerStatic())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// stop shuts down the writer and the deliver goroutine when AcceptReceive
	// returns. Deferred before any return path below.
	stop := make(chan struct{})
	defer close(stop)

	deliverCh := handler.OnReceiveConnect(ctx, deviceKey)

	// Detect Subscribe failure: if the deliver channel is already closed,
	// the router could not register this device. Close the connection so
	// the client reconnects, rather than hanging silently unable to receive.
	select {
	case _, ok := <-deliverCh:
		if !ok {
			return fmt.Errorf("receive: subscribe failed — closing connection")
		}
		// An early blob arrived; we already consumed it from the channel.
		// This path is safe to ignore: the flush goroutine will re-flush
		// from the store, so no blob is permanently lost.
	default:
		// Channel is open and empty — normal path.
	}

	// One writer per Receive Session: the sole owner of cs2 and the conn's
	// write side. Deliver and Heartbeat-echo frames both go through it.
	writer := newSessionWriter(conn, cs2, stop)

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
		raw, err := readNoiseMsg(conn)
		if err != nil {
			if err == io.EOF || isClosedErr(err) {
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
				continue
			}
			if err := handler.OnDeliverAck(ctx, deviceKey, int64(blobID)); err != nil {
				// Field-free apart from the store error (ADR-0012): the
				// device key and Blob ID are client metadata.
				log.Printf("recv: failed to delete acknowledged blob: %v", err)
			}
		default:
			// drop silently
		}
	}
}
