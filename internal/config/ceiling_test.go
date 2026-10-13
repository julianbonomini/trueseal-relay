package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/config"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

// RELAY-8 (issue #45): startup refusals at the two protocol ceilings.
// sync ADR-0025: the relay size limit may only be LOWERED below the
// 65,482-byte Envelope ceiling (one Noise u16 frame). sync ADR-0034 §2 +
// relay ADR-0012: the inbox TTL is capped at 30 days. An over-ceiling value
// must REFUSE STARTUP with a clear operator-facing message — never be
// silently clamped or ignored.

const ceilingBaseTOML = `
[relay]
keypair_path   = "/etc/kp.hex"
listen_push    = ":7701"
listen_receive = ":7700"

[store]
type        = "sqlite"
sqlite_path = "/tmp/inbox.db"
`

func withRelayLine(t *testing.T, line string) string {
	t.Helper()
	return writeFile(t, strings.Replace(ceilingBaseTOML, "[relay]\n", "[relay]\n"+line+"\n", 1))
}

func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TRUESEAL_RELAY_KEYPAIR_PATH", "/data/keypair.hex")
	t.Setenv("TRUESEAL_RELAY_STORE_SQLITE_PATH", "/data/inbox.db")
}

// ── TTL cap: exactly 30 days starts, 30 days + 1 s refuses (both TOML and env).

func TestConfig_TTLAtCeilingStarts(t *testing.T) {
	p := withRelayLine(t, `ttl = "720h"`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("TTL of exactly 30 days must start, got: %v", err)
	}
	if cfg.Relay.TTL != 30*24*time.Hour {
		t.Errorf("TTL: want 720h, got %v", cfg.Relay.TTL)
	}
}

func TestConfig_TTLOneSecondOverCeilingRefuses(t *testing.T) {
	p := withRelayLine(t, `ttl = "720h0m1s"`)
	_, err := config.Load(p)
	if err == nil {
		t.Fatal("TTL of 30 days + 1 s must refuse startup")
	}
	if !strings.Contains(err.Error(), "30-day") && !strings.Contains(err.Error(), "30 days") {
		t.Errorf("refusal must name the cap, got: %v", err)
	}
	if !strings.Contains(err.Error(), "only lower") {
		t.Errorf("refusal must tell the operator they may only lower it, got: %v", err)
	}
}

func TestConfig_TTLCeilingBoundaryEnv(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("TRUESEAL_RELAY_TTL", "720h")
	if _, err := config.Load("/nonexistent/relay.toml"); err != nil {
		t.Fatalf("env TTL of exactly 30 days must start, got: %v", err)
	}

	setBaseEnv(t)
	t.Setenv("TRUESEAL_RELAY_TTL", "720h0m1s")
	if _, err := config.Load("/nonexistent/relay.toml"); err == nil {
		t.Fatal("env TTL of 30 days + 1 s must refuse startup")
	}
}

func TestConfig_TTLJustUnderCeilingStarts(t *testing.T) {
	p := withRelayLine(t, `ttl = "719h"`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("TTL under the cap must start, got: %v", err)
	}
	if cfg.Relay.TTL != 719*time.Hour {
		t.Errorf("TTL: want 719h, got %v", cfg.Relay.TTL)
	}
}

// ── Size ceiling: exactly 65,482 starts, 65,483 refuses (both TOML and env).

func TestConfig_EnvelopeLimitAtCeilingStarts(t *testing.T) {
	p := withRelayLine(t, "max_envelope_bytes = 65482")
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("envelope limit at the ceiling must start, got: %v", err)
	}
	if cfg.Relay.MaxEnvelopeBytes != 65482 {
		t.Errorf("MaxEnvelopeBytes: want 65482, got %d", cfg.Relay.MaxEnvelopeBytes)
	}
}

func TestConfig_EnvelopeLimitOneOverCeilingRefuses(t *testing.T) {
	p := withRelayLine(t, "max_envelope_bytes = 65483")
	_, err := config.Load(p)
	if err == nil {
		t.Fatal("envelope limit of ceiling + 1 must refuse startup")
	}
	if !strings.Contains(err.Error(), "65482") {
		t.Errorf("refusal must name the ceiling, got: %v", err)
	}
	if !strings.Contains(err.Error(), "only lower") {
		t.Errorf("refusal must tell the operator they may only lower it, got: %v", err)
	}
}

func TestConfig_EnvelopeCeilingBoundaryEnv(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("TRUESEAL_RELAY_MAX_ENVELOPE_BYTES", "65482")
	if _, err := config.Load("/nonexistent/relay.toml"); err != nil {
		t.Fatalf("env limit at the ceiling must start, got: %v", err)
	}

	setBaseEnv(t)
	t.Setenv("TRUESEAL_RELAY_MAX_ENVELOPE_BYTES", "65483")
	if _, err := config.Load("/nonexistent/relay.toml"); err == nil {
		t.Fatal("env limit of ceiling + 1 must refuse startup")
	}
}

// ── Invalid env values for the two ceiling keys are startup errors, not
// silent defaults (issue #45 likely approach).

func TestConfig_InvalidCeilingEnvIsStartupError(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("TRUESEAL_RELAY_TTL", "not-a-duration")
	if _, err := config.Load("/nonexistent/relay.toml"); err == nil {
		t.Fatal("invalid TRUESEAL_RELAY_TTL must be a startup error, not a silent default")
	}

	setBaseEnv(t)
	t.Setenv("TRUESEAL_RELAY_MAX_ENVELOPE_BYTES", "lots")
	if _, err := config.Load("/nonexistent/relay.toml"); err == nil {
		t.Fatal("invalid TRUESEAL_RELAY_MAX_ENVELOPE_BYTES must be a startup error, not a silent default")
	}
}

// ── Explicit vs unset (the RELAY-6 review note): an explicit non-positive
// value REFUSES; an unset value takes the default. The old collapse of
// "unset" with "explicit 0" hid operator mistakes.

func TestConfig_ExplicitNonPositiveRefuses(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"ttl zero", `ttl = "0s"`},
		{"ttl negative", `ttl = "-1h"`},
		{"envelope limit zero", "max_envelope_bytes = 0"},
		{"envelope limit negative", "max_envelope_bytes = -5"},
		{"reap interval zero", `reap_interval = "0s"`},
		{"handshake deadline negative", `handshake_deadline = "-1s"`},
		{"blobs quota zero", "max_blobs_per_inbox = 0"},
		{"rate zero", "push_rate_per_second = 0"},
		{"rate negative", "push_rate_per_second = -1.5"},
		{"burst negative", "push_rate_burst = -1"},
		{"sessions zero", "max_receive_sessions_per_device = 0"},
		{"connections negative", "max_connections = -1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := withRelayLine(t, tc.line)
			_, err := config.Load(p)
			if err == nil {
				t.Fatalf("explicit %q must refuse startup", tc.line)
			}
			if !strings.Contains(err.Error(), "positive") && !strings.Contains(err.Error(), "omit") {
				t.Errorf("refusal should say the value must be positive / be omitted, got: %v", err)
			}
		})
	}
}

// max_connections keeps its documented "0 means no limit" meaning — but only
// when the operator says 0 explicitly; unset still defaults to 1000.
func TestConfig_MaxConnectionsExplicitZeroMeansNoLimit(t *testing.T) {
	p := withRelayLine(t, "max_connections = 0")
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("explicit max_connections = 0 (no limit, documented) must start, got: %v", err)
	}
	if cfg.Relay.MaxConnections != 0 {
		t.Errorf("MaxConnections: want explicit 0 preserved, got %d", cfg.Relay.MaxConnections)
	}
}

// ── Non-finite floats (the RELAY-6 review NaN/+Inf hole): a NaN or Inf rate
// must never reach the limiter — TOML-explicit refuses startup; an invalid
// env value is ignored (the non-ceiling policy) and CANNOT be accepted.

func TestConfig_NonFiniteRateRefused(t *testing.T) {
	for _, literal := range []string{"nan", "inf", "+inf", "-inf"} {
		p := withRelayLine(t, "push_rate_per_second = "+literal)
		if _, err := config.Load(p); err == nil {
			t.Errorf("TOML push_rate_per_second = %s must refuse startup", literal)
		}
	}
}

func TestConfig_NonFiniteRateEnvIgnored(t *testing.T) {
	for _, v := range []string{"nan", "NaN", "inf", "+Inf"} {
		setBaseEnv(t)
		t.Setenv("TRUESEAL_RELAY_PUSH_RATE_PER_SECOND", v)
		cfg, err := config.Load("/nonexistent/relay.toml")
		if err != nil {
			t.Fatalf("invalid env rate should be ignored (non-ceiling key), got: %v", err)
		}
		if cfg.Relay.PushRatePerSecond != config.DefaultPushRatePerSecond {
			t.Errorf("env rate %q must not reach the config: want default %v, got %v",
				v, config.DefaultPushRatePerSecond, cfg.Relay.PushRatePerSecond)
		}
	}
}

// ── Defaults stay within both ceilings, and the constants cannot drift from
// the relay package's protocol values.

func TestConfig_DefaultsWithinCeilings(t *testing.T) {
	if config.DefaultTTL > config.MaxRelayTTL {
		t.Errorf("DefaultTTL %v exceeds the cap %v", config.DefaultTTL, config.MaxRelayTTL)
	}
	if config.DefaultMaxEnvelopeBytes > config.ProtocolMaxEnvelopeBytes {
		t.Errorf("DefaultMaxEnvelopeBytes %d exceeds the ceiling %d",
			config.DefaultMaxEnvelopeBytes, config.ProtocolMaxEnvelopeBytes)
	}
	// One constant, one home: config aliases the relay package's protocol
	// values so they cannot drift apart.
	if config.DefaultTTL != relay.DefaultTTL {
		t.Errorf("config.DefaultTTL %v != relay.DefaultTTL %v", config.DefaultTTL, relay.DefaultTTL)
	}
	if config.DefaultMaxEnvelopeBytes != relay.ProtocolMaxEnvelopeBytes {
		t.Errorf("config.DefaultMaxEnvelopeBytes %d != relay.ProtocolMaxEnvelopeBytes %d",
			config.DefaultMaxEnvelopeBytes, relay.ProtocolMaxEnvelopeBytes)
	}
	if config.MaxRelayTTL != 30*24*time.Hour {
		t.Errorf("MaxRelayTTL drifted from ADR-0034's 30 days: %v", config.MaxRelayTTL)
	}
}
