package session

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// driveClient runs the client half of the exchange concurrently (net.Pipe is
// synchronous): writes prefix, reads one reply line, returns it.
func driveClient(t *testing.T, conn net.Conn, prefix []byte) []byte {
	t.Helper()
	if _, err := conn.Write(prefix); err != nil {
		t.Fatalf("client: write prefix: %v", err)
	}
	// Read the reply: either the single 0x01 accept byte or an
	// "unsupported ...\n" line.
	first := make([]byte, 1)
	if _, err := io.ReadFull(conn, first); err != nil {
		t.Fatalf("client: read reply: %v", err)
	}
	if first[0] == 0x01 {
		return first
	}
	reply := append([]byte{}, first...)
	buf := make([]byte, 1)
	for len(reply) < 64 {
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("client: read refusal line: %v", err)
		}
		reply = append(reply, buf[0])
		if buf[0] == '\n' {
			break
		}
	}
	return reply
}

// runExchange drives the relay half in a goroutine and the client half in
// the caller. Returns (replyBytes, prologue, relayErr).
func runExchange(t *testing.T, clientPrefix []byte) ([]byte, []byte, error) {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close()

	type result struct {
		prologue []byte
		err      error
	}
	resCh := make(chan result, 1)
	go func() {
		p, err := exchangeTransportVersion(server)
		resCh <- result{p, err}
	}()

	reply := driveClient(t, client, clientPrefix)
	res := <-resCh
	server.Close()
	return reply, res.prologue, res.err
}

func TestExchange_SupportedVersionAccepted(t *testing.T) {
	prefix := []byte{0x54, 0x53, 0x50, 0x00, 0x01} // "TSP" || 1:u16 BE
	reply, prologue, err := runExchange(t, prefix)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(reply) != 1 || reply[0] != 0x01 {
		t.Fatalf("reply = %x, want 01", reply)
	}
	if string(prologue) != string(prefix) {
		t.Fatalf("prologue = %x, want the exact prefix bytes %x", prologue, prefix)
	}
}

func TestExchange_UnsupportedVersionRefused(t *testing.T) {
	prefix := []byte{0x54, 0x53, 0x50, 0x00, 0x02} // version 2
	reply, prologue, err := runExchange(t, prefix)
	var refusal *VersionRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *VersionRefusalError", err)
	}
	if refusal.Min != 1 || refusal.Max != 1 {
		t.Fatalf("refusal range = %d..%d, want 1..1", refusal.Min, refusal.Max)
	}
	if string(reply) != "unsupported 1..1\n" {
		t.Fatalf("reply = %q, want %q", reply, "unsupported 1..1\n")
	}
	if prologue != nil {
		t.Fatalf("prologue = %x, want nil on refusal", prologue)
	}
}

func TestExchange_VersionZeroRefused(t *testing.T) {
	// 0 is the reserved pre-version format (ADR-0022).
	prefix := []byte{0x54, 0x53, 0x50, 0x00, 0x00}
	reply, _, err := runExchange(t, prefix)
	var refusal *VersionRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *VersionRefusalError", err)
	}
	if string(reply) != "unsupported 1..1\n" {
		t.Fatalf("reply = %q", reply)
	}
}

func TestExchange_LegacyClientRefused(t *testing.T) {
	// A pre-version client opens with the Noise length prefix + handshake
	// bytes — no "TSP" magic. The relay must reply with the refusal line and
	// close (ADR-0022: today's unversioned format is refused).
	legacy := []byte{0x00, 0x30, 0xde, 0xad, 0xbe}
	reply, _, err := runExchange(t, legacy)
	var refusal *VersionRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *VersionRefusalError", err)
	}
	if string(reply) != "unsupported 1..1\n" {
		t.Fatalf("reply = %q", reply)
	}
}

func TestExchange_EOFBeforePrefixIsIOErrorNotRefusal(t *testing.T) {
	server, client := net.Pipe()
	client.Close() // nothing is ever written
	_, err := exchangeTransportVersion(server)
	if err == nil {
		t.Fatal("want error on EOF")
	}
	var refusal *VersionRefusalError
	if errors.As(err, &refusal) {
		t.Fatalf("EOF must not be a version refusal: %v", err)
	}
}

func TestExchange_RefusalLineIsWrittenBeforeReturn(t *testing.T) {
	// The client must be able to READ the refusal — the write happens before
	// exchangeTransportVersion returns (the caller then closes).
	server, client := net.Pipe()
	defer server.Close()
	resCh := make(chan error, 1)
	go func() {
		_, err := exchangeTransportVersion(server)
		resCh <- err
	}()
	if _, err := client.Write([]byte{0x54, 0x53, 0x50, 0x00, 0x09}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	reply := make([]byte, 0, 16)
	buf := make([]byte, 1)
	for {
		if _, err := io.ReadFull(client, buf); err != nil {
			t.Fatalf("read refusal: %v", err)
		}
		reply = append(reply, buf[0])
		if buf[0] == '\n' {
			break
		}
	}
	if string(reply) != "unsupported 1..1\n" {
		t.Fatalf("reply = %q", reply)
	}
	<-resCh
}
