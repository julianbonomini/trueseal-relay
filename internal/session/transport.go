package session

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
)

// writeNoiseMsg writes a length-prefixed message: [len:u16 BE][body].
func writeNoiseMsg(conn net.Conn, msg []byte) error {
	lenbuf := [2]byte{byte(len(msg) >> 8), byte(len(msg))}
	if _, err := conn.Write(lenbuf[:]); err != nil {
		return err
	}
	_, err := conn.Write(msg)
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
