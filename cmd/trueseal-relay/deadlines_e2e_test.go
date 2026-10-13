package main

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flynn/noise"
)

// clientXXHandshake performs the client half of the pinned v1 version
// exchange + Noise XX handshake over a real conn (mirrors
// shutdown_test.go's helper, which lives in the external test package).
func clientXXHandshake(t *testing.T, conn net.Conn, deviceKey noise.DHKey) {
	t.Helper()
	prefix := []byte{0x54, 0x53, 0x50, 0x00, 0x01}
	if _, err := conn.Write(prefix); err != nil {
		t.Fatalf("version write: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("version read: %v", err)
	}
	if reply[0] != 0x01 {
		t.Fatalf("version refused (0x%02x)", reply[0])
	}
	cfg := noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		StaticKeypair: deviceKey,
		Prologue:      prefix,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		t.Fatalf("handshake state: %v", err)
	}
	writeU16 := func(msg []byte) {
		out := make([]byte, 2+len(msg))
		binary.BigEndian.PutUint16(out, uint16(len(msg)))
		copy(out[2:], msg)
		if _, err := conn.Write(out); err != nil {
			t.Fatalf("handshake write: %v", err)
		}
	}
	readU16 := func() []byte {
		var lenbuf [2]byte
		if _, err := io.ReadFull(conn, lenbuf[:]); err != nil {
			t.Fatalf("handshake read len: %v", err)
		}
		buf := make([]byte, binary.BigEndian.Uint16(lenbuf[:]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("handshake read body: %v", err)
		}
		return buf
	}
	msg, _, _, err := hs.WriteMessage(nil, nil) // → e
	if err != nil {
		t.Fatalf("xx e: %v", err)
	}
	writeU16(msg)
	if _, _, _, err = hs.ReadMessage(nil, readU16()); err != nil { // ← e,ee,s,es
		t.Fatalf("xx read: %v", err)
	}
	msg, _, _, err = hs.WriteMessage(nil, nil) // → s,se
	if err != nil {
		t.Fatalf("xx s,se: %v", err)
	}
	writeU16(msg)
}

// freePort reserves an ephemeral port, releases it, and returns it for the
// relay to bind. The small race is inherent to testing a separate process.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()
	return strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:")
}

// startRelayWithEnv boots the real binary with the given extra env and
// returns a buffer capturing its (normal-mode) log output. The buffer is
// mutex-guarded: exec copies the child's stdout and stderr from separate
// goroutines while the test reads (CI's -race run caught a plain
// bytes.Buffer racing here).
func startRelayWithEnv(t *testing.T, keyPath string, extra ...string) (*exec.Cmd, *lockedBuf) {
	t.Helper()
	bin := buildBinary(t)
	cmd := exec.Command(bin, "-no-config")
	cmd.Env = append(os.Environ(), serveEnv(t, keyPath)...)
	cmd.Env = append(cmd.Env, extra...)
	out := &lockedBuf{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	return cmd, out
}

// awaitClosed asserts the connection is closed by the peer within maxWait.
func awaitClosed(t *testing.T, conn net.Conn, maxWait time.Duration, what string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(maxWait))
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatalf("%s: connection still open after %v — the configured deadline did not fire", what, maxWait)
	}
}

// TestServe_IdleConnectionsClosedFromConfig is the RELAY-6 completion
// criterion end-to-end: with SHORT VALUES FROM CONFIG (env), the real
// binary closes (a) an idle handshake on both listeners — the client sends
// nothing at all — and (b) a silent Receive Session after a completed
// handshake. And per ADR-0012, none of it produces a per-connection log
// line in normal mode.
func TestServe_IdleConnectionsClosedFromConfig(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	recvPort, pushPort := freePort(t), freePort(t)

	_, logs := startRelayWithEnv(t, keyPath,
		"TRUESEAL_RELAY_LISTEN_RECEIVE=127.0.0.1:"+recvPort,
		"TRUESEAL_RELAY_LISTEN_PUSH=127.0.0.1:"+pushPort,
		"TRUESEAL_RELAY_HANDSHAKE_DEADLINE=150ms",
		"TRUESEAL_RELAY_RECEIVE_IDLE_DEADLINE=200ms",
		"TRUESEAL_RELAY_WRITE_DEADLINE=1s",
	)

	// Wait for the listeners to come up (keypair generation happens first).
	dial := func(port string) net.Conn {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			conn, err := net.Dial("tcp", "127.0.0.1:"+port)
			if err == nil {
				return conn
			}
			if time.Now().After(deadline) {
				t.Fatalf("relay never listened on port %s: %v", port, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// (a) Idle handshake on the RECEIVE listener: connect, send nothing.
	conn := dial(recvPort)
	awaitClosed(t, conn, 5*time.Second, "idle receive handshake")
	conn.Close()

	// (a) Idle handshake on the PUSH listener: connect, send nothing.
	conn = dial(pushPort)
	awaitClosed(t, conn, 5*time.Second, "idle push handshake")
	conn.Close()

	// (b) Silent Receive Session: complete the XX handshake, then go
	// silent — the 200 ms receive-idle deadline closes it.
	conn = dial(recvPort)
	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	clientXXHandshake(t, conn, deviceKey)
	awaitClosed(t, conn, 5*time.Second, "silent receive session")
	conn.Close()

	// ADR-0012 / issue #43: no refusal and no per-connection session error
	// is logged in normal mode — three idle connections produced none.
	time.Sleep(100 * time.Millisecond) // let any (wrongly) emitted line land
	out := logs.String()
	if strings.Contains(out, "session error") || strings.Contains(out, "refus") {
		t.Errorf("idle connections produced normal-mode log lines: %q", out)
	}
}

// TestServe_SessionErrorLogRateLimitedFromConfig drives the full binary:
// garbage-spewing connections (a handshake failure each) produce at most
// one rate-limited, field-free "push session error" line per minute.
func TestServe_SessionErrorLogRateLimitedFromConfig(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	pushPort := freePort(t)

	_, logs := startRelayWithEnv(t, keyPath,
		"TRUESEAL_RELAY_LISTEN_PUSH=127.0.0.1:"+pushPort,
		"TRUESEAL_RELAY_HANDSHAKE_DEADLINE=1s",
	)

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("tcp", "127.0.0.1:"+pushPort)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay never listened: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Ten connections each send one byte of garbage — enough to fail the
	// version prefix and, without rate limiting, produce ten log lines.
	for i := 0; i < 10; i++ {
		conn, err := net.Dial("tcp", "127.0.0.1:"+pushPort)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_, _ = conn.Write([]byte("NOTTSP-garbage"))
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Read(make([]byte, 64)) // read the refusal line, then close
		conn.Close()
	}

	// Let the (single) rate-limited line land, then count.
	time.Sleep(300 * time.Millisecond)
	out := logs.String()
	n := strings.Count(out, "push session error")
	if n > 1 {
		t.Errorf("10 garbage connections produced %d session-error lines (rate limit broken): %q", n, out)
	}
	if strings.Contains(out, "NOTTSP") || strings.Contains(out, "addr=") {
		t.Errorf("session-error log leaks client data: %q", out)
	}
}
