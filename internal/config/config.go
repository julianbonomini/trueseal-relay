package config

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

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
	KeypairPath   string        `toml:"keypair_path"`
	ListenPush    string        `toml:"listen_push"`
	ListenReceive string        `toml:"listen_receive"`
	TTL           time.Duration `toml:"ttl"`
	ReapInterval  time.Duration `toml:"reap_interval"`
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
