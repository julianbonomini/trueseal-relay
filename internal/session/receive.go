package session

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/flynn/noise"
	"github.com/julianbonomini/hush-relay/internal/relay"
)

// AcceptReceive handles an inbound Noise XX Receive Session.
// Mutual authentication — relay and device verify each other's static keys.
// Calls handler.OnReceiveConnect with the device's public key, then:
//   - ranges the returned deliver channel, sending Deliver frames to the device
//   - echoes Heartbeat frames
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

	// Deliver goroutine: send blobs from routing loop to device
	go func() {
		for blob := range deliverCh {
			frame := Frame(MsgTypeDeliver, blob)
			encrypted, err := cs2.Encrypt(nil, nil, frame)
			if err != nil {
				return
			}
			if err := writeNoiseMsg(conn, encrypted); err != nil {
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

		typ, _, ok := Parse(plain)
		if !ok {
			continue
		}

		switch typ {
		case MsgTypeHeartbeat:
			hb, err := cs2.Encrypt(nil, nil, Frame(MsgTypeHeartbeat, nil))
			if err != nil {
				return formatErr("receive: encrypt heartbeat", err)
			}
			if err := writeNoiseMsg(conn, hb); err != nil {
				return formatErr("receive: send heartbeat", err)
			}
		default:
			// drop silently
		}
	}
}
