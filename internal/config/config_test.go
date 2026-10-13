package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/config"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "relay.toml")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

const validTOML = `
[relay]
keypair_path   = "/etc/trueseal-relay/keypair.hex"
listen_push    = ":7701"
listen_receive = ":7700"
ttl            = "720h"
reap_interval  = "1h"

[store]
type        = "sqlite"
sqlite_path = "/var/lib/trueseal-relay/inbox.db"
`

// If config file does not exist, falls back to defaults + env vars only.
func TestConfig_MissingFileUsesEnvVars(t *testing.T) {
	t.Setenv("TRUESEAL_RELAY_KEYPAIR_PATH", "/data/keypair.hex")
	t.Setenv("TRUESEAL_RELAY_STORE_SQLITE_PATH", "/data/inbox.db")

	cfg, err := config.Load("/nonexistent/relay.toml")
	if err != nil {
		t.Fatalf("want nil when file missing + env vars set, got: %v", err)
	}
	if cfg.Relay.KeypairPath != "/data/keypair.hex" {
		t.Errorf("KeypairPath: got %q", cfg.Relay.KeypairPath)
	}
}

// Valid TOML parses without error and fields are populated.
func TestConfig_ParsesValidTOML(t *testing.T) {
	p := writeFile(t, validTOML)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Relay.KeypairPath != "/etc/trueseal-relay/keypair.hex" {
		t.Errorf("KeypairPath: got %q", cfg.Relay.KeypairPath)
	}
	if cfg.Relay.ListenPush != ":7701" {
		t.Errorf("ListenPush: got %q", cfg.Relay.ListenPush)
	}
	if cfg.Relay.ListenReceive != ":7700" {
		t.Errorf("ListenReceive: got %q", cfg.Relay.ListenReceive)
	}
	if cfg.Store.Type != "sqlite" {
		t.Errorf("Store.Type: got %q", cfg.Store.Type)
	}
	if cfg.Store.SQLitePath != "/var/lib/trueseal-relay/inbox.db" {
		t.Errorf("SQLitePath: got %q", cfg.Store.SQLitePath)
	}
}

// Missing keypair_path fails fast.
func TestConfig_MissingKeypairPath_Fails(t *testing.T) {
	p := writeFile(t, `
[relay]
listen_push    = ":7701"
listen_receive = ":7700"

[store]
type        = "sqlite"
sqlite_path = "/tmp/inbox.db"
`)
	_, err := config.Load(p)
	if err == nil {
		t.Error("want error for missing keypair_path, got nil")
	}
}

// Missing sqlite_path when store type is sqlite fails fast.
func TestConfig_MissingSQLitePath_Fails(t *testing.T) {
	p := writeFile(t, `
[relay]
keypair_path   = "/etc/kp.hex"
listen_push    = ":7701"
listen_receive = ":7700"

[store]
type = "sqlite"
`)
	_, err := config.Load(p)
	if err == nil {
		t.Error("want error for missing sqlite_path, got nil")
	}
}

// TTL and reap_interval have sensible defaults when omitted.
func TestConfig_Defaults(t *testing.T) {
	p := writeFile(t, `
[relay]
keypair_path   = "/etc/kp.hex"
listen_push    = ":7701"
listen_receive = ":7700"

[store]
type        = "sqlite"
sqlite_path = "/tmp/inbox.db"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Relay.TTL <= 0 {
		t.Errorf("want positive default TTL, got %v", cfg.Relay.TTL)
	}
	if cfg.Relay.ReapInterval <= 0 {
		t.Errorf("want positive default ReapInterval, got %v", cfg.Relay.ReapInterval)
	}
}

// Env vars TRUESEAL_RELAY_TTL and TRUESEAL_RELAY_REAP_INTERVAL override duration fields.
func TestConfig_TTLAndReapIntervalEnvOverrides(t *testing.T) {
	t.Setenv("TRUESEAL_RELAY_KEYPAIR_PATH", "/data/keypair.hex")
	t.Setenv("TRUESEAL_RELAY_STORE_SQLITE_PATH", "/data/inbox.db")
	t.Setenv("TRUESEAL_RELAY_TTL", "48h")
	t.Setenv("TRUESEAL_RELAY_REAP_INTERVAL", "30m")

	cfg, err := config.Load("/nonexistent/relay.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Relay.TTL != 48*time.Hour {
		t.Errorf("TTL: want 48h, got %v", cfg.Relay.TTL)
	}
	if cfg.Relay.ReapInterval != 30*time.Minute {
		t.Errorf("ReapInterval: want 30m, got %v", cfg.Relay.ReapInterval)
	}
}

// Invalid duration strings for NON-ceiling knobs are ignored with a log,
// falling through to the defaults. (TRUESEAL_RELAY_TTL is a ceiling key —
// RELAY-8 makes an invalid value a startup error instead; see ceiling_test.go.)
func TestConfig_InvalidDurationEnvIgnored(t *testing.T) {
	t.Setenv("TRUESEAL_RELAY_KEYPAIR_PATH", "/data/keypair.hex")
	t.Setenv("TRUESEAL_RELAY_STORE_SQLITE_PATH", "/data/inbox.db")
	t.Setenv("TRUESEAL_RELAY_REAP_INTERVAL", "also-bad")

	cfg, err := config.Load("/nonexistent/relay.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Should fall through to defaults
	if cfg.Relay.TTL != config.DefaultTTL {
		t.Errorf("TTL: want default %v, got %v", config.DefaultTTL, cfg.Relay.TTL)
	}
	if cfg.Relay.ReapInterval != config.DefaultReapInterval {
		t.Errorf("ReapInterval: want default %v, got %v", config.DefaultReapInterval, cfg.Relay.ReapInterval)
	}
}

func TestConfig_EnvOverride(t *testing.T) {
	p := writeFile(t, validTOML)
	t.Setenv("TRUESEAL_RELAY_STORE_SQLITE_PATH", "/tmp/override.db")

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.SQLitePath != "/tmp/override.db" {
		t.Errorf("want /tmp/override.db, got %q", cfg.Store.SQLitePath)
	}
}

// Postgres type requires postgres_dsn.
func TestConfig_MissingPostgresDSN_Fails(t *testing.T) {
	p := writeFile(t, `
[relay]
keypair_path = "/etc/keypair.hex"

[store]
type = "postgres"
`)
	_, err := config.Load(p)
	if err == nil {
		t.Error("want error for missing postgres_dsn, got nil")
	}
}

// Postgres DSN accepted via env var.
func TestConfig_PostgresDSN_FromEnv(t *testing.T) {
	p := writeFile(t, `
[relay]
keypair_path = "/etc/keypair.hex"

[store]
type = "postgres"
`)
	t.Setenv("TRUESEAL_RELAY_STORE_POSTGRES_DSN", "postgres://relay:pass@db:5432/trueseal")
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.PostgresDSN != "postgres://relay:pass@db:5432/trueseal" {
		t.Errorf("want DSN from env, got %q", cfg.Store.PostgresDSN)
	}
}

// RELAY-6 (ADR-0012): the abuse-limit and deadline defaults match the ADR
// exactly — the preview ships with the limits on.
func TestConfig_ADR0012Defaults(t *testing.T) {
	t.Setenv("TRUESEAL_RELAY_KEYPAIR_PATH", "/data/keypair.hex")
	t.Setenv("TRUESEAL_RELAY_STORE_SQLITE_PATH", "/data/inbox.db")

	cfg, err := config.Load("/nonexistent/relay.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := map[string]struct{ got, want any }{
		"HandshakeDeadline":           {cfg.Relay.HandshakeDeadline, config.DefaultHandshakeDeadline},
		"PushSessionDeadline":         {cfg.Relay.PushSessionDeadline, config.DefaultPushSessionDeadline},
		"ReceiveIdleDeadline":         {cfg.Relay.ReceiveIdleDeadline, config.DefaultReceiveIdleDeadline},
		"WriteDeadline":               {cfg.Relay.WriteDeadline, config.DefaultWriteDeadline},
		"MaxBlobsPerInbox":            {cfg.Relay.MaxBlobsPerInbox, int64(10_000)},
		"MaxBytesPerInbox":            {cfg.Relay.MaxBytesPerInbox, int64(256 * 1024 * 1024)},
		"PushRatePerSecond":           {cfg.Relay.PushRatePerSecond, 20.0},
		"PushRateBurst":               {cfg.Relay.PushRateBurst, 500},
		"MaxReceiveSessionsPerDevice": {cfg.Relay.MaxReceiveSessionsPerDevice, 4},
	}
	for name, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: want ADR-0012 default %v, got %v", name, c.want, c.got)
		}
	}
	// The ADR's numbers, pinned so a drift in the constants fails loudly.
	if config.DefaultHandshakeDeadline != 10*time.Second ||
		config.DefaultPushSessionDeadline != 30*time.Second ||
		config.DefaultReceiveIdleDeadline != 75*time.Second {
		t.Error("deadline defaults drifted from ADR-0012 (10s handshake / 30s push / 75s idle)")
	}
}

// RELAY-6: every limit and deadline is settable through env vars, with
// short values — the e2e requirement.
func TestConfig_LimitsEnvOverrides(t *testing.T) {
	t.Setenv("TRUESEAL_RELAY_KEYPAIR_PATH", "/data/keypair.hex")
	t.Setenv("TRUESEAL_RELAY_STORE_SQLITE_PATH", "/data/inbox.db")
	t.Setenv("TRUESEAL_RELAY_HANDSHAKE_DEADLINE", "250ms")
	t.Setenv("TRUESEAL_RELAY_PUSH_SESSION_DEADLINE", "1s")
	t.Setenv("TRUESEAL_RELAY_RECEIVE_IDLE_DEADLINE", "500ms")
	t.Setenv("TRUESEAL_RELAY_WRITE_DEADLINE", "100ms")
	t.Setenv("TRUESEAL_RELAY_MAX_BLOBS_PER_INBOX", "3")
	t.Setenv("TRUESEAL_RELAY_MAX_BYTES_PER_INBOX", "1024")
	t.Setenv("TRUESEAL_RELAY_PUSH_RATE_PER_SECOND", "0.5")
	t.Setenv("TRUESEAL_RELAY_PUSH_RATE_BURST", "2")
	t.Setenv("TRUESEAL_RELAY_MAX_RECEIVE_SESSIONS_PER_DEVICE", "1")

	cfg, err := config.Load("/nonexistent/relay.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Relay.HandshakeDeadline != 250*time.Millisecond {
		t.Errorf("HandshakeDeadline: got %v", cfg.Relay.HandshakeDeadline)
	}
	if cfg.Relay.PushSessionDeadline != time.Second {
		t.Errorf("PushSessionDeadline: got %v", cfg.Relay.PushSessionDeadline)
	}
	if cfg.Relay.ReceiveIdleDeadline != 500*time.Millisecond {
		t.Errorf("ReceiveIdleDeadline: got %v", cfg.Relay.ReceiveIdleDeadline)
	}
	if cfg.Relay.WriteDeadline != 100*time.Millisecond {
		t.Errorf("WriteDeadline: got %v", cfg.Relay.WriteDeadline)
	}
	if cfg.Relay.MaxBlobsPerInbox != 3 {
		t.Errorf("MaxBlobsPerInbox: got %v", cfg.Relay.MaxBlobsPerInbox)
	}
	if cfg.Relay.MaxBytesPerInbox != 1024 {
		t.Errorf("MaxBytesPerInbox: got %v", cfg.Relay.MaxBytesPerInbox)
	}
	if cfg.Relay.PushRatePerSecond != 0.5 {
		t.Errorf("PushRatePerSecond: got %v", cfg.Relay.PushRatePerSecond)
	}
	if cfg.Relay.PushRateBurst != 2 {
		t.Errorf("PushRateBurst: got %v", cfg.Relay.PushRateBurst)
	}
	if cfg.Relay.MaxReceiveSessionsPerDevice != 1 {
		t.Errorf("MaxReceiveSessionsPerDevice: got %v", cfg.Relay.MaxReceiveSessionsPerDevice)
	}
}

// RELAY-6: invalid limit env values are ignored with a log, falling through
// to the ADR defaults — a typo must not silently disable a limit.
func TestConfig_InvalidLimitsEnvIgnored(t *testing.T) {
	t.Setenv("TRUESEAL_RELAY_KEYPAIR_PATH", "/data/keypair.hex")
	t.Setenv("TRUESEAL_RELAY_STORE_SQLITE_PATH", "/data/inbox.db")
	t.Setenv("TRUESEAL_RELAY_MAX_BLOBS_PER_INBOX", "-5")
	t.Setenv("TRUESEAL_RELAY_PUSH_RATE_PER_SECOND", "fast")
	t.Setenv("TRUESEAL_RELAY_RECEIVE_IDLE_DEADLINE", "soon")

	cfg, err := config.Load("/nonexistent/relay.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Relay.MaxBlobsPerInbox != config.DefaultMaxBlobsPerInbox {
		t.Errorf("MaxBlobsPerInbox: want default on invalid env, got %v", cfg.Relay.MaxBlobsPerInbox)
	}
	if cfg.Relay.PushRatePerSecond != config.DefaultPushRatePerSecond {
		t.Errorf("PushRatePerSecond: want default on invalid env, got %v", cfg.Relay.PushRatePerSecond)
	}
	if cfg.Relay.ReceiveIdleDeadline != config.DefaultReceiveIdleDeadline {
		t.Errorf("ReceiveIdleDeadline: want default on invalid env, got %v", cfg.Relay.ReceiveIdleDeadline)
	}
}
