package relay

import "fmt"

// extractEnvelopeRecipient reads exactly one field from a serialized proto
// Envelope: recipient_pub (field 3, 32 bytes — the layout sync ADR-0031
// leaves Relay-readable, pinned by the sync test vectors'
// sealedEnvelope.envelopeProtoLayout). The relay reads nothing else: the
// payload stays opaque and is never inspected.
//
// This is a minimal protobuf wire reader, not a generated decoder — the
// relay has no protobuf dependency. It walks the wire format field by
// field, skipping everything that isn't field 3, and rejects anything it
// can't walk (truncated varints, overlong lengths, deprecated group wire
// types) as malformed.
//
// Errors are returned as plain values; the caller wraps them in
// ErrPushRecipientMalformed so the session layer maps them onto the
// permanent malformed reason code (ADR-0026 relay contract).
func extractEnvelopeRecipient(envelope []byte) (RecipientKey, error) {
	var key RecipientKey
	found := 0

	for pos := 0; pos < len(envelope); {
		tag, n := readVarint(envelope[pos:])
		if n == 0 {
			return key, fmt.Errorf("truncated field tag at offset %d", pos)
		}
		pos += n
		fieldNum := tag >> 3
		wireType := tag & 0x7

		if fieldNum == 0 {
			// Field number 0 is invalid in protobuf — a real decoder (prost
			// on the client) rejects the whole message. Refuse it too rather
			// than skip it, so the relay never accepts an Envelope the
			// recipient would reject wholesale.
			return key, fmt.Errorf("invalid field number 0 at offset %d", pos-n)
		}

		switch wireType {
		case 0: // varint
			_, n := readVarint(envelope[pos:])
			if n == 0 {
				return key, fmt.Errorf("truncated varint field %d at offset %d", fieldNum, pos)
			}
			pos += n
		case 1: // 64-bit
			if len(envelope)-pos < 8 {
				return key, fmt.Errorf("truncated 64-bit field %d at offset %d", fieldNum, pos)
			}
			pos += 8
		case 2: // length-delimited
			length, n := readVarint(envelope[pos:])
			if n == 0 {
				return key, fmt.Errorf("truncated length for field %d at offset %d", fieldNum, pos)
			}
			pos += n
			if uint64(len(envelope)-pos) < length {
				return key, fmt.Errorf("field %d claims %d bytes, %d remain", fieldNum, length, len(envelope)-pos)
			}
			if fieldNum == envelopeRecipientPubField {
				found++
				if length != 32 {
					return key, fmt.Errorf("recipient_pub is %d bytes, want exactly 32", length)
				}
				copy(key[:], envelope[pos:pos+32])
			}
			pos += int(length)
		case 5: // 32-bit
			if len(envelope)-pos < 4 {
				return key, fmt.Errorf("truncated 32-bit field %d at offset %d", fieldNum, pos)
			}
			pos += 4
		default: // 3, 4 (deprecated groups) and 6, 7 (invalid)
			return key, fmt.Errorf("unsupported wire type %d for field %d", wireType, fieldNum)
		}
	}

	if found == 0 {
		return key, fmt.Errorf("no recipient_pub field")
	}
	if found > 1 {
		return key, fmt.Errorf("recipient_pub appears %d times — ambiguous addressee", found)
	}
	return key, nil
}

// envelopeRecipientPubField is the proto field number of recipient_pub in
// the Envelope message. Pinned by the sync test vectors
// (sealedEnvelope.envelopeProtoLayout: "recipient_pub: field 3") — the
// vector-checking tests anchor this constant to real envelope bytes.
const envelopeRecipientPubField = 3

// readVarint decodes one protobuf varint from the front of b.
// Returns the value and the number of bytes consumed; 0 bytes consumed
// means the input is truncated or overlong (a varint may not exceed 10
// bytes, and the 10th may carry only one value bit).
func readVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		if i == 9 && c > 1 {
			return 0, 0 // overlong varint
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}
