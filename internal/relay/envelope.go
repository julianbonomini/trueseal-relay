package relay

import "time"

// RecipientKey is the 32-byte X25519 public key that identifies a Device's Inbox.
// It is the only routing identity the relay knows — no usernames, no accounts,
// no group membership. See CONTEXT.md and the blind principle in MANIFESTO.md.
type RecipientKey [32]byte

// Envelope is the relay's internal representation of a blob in transit.
// The relay extracts only what it needs for routing (Recipient) and stores
// the rest as opaque bytes (Raw) — never inspecting, modifying, or decrypting.
//
// Raw is the entire serialized proto Envelope received from the Push Session.
// It is stored verbatim and forwarded verbatim to the recipient's Receive Session.
// The recipient's hush-sync client deserializes, decrypts, and verifies it.
type Envelope struct {
	// Recipient is extracted from the proto Envelope's recipient_pub field.
	// Used for routing only — to know which Inbox to store the blob in.
	Recipient RecipientKey

	// Raw is the complete serialized proto Envelope bytes as received from
	// the wire. The relay never modifies this. What the sender sent is exactly
	// what the recipient receives.
	Raw []byte
}

// DefaultTTL is the default maximum time an undelivered Envelope remains in
// an Inbox before being reaped. Operators should set this high — reaping is
// a last resort for abandoned devices, not routine housekeeping.
// Overridden by operator config. See ADR-0003.
const DefaultTTL = 30 * 24 * time.Hour
