package session

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// Transport Version exchange (sync ADR-0022; the SYNC-2 JSON vectors pin the
// exact bytes — see internal/vectors). Every connection, Receive (XX) and
// Push (NK), starts with a plaintext 5-byte prefix BEFORE the Noise
// handshake:
//
//	client → relay : "TSP" || version:u16 BE   (5 bytes, plaintext)
//	relay → client : 0x01                      (supported — proceed)
//	               | "unsupported <min>..<max>\n" (refused — then close)
//
// On 0x01 the exact 5 prefix bytes become the Noise prologue, binding the
// Transport Version into the handshake hash: a tampered prefix breaks the
// handshake. The relay's min equals its max — it speaks exactly one version
// and refuses everything else, including the pre-version format (no
// compatibility decoder).
const (
	// TransportVersion is the one Transport Version this relay speaks.
	TransportVersion uint16 = 1
	// MinTransportVersion and MaxTransportVersion are the supported range
	// reported in the refusal line. Both equal TransportVersion (ADR-0022:
	// no negotiation, exactly one version).
	MinTransportVersion uint16 = TransportVersion
	MaxTransportVersion uint16 = TransportVersion
)

// transportPrefixLen is the wire length of the version prefix:
// 3 bytes magic + 2 bytes version.
const transportPrefixLen = 5

// replySupported is the accept byte (sync version.rs REPLY_SUPPORTED).
const replySupported = 0x01

var prefixMagic = []byte("TSP")

// VersionRefusalError is returned when a client offers a prefix the relay
// refuses (wrong magic, unsupported version, or the reserved version 0). The
// refusal line has already been written to the connection; the caller just
// closes.
type VersionRefusalError struct {
	Min, Max uint16
}

func (e *VersionRefusalError) Error() string {
	return fmt.Sprintf("session: unsupported transport version (supported %d..%d)", e.Min, e.Max)
}

// exchangeTransportVersion performs the relay half of the ADR-0022 prefix
// exchange on a fresh connection. On success it returns the exact 5 prefix
// bytes to bind as the Noise prologue. On refusal it writes
// "unsupported <min>..<max>\n" and returns a *VersionRefusalError. A plain
// I/O error (EOF, reset) is returned as-is — nothing is written.
//
// The read has no deadline of its own: handshake and session deadlines are
// RELAY-6's scope (ADR-0012), applied by the listener around the accept path.
func exchangeTransportVersion(conn net.Conn) ([]byte, error) {
	prefix := make([]byte, transportPrefixLen)
	if _, err := io.ReadFull(conn, prefix); err != nil {
		return nil, err
	}
	if bytes.Equal(prefix[:3], prefixMagic) {
		v := binary.BigEndian.Uint16(prefix[3:])
		if v >= MinTransportVersion && v <= MaxTransportVersion {
			if _, err := conn.Write([]byte{replySupported}); err != nil {
				return nil, err
			}
			return prefix, nil
		}
	}
	// Refused: wrong magic (legacy/pre-version client) or a version outside
	// the supported range. The line is written BEFORE returning so the
	// client can always read it; the caller closes after.
	line := fmt.Sprintf("unsupported %d..%d\n", MinTransportVersion, MaxTransportVersion)
	_, _ = conn.Write([]byte(line)) // best-effort; the refusal stands either way
	return nil, &VersionRefusalError{Min: MinTransportVersion, Max: MaxTransportVersion}
}
