package config

import (
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

// ProtocolMaxEnvelopeBytes is the largest Envelope one Noise transport frame
// can carry (sync ADR-0025: u16 length prefix 65,535 − 16-byte AEAD tag −
// 5-byte frame header − 32-byte recipient key prefix). RELAY-8: an operator
// may only LOWER the relay limit below this ceiling — a configured value
// above it refuses startup. Aliased from the relay package so the ceiling
// has exactly one home.
const ProtocolMaxEnvelopeBytes = relay.ProtocolMaxEnvelopeBytes

// DefaultMaxEnvelopeBytes is the default maximum envelope size — the
// protocol ceiling itself.
const DefaultMaxEnvelopeBytes = ProtocolMaxEnvelopeBytes

// MaxRelayTTL caps the inbox TTL at 30 days (sync ADR-0034 §2: the 60-day
// Replay Window minus the 30-day outbox expiry). RELAY-8: an operator may
// only LOWER it — a configured value above it refuses startup.
const MaxRelayTTL = 30 * 24 * time.Hour

// DefaultTTL is the default envelope TTL — the cap itself (30 days).
const DefaultTTL = MaxRelayTTL

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

	// Dev selects dev logging mode (RELAY-9, ADR-0012 "Logging"). Per the
	// ADR it is set by the -dev flag or TRUESEAL_RELAY_DEV only — never
	// TOML — so verbose logging is always an explicit, per-invocation
	// choice an operator cannot forget in a config file. Dev mode may log
	// key prefixes, sizes and Blob IDs; it never logs payload bytes or
	// peer addresses, in any mode.
	Dev bool `toml:"-"`
}

// StoreConfig holds storage adapter settings.
type StoreConfig struct {
	Type        string `toml:"type"`
	SQLitePath  string `toml:"sqlite_path"`
	PostgresDSN string `toml:"postgres_dsn"`
}

// relayOverlay mirrors RelayConfig's numeric knobs as pointers, so Load can
// tell an explicitly-set value apart from an absent one (RELAY-8 + the
// RELAY-6 review note): an explicit out-of-range value refuses startup,
// while an absent one takes the default. The old zero-value collapse could
// not tell "unset" from "explicit 0" and silently defaulted both.
// The operator-facing TOML format is unchanged.
type relayOverlay struct {
	TTL                         *time.Duration `toml:"ttl"`
	ReapInterval                *time.Duration `toml:"reap_interval"`
	MaxEnvelopeBytes            *int64         `toml:"max_envelope_bytes"`
	MaxConnections              *int           `toml:"max_connections"`
	HandshakeDeadline           *time.Duration `toml:"handshake_deadline"`
	PushSessionDeadline         *time.Duration `toml:"push_session_deadline"`
	ReceiveIdleDeadline         *time.Duration `toml:"receive_idle_deadline"`
	WriteDeadline               *time.Duration `toml:"write_deadline"`
	MaxBlobsPerInbox            *int64         `toml:"max_blobs_per_inbox"`
	MaxBytesPerInbox            *int64         `toml:"max_bytes_per_inbox"`
	PushRatePerSecond           *float64       `toml:"push_rate_per_second"`
	PushRateBurst               *int           `toml:"push_rate_burst"`
	MaxReceiveSessionsPerDevice *int           `toml:"max_receive_sessions_per_device"`
}

type overlay struct {
	Relay relayOverlay `toml:"relay"`
}

// Load reads the TOML config at path, applies env var overrides, and
// validates required fields. If path is empty or does not exist, falls back to
// defaults + env vars only — useful for Docker deployments.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	ov := &overlay{}

	if path != "" {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("config: parse %s: %w", path, err)
			}
			// file missing — continue with defaults + env vars
		} else if _, err := toml.DecodeFile(path, ov); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	if err := applyEnvOverrides(cfg, ov); err != nil {
		return nil, err
	}
	setDefaults(cfg)
	if err := resolve(cfg, ov); err != nil {
		return nil, err
	}

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolve applies the operator's explicit numeric values, refusing startup
// on an explicit out-of-range value, and fills every knob the operator left
// unset with its default.
func resolve(cfg *Config, ov *overlay) error {
	r := &cfg.Relay
	o := &ov.Relay
	var err error

	if r.TTL, err = pickPositiveDuration("relay.ttl", o.TTL, DefaultTTL); err != nil {
		return err
	}
	if r.ReapInterval, err = pickPositiveDuration("relay.reap_interval", o.ReapInterval, DefaultReapInterval); err != nil {
		return err
	}
	if r.MaxEnvelopeBytes, err = pickPositiveInt64("relay.max_envelope_bytes", o.MaxEnvelopeBytes, DefaultMaxEnvelopeBytes); err != nil {
		return err
	}
	// max_connections keeps its documented "0 means no limit" meaning — but
	// only when the operator says 0 explicitly.
	if r.MaxConnections, err = pickNonNegativeInt("relay.max_connections", o.MaxConnections, 1000); err != nil {
		return err
	}
	if r.HandshakeDeadline, err = pickPositiveDuration("relay.handshake_deadline", o.HandshakeDeadline, DefaultHandshakeDeadline); err != nil {
		return err
	}
	if r.PushSessionDeadline, err = pickPositiveDuration("relay.push_session_deadline", o.PushSessionDeadline, DefaultPushSessionDeadline); err != nil {
		return err
	}
	if r.ReceiveIdleDeadline, err = pickPositiveDuration("relay.receive_idle_deadline", o.ReceiveIdleDeadline, DefaultReceiveIdleDeadline); err != nil {
		return err
	}
	if r.WriteDeadline, err = pickPositiveDuration("relay.write_deadline", o.WriteDeadline, DefaultWriteDeadline); err != nil {
		return err
	}
	if r.MaxBlobsPerInbox, err = pickPositiveInt64("relay.max_blobs_per_inbox", o.MaxBlobsPerInbox, DefaultMaxBlobsPerInbox); err != nil {
		return err
	}
	if r.MaxBytesPerInbox, err = pickPositiveInt64("relay.max_bytes_per_inbox", o.MaxBytesPerInbox, DefaultMaxBytesPerInbox); err != nil {
		return err
	}
	if r.PushRatePerSecond, err = pickPositiveFloat("relay.push_rate_per_second", o.PushRatePerSecond, DefaultPushRatePerSecond); err != nil {
		return err
	}
	if r.PushRateBurst, err = pickPositiveInt("relay.push_rate_burst", o.PushRateBurst, DefaultPushRateBurst); err != nil {
		return err
	}
	if r.MaxReceiveSessionsPerDevice, err = pickPositiveInt("relay.max_receive_sessions_per_device", o.MaxReceiveSessionsPerDevice, DefaultMaxReceiveSessionsPerDevice); err != nil {
		return err
	}
	return nil
}

// pickPositiveDuration resolves one knob: nil (unset) → the default; an
// explicit non-positive value → a startup error naming the knob, so an
// operator mistake can never masquerade as a default.
func pickPositiveDuration(name string, v *time.Duration, def time.Duration) (time.Duration, error) {
	if v == nil {
		return def, nil
	}
	if *v <= 0 {
		return 0, fmt.Errorf("config: %s must be positive (got %v); omit the setting to use the default %v", name, *v, def)
	}
	return *v, nil
}

func pickPositiveInt64(name string, v *int64, def int64) (int64, error) {
	if v == nil {
		return def, nil
	}
	if *v <= 0 {
		return 0, fmt.Errorf("config: %s must be positive (got %d); omit the setting to use the default %d", name, *v, def)
	}
	return *v, nil
}

func pickPositiveInt(name string, v *int, def int) (int, error) {
	if v == nil {
		return def, nil
	}
	if *v <= 0 {
		return 0, fmt.Errorf("config: %s must be positive (got %d); omit the setting to use the default %d", name, *v, def)
	}
	return *v, nil
}

// pickNonNegativeInt is for the one knob where 0 is meaningful
// (max_connections = no limit): only a negative value refuses.
func pickNonNegativeInt(name string, v *int, def int) (int, error) {
	if v == nil {
		return def, nil
	}
	if *v < 0 {
		return 0, fmt.Errorf("config: %s must be zero (no limit) or positive (got %d); omit the setting to use the default %d", name, *v, def)
	}
	return *v, nil
}

// pickPositiveFloat also refuses NaN and ±Inf — a non-finite rate must never
// reach the token bucket (the RELAY-6 review: NaN comparisons are false, so
// a bare `<= 0` check lets NaN through and silently disables the limiter).
func pickPositiveFloat(name string, v *float64, def float64) (float64, error) {
	if v == nil {
		return def, nil
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
		return 0, fmt.Errorf("config: %s must be a finite positive number (got %v); omit the setting to use the default %v", name, *v, def)
	}
	return *v, nil
}

// setDefaults fills the string knobs only. Numeric knobs are resolved
// explicitly in resolve() — a zero there can be an operator's mistake, not
// a missing value.
func setDefaults(cfg *Config) {
	if cfg.Relay.ListenPush == "" {
		cfg.Relay.ListenPush = ":7701"
	}
	if cfg.Relay.ListenReceive == "" {
		cfg.Relay.ListenReceive = ":7700"
	}
	if cfg.Relay.ListenHealth == "" {
		cfg.Relay.ListenHealth = ":7702"
	}
	if cfg.Store.Type == "" {
		cfg.Store.Type = "sqlite"
	}
}

func applyEnvOverrides(cfg *Config, ov *overlay) error {
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
	// RELAY-8: the two ceiling keys escalate — an invalid env value is a
	// startup error, never a silent default. (An over-ceiling but VALID
	// value is refused later, in validate, on the resolved config.)
	if v := os.Getenv("TRUESEAL_RELAY_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: invalid TRUESEAL_RELAY_TTL %q: %w", v, err)
		}
		ov.Relay.TTL = &d
	}
	if v := os.Getenv("TRUESEAL_RELAY_MAX_ENVELOPE_BYTES"); v != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return fmt.Errorf("config: invalid TRUESEAL_RELAY_MAX_ENVELOPE_BYTES %q: %w", v, err)
		}
		ov.Relay.MaxEnvelopeBytes = &n
	}
	// Non-ceiling numeric knobs: an unparseable env value is logged and
	// ignored — a typo must not silently disable a limit. A parseable but
	// non-positive value is kept as EXPLICIT and refused in resolve(): the
	// operator set it on purpose, and silently defaulting hides that.
	if d, ok := envDuration("TRUESEAL_RELAY_REAP_INTERVAL"); ok {
		ov.Relay.ReapInterval = &d
	}
	if n, ok := envInt("TRUESEAL_RELAY_MAX_CONNECTIONS"); ok {
		ov.Relay.MaxConnections = &n
	}
	// RELAY-6 (ADR-0012) deadlines and abuse limits — short values for e2e.
	if d, ok := envDuration("TRUESEAL_RELAY_HANDSHAKE_DEADLINE"); ok {
		ov.Relay.HandshakeDeadline = &d
	}
	if d, ok := envDuration("TRUESEAL_RELAY_PUSH_SESSION_DEADLINE"); ok {
		ov.Relay.PushSessionDeadline = &d
	}
	if d, ok := envDuration("TRUESEAL_RELAY_RECEIVE_IDLE_DEADLINE"); ok {
		ov.Relay.ReceiveIdleDeadline = &d
	}
	if d, ok := envDuration("TRUESEAL_RELAY_WRITE_DEADLINE"); ok {
		ov.Relay.WriteDeadline = &d
	}
	if n, ok := envInt64("TRUESEAL_RELAY_MAX_BLOBS_PER_INBOX"); ok {
		ov.Relay.MaxBlobsPerInbox = &n
	}
	if n, ok := envInt64("TRUESEAL_RELAY_MAX_BYTES_PER_INBOX"); ok {
		ov.Relay.MaxBytesPerInbox = &n
	}
	if f, ok := envFloat("TRUESEAL_RELAY_PUSH_RATE_PER_SECOND"); ok {
		ov.Relay.PushRatePerSecond = &f
	}
	if n, ok := envInt("TRUESEAL_RELAY_PUSH_RATE_BURST"); ok {
		ov.Relay.PushRateBurst = &n
	}
	if n, ok := envInt("TRUESEAL_RELAY_MAX_RECEIVE_SESSIONS_PER_DEVICE"); ok {
		ov.Relay.MaxReceiveSessionsPerDevice = &n
	}
	// RELAY-9 (ADR-0012): dev logging mode. Only a recognisable truthy
	// value enables it — anything else (unset, "0", "false", a typo) is
	// normal mode, so verbose logging can never be enabled by accident.
	if v := strings.TrimSpace(os.Getenv("TRUESEAL_RELAY_DEV")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil && b {
			cfg.Relay.Dev = true
		}
	}
	return nil
}

// envDuration parses a Go duration env override (e.g. "250ms"). ok is false
// when the var is unset or unparseable (logged + ignored).
func envDuration(name string) (time.Duration, bool) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("config: ignoring invalid %s %q: %v", name, v, err)
		return 0, false
	}
	return d, true
}

// envInt64 parses a whole-string integer env override. Parseable but
// non-positive values return ok — resolve() refuses them explicitly.
func envInt64(name string) (int64, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Printf("config: ignoring invalid %s %q", name, v)
		return 0, false
	}
	return n, true
}

func envInt(name string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("config: ignoring invalid %s %q", name, v)
		return 0, false
	}
	return n, true
}

// envFloat parses a finite float env override. NaN and ±Inf are treated as
// invalid here too (logged + ignored) — a non-finite rate must never reach
// the token bucket (the RELAY-6 review NaN hole).
func envFloat(name string) (float64, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		log.Printf("config: ignoring invalid %s %q", name, v)
		return 0, false
	}
	return f, true
}

func validate(cfg *Config) error {
	if cfg.Relay.KeypairPath == "" {
		return fmt.Errorf("config: relay.keypair_path is required")
	}
	// RELAY-8: the two protocol ceilings. Operators may only LOWER either
	// limit — an over-ceiling value refuses startup instead of being
	// silently unreachable (sync ADR-0025) or silently losing honest
	// messages at the Replay Window edge (sync ADR-0034 §2).
	if cfg.Relay.MaxEnvelopeBytes > ProtocolMaxEnvelopeBytes {
		return fmt.Errorf("config: relay.max_envelope_bytes %d exceeds the protocol ceiling %d — operators may only lower the limit (sync ADR-0025)",
			cfg.Relay.MaxEnvelopeBytes, ProtocolMaxEnvelopeBytes)
	}
	if cfg.Relay.TTL > MaxRelayTTL {
		return fmt.Errorf("config: relay.ttl %v exceeds the 30-day protocol cap — operators may only lower it (sync ADR-0034)",
			cfg.Relay.TTL)
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
