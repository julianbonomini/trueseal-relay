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
// The recipient's trueseal-sync client deserializes, decrypts, and verifies it.
type Envelope struct {
	// Recipient is extracted from the proto Envelope's recipient_pub field.
	// Used for routing only — to know which Inbox to store the blob in.
	Recipient RecipientKey

	// Raw is the complete serialized proto Envelope bytes as received from
	// the wire. The relay never modifies this. What the sender sent is exactly
	// what the recipient receives.
	Raw []byte
}

// DeliveryBlob is a blob ready to be delivered to a Device over a Receive Session.
// BlobID is the opaque store-assigned identifier sent in the Deliver frame prefix.
// The Device echoes it back in a DeliverAck frame; the relay then calls
// DeleteForRecipient scoped to that Device's Inbox.
// See ADR-0009.
type DeliveryBlob struct {
	BlobID   int64
	Envelope []byte
}

// DefaultTTL is the default maximum time an undelivered Envelope remains in
// an Inbox before being reaped. Operators should set this high — reaping is
// a last resort for abandoned devices, not routine housekeeping.
// Overridden by operator config. See ADR-0003.
const DefaultTTL = 30 * 24 * time.Hour

// ProtocolMaxEnvelopeBytes is the largest Envelope one Noise transport frame
// can carry (sync ADR-0025: u16 length prefix 65,535, minus the 16-byte AEAD
// tag, the 5-byte frame header and the 32-byte recipient key prefix). An
// operator may only LOWER the relay's size limit below this ceiling — the
// relay refuses to start with a higher one (RELAY-8). The client enforces
// its own 61,440-byte Sync body limit below this value, so the ceiling is
// never reachable by honest traffic.
const ProtocolMaxEnvelopeBytes int64 = 65_482
