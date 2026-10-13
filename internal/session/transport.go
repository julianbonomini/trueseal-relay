package session

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
)

// writeNoiseMsg writes a length-prefixed message: [len:u16 BE][body].
// Length and body go out in a single Write so frames from different writers
// can never interleave on the wire (RELAY-2).
func writeNoiseMsg(conn net.Conn, msg []byte) error {
	out := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(out, uint16(len(msg)))
	copy(out[2:], msg)
	_, err := conn.Write(out)
	return err
}

// readNoiseMsg reads a length-prefixed message: [len:u16 BE][body].
func readNoiseMsg(conn net.Conn) ([]byte, error) {
	var lenbuf [2]byte
	if _, err := io.ReadFull(conn, lenbuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(lenbuf[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset")
}

func formatErr(op string, err error) error {
	return fmt.Errorf("%s: %w", op, err)
}
