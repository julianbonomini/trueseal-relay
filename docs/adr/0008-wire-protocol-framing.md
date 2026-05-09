# Wire protocol: raw framing with Ack and Heartbeat control messages

## Framing

All messages over a Noise session use the framing already defined in hush-sync/src/relay.rs:

```
[type: u8][len: u32 BE][body: bytes]
```

Heartbeat and Ack have zero-byte bodies — the type tag is the entire message minus the length prefix. `frame(MsgType::Heartbeat, &[])` is five bytes. No protobuf is used for control messages — proto buys nothing when there are no fields to name.

## Message types

Owned by hush-sync as the protocol authority (ADR-0005). hush-relay implements against them.

| Tag | Name | Direction | Body |
|---|---|---|---|
| `0x01` | Push | client → relay | `[recipient_pub: 32 bytes][protobuf Envelope bytes]` |
| `0x02` | Deliver | relay → client | `[blob_id: 8 bytes u64 BE][protobuf Envelope bytes]` |
| `0x03` | Heartbeat | bidirectional | empty |
| `0x04` | Ack | relay → client | empty |
| `0x05` | Error | relay → client | empty |
| `0x06` | DeliverAck | client → relay | `[blob_id: 8 bytes u64 BE]` |

## Push body layout (wire format contract)

**Confirmed with hush-sync (protocol authority). This is the single most critical integration point between hush-relay and hush-sync — any divergence silently misroutes every blob.**

The Push frame body has this exact byte layout:

```
[recipient_pub: 32 bytes][envelope: variable]
```

- `recipient_pub` — the recipient's X25519 static public key, raw bytes (no encoding). The relay reads `body[0:32]` as the inbox routing key. **Never proto-decoded** — the relay routes by address, not content.
- `envelope` — a protobuf-encoded `Envelope` message (`hush.sync.v0.Envelope`). Stored verbatim, forwarded verbatim. The relay never deserialises this field.

The Deliver frame body contains an 8-byte opaque blob_id prefix followed by the envelope bytes: `[blob_id: 8 bytes u64 BE][envelope bytes]`. The relay stores `body[32:]` of the Push frame and delivers it as the envelope portion of the Deliver body. hush-sync recipients strip the first 8 bytes, then call `Envelope::decode(remaining)` on the Deliver body.

**Why raw prefix, not proto-parsed `recipient_pub`?**
Parsing proto on the relay would introduce a proto decode dependency on every push. The relay is a zero-knowledge router — it must route, not interpret. A raw fixed-width prefix is faster, simpler, and avoids any dependency on the Envelope proto schema. The relay is explicitly not a consumer of the hush-sync Envelope format.

**Enforcement:**
The relay validates `len(body) >= 32` — if not, the blob is rejected and no Ack is sent. The relay also enforces a configurable maximum envelope size (`relay.max_envelope_bytes`) on `body[32:]` — see below.

## DeliverAck semantics

`DeliverAck (0x06)` is sent by the Device after it has durably received and persisted a `Deliver` frame. The body carries the opaque `blob_id` echoed from the corresponding Deliver frame — the relay uses it to delete the blob from the InboxStore.

The `blob_id` is an opaque u64 assigned by the relay. The client must not interpret it — only echo it back. The relay currently uses the InboxStore's internal row ID, but the wire contract makes no guarantee about its meaning.

Sequence for Receive Sessions:
1. Device opens Noise XX Receive Session
2. Relay Peeks inbox — fetches blobs without deleting
3. For each blob: relay sends `Deliver [blob_id][envelope]`
4. Device persists envelope, sends `DeliverAck [blob_id]`
5. Relay receives DeliverAck → deletes blob from InboxStore by blob_id
6. If session closes before DeliverAck arrives, blob remains in store and is re-delivered on next Receive Session

Deduplication is the client's responsibility — a blob may be delivered more than once across sessions.

## Ack semantics

Ack means **"I persisted this blob to the InboxStore."** It does not mean "I received your bytes" — that is already guaranteed by Noise transport.

The distinction matters for crash safety. A relay that crashes between receiving a Push and writing to the InboxStore has lost the blob. Without an explicit Ack, the client (hush-sync outbox) cannot distinguish "relay persisted" from "bytes were written to TCP." With Ack, the outbox only marks a blob as relay-confirmed after receiving the Ack for its sequence number.

Sequence for Push Sessions:
1. Client opens Noise NK Push Session
2. Client sends one or more `Push` frames
3. For each Push: relay writes to InboxStore, then sends `Ack` (empty body)
4. Client reads Acks — session stays open until all Acks received
5. Client closes session

**Why the Ack body is empty (rejected: 8-byte u64 BE sequence):** An earlier design carried the confirmed envelope's sequence number in the Ack body so the sender could match Acks to Pushes. This was rejected for three reasons: (1) the relay never uses sequence numbers — ordering is the recipient's responsibility; (2) dedup by sequence is impossible on NK sessions because the relay never learns the sender's identity; (3) the Noise NK channel already authenticates the relay, so a well-formed Ack is proof enough that the relay persisted the blob. The sequence number added no safety, only complexity.

A Push Session that closes before all Acks are received is treated as unconfirmed. hush-sync replays unconfirmed blobs on the next Push Session.

## Error semantics

`Error (0x05)` is sent by the relay in response to a `Push` frame that was permanently rejected. It signals that the relay will never accept or store this blob — the client must **not** retry it.

When `AcceptPush` receives a `Push` frame and the handler returns an error (e.g. the envelope exceeds the size limit), the relay:
1. Sends an `Error` frame (encrypted, same Noise channel as Ack)
2. Does **not** send an `Ack`
3. Does **not** store the blob
4. Continues reading the session — the rejection is per-blob, not per-session

The body is empty. No diagnostic detail is included — the relay is blind by design and has no obligation to explain rejection to an anonymous sender.

**Permanent rejection, non-retryable.** An `Error` response means the blob will never be accepted on any retry. hush-sync (protocol authority) must discard the blob from its outbox on receipt of an `Error` frame. Retrying would waste bandwidth and never succeed.

## Two-layer enforcement model

Envelope size limits are enforced at two layers:

**Layer 1 — hush-sync (protocol authority, primary):** hush-sync enforces `MAX_ENVELOPE_BYTES` (1 MiB) before constructing a Push frame. Compliant clients never produce a Push frame with an oversized envelope. This is the primary enforcement point.

**Layer 2 — hush-relay (backstop):** The relay enforces `max_envelope_bytes` (default 1 MiB, configurable via `relay.max_envelope_bytes` or `HUSH_RELAY_MAX_ENVELOPE_BYTES`) on `body[32:]` of every received Push frame. If the limit is exceeded, the relay sends an `Error` frame and discards the blob. This layer exists as a backstop against non-compliant or malicious clients — it does not imply the relay inspects content. The relay rejects by size, never by content.

The 1 MiB default is sourced from `MAX_ENVELOPE_BYTES` in hush-sync (protocol authority), ensuring both layers agree on the limit by default.

## Heartbeat

Sent by the relay on idle Receive Sessions to prevent NAT/firewall timeout (see ADR-0006). Client echoes back a Heartbeat. Either side may initiate. Interval is operator-configured.

## Envelope size limit

The relay enforces a maximum size on the envelope bytes (`body[32:]` of the Push frame body). If the envelope exceeds the limit, the relay returns an error and does **not** send an Ack — the client outbox retains the blob.

Default: **1 048 576 bytes (1 MiB)**. Configurable via `relay.max_envelope_bytes` in `relay.toml` or `HUSH_RELAY_MAX_ENVELOPE_BYTES` env var.

Rationale: a compromised or malicious peer who knows a recipient public key can push arbitrarily large blobs, exhausting InboxStore disk. The limit is applied before persistence. The default of 1 MiB comfortably covers all legitimate hush-sync payloads (clipboard text, secrets, GroupManifest messages) while bounding worst-case per-blob storage impact.
