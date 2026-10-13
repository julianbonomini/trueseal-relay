package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RELAY-8 (issue #45) completion criterion 3, end-to-end: the REAL binary
// refuses to start over either ceiling — exit non-zero, with a clear
// operator-facing message that carries no client data.

func TestServe_RefusesToStartAboveTheCeilings(t *testing.T) {
	bin := buildBinary(t)
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")

	cases := []struct {
		name    string
		env     string
		wantSub string
	}{
		{"ttl over the 30-day cap", "TRUESEAL_RELAY_TTL=721h", "30-day"},
		{"size over the protocol ceiling", "TRUESEAL_RELAY_MAX_ENVELOPE_BYTES=65483", "65482"},
		{"invalid ttl env", "TRUESEAL_RELAY_TTL=banana", "TRUESEAL_RELAY_TTL"},
		{"invalid size env", "TRUESEAL_RELAY_MAX_ENVELOPE_BYTES=lots", "TRUESEAL_RELAY_MAX_ENVELOPE_BYTES"},
		{"explicit zero ttl env", "TRUESEAL_RELAY_TTL=0s", "positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, "-no-config")
			cmd.Env = append(os.Environ(), serveEnv(t, keyPath)...)
			cmd.Env = append(cmd.Env, tc.env)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("relay started with %s — it must refuse", tc.env)
			}
			if !strings.Contains(string(out), tc.wantSub) {
				t.Errorf("refusal message should contain %q, got: %s", tc.wantSub, out)
			}
			// ADR-0012: a refusal message is operator-facing — no client data.
			for _, banned := range []string{"addr=", "key=", "blob"} {
				if strings.Contains(string(out), banned) {
					t.Errorf("refusal message contains %q: %s", banned, out)
				}
			}
		})
	}
}

// The same binary starts with both knobs exactly AT the ceilings.
func TestServe_StartsAtTheCeilings(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "keypair.hex")
	recvPort, pushPort := freePort(t), freePort(t)

	_, logs := startRelayWithEnv(t, keyPath,
		"TRUESEAL_RELAY_LISTEN_RECEIVE=127.0.0.1:"+recvPort,
		"TRUESEAL_RELAY_LISTEN_PUSH=127.0.0.1:"+pushPort,
		"TRUESEAL_RELAY_TTL=720h",
		"TRUESEAL_RELAY_MAX_ENVELOPE_BYTES=65482",
	)
	// Assert ACTUAL liveness (RELAY-9 note: the old ProcessState check was
	// dead — it is always nil before Wait, so the test passed even if the
	// binary instantly refused). Proof of life: the startup line appears
	// and both listeners accept a connection.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "trueseal-relay running") {
		if time.Now().After(deadline) {
			t.Fatalf("relay never reached the serving state at the ceilings. Output:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, port := range []string{recvPort, pushPort} {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 2*time.Second)
		if err != nil {
			t.Fatalf("listener :%s not accepting at the ceilings: %v", port, err)
		}
		_ = conn.Close()
	}
}
