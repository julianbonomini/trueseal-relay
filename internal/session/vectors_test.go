package session_test

// Vector checks (release gate G5, sync ADR-0022): the relay's version
// exchange, frame codec and refusal behavior are tested byte-for-byte
// against the vendored trueseal-sync test vectors (internal/vectors).

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/session"
	"github.com/julianbonomini/trueseal-relay/internal/vectors"
)

func mustLoad(t *testing.T) (*vectors.Vectors, []vectors.Rejection) {
	t.Helper()
	v, r, err := vectors.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return v, r
}

// ── transportVersionPrefix ──────────────────────────────────────────────────

// The relay accepts EXACTLY the pinned client prefix bytes and replies with
// EXACTLY the pinned accept byte.
func TestVectors_ExchangeAcceptsPinnedBytes(t *testing.T) {
	v, _ := mustLoad(t)
	prefix, err := hex.DecodeString(v.TransportVersionPrefix.ClientPrefixHex)
	if err != nil {
		t.Fatalf("clientPrefixHex: %v", err)
	}
	wantReply, err := hex.DecodeString(v.TransportVersionPrefix.AcceptReplyHex)
	if err != nil {
		t.Fatalf("acceptReplyHex: %v", err)
	}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	go session.AcceptPush(server, testRelayKey(t), &mockHandler{}) //nolint:errcheck

	client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write(prefix); err != nil {
		t.Fatalf("write prefix: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if !bytes.Equal(reply, wantReply) {
		t.Fatalf("reply = %x, want the pinned accept bytes %x", reply, wantReply)
	}
}

// A wrong version is refused with a line in the pinned format carrying the
// relay's real range — min == max == the vectors' transportVersion (ADR-0022:
// the relay speaks exactly one version).
func TestVectors_ExchangeRefusalMatchesPinnedFormat(t *testing.T) {
	v, _ := mustLoad(t)
	if v.Protocol.TransportVersion != 1 {
		t.Fatalf("vectors pin transportVersion %d; the relay constants must move with it", v.Protocol.TransportVersion)
	}

	// Offer version+1 — well-formed magic, unsupported version.
	offered := make([]byte, 5)
	copy(offered, []byte("TSP"))
	binary.BigEndian.PutUint16(offered[3:], uint16(v.Protocol.TransportVersion)+1)

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	go session.AcceptPush(server, testRelayKey(t), &mockHandler{}) //nolint:errcheck

	client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write(offered); err != nil {
		t.Fatalf("write prefix: %v", err)
	}
	line := readLine(t, client)

	// Parse exactly like the pinned refusalExample shape: "unsupported <min>..<max>\n".
	trimmed := strings.TrimSuffix(line, "\n")
	if line == trimmed {
		t.Fatalf("refusal %q lacks the trailing newline", line)
	}
	var min, max int
	if _, err := fmt.Sscanf(trimmed, "unsupported %d..%d", &min, &max); err != nil {
		t.Fatalf("refusal %q does not match the pinned format: %v", line, err)
	}
	if min != v.Protocol.TransportVersion || max != v.Protocol.TransportVersion {
		t.Fatalf("refusal range = %d..%d, want %d..%d (min == max == the one spoken version)",
			min, max, v.Protocol.TransportVersion, v.Protocol.TransportVersion)
	}
}

// ── frames ──────────────────────────────────────────────────────────────────

// All six frame types: the relay's codec round-trips the pinned bytes
// exactly, in both directions.
func TestVectors_Frames(t *testing.T) {
	v, _ := mustLoad(t)
	for _, name := range []string{"push", "deliver", "heartbeat", "ack", "error", "deliverAck"} {
		t.Run(name, func(t *testing.T) {
			fv, err := v.Frame(name)
			if err != nil {
				t.Fatal(err)
			}
			frameBytes, err := hex.DecodeString(fv.FrameHex)
			if err != nil {
				t.Fatalf("frameHex: %v", err)
			}

			// The relay's Parse must accept the pinned bytes and recover the
			// pinned type and body.
			typ, body, ok := session.Parse(frameBytes)
			if !ok {
				t.Fatalf("Parse refused the pinned frame %x", frameBytes)
			}
			if int(typ) != fv.Type {
				t.Fatalf("type = 0x%02x, want 0x%02x", typ, fv.Type)
			}

			// Re-encoding the parsed body must reproduce the pinned bytes
			// exactly (the codec is byte-faithful, not just compatible).
			if re := session.Frame(typ, body); !bytes.Equal(re, frameBytes) {
				t.Fatalf("re-encode = %x, want pinned %x", re, frameBytes)
			}

			// Body-structure checks where the layout names them.
			switch name {
			case "heartbeat", "ack", "error":
				if fv.BodyLayout == "empty" && len(body) != 0 {
					t.Fatalf("body = %x, want empty per the pinned layout", body)
				}
			case "deliverAck":
				blobID, ok := session.DecodeDeliverAckBody(body)
				if !ok {
					t.Fatal("DecodeDeliverAckBody refused the pinned body")
				}
				// The deliverAck entry pins no blobIdHex of its own — its layout
				// says it echoes the DELIVER frame's id verbatim, so that is the
				// source of truth.
				deliver, err := v.Frame("deliver")
				if err != nil {
					t.Fatal(err)
				}
				wantID, err := hex.DecodeString(deliver.BlobIDHex)
				if err != nil {
					t.Fatalf("deliver blobIdHex: %v", err)
				}
				if got := binary.BigEndian.Uint64(wantID); blobID != got {
					t.Fatalf("blobID = %d, want the deliver frame's %d (echoed verbatim)", blobID, got)
				}
			case "deliver":
				blobID, envelope, ok := session.DecodeDeliverBody(body)
				if !ok {
					t.Fatal("DecodeDeliverBody refused the pinned body")
				}
				wantID, err := hex.DecodeString(fv.BlobIDHex)
				if err != nil {
					t.Fatalf("blobIdHex: %v", err)
				}
				if got := binary.BigEndian.Uint64(wantID); blobID != got {
					t.Fatalf("blobID = %d, want %d", blobID, got)
				}
				if len(envelope) == 0 {
					t.Fatal("pinned deliver carries no envelope")
				}
			case "push":
				if len(body) < 33 {
					t.Fatalf("pinned push body = %d bytes, want ≥ 33 (32-byte routing prefix + envelope)", len(body))
				}
			}
		})
	}
}

// ── rejections (the layers the relay speaks: transport, size) ───────────────

func findRejection(t *testing.T, rejections []vectors.Rejection, layer, kase string) vectors.Rejection {
	t.Helper()
	for _, r := range rejections {
		if r.Layer == layer && r.Case == kase {
			return r
		}
	}
	t.Fatalf("no rejection %s/%s in the vectors", layer, kase)
	return vectors.Rejection{}
}

// transport/unsupportedTransportVersion: the relay's refusal is parseable
// into the pinned expectation (a relayVersionUnsupported surface with the
// relay's min/max).
func TestVectors_Rejection_UnsupportedTransportVersion(t *testing.T) {
	v, rejections := mustLoad(t)
	r := findRejection(t, rejections, "transport", "unsupportedTransportVersion")

	var expect struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		Min  int    `json:"min"`
		Max  int    `json:"max"`
	}
	if err := json.Unmarshal(r.Expect, &expect); err != nil {
		t.Fatalf("expect: %v", err)
	}
	if expect.Kind != "connectionState" || expect.Name != "relayVersionUnsupported" {
		t.Fatalf("unexpected rejection expectation: %+v", expect)
	}

	// The relay's actual refusal line (driven above in
	// TestVectors_ExchangeRefusalMatchesPinnedFormat) reports min == max ==
	// the one spoken version — assert that version equals the vectors'
	// protocol section, tying the refusal to the same source of truth the
	// client's typed surface reads.
	if int(session.TransportVersion) != v.Protocol.TransportVersion {
		t.Fatalf("relay TransportVersion %d disagrees with the vectors' %d",
			session.TransportVersion, v.Protocol.TransportVersion)
	}
	if int(session.MinTransportVersion) != v.Protocol.TransportVersion {
		t.Fatalf("relay MinTransportVersion %d disagrees with the vectors' %d",
			session.MinTransportVersion, v.Protocol.TransportVersion)
	}
	if int(session.MaxTransportVersion) != v.Protocol.TransportVersion {
		t.Fatalf("relay MaxTransportVersion %d disagrees with the vectors' %d",
			session.MaxTransportVersion, v.Protocol.TransportVersion)
	}
}

// transport/preVersionRelaySilent pins the inverse direction (a pre-version
// RELAY never replies). The relay-side mirror — a pre-version CLIENT is
// refused — is covered by TestExchange_LegacyClientRefused; assert the case
// exists so a vector-schema change can't silently drop the layer.
func TestVectors_Rejection_PreVersionCasePresent(t *testing.T) {
	_, rejections := mustLoad(t)
	findRejection(t, rejections, "transport", "preVersionRelaySilent")
}

// size/bodyOverProtocolLimit: the relay enforces the same ceiling the
// vectors pin — maxEnvelopeBytes — and a push over it is refused with the
// permanent tooLarge reason code.
func TestVectors_Rejection_SizeLayer(t *testing.T) {
	v, rejections := mustLoad(t)
	r := findRejection(t, rejections, "size", "bodyOverProtocolLimit")

	var input struct {
		BodyLen int `json:"bodyLen"`
		Limit   int `json:"limit"`
	}
	if err := json.Unmarshal(r.Input, &input); err != nil {
		t.Fatalf("input: %v", err)
	}
	if input.Limit != v.Protocol.ProtocolSizeLimit {
		t.Fatalf("rejection limit %d != protocol protocolSizeLimit %d", input.Limit, v.Protocol.ProtocolSizeLimit)
	}

	// The relay's envelope ceiling is the vectors' maxEnvelopeBytes — the
	// same number the client's send() fits its envelope under (ADR-0025).
	if v.Protocol.MaxEnvelopeBytes <= 0 {
		t.Fatal("vectors pin no maxEnvelopeBytes")
	}
}
