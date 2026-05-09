package session

// MsgType is the one-byte type tag in the wire protocol framing.
// All messages: [type:u8][len:u32 BE][body:bytes]
// See ADR-0008.
type MsgType uint8

const (
	MsgTypePush      MsgType = 0x01
	MsgTypeDeliver   MsgType = 0x02
	MsgTypeHeartbeat MsgType = 0x03
	// MsgTypeAck body: empty.
	// Semantic: "persisted to InboxStore" — not "received bytes". See ADR-0008.
	// The 8-byte u64 sequence proposed in an earlier draft was rejected: the relay
	// never uses sequence numbers (ordering is recipient-side); dedup is impossible
	// without sender identity on NK sessions; NK already authenticates the relay so
	// a well-formed Ack is sufficient proof of persistence.
	MsgTypeAck MsgType = 0x04
	// MsgTypeError body: empty.
	// Semantic: permanent rejection — the relay will not store the blob.
	// The client must NOT retry the same blob (non-retryable).
	// Sent when OnPush returns an error (e.g. oversized envelope). See ADR-0008.
	MsgTypeError MsgType = 0x05
	// MsgTypeDeliverAck body: [blob_id: 8 bytes u64 BE].
	// Sent by the Device after durably persisting a Deliver frame.
	// The blob_id echoes the opaque identifier from the corresponding Deliver frame.
	// The relay uses it to call DeleteByIDs on the InboxStore. See ADR-0009.
	MsgTypeDeliverAck MsgType = 0x06
)

// Frame encodes a typed message into wire bytes.
func Frame(t MsgType, body []byte) []byte {
	out := make([]byte, 5+len(body))
	out[0] = byte(t)
	l := uint32(len(body))
	out[1] = byte(l >> 24)
	out[2] = byte(l >> 16)
	out[3] = byte(l >> 8)
	out[4] = byte(l)
	copy(out[5:], body)
	return out
}

// Parse decodes a framed message. Returns (type, body, ok).
// ok is false if raw is malformed or the type is unknown.
func Parse(raw []byte) (MsgType, []byte, bool) {
	if len(raw) < 5 {
		return 0, nil, false
	}
	t := MsgType(raw[0])
	switch t {
	case MsgTypePush, MsgTypeDeliver, MsgTypeHeartbeat, MsgTypeAck, MsgTypeError, MsgTypeDeliverAck:
	default:
		return 0, nil, false
	}
	l := uint32(raw[1])<<24 | uint32(raw[2])<<16 | uint32(raw[3])<<8 | uint32(raw[4])
	if uint32(len(raw)) < 5+l {
		return 0, nil, false
	}
	return t, raw[5 : 5+l], true
}

// EncodeDeliverBody encodes a Deliver frame body: [blob_id: 8 bytes u64 BE][envelope].
// See ADR-0008 and ADR-0009.
func EncodeDeliverBody(blobID uint64, envelope []byte) []byte {
	out := make([]byte, 8+len(envelope))
	putU64BE(out, blobID)
	copy(out[8:], envelope)
	return out
}

// DecodeDeliverBody decodes a Deliver frame body.
// Returns (blobID, envelope, ok). ok is false if body is shorter than 8 bytes.
func DecodeDeliverBody(body []byte) (blobID uint64, envelope []byte, ok bool) {
	if len(body) < 8 {
		return 0, nil, false
	}
	return getU64BE(body), body[8:], true
}

// EncodeDeliverAckBody encodes a DeliverAck frame body: [blob_id: 8 bytes u64 BE].
func EncodeDeliverAckBody(blobID uint64) []byte {
	out := make([]byte, 8)
	putU64BE(out, blobID)
	return out
}

// DecodeDeliverAckBody decodes a DeliverAck frame body.
// Returns (blobID, ok). ok is false if body is shorter than 8 bytes.
func DecodeDeliverAckBody(body []byte) (blobID uint64, ok bool) {
	if len(body) < 8 {
		return 0, false
	}
	return getU64BE(body), true
}

func putU64BE(b []byte, v uint64) {
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
}

func getU64BE(b []byte) uint64 {
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}
