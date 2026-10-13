# Wire protocol: raw framing with Ack and Heartbeat control messages

Amended 2026-10-13 by RELAY-5: added the Transport Version prefix exchange
(sync ADR-0022) and typed Error reason codes (the ADR-0026 relay contract).

## Transport Version prefix (added by RELAY-5)

Every connection — Receive (XX) and Push (NK) — begins with a plaintext
5-byte Transport Version prefix BEFORE the Noise handshake (sync ADR-0022
transport-half; the exact bytes are pinned by the sync test vectors,
checked in Go under gate G5):

```
client → relay : "TSP" || version:u16 BE        (5 bytes, plaintext)
relay → client : 0x01                           (supported — proceed)
               | "unsupported <min>..<max>\n"    (refused — then close)
```

- On `0x01`, the exact 5 prefix bytes become the Noise prologue on both
  sides, binding the Transport Version into the handshake hash — a prefix
  tampered in transit breaks the handshake.
- The relay speaks exactly one version: its min equals its max
  (`unsupported 1..1\n` today). There is no negotiation and no
  compatibility decoder — a pre-version client (no `"TSP"` magic) is
  refused identically. Version 0 is reserved and always refused.
- The refusal line is written before the connection closes so the client
  can always read it; trueseal-sync surfaces it as the connection state
  `relayVersionUnsupported{min,max}`.

## Framing

All messages over a Noise session use the framing already defined in trueseal-sync/src/relay.rs:

```
[type: u8][len: u32 BE][body: bytes]
```

Heartbeat and Ack have zero-byte bodies — the type tag is the entire message minus the length prefix. `frame(MsgType::Heartbeat, &[])` is five bytes. No protobuf is used for control messages — proto buys nothing when there are no fields to name.

## Message types

Owned by trueseal-sync as the protocol authority (ADR-0005). trueseal-relay implements against them.

| Tag | Name | Direction | Body |
|---|---|---|---|
| `0x01` | Push | client → relay | `[recipient_pub: 32 bytes][protobuf Envelope bytes]` |
| `0x02` | Deliver | relay → client | `[blob_id: 8 bytes u64 BE][protobuf Envelope bytes]` |
| `0x03` | Heartbeat | bidirectional | empty |
| `0x04` | Ack | relay → client | empty |
| `0x05` | Error | relay → client | `[reason: u8]` (see Error semantics) |
| `0x06` | DeliverAck | client → relay | `[blob_id: 8 bytes u64 BE]` |

## Push body layout (wire format contract)

**Confirmed with trueseal-sync (protocol authority). This is the single most critical integration point between trueseal-relay and trueseal-sync — any divergence silently misroutes every blob.**

The Push frame body has this exact byte layout:

```
[recipient_pub: 32 bytes][envelope: variable]
```

- `recipient_pub` — the recipient's X25519 static public key, raw bytes (no encoding). The relay reads `body[0:32]` as the inbox routing key. The relay routes by address, not content.
- `envelope` — a protobuf-encoded `Envelope` message (`trueseal.sync.v0.Envelope`). Stored verbatim, forwarded verbatim. The relay never deserialises this field — with exactly one exception, amended below.

The Deliver frame body contains an 8-byte opaque blob_id prefix followed by the envelope bytes: `[blob_id: 8 bytes u64 BE][envelope bytes]`. The relay stores `body[32:]` of the Push frame and delivers it as the envelope portion of the Deliver body. trueseal-sync recipients strip the first 8 bytes, then call `Envelope::decode(remaining)` on the Deliver body.

**Why raw prefix, not proto-parsed `recipient_pub`?**
The relay is a zero-knowledge router — it must route, not interpret. It is explicitly not a consumer of the trueseal-sync Envelope format, and it carries no protobuf dependency.

*Amended (RELAY-3, trueseal-relay#40; threat-model decision trueseal-roadmap#16):* the routing address is the raw prefix **verified against the one Envelope field the sealed-Envelope contract leaves Relay-readable** — `recipient_pub` (proto field 3, per sync ADR-0031; pinned by the sync test vectors). A minimal protobuf wire reader (not a generated decoder — no proto dependency) walks the Envelope's fields, skipping everything that isn't field 3, and refuses the push with the permanent `malformed` reason code (0x02) when:

- the routing prefix and the Envelope's `recipient_pub` disagree (a pusher trying to file a Blob in an Inbox it isn't addressed to);
- `recipient_pub` is missing, not exactly 32 bytes, or duplicated;
- the Envelope's wire framing can't be walked at all.

Nothing is stored for a refused push, no Ack is sent, and nothing about the refusal is logged in normal mode (ADR-0012). The relay reads no other field: the payload bytes are never touched, parsed, or inspected. "Routes by address, not content" still holds — the address is now the prefix *plus* the one field the sealed Envelope exposes, and they must agree.

**Enforcement:**
The relay validates `len(body) >= 32` — if not, the blob is rejected and no Ack is sent. It enforces a configurable maximum envelope size (`relay.max_envelope_bytes`) on `body[32:]` (checked before the recipient verification above, so an oversized push is refused on size alone) — see below.

## DeliverAck semantics

`DeliverAck (0x06)` is sent by the Device immediately after receiving a `Deliver` frame. The body carries the opaque `blob_id` echoed from the corresponding Deliver frame — the relay uses it to delete the blob from the InboxStore. The Ack confirms transport receipt by the client process, not decryption or an application database commit.

The `blob_id` is an opaque u64 assigned by the relay. The client must not interpret it — only echo it back. The relay currently uses the InboxStore's internal row ID, but the wire contract makes no guarantee about its meaning.

Sequence for Receive Sessions:
1. Device opens Noise XX Receive Session
2. Relay Peeks inbox — fetches blobs without deleting
3. For each blob: relay sends `Deliver [blob_id][envelope]`
4. Device receives frame bytes, sends `DeliverAck [blob_id]`
5. Relay receives DeliverAck → deletes blob from InboxStore by blob_id
6. If session closes before DeliverAck arrives, blob remains in store and is re-delivered on next Receive Session

Deduplication is the client's responsibility — a blob may be delivered more than once across sessions. trueseal-sync derives a stable application Message ID from signed Envelope metadata; the relay's `blob_id` remains internal and must not be used for application deduplication.

## Ack semantics

Ack means **"I persisted this blob to the InboxStore."** It does not mean "I received your bytes" — that is already guaranteed by Noise transport.

The distinction matters for crash safety. A relay that crashes between receiving a Push and writing to the InboxStore has lost the blob. Without an explicit Ack, the client (trueseal-sync outbox) cannot distinguish "relay persisted" from "bytes were written to TCP." With Ack, the outbox only marks a blob as relay-confirmed after receiving the Ack for its sequence number.

Sequence for Push Sessions:
1. Client opens Noise NK Push Session
2. Client sends one or more `Push` frames
3. For each Push: relay writes to InboxStore, then sends `Ack` (empty body)
4. Client reads Acks — session stays open until all Acks received
5. Client closes session

**Why the Ack body is empty (rejected: 8-byte u64 BE sequence):** An earlier design carried the confirmed envelope's sequence number in the Ack body so the sender could match Acks to Pushes. This was rejected for three reasons: (1) the relay never uses sequence numbers — ordering is the recipient's responsibility; (2) dedup by sequence is impossible on NK sessions because the relay never learns the sender's identity; (3) the Noise NK channel already authenticates the relay, so a well-formed Ack is proof enough that the relay persisted the blob. The sequence number added no safety, only complexity.

A Push Session that closes before all Acks are received is treated as unconfirmed. trueseal-sync replays unconfirmed blobs on the next Push Session.

## Error semantics

`Error (0x05)` is sent by the relay when it refuses something. Amended by RELAY-5 (the ADR-0026 relay contract): the body is a **one-byte reason code** so the client can tell permanent refusals (never retry) from temporary ones (retry later). Bit 7 set = temporary.

| Code | Name | Class | Meaning |
|---|---|---|---|
| `0x01` | `tooLarge` | permanent | Envelope over the relay's size limit (ADR-0025: a size refusal is permanent and typed) |
| `0x02` | `malformed` | permanent | Unparseable framing, an unknown frame type, or an undeliverable body |
| `0x81` | `inboxFull` | temporary | Recipient Inbox quota exhausted (RELAY-6) |
| `0x82` | `rateLimited` | temporary | Per-recipient rate limit fired (RELAY-6) |
| `0x83` | `unavailable` | temporary | Inbox store unavailable |
| `0x84` | `internal` | temporary | Unclassified internal failure |

When `AcceptPush` receives a `Push` frame and the handler returns an error, the relay:
1. Sends an `Error` frame carrying the classified reason code (encrypted, same Noise channel as Ack)
2. Does **not** send an `Ack`
3. Does **not** store the blob
4. Continues reading the session — the rejection is per-blob, not per-session

**Unknown frame types** (and malformed framing, and known types that do not belong on the session kind) get a generic `Error` with the `malformed` code that **echoes no frame bytes** (sync ADR-0022). A zero-length Error body remains well-formed for compatibility with the original empty-body layout — the sync vectors pin an empty-body Error as a valid frame — but the relay always sends a code.

The code is the only diagnostic — no echoed bytes, no strings. The relay is blind by design; the reason code is the contract, not an explanation.

**Permanent rejection, non-retryable.** A permanent `Error` means the blob will never be accepted on any retry; trueseal-sync discards it from the outbox (a size refusal is the ADR-0025 case). A temporary `Error` (bit 7 set) means the client may retry later — trueseal-sync's outbox treats it as a temporary relay failure class (ADR-0026). The current client maps any Error body to its permanent PushRejected path; parsing the codes is a client-side refinement that changes no wire bytes.

## Two-layer enforcement model

Envelope size limits are enforced at two layers:

**Layer 1 — trueseal-sync (protocol authority, primary):** trueseal-sync enforces `MAX_ENVELOPE_BYTES` (1 MiB) before constructing a Push frame. Compliant clients never produce a Push frame with an oversized envelope. This is the primary enforcement point.

**Layer 2 — trueseal-relay (backstop):** The relay enforces `max_envelope_bytes` (default 1 MiB, configurable via `relay.max_envelope_bytes` or `TRUESEAL_RELAY_MAX_ENVELOPE_BYTES`) on `body[32:]` of every received Push frame. If the limit is exceeded, the relay sends an `Error` frame and discards the blob. This layer exists as a backstop against non-compliant or malicious clients — it does not imply the relay inspects content. The relay rejects by size, never by content.

The 1 MiB default is sourced from `MAX_ENVELOPE_BYTES` in trueseal-sync (protocol authority), ensuring both layers agree on the limit by default.

## Heartbeat

Sent by the relay on idle Receive Sessions to prevent NAT/firewall timeout (see ADR-0006). Client echoes back a Heartbeat. Either side may initiate. Interval is operator-configured.

## Envelope size limit

The relay enforces a maximum size on the envelope bytes (`body[32:]` of the Push frame body). If the envelope exceeds the limit, the relay returns an error and does **not** send an Ack — the client outbox retains the blob.

Default: **1 048 576 bytes (1 MiB)**. Configurable via `relay.max_envelope_bytes` in `relay.toml` or `TRUESEAL_RELAY_MAX_ENVELOPE_BYTES` env var.

Rationale: a compromised or malicious peer who knows a recipient public key can push arbitrarily large blobs, exhausting InboxStore disk. The limit is applied before persistence. The default of 1 MiB comfortably covers all legitimate trueseal-sync payloads (clipboard text, secrets, GroupManifest messages) while bounding worst-case per-blob storage impact.
