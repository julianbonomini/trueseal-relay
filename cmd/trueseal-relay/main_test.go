package main

import (
	"bytes"
	"encoding/hex"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/keypair"
)

type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestConnectionRefusal_SilentInNormalMode is the RELAY-6 logging rule
// (issue #43, ADR-0012): no refusal is logged in normal mode. It
// supersedes the RELAY-4 test that pinned the (then logged) refusal line's
// field-freeness — a line that is never emitted trivially carries no
// client address, and a scanner cannot flood the operator's logs.
func TestConnectionRefusal_SilentInNormalMode(t *testing.T) {
	var lb lockedBuf
	prev := log.Writer()
	log.SetOutput(&lb)
	defer log.SetOutput(prev)

	// The accept loop's refusal path: count only, never log.
	refusalCounters.push.Add(1)
	refusalCounters.recv.Add(1)

	if out := lb.String(); out != "" {
		t.Errorf("a connection refusal produced a normal-mode log line: %q", out)
	}
}

// TestSessionErrorLog_RateLimited (RELAY-6): session errors may be logged
// (ADR-0012 allows errors) but never one line per connection — at most one
// line per listener kind per minute, with the suppressed count attached.
func TestSessionErrorLog_RateLimited(t *testing.T) {
	var lb lockedBuf
	prev := log.Writer()
	log.SetOutput(&lb)
	defer log.SetOutput(prev)

	now := time.Now()
	clock := func() time.Time { return now }
	el := newSessionErrorLog(clock)

	el.logf("push") // first line: emitted
	el.logf("push") // inside the window: suppressed
	el.logf("push") // inside the window: suppressed
	el.logf("recv") // a different kind has its own window: emitted

	lines := strings.Count(lb.String(), "\n")
	if lines != 2 {
		t.Fatalf("want 2 log lines (one per kind), got %d: %q", lines, lb.String())
	}

	// After the window, the next error reports the suppressed count.
	now = now.Add(2 * sessionErrorLogInterval)
	el.logf("push")
	out := lb.String()
	if !strings.Contains(out, "3 since last report") {
		t.Errorf("suppressed count missing from the post-window line: %q", out)
	}
	for _, banned := range []string{"203.0.113.7", "addr="} {
		if strings.Contains(out, banned) {
			t.Errorf("session error log contains client metadata %q: %q", banned, out)
		}
	}
}

// buildBinary compiles the real relay binary once per test run, so the tests
// below exercise the exact artifact the Dockerfile and entrypoint run.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "trueseal-relay")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// serveEnv points every path and listener of a `-no-config` relay at
// throwaway locations, so the binary can start inside a test.
func serveEnv(t *testing.T, keyPath string) []string {
	t.Helper()
	dir := filepath.Dir(keyPath)
	return []string{
		"TRUESEAL_RELAY_KEYPAIR_PATH=" + keyPath,
		"TRUESEAL_RELAY_STORE_SQLITE_PATH=" + filepath.Join(dir, "inbox.db"),
		"TRUESEAL_RELAY_LISTEN_RECEIVE=127.0.0.1:0",
		"TRUESEAL_RELAY_LISTEN_PUSH=127.0.0.1:0",
		"TRUESEAL_RELAY_LISTEN_HEALTH=127.0.0.1:0",
	}
}

// commandEnv is serveEnv for the pubkey/address subcommands: valid fixed
// ports (the subcommands never bind them, but address advertises them).
func commandEnv(t *testing.T, keyPath string) []string {
	t.Helper()
	dir := filepath.Dir(keyPath)
	return []string{
		"TRUESEAL_RELAY_KEYPAIR_PATH=" + keyPath,
		"TRUESEAL_RELAY_STORE_SQLITE_PATH=" + filepath.Join(dir, "inbox.db"),
		"TRUESEAL_RELAY_LISTEN_RECEIVE=127.0.0.1:7700",
		"TRUESEAL_RELAY_LISTEN_PUSH=127.0.0.1:7701",
		"TRUESEAL_RELAY_LISTEN_HEALTH=127.0.0.1:7702",
	}
}

// RED (RELAY-12, completion criterion 1+2): the bare binary's documented
// first start must generate a keypair at mode 0600 and log the public key.
// Today it dies at keypair.Load, and only the Docker entrypoint generated
// keys — which is exactly the setup path that was broken.
func TestFirstStart_GeneratesKeypair(t *testing.T) {
	bin := buildBinary(t)
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")

	cmd := exec.Command(bin, "-no-config")
	cmd.Env = append(os.Environ(), serveEnv(t, keyPath)...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if info, err := os.Stat(keyPath); err == nil {
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("generated keypair mode = %o, want 600", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first start did not generate a keypair within 10s (defect: bare binary fails at keypair.Load)")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := keypair.Load(keyPath); err != nil {
		t.Fatalf("generated keypair does not load: %v", err)
	}
}

// RED (RELAY-12): `trueseal-relay pubkey` prints the hex public key derived
// from the configured keypair — the same key the Noise handshake uses,
// because both come from the same resolution path.
func TestPubkey_PrintsHandshakeKey(t *testing.T) {
	bin := buildBinary(t)
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	if err := keypair.Generate(keyPath); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want, err := keypair.Load(keyPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cmd := exec.Command(bin, "pubkey")
	cmd.Env = append(os.Environ(), serveEnv(t, keyPath)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pubkey exited %v: %s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if got != hex.EncodeToString(want.Public) {
		t.Fatalf("pubkey printed %q, want %x", got, want.Public)
	}
}

// adr0028Address is the strict grammar of sync ADR-0028:
// trueseal://<relay public key hex>@host[:receivePort][?push=pushPort]
var adr0028Address = regexp.MustCompile(`^trueseal://([0-9a-f]{64})@([^:?\s]+)(?::([0-9]+))?(?:\?push=([0-9]+))?$`)

// RED (RELAY-12): `trueseal-relay address --host <host>` prints a Relay
// Address matching the ADR-0028 grammar, carrying the configured ports.
func TestAddress_MatchesADR0028Grammar(t *testing.T) {
	bin := buildBinary(t)
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	if err := keypair.Generate(keyPath); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want, err := keypair.Load(keyPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	run := func(extraEnv []string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), append(commandEnv(t, keyPath), extraEnv...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("address %v exited %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Default ports: explicit in the output (a port is never dropped silently).
	got := run(nil, "address", "--host", "relay.example.com")
	m := adr0028Address.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("address output %q does not match the ADR-0028 grammar", got)
	}
	if m[1] != hex.EncodeToString(want.Public) {
		t.Fatalf("address key %q, want %x", m[1], want.Public)
	}
	if m[2] != "relay.example.com" {
		t.Fatalf("address host %q", m[2])
	}
	if m[3] != "7700" || m[4] != "7701" {
		t.Fatalf("default ports = %s/?push=%s, want 7700/?push=7701", m[3], m[4])
	}

	// Non-default ports round-trip through the grammar.
	got = run([]string{
		"TRUESEAL_RELAY_LISTEN_RECEIVE=0.0.0.0:8800",
		"TRUESEAL_RELAY_LISTEN_PUSH=0.0.0.0:8801",
	}, "address", "--host", "relay.example.com")
	m = adr0028Address.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("non-default address output %q does not match the ADR-0028 grammar", got)
	}
	if m[3] != "8800" || m[4] != "8801" {
		t.Fatalf("non-default ports = %s/?push=%s, want 8800/?push=8801", m[3], m[4])
	}
}

// RELAY-12 (review P2.3): a --host containing ':', '@' or '?' would corrupt
// the ADR-0028 grammar; it is rejected before anything is printed.
func TestAddress_RejectsGrammarBreakingHost(t *testing.T) {
	bin := buildBinary(t)
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	if err := keypair.Generate(keyPath); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, host := range []string{"evil.com:1@real.com", "a?push=1", "a:b"} {
		cmd := exec.Command(bin, "address", "--host", host)
		cmd.Env = append(os.Environ(), commandEnv(t, keyPath)...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("address --host %q exited 0 — a corrupting host was accepted", host)
			continue
		}
		if strings.Contains(string(out), "trueseal://") {
			t.Errorf("address --host %q printed an address before failing: %s", host, out)
		}
	}
}

// RELAY-12 (review P2.4): subcommands reject stray positional arguments
// instead of silently ignoring them.
func TestSubcommands_RejectStrayPositionals(t *testing.T) {
	bin := buildBinary(t)
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	if err := keypair.Generate(keyPath); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, args := range [][]string{{"pubkey", "bogus"}, {"genkey", "bogus"}, {"address", "--host", "h", "bogus"}} {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), commandEnv(t, keyPath)...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("%v exited 0 — stray positional was silently ignored", args)
			continue
		}
		if !strings.Contains(string(out), "unexpected argument") {
			t.Errorf("%v failed without guidance: %s", args, out)
		}
	}
}

// RED (RELAY-12, the Docker defect shape): the doubled-binary-path argv the
// old entrypoint produced ("/trueseal-relay" as a positional arg) must fail
// loudly with guidance, never silently start a relay.
func TestDoubledEntrypointPath_FailsLoudlyWithGuidance(t *testing.T) {
	bin := buildBinary(t)
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	if err := keypair.Generate(keyPath); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cmd := exec.Command(bin, "/trueseal-relay", "pubkey")
	cmd.Env = append(os.Environ(), serveEnv(t, keyPath)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("doubled-path invocation exited 0 — positional args were silently ignored")
	}
	if !strings.Contains(string(out), "unexpected argument") {
		t.Fatalf("doubled-path invocation failed without guidance: %s", out)
	}
}

// RELAY-12: the entrypoint passes subcommands straight through to the binary
// (the class of bug where `exec /trueseal-relay "$@"` doubled the path).
// RELAY_BINARY is the entrypoint's documented test seam.
func TestEntrypoint_PassesSubcommandThrough(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	bin := buildBinary(t)
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	if err := keypair.Generate(keyPath); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want, err := keypair.Load(keyPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	script := filepath.Join("..", "..", "docker", "entrypoint.sh")
	cmd := exec.Command("sh", script, "pubkey")
	cmd.Env = append(os.Environ(), append(commandEnv(t, keyPath),
		"RELAY_BINARY="+bin,
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entrypoint pubkey exited %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != hex.EncodeToString(want.Public) {
		t.Fatalf("entrypoint pubkey printed %q, want %x", got, want.Public)
	}
}

// RELAY-12: an operator-supplied TRUESEAL_RELAY_KEYPAIR_HEX wins over the
// file, for serve and pubkey alike.
func TestPubkey_PrefersEnvKeypairHex(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	filePath := filepath.Join(dir, "keypair.hex")
	if err := keypair.Generate(filePath); err != nil {
		t.Fatalf("Generate file key: %v", err)
	}
	envKey, err := keypair.GenerateHex()
	if err != nil {
		t.Fatalf("GenerateHex: %v", err)
	}

	cmd := exec.Command(bin, "pubkey")
	cmd.Env = append(os.Environ(), append(commandEnv(t, filePath),
		"TRUESEAL_RELAY_KEYPAIR_HEX="+envKey,
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pubkey exited %v: %s", err, out)
	}
	envKP, err := keypair.ParseHex(envKey)
	if err != nil {
		t.Fatalf("ParseHex: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != hex.EncodeToString(envKP.Public) {
		t.Fatalf("pubkey printed %q, want env key %x", got, envKP.Public)
	}
}
