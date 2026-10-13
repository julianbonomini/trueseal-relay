package main

// RELAY-9 (issue #29, ADR-0012 "Logging"/"IP addresses", release gate
// G7): dev mode end-to-end against the REAL binary over REAL TCP.
//
// Dev mode must (a) print the prominent startup warning, (b) report
// dev_mode on /healthz, (c) emit the verbose per-message detail a
// developer needs — key prefixes, sizes, Blob IDs, session lifecycle —
// and (d) still never log a peer address or payload bytes. That last
// property is what makes the "never logs IPs" promise unconditional.

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flynn/noise"
)

// ── test client helpers (binary-level; the in-process suites have their own) ──

// devClientVersionExchange performs the pinned v1 prefix exchange and
// returns the prologue.
func devClientVersionExchange(t *testing.T, conn net.Conn) []byte {
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
	return prefix
}

func devWriteMsg(t *testing.T, conn net.Conn, msg []byte) {
	t.Helper()
	out := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(out, uint16(len(msg)))
	copy(out[2:], msg)
	if _, err := conn.Write(out); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func devReadMsg(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	var lenbuf [2]byte
	if _, err := io.ReadFull(conn, lenbuf[:]); err != nil {
		t.Fatalf("read len: %v", err)
	}
	buf := make([]byte, binary.BigEndian.Uint16(lenbuf[:]))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return buf
}

// devFrame builds a wire frame: [type:1][body length:4 u32 BE][body]
// (internal/session/frame.go's layout).
func devFrame(t byte, body []byte) []byte {
	out := make([]byte, 5+len(body))
	out[0] = t
	binary.BigEndian.PutUint32(out[1:5], uint32(len(body)))
	copy(out[5:], body)
	return out
}

// devPush performs one anonymous NK Push Session against the real binary:
// handshake, one Push frame, expect Ack. The envelope carries field 3 =
// the recipient (RELAY-3's routing check) plus an unknown field holding
// the payload marker, so the test can prove the marker never reaches a
// log line.
func devPush(t *testing.T, pushAddr, relayPubHex string, recipient [32]byte, marker string) int {
	t.Helper()
	relayPub, err := hex.DecodeString(relayPubHex)
	if err != nil {
		t.Fatalf("relay pub hex: %v", err)
	}
	conn, err := net.Dial("tcp", pushAddr)
	if err != nil {
		t.Fatalf("dial push: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	prologue := devClientVersionExchange(t, conn)
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:     noise.HandshakeNK,
		Initiator:   true,
		PeerStatic:  relayPub,
		Prologue:    prologue,
	})
	if err != nil {
		t.Fatalf("NK hs: %v", err)
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("NK write e: %v", err)
	}
	devWriteMsg(t, conn, msg)
	_, cs1, cs2, err := hs.ReadMessage(nil, devReadMsg(t, conn))
	if err != nil {
		t.Fatalf("NK read resp: %v", err)
	}

	// Envelope: field 3 (recipient) + field 15 (the payload marker, which
	// the relay must never parse or log).
	envelope := []byte{0x1a, 0x20}
	envelope = append(envelope, recipient[:]...)
	envelope = append(envelope, 0x7a, byte(len(marker)))
	envelope = append(envelope, marker...)
	body := make([]byte, 32+len(envelope))
	copy(body[:32], recipient[:])
	copy(body[32:], envelope)

	// MsgTypePush (0x01), properly framed — the FIRST and only encrypt on
	// cs1, so the nonce sequence stays aligned with the relay's cs1.
	pushFrame, err := cs1.Encrypt(nil, nil, devFrame(0x01, body))
	if err != nil {
		t.Fatalf("encrypt push: %v", err)
	}
	devWriteMsg(t, conn, pushFrame)

	// The reply must be an Ack (0x04), not an Error (0x05) — decrypt it.
	raw := devReadMsg(t, conn)
	plain, err := cs2.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt reply: %v", err)
	}
	if len(plain) < 1 || plain[0] != 0x04 {
		reason := byte(0)
		if len(plain) >= 6 {
			reason = plain[5]
		}
		t.Fatalf("push refused: reply frame type 0x%02x (reason 0x%02x), want Ack 0x04", plain[0], reason)
	}
	return len(envelope)
}

// devReceive opens a Receive Session for deviceKey, reads one Deliver
// frame and answers with its DeliverAck. Returns the delivered blob ID.
func devReceive(t *testing.T, recvAddr string, deviceKey noise.DHKey) int64 {
	t.Helper()
	conn, err := net.Dial("tcp", recvAddr)
	if err != nil {
		t.Fatalf("dial receive: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	prologue := devClientVersionExchange(t, conn)
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s),
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		StaticKeypair: deviceKey,
		Prologue:      prologue,
	})
	if err != nil {
		t.Fatalf("XX hs: %v", err)
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("XX write e: %v", err)
	}
	devWriteMsg(t, conn, msg)
	if _, _, _, err = hs.ReadMessage(nil, devReadMsg(t, conn)); err != nil {
		t.Fatalf("XX read e,ee,s,es: %v", err)
	}
	msg, csSend, csRecv, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatalf("XX write s,se: %v", err)
	}
	devWriteMsg(t, conn, msg)

	// Read one Deliver frame: type 0x02, body = [blob_id:8][envelope].
	raw := devReadMsg(t, conn)
	plain, err := csRecv.Decrypt(nil, nil, raw)
	if err != nil {
		t.Fatalf("decrypt deliver: %v", err)
	}
	if len(plain) < 5+8 || plain[0] != 0x02 /* MsgTypeDeliver */ {
		t.Fatalf("want a Deliver frame, got %x", plain)
	}
	blobID := int64(binary.BigEndian.Uint64(plain[5:13]))

	// DeliverAck: type 0x06, body = the 8-byte blob ID.
	ackBody := make([]byte, 8)
	binary.BigEndian.PutUint64(ackBody, uint64(blobID))
	ackFrame, err := csSend.Encrypt(nil, nil, devFrame(0x06, ackBody))
	if err != nil {
		t.Fatalf("encrypt ack: %v", err)
	}
	devWriteMsg(t, conn, ackFrame)
	return blobID
}

// awaitLog polls the relay's captured log output for substr.
func awaitLog(t *testing.T, logs *lockedBuf, substr string, maxWait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), substr) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log line %q never appeared within %v. Output so far:\n%s", substr, maxWait, logs.String())
}

// relayPubFromLogs extracts the startup "relay public key: <hex>" line.
func relayPubFromLogs(t *testing.T, logs *lockedBuf) string {
	t.Helper()
	awaitLog(t, logs, "relay public key: ", 5*time.Second)
	for _, line := range strings.Split(logs.String(), "\n") {
		if i := strings.Index(line, "relay public key: "); i >= 0 {
			return strings.TrimSpace(line[i+len("relay public key: "):])
		}
	}
	t.Fatal("relay public key line vanished")
	return ""
}

func getHealthz(t *testing.T, addr string) map[string]any {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for i := 0; i < 50; i++ {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz status = %d", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("healthz body is not JSON: %v", err)
		}
		return body
	}
	t.Fatalf("healthz unreachable: %v", lastErr)
	return nil
}

// TestServe_DevMode is the criterion end-to-end: the real binary in dev
// mode (via TRUESEAL_RELAY_DEV=1) warns at startup, reports dev_mode on
// /healthz, logs the verbose detail (key prefix, envelope size, blob ID,
// session lifecycle) — and over REAL TCP still never logs the peer
// address or the payload marker.
func TestServe_DevMode(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	recvPort, pushPort, healthPort := freePort(t), freePort(t), freePort(t)

	_, logs := startRelayWithEnv(t, keyPath,
		"TRUESEAL_RELAY_LISTEN_RECEIVE=127.0.0.1:"+recvPort,
		"TRUESEAL_RELAY_LISTEN_PUSH=127.0.0.1:"+pushPort,
		"TRUESEAL_RELAY_LISTEN_HEALTH=127.0.0.1:"+healthPort,
		"TRUESEAL_RELAY_DEV=1",
	)

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("relay output:\n%s", logs.String())
		}
	})

	// (a) prominent startup warning.
	awaitLog(t, logs, "DEV MODE ENABLED", 5*time.Second)

	// (b) /healthz reports dev_mode.
	health := getHealthz(t, "127.0.0.1:"+healthPort)
	if health["dev_mode"] != true {
		t.Errorf("dev-mode /healthz dev_mode = %v, want true", health["dev_mode"])
	}
	if health["status"] != "ok" {
		t.Errorf("dev-mode /healthz status = %v, want ok", health["status"])
	}

	relayPub := relayPubFromLogs(t, logs)

	// A full cycle over real TCP: push to a device, receive + DeliverAck.
	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device keypair: %v", err)
	}
	var recipient [32]byte
	copy(recipient[:], deviceKey.Public)
	marker := "PAYLOAD-MARKER-7f3a9c-never-in-logs"
	envSize := devPush(t, "127.0.0.1:"+pushPort, relayPub, recipient, marker)
	blobID := devReceive(t, "127.0.0.1:"+recvPort, deviceKey)

	// (c) verbose detail is present.
	prefix := hex.EncodeToString(deviceKey.Public[:4])
	awaitLog(t, logs, "recv: device connected key="+prefix, 5*time.Second)
	awaitLog(t, logs, fmt.Sprintf("push: recipient=%s envelope=%d bytes", prefix, envSize), 5*time.Second)
	awaitLog(t, logs, fmt.Sprintf("ack: device=%s blob_id=%d", prefix, blobID), 5*time.Second)
	awaitLog(t, logs, "push: NK handshake complete (anonymous sender)", 5*time.Second)

	out := logs.String()

	// (d) never a peer address, never payload bytes — in dev mode, over
	// real TCP. The client dialed FROM 127.0.0.1 with ephemeral ports;
	// neither the loopback IP nor any client port may appear. (Listener
	// addresses in the startup lines are the relay's OWN config, allowed
	// by ADR-0012 — so assert on the client-visible forms instead.)
	if strings.Contains(out, marker) {
		t.Errorf("dev-mode log contains payload bytes: %q", out)
	}
	fullKey := hex.EncodeToString(deviceKey.Public)
	if strings.Contains(out, fullKey) {
		t.Errorf("dev-mode log contains the FULL device key: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		// Listener startup lines name the relay's own configured
		// addresses; every other line must be address-free.
		if strings.Contains(line, "listener on ") || strings.Contains(line, "running (push=") {
			continue
		}
		if strings.Contains(line, "127.0.0.1") {
			t.Errorf("dev-mode log line contains a peer address: %q", line)
		}
	}
}

// TestServe_DevModeFlag: the -dev flag enables dev mode exactly as the
// env var does (criterion: each enables it).
func TestServe_DevModeFlag(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	healthPort := freePort(t)

	bin := buildBinary(t)
	cmd, logs := startRelayWithArgs(t, bin, keyPath,
		[]string{"-dev"},
		"TRUESEAL_RELAY_LISTEN_HEALTH=127.0.0.1:"+healthPort,
	)
	_ = cmd

	awaitLog(t, logs, "DEV MODE ENABLED", 5*time.Second)
	health := getHealthz(t, "127.0.0.1:"+healthPort)
	if health["dev_mode"] != true {
		t.Errorf("-dev /healthz dev_mode = %v, want true", health["dev_mode"])
	}
}

// TestServe_NormalModeDefault: dev mode is OFF by default, /healthz says
// so, and a full session produces none of the dev detail lines.
func TestServe_NormalModeDefault(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	recvPort, pushPort, healthPort := freePort(t), freePort(t), freePort(t)

	_, logs := startRelayWithEnv(t, keyPath,
		"TRUESEAL_RELAY_LISTEN_RECEIVE=127.0.0.1:"+recvPort,
		"TRUESEAL_RELAY_LISTEN_PUSH=127.0.0.1:"+pushPort,
		"TRUESEAL_RELAY_LISTEN_HEALTH=127.0.0.1:"+healthPort,
	)

	health := getHealthz(t, "127.0.0.1:"+healthPort)
	if health["dev_mode"] != false {
		t.Errorf("normal /healthz dev_mode = %v, want false", health["dev_mode"])
	}
	if strings.Contains(logs.String(), "DEV MODE") {
		t.Errorf("normal mode printed the dev warning: %q", logs.String())
	}

	relayPub := relayPubFromLogs(t, logs)
	deviceKey, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("device keypair: %v", err)
	}
	var recipient [32]byte
	copy(recipient[:], deviceKey.Public)
	marker := "PAYLOAD-MARKER-normal-mode"
	devPush(t, "127.0.0.1:"+pushPort, relayPub, recipient, marker)
	devReceive(t, "127.0.0.1:"+recvPort, deviceKey)

	// Give any (incorrect) per-message line a moment to appear.
	time.Sleep(300 * time.Millisecond)
	out := logs.String()
	prefix := hex.EncodeToString(deviceKey.Public[:4])
	for _, banned := range []string{prefix, marker, "blob_id=", "envelope="} {
		if strings.Contains(out, banned) {
			t.Errorf("normal-mode log contains per-message detail %q: %q", banned, out)
		}
	}
}

// startRelayWithArgs is startRelayWithEnv plus explicit argv (for -dev).
func startRelayWithArgs(t *testing.T, bin, keyPath string, args []string, extraEnv ...string) (*exec.Cmd, *lockedBuf) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"-no-config"}, args...)...)
	cmd.Env = append(os.Environ(), serveEnv(t, keyPath)...)
	cmd.Env = append(cmd.Env, extraEnv...)
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
