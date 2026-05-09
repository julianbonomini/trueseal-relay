package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/julianbonomini/hush-relay/internal/config"
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
keypair_path   = "/etc/hush-relay/keypair.hex"
listen_push    = ":7701"
listen_receive = ":7700"
ttl            = "720h"
reap_interval  = "1h"

[store]
type        = "sqlite"
sqlite_path = "/var/lib/hush-relay/inbox.db"
`

// Valid TOML parses without error and fields are populated.
func TestConfig_ParsesValidTOML(t *testing.T) {
	p := writeFile(t, validTOML)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Relay.KeypairPath != "/etc/hush-relay/keypair.hex" {
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
	if cfg.Store.SQLitePath != "/var/lib/hush-relay/inbox.db" {
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

// Env var overrides TOML value.
func TestConfig_EnvOverride(t *testing.T) {
	p := writeFile(t, validTOML)
	t.Setenv("HUSH_RELAY_STORE_SQLITE_PATH", "/tmp/override.db")

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.SQLitePath != "/tmp/override.db" {
		t.Errorf("want /tmp/override.db, got %q", cfg.Store.SQLitePath)
	}
}
