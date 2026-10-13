package session

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

// AcceptPush handles an inbound Noise NK Push Session.
// The relay authenticates itself; the sender remains anonymous (ADR-0002).
// For each Push frame: calls handler.OnPush, then sends Ack (ADR-0008).
// Session is closed by the client after all blobs are sent.
func AcceptPush(conn net.Conn, relayKey noise.DHKey, handler Handler) error {
	defer conn.Close()

	// ADR-0022: the Transport Version prefix precedes the handshake and is
	// bound into it as the Noise prologue.
	prologue, err := exchangeTransportVersion(conn)
	if err != nil {
		return err
	}

	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeNK,
		Initiator:     false,
		StaticKeypair: relayKey,
		Prologue:      prologue,
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
			// Unknown frame type or malformed framing on a Push Session:
			// generic Error, malformed code, no echoed bytes (ADR-0022).
			sendPushError(conn, cs2, ReasonMalformed)
			continue
		}

		if err := handler.OnPush(ctx, body); err != nil {
			// Typed refusal (ADR-0008 amended; the ADR-0026 relay contract) —
			// send the Error frame with its reason code so the client can tell
			// "never retry" from "retry later". Do NOT Ack, do NOT store.
			sendPushError(conn, cs2, pushRefusalReason(err))
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

// pushRefusalReason classifies an OnPush failure onto an Error frame reason
// code (ADR-0026 relay contract). The relay's permanent refusals are the
// sentinel errors from the routing loop; everything else is a temporary
// internal failure — a client that retries later may succeed.
func pushRefusalReason(err error) ErrorReason {
	switch {
	case errors.Is(err, relay.ErrEnvelopeTooLarge):
		return ReasonTooLarge
	case errors.Is(err, relay.ErrPushBodyTooShort):
		return ReasonMalformed
	default:
		return ReasonInternal
	}
}

// sendPushError best-effort sends an Error frame with the given reason code.
// The body is exactly the code byte — never echoed frame bytes (ADR-0022).
func sendPushError(conn net.Conn, cs *noise.CipherState, reason ErrorReason) {
	errFrame, encErr := cs.Encrypt(nil, nil, Frame(MsgTypeError, []byte{byte(reason)}))
	if encErr == nil {
		_ = writeNoiseMsg(conn, errFrame) // best-effort; ignore send failure
	}
}
