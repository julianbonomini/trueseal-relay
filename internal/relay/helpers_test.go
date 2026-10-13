package relay_test

import (
	"io"
	"net"
	"testing"
)

// testClientPrefix is the pinned Transport Version prefix a v1 client sends
// (ADR-0022; internal/session/version.go; the sync test vectors pin the same
// bytes — internal/vectors).
var testClientPrefix = []byte{0x54, 0x53, 0x50, 0x00, 0x01}

// clientVersionExchange performs the client half of the ADR-0022 version
// exchange and returns the prologue the Noise handshake must bind.
func clientVersionExchange(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	if _, err := conn.Write(testClientPrefix); err != nil {
		t.Fatalf("version exchange write: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("version exchange read: %v", err)
	}
	if reply[0] != 0x01 {
		t.Fatalf("version exchange: relay refused (reply 0x%02x)", reply[0])
	}
	return testClientPrefix
}

func writeMsg(t *testing.T, conn net.Conn, msg []byte) {
	t.Helper()
	out := make([]byte, 2+len(msg))
	out[0] = byte(len(msg) >> 8)
	out[1] = byte(len(msg))
	copy(out[2:], msg)
	if _, err := conn.Write(out); err != nil {
		t.Fatalf("writeMsg: %v", err)
	}
}

func readMsg(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	lenbuf := make([]byte, 2)
	if _, err := conn.Read(lenbuf); err != nil {
		t.Fatalf("readMsg len: %v", err)
	}
	n := int(lenbuf[0])<<8 | int(lenbuf[1])
	buf := make([]byte, n)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("readMsg body: %v", err)
	}
	return buf
}

func writeMsgRaw(conn net.Conn, msg []byte) error {
	out := make([]byte, 2+len(msg))
	out[0] = byte(len(msg) >> 8)
	out[1] = byte(len(msg))
	copy(out[2:], msg)
	_, err := conn.Write(out)
	return err
}
