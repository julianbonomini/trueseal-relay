package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

	cmd, _ := startRelayWithEnv(t, keyPath,
		"TRUESEAL_RELAY_LISTEN_RECEIVE=127.0.0.1:"+recvPort,
		"TRUESEAL_RELAY_LISTEN_PUSH=127.0.0.1:"+pushPort,
		"TRUESEAL_RELAY_TTL=720h",
		"TRUESEAL_RELAY_MAX_ENVELOPE_BYTES=65482",
	)
	// startRelayWithEnv's cleanup SIGTERMs the process; if the relay had
	// refused to start, Wait would already have returned a non-zero exit —
	// assert it is still running.
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		t.Fatalf("relay exited at the ceilings: %v", cmd.ProcessState)
	}
}
