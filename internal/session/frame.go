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
	case MsgTypePush, MsgTypeDeliver, MsgTypeHeartbeat, MsgTypeAck:
	default:
		return 0, nil, false
	}
	l := uint32(raw[1])<<24 | uint32(raw[2])<<16 | uint32(raw[3])<<8 | uint32(raw[4])
	if uint32(len(raw)) < 5+l {
		return 0, nil, false
	}
	return t, raw[5 : 5+l], true
}
