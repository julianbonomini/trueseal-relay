package config

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultMaxEnvelopeBytes is the default maximum envelope size.
// The Noise transport framing uses a u16 length prefix (max 65 535 bytes).
// After subtracting the 16-byte AEAD tag, 5-byte frame header, and 32-byte
// recipient key prefix, the effective max envelope is 65 482 bytes.
// Operator-configurable; values above 65 482 are silently unreachable.
const DefaultMaxEnvelopeBytes = 65_482

// DefaultTTL is the default envelope TTL (30 days).
const DefaultTTL = 30 * 24 * time.Hour

// DefaultReapInterval is the default reaper run interval.
const DefaultReapInterval = time.Hour

// Config is the operator-facing configuration for hush-relay.
// Source: TOML file, with optional env var overrides (HUSH_RELAY_ prefix).
type Config struct {
	Relay RelayConfig `toml:"relay"`
	Store StoreConfig `toml:"store"`
}

// RelayConfig holds relay-level settings.
type RelayConfig struct {
	KeypairPath      string        `toml:"keypair_path"`
	ListenPush       string        `toml:"listen_push"`
	ListenReceive    string        `toml:"listen_receive"`
	ListenHealth     string        `toml:"listen_health"`
	TTL              time.Duration `toml:"ttl"`
	ReapInterval     time.Duration `toml:"reap_interval"`
	// MaxEnvelopeBytes is the maximum size of the protobuf Envelope bytes
	// (body[32:] of the Push frame body). Blobs exceeding this limit are rejected
	// without Ack. See ADR-0008.
	MaxEnvelopeBytes int64 `toml:"max_envelope_bytes"`
	// MaxConnections is the maximum number of concurrent connections per listener
	// (push and receive counted separately). Connections beyond the cap are
	// rejected immediately with a log warning. 0 means no limit.
	MaxConnections int `toml:"max_connections"`
}

// StoreConfig holds storage adapter settings.
type StoreConfig struct {
	Type       string `toml:"type"`
	SQLitePath string `toml:"sqlite_path"`
}

// Load reads the TOML config at path, applies env var overrides, and
// validates required fields. If path is empty or does not exist, falls back to
// defaults + env vars only — useful for Docker deployments.
func Load(path string) (*Config, error) {
	cfg := &Config{}

	if path != "" {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("config: parse %s: %w", path, err)
			}
			// file missing — continue with defaults + env vars
		}
	}

	applyEnvOverrides(cfg)
	setDefaults(cfg)

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func setDefaults(cfg *Config) {
	if cfg.Relay.TTL <= 0 {
		cfg.Relay.TTL = DefaultTTL
	}
	if cfg.Relay.ReapInterval <= 0 {
		cfg.Relay.ReapInterval = DefaultReapInterval
	}
	if cfg.Relay.ListenPush == "" {
		cfg.Relay.ListenPush = ":7701"
	}
	if cfg.Relay.ListenReceive == "" {
		cfg.Relay.ListenReceive = ":7700"
	}
	if cfg.Relay.ListenHealth == "" {
		cfg.Relay.ListenHealth = ":7702"
	}
	if cfg.Relay.MaxConnections <= 0 {
		cfg.Relay.MaxConnections = 1000
	}
	if cfg.Relay.MaxEnvelopeBytes <= 0 {
		cfg.Relay.MaxEnvelopeBytes = DefaultMaxEnvelopeBytes
	}
	if cfg.Store.Type == "" {
		cfg.Store.Type = "sqlite"
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("HUSH_RELAY_KEYPAIR_PATH"); v != "" {
		cfg.Relay.KeypairPath = v
	}
	if v := os.Getenv("HUSH_RELAY_LISTEN_PUSH"); v != "" {
		cfg.Relay.ListenPush = v
	}
	if v := os.Getenv("HUSH_RELAY_LISTEN_RECEIVE"); v != "" {
		cfg.Relay.ListenReceive = v
	}
	if v := os.Getenv("HUSH_RELAY_LISTEN_HEALTH"); v != "" {
		cfg.Relay.ListenHealth = v
	}
	if v := os.Getenv("HUSH_RELAY_STORE_TYPE"); v != "" {
		cfg.Store.Type = v
	}
	if v := os.Getenv("HUSH_RELAY_STORE_SQLITE_PATH"); v != "" {
		cfg.Store.SQLitePath = v
	}
	if v := os.Getenv("HUSH_RELAY_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Relay.TTL = d
		} else {
			log.Printf("config: ignoring invalid HUSH_RELAY_TTL %q: %v", v, err)
		}
	}
	if v := os.Getenv("HUSH_RELAY_REAP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Relay.ReapInterval = d
		} else {
			log.Printf("config: ignoring invalid HUSH_RELAY_REAP_INTERVAL %q: %v", v, err)
		}
	}
	if v := os.Getenv("HUSH_RELAY_MAX_CONNECTIONS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Relay.MaxConnections = n
		} else {
			log.Printf("config: ignoring invalid HUSH_RELAY_MAX_CONNECTIONS %q", v)
		}
	}
	if v := os.Getenv("HUSH_RELAY_MAX_ENVELOPE_BYTES"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Relay.MaxEnvelopeBytes = n
		} else {
			log.Printf("config: ignoring invalid HUSH_RELAY_MAX_ENVELOPE_BYTES %q", v)
		}
	}
}

func validate(cfg *Config) error {
	if cfg.Relay.KeypairPath == "" {
		return fmt.Errorf("config: relay.keypair_path is required")
	}
	if cfg.Store.Type == "sqlite" && cfg.Store.SQLitePath == "" {
		return fmt.Errorf("config: store.sqlite_path is required when store.type = sqlite")
	}
	if cfg.Store.Type != "sqlite" && cfg.Store.Type != "postgres" {
		return fmt.Errorf("config: store.type must be sqlite or postgres, got %q", cfg.Store.Type)
	}
	return nil
}
