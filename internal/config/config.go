package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
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

// ADR-0012 defaults for the RELAY-6 abuse limits and liveness deadlines.
// Every value is operator-configurable (short values for e2e); the
// envelope size limit and the TTL may only be lowered (RELAY-8 owns the
// startup refusals for values above the ceilings).
const (
	// DefaultHandshakeDeadline bounds the version-prefix read + Noise
	// handshake on both listeners.
	DefaultHandshakeDeadline = 10 * time.Second
	// DefaultPushSessionDeadline bounds a whole Push Session, accept to close.
	DefaultPushSessionDeadline = 30 * time.Second
	// DefaultReceiveIdleDeadline closes a Receive Session after this much
	// silence; ANY inbound frame resets it (the client heartbeats every
	// 25 s, so 75 s tolerates two lost beats).
	DefaultReceiveIdleDeadline = 75 * time.Second
	// DefaultWriteDeadline bounds one outbound write — a stalled client
	// cannot pin the writer goroutine (the RELAY-2 review follow-up).
	DefaultWriteDeadline = 30 * time.Second
	// DefaultMaxBlobsPerInbox caps undelivered blobs per Inbox.
	DefaultMaxBlobsPerInbox int64 = 10_000
	// DefaultMaxBytesPerInbox caps undelivered envelope bytes per Inbox (256 MiB).
	DefaultMaxBytesPerInbox int64 = 256 * 1024 * 1024
	// DefaultPushRatePerSecond is the sustained per-recipient push rate.
	DefaultPushRatePerSecond = 20.0
	// DefaultPushRateBurst is the per-recipient token-bucket capacity.
	DefaultPushRateBurst = 500
	// DefaultMaxReceiveSessionsPerDevice caps concurrent Receive Sessions
	// per Device key (reconnect overlap needs headroom above 1).
	DefaultMaxReceiveSessionsPerDevice = 4
)

// Config is the operator-facing configuration for trueseal-relay.
// Source: TOML file, with optional env var overrides (TRUESEAL_RELAY_ prefix).
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
	// refused and closed silently (ADR-0012: no refusal is logged in normal
	// mode). 0 means no limit.
	MaxConnections int `toml:"max_connections"`

	// RELAY-6 (ADR-0012): liveness deadlines and abuse limits. Every limit
	// is keyed on the recipient Inbox, the Device key or a global count —
	// never on a peer address.
	HandshakeDeadline           time.Duration `toml:"handshake_deadline"`
	PushSessionDeadline         time.Duration `toml:"push_session_deadline"`
	ReceiveIdleDeadline         time.Duration `toml:"receive_idle_deadline"`
	WriteDeadline               time.Duration `toml:"write_deadline"`
	MaxBlobsPerInbox            int64         `toml:"max_blobs_per_inbox"`
	MaxBytesPerInbox            int64         `toml:"max_bytes_per_inbox"`
	PushRatePerSecond           float64       `toml:"push_rate_per_second"`
	PushRateBurst               int           `toml:"push_rate_burst"`
	MaxReceiveSessionsPerDevice int           `toml:"max_receive_sessions_per_device"`
}

// StoreConfig holds storage adapter settings.
type StoreConfig struct {
	Type        string `toml:"type"`
	SQLitePath  string `toml:"sqlite_path"`
	PostgresDSN string `toml:"postgres_dsn"`
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
	if cfg.Relay.HandshakeDeadline <= 0 {
		cfg.Relay.HandshakeDeadline = DefaultHandshakeDeadline
	}
	if cfg.Relay.PushSessionDeadline <= 0 {
		cfg.Relay.PushSessionDeadline = DefaultPushSessionDeadline
	}
	if cfg.Relay.ReceiveIdleDeadline <= 0 {
		cfg.Relay.ReceiveIdleDeadline = DefaultReceiveIdleDeadline
	}
	if cfg.Relay.WriteDeadline <= 0 {
		cfg.Relay.WriteDeadline = DefaultWriteDeadline
	}
	if cfg.Relay.MaxBlobsPerInbox <= 0 {
		cfg.Relay.MaxBlobsPerInbox = DefaultMaxBlobsPerInbox
	}
	if cfg.Relay.MaxBytesPerInbox <= 0 {
		cfg.Relay.MaxBytesPerInbox = DefaultMaxBytesPerInbox
	}
	if cfg.Relay.PushRatePerSecond <= 0 {
		cfg.Relay.PushRatePerSecond = DefaultPushRatePerSecond
	}
	if cfg.Relay.PushRateBurst <= 0 {
		cfg.Relay.PushRateBurst = DefaultPushRateBurst
	}
	if cfg.Relay.MaxReceiveSessionsPerDevice <= 0 {
		cfg.Relay.MaxReceiveSessionsPerDevice = DefaultMaxReceiveSessionsPerDevice
	}
	if cfg.Store.Type == "" {
		cfg.Store.Type = "sqlite"
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("TRUESEAL_RELAY_KEYPAIR_PATH"); v != "" {
		cfg.Relay.KeypairPath = v
	}
	if v := os.Getenv("TRUESEAL_RELAY_LISTEN_PUSH"); v != "" {
		cfg.Relay.ListenPush = v
	}
	if v := os.Getenv("TRUESEAL_RELAY_LISTEN_RECEIVE"); v != "" {
		cfg.Relay.ListenReceive = v
	}
	if v := os.Getenv("TRUESEAL_RELAY_LISTEN_HEALTH"); v != "" {
		cfg.Relay.ListenHealth = v
	}
	if v := os.Getenv("TRUESEAL_RELAY_STORE_TYPE"); v != "" {
		cfg.Store.Type = v
	}
	if v := os.Getenv("TRUESEAL_RELAY_STORE_SQLITE_PATH"); v != "" {
		cfg.Store.SQLitePath = v
	}
	if v := os.Getenv("TRUESEAL_RELAY_STORE_POSTGRES_DSN"); v != "" {
		cfg.Store.PostgresDSN = v
	}
	if v := os.Getenv("TRUESEAL_RELAY_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Relay.TTL = d
		} else {
			log.Printf("config: ignoring invalid TRUESEAL_RELAY_TTL %q: %v", v, err)
		}
	}
	if v := os.Getenv("TRUESEAL_RELAY_REAP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Relay.ReapInterval = d
		} else {
			log.Printf("config: ignoring invalid TRUESEAL_RELAY_REAP_INTERVAL %q: %v", v, err)
		}
	}
	if v := os.Getenv("TRUESEAL_RELAY_MAX_CONNECTIONS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Relay.MaxConnections = n
		} else {
			log.Printf("config: ignoring invalid TRUESEAL_RELAY_MAX_CONNECTIONS %q", v)
		}
	}
	if v := os.Getenv("TRUESEAL_RELAY_MAX_ENVELOPE_BYTES"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Relay.MaxEnvelopeBytes = n
		} else {
			log.Printf("config: ignoring invalid TRUESEAL_RELAY_MAX_ENVELOPE_BYTES %q", v)
		}
	}
	// RELAY-6 (ADR-0012) deadlines and abuse limits — short values for e2e.
	parseDurationEnv("TRUESEAL_RELAY_HANDSHAKE_DEADLINE", &cfg.Relay.HandshakeDeadline)
	parseDurationEnv("TRUESEAL_RELAY_PUSH_SESSION_DEADLINE", &cfg.Relay.PushSessionDeadline)
	parseDurationEnv("TRUESEAL_RELAY_RECEIVE_IDLE_DEADLINE", &cfg.Relay.ReceiveIdleDeadline)
	parseDurationEnv("TRUESEAL_RELAY_WRITE_DEADLINE", &cfg.Relay.WriteDeadline)
	parseInt64Env("TRUESEAL_RELAY_MAX_BLOBS_PER_INBOX", &cfg.Relay.MaxBlobsPerInbox)
	parseInt64Env("TRUESEAL_RELAY_MAX_BYTES_PER_INBOX", &cfg.Relay.MaxBytesPerInbox)
	parseFloatEnv("TRUESEAL_RELAY_PUSH_RATE_PER_SECOND", &cfg.Relay.PushRatePerSecond)
	parseIntEnv("TRUESEAL_RELAY_PUSH_RATE_BURST", &cfg.Relay.PushRateBurst)
	parseIntEnv("TRUESEAL_RELAY_MAX_RECEIVE_SESSIONS_PER_DEVICE", &cfg.Relay.MaxReceiveSessionsPerDevice)
}

// parseDurationEnv applies a Go duration env override (e.g. "250ms").
func parseDurationEnv(name string, dst *time.Duration) {
	v := os.Getenv(name)
	if v == "" {
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("config: ignoring invalid %s %q: %v", name, v, err)
		return
	}
	*dst = d
}

func parseInt64Env(name string, dst *int64) {
	v := os.Getenv(name)
	if v == "" {
		return
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		log.Printf("config: ignoring invalid %s %q", name, v)
		return
	}
	*dst = n
}

func parseIntEnv(name string, dst *int) {
	v := os.Getenv(name)
	if v == "" {
		return
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		log.Printf("config: ignoring invalid %s %q", name, v)
		return
	}
	*dst = n
}

func parseFloatEnv(name string, dst *float64) {
	v := os.Getenv(name)
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		log.Printf("config: ignoring invalid %s %q", name, v)
		return
	}
	*dst = f
}

func validate(cfg *Config) error {
	if cfg.Relay.KeypairPath == "" {
		return fmt.Errorf("config: relay.keypair_path is required")
	}
	if cfg.Store.Type == "sqlite" && cfg.Store.SQLitePath == "" {
		return fmt.Errorf("config: store.sqlite_path is required when store.type = sqlite")
	}
	if cfg.Store.Type == "postgres" && cfg.Store.PostgresDSN == "" {
		return fmt.Errorf("config: store.postgres_dsn is required when store.type = postgres (or set TRUESEAL_RELAY_STORE_POSTGRES_DSN)")
	}
	if cfg.Store.Type != "sqlite" && cfg.Store.Type != "postgres" {
		return fmt.Errorf("config: store.type must be sqlite or postgres, got %q", cfg.Store.Type)
	}
	return nil
}
