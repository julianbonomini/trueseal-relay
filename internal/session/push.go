package session

import (
	"context"
	"io"
	"net"

	"github.com/flynn/noise"
)

// AcceptPush handles an inbound Noise NK Push Session.
// The relay authenticates itself; the sender remains anonymous (ADR-0002).
// For each Push frame: calls handler.OnPush, then sends Ack (ADR-0008).
// Session is closed by the client after all blobs are sent.
func AcceptPush(conn net.Conn, relayKey noise.DHKey, handler Handler) error {
	defer conn.Close()

	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeNK,
		Initiator:     false,
		StaticKeypair: relayKey,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return formatErr("push: handshake state", err)
	}

	// NK: read initiator ephemeral
	msg, err := readNoiseMsg(conn)
	if err != nil {
		return formatErr("push: read e", err)
	}
	if _, _, _, err = hs.ReadMessage(nil, msg); err != nil {
		return formatErr("push: read e msg", err)
	}

	// NK: send relay response — handshake complete
	// cs1 = initiator→responder (relay receives), cs2 = responder→initiator (relay sends)
	resp, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return formatErr("push: write resp", err)
	}
	if err := writeNoiseMsg(conn, resp); err != nil {
		return formatErr("push: send resp", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for {
		raw, err := readNoiseMsg(conn)
		if err != nil {
			if err == io.EOF || isClosedErr(err) {
				return nil
			}
			return formatErr("push: read frame", err)
		}

		plain, err := cs1.Decrypt(nil, nil, raw)
		if err != nil {
			return formatErr("push: decrypt", err)
		}

		typ, body, ok := Parse(plain)
		if !ok || typ != MsgTypePush {
			continue // drop unknown/malformed frames silently
		}

		if err := handler.OnPush(ctx, body); err != nil {
			// Permanent rejection — send Error frame so the client does not retry.
			// Do NOT Ack, do NOT store the blob (ADR-0008).
			errFrame, encErr := cs2.Encrypt(nil, nil, Frame(MsgTypeError, nil))
			if encErr == nil {
				_ = writeNoiseMsg(conn, errFrame) // best-effort; ignore send failure
			}
			continue
		}

		// Ack: body is empty (ADR-0008 — Ack = "persisted to InboxStore")
		ack, err := cs2.Encrypt(nil, nil, Frame(MsgTypeAck, nil))
		if err != nil {
			return formatErr("push: encrypt ack", err)
		}
		if err := writeNoiseMsg(conn, ack); err != nil {
			return formatErr("push: send ack", err)
		}
	}
}
