package session_test

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
)

// Error frame reason codes (ADR-0008 amended by RELAY-5; the ADR-0026 relay
// contract): permanent refusals are 0x0x, temporary 0x8x. The client must be
// able to tell "too large — never retry" from "internal — retry later".

func TestPushRefusal_OversizedGetsTooLargeCode(t *testing.T) {
	body := pushRefusalBody(t, relay.ErrEnvelopeTooLarge)
	if len(body) != 1 || body[0] != 0x01 {
		t.Fatalf("want Error body [0x01] (tooLarge), got %x", body)
	}
}

func TestPushRefusal_ShortBodyGetsMalformedCode(t *testing.T) {
	body := pushRefusalBody(t, relay.ErrPushBodyTooShort)
	if len(body) != 1 || body[0] != 0x02 {
		t.Fatalf("want Error body [0x02] (malformed), got %x", body)
	}
}

func TestPushRefusal_UnclassifiedGetsInternalCode(t *testing.T) {
	body := pushRefusalBody(t, errors.New("disk on fire"))
	if len(body) != 1 || body[0] != 0x84 {
		t.Fatalf("want Error body [0x84] (internal), got %x", body)
	}
}

// RELAY-6: the ADR-0012 abuse-limit refusals reach the wire with their
// typed TEMPORARY codes (ADR-0026: the client retries these later).
func TestPushRefusal_InboxFullGetsTemporaryCode(t *testing.T) {
	body := pushRefusalBody(t, relay.ErrInboxFull)
	if len(body) != 1 || body[0] != 0x81 {
		t.Fatalf("want Error body [0x81] (inboxFull, temporary), got %x", body)
	}
}

func TestPushRefusal_RateLimitedGetsTemporaryCode(t *testing.T) {
	body := pushRefusalBody(t, fmt.Errorf("wrap: %w", relay.ErrRateLimited))
	if len(body) != 1 || body[0] != 0x82 {
		t.Fatalf("want Error body [0x82] (rateLimited, temporary), got %x", body)
	}
}

func TestPushRefusal_WrappedSentinelStillClassifies(t *testing.T) {
	body := pushRefusalBody(t, errors.Join(errors.New("put"), relay.ErrEnvelopeTooLarge))
	if len(body) != 1 || body[0] != 0x01 {
		t.Fatalf("want Error body [0x01] (tooLarge) through wrapping, got %x", body)
	}
}

func TestPushRefusal_NonPushFrameGetsMalformedCode(t *testing.T) {
	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	client, server := net.Pipe()
	go session.AcceptPush(server, relayKey, &mockHandler{}) //nolint:errcheck
	cs1, cs2 := pushClientHandshake(t, client, relayKey.Public)

	// A Heartbeat on a Push Session is not a Push frame: generic Error,
	// malformed, echoing no bytes (ADR-0022).
	enc, err := cs1.Encrypt(nil, nil, session.Frame(session.MsgTypeHeartbeat, nil))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := client.Write(encodeMsg(enc)); err != nil {
		t.Fatalf("write: %v", err)
	}
	body := readErrorFrame(t, client, cs2)
	if len(body) != 1 || body[0] != 0x02 {
		t.Fatalf("want Error body [0x02] (malformed), got %x", body)
	}
	client.Close()
}

// pushRefusalBody drives one Push whose handler fails with err and returns
// the Error frame body.
func pushRefusalBody(t *testing.T, handlerErr error) []byte {
	t.Helper()
	relayKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	client, server := net.Pipe()
	go session.AcceptPush(server, relayKey, &mockHandler{err: handlerErr}) //nolint:errcheck
	cs1, cs2 := pushClientHandshake(t, client, relayKey.Public)

	env := make([]byte, 40) // ≥32 bytes so the mock sees a well-formed shape
	sendPushFrame(t, client, cs1, env)
	body := readErrorFrame(t, client, cs2)
	client.Close()
	return body
}

func TestAcceptReceive_UnknownFrameTypeGetsGenericError(t *testing.T) {
	relayKey, _ := noise.DH25519.GenerateKeypair(nil)
	deviceKey, _ := noise.DH25519.GenerateKeypair(nil)

	client, server := net.Pipe()
	h := newReceiveHandler()
	go session.AcceptReceive(server, relayKey, h) //nolint:errcheck
	cs1, cs2 := doXXHandshake(t, client, relayKey.Public, deviceKey)

	select {
	case <-h.connectedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReceiveConnect was not called")
	}

	// Unknown frame type 0x7f: generic Error, malformed code, no echoed bytes.
	enc, err := cs1.Encrypt(nil, nil, []byte{0x7f, 0, 0, 0, 0})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	writeMsg(t, client, enc)

	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	raw := readMsg(t, client)
	plain, err := cs2.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	typ, body, ok := session.Parse(plain)
	if !ok || typ != session.MsgTypeError {
		t.Fatalf("want Error frame, got type=0x%02x ok=%v", typ, ok)
	}
	if len(body) != 1 || body[0] != 0x02 {
		t.Fatalf("want Error body [0x02] (malformed), got %x", body)
	}
	client.Close()
}

func TestErrorReason_TemporaryBit(t *testing.T) {
	if session.ReasonTooLarge.Temporary() || session.ReasonMalformed.Temporary() {
		t.Error("permanent codes must not report Temporary()")
	}
	for _, r := range []session.ErrorReason{
		session.ReasonInboxFull, session.ReasonRateLimited,
		session.ReasonUnavailable, session.ReasonInternal,
	} {
		if !r.Temporary() {
			t.Errorf("code 0x%02x must report Temporary()", byte(r))
		}
	}
}
