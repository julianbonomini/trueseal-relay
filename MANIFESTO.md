# hush-relay Manifesto

## Purpose

A relay is a forced trust boundary. hush-relay exists to eliminate that forcing.

## Principles

**Blind**
The relay retains no information beyond what active routing requires. Recipient public key is unavoidable — it is the address. Everything else: sender identity, group membership, blob content — is structurally withheld, not policy-withheld. There is nothing to betray.

**Durable until delivered**
An accepted blob is never lost before delivery. TTL reaping is not data loss — it is policy. A crash is. Persistent inbox storage and crash recovery are non-negotiable.

**Operational simplicity**
A single binary. A docker compose. Zero ceremony to run. Self-hostable by anyone with a VPS and five minutes. Complexity scales with operator choice, not with the relay itself.

**No relay is irreplaceable**
Any instance can fail, be swapped, or be replaced. The security model does not change. No specific relay is load-bearing. Shared inbox state enables clustering — any standard external store suffices.

## Boundary

- **Not a durable log** — delivered blobs are deleted immediately. History is the caller's responsibility.
- **Not an identity system** — no accounts, users, or group membership. The relay sees public keys, nothing more.
- **Not a smart router** — no content inspection, filtering, or permission enforcement. Blind by design.
- **Not a push notification server** — push requires device identity (APNs token, FCM token). The relay has no concept of identity, so it cannot reach offline devices. Clients must connect to receive.

## Consequences

- **Anonymous push sessions** — sender identity uses a fresh ephemeral keypair per push. Follows from blind.
- **Persistent inbox storage** — crash recovery is required, not optional. Follows from durable until delivered.
- **Single binary distribution** — docker compose ships everything needed. Follows from operational simplicity.
- **Externalizable inbox state** — shared storage required for clustering; no node-local state. Follows from no relay is irreplaceable.
