# trueseal-relay

A deployable Go binary providing encrypted blob routing for the trueseal ecosystem. The relay is the infrastructure layer of the trueseal stack — a zero-knowledge, always-in-path intermediary that stores and forwards ciphertext blobs between Devices. It never decrypts content and never learns group membership.

See [trueseal-sync](https://github.com/julianbonomini/trueseal-sync) for the client library, protocol definitions, and envelope format that trueseal-relay implements.

## Language

**Relay**:
This binary. A server that accepts Noise XX sessions from Devices, receives Envelopes addressed to recipient public keys, stores them ephemerally, and delivers them when the recipient connects. Zero-knowledge — it never decrypts content.
_Avoid_: server, hub, broker, proxy

**Receive Session**:
A live, authenticated, forward-secret connection between a Device and the relay, established via a Noise XX handshake over TCP. Used by a Device to receive Envelopes. The relay learns the connecting Device's static public key — nothing else. Multiple Receive Sessions may be open concurrently.
_Avoid_: connection, socket, channel, session (unqualified — always specify Receive or Push)

**Push Session**:
A short-lived, anonymous connection used by a Device to send Envelopes to the relay. Established via a Noise NK handshake — the Device authenticates the relay's static public key, but the relay never learns the sender's identity. A fresh ephemeral X25519 keypair is generated for each Push Session and discarded immediately after. The relay sees an unlinkable anonymous peer. Push Sessions are closed immediately after all Envelopes are sent.
_Avoid_: connection, socket, upload session

**Inbox**:
The set of undelivered Envelopes addressed to a given Device public key, held durably by the relay until Delivery or TTL expiry. "Ephemeral" refers to the lifetime of the data (exists only until delivered), not the storage mechanism — the relay must persist Inboxes durably across crashes. Flushed on Delivery.
_Avoid_: queue, mailbox, buffer, store

**Delivery**:
The act of the relay forwarding an Envelope from an Inbox to a Device's active Receive Session, confirmed by a DeliverAck from the Device. The Envelope is deleted from the Inbox only after the DeliverAck is received. If no Session is active, or if a Session closes before the Ack arrives, the Envelope remains in the Inbox and is re-delivered on the next Receive Session.
_Avoid_: push, send, forward, flush

**TTL (Time-to-Live)**:
The maximum time an undelivered Envelope remains in an Inbox. Envelopes exceeding the TTL are Reaped regardless of delivery status. Protects against unbounded storage growth from abandoned or lost Devices.
_Avoid_: expiry, timeout, retention period

**Reap**:
The act of deleting an Envelope whose TTL has elapsed without Delivery. Performed by a background process on the relay. Reaping is not data loss — it is an operator-configured policy for handling abandoned or permanently offline Devices.
_Avoid_: purge, eviction, expiry, cleanup

**Node**:
A single running instance of the trueseal-relay binary. Multiple Nodes may be deployed as a cluster, sharing a common Inbox store. Any Node can fail or be replaced without affecting the security model or losing accepted Envelopes. No Node holds state that is not shared.
_Avoid_: server, instance, replica

**Operator**:
The person or organisation deploying trueseal-relay. Responsible for: generating and safeguarding the relay Keypair, distributing the relay public key to Device operators, and configuring TTL and blob size limits. trueseal-relay makes no assumptions about who the operator is — a relay run by a trusted friend and a relay run by an adversary provide identical security guarantees to end users. Deployment is self-contained: a single binary or docker compose, no external dependencies required for a single-Node deployment.
_Avoid_: admin, owner, host

## Relationships

- The **Relay** accepts one **Receive Session** per connected **Device**
- A **Receive Session** is authenticated by the **Device**'s static public key via Noise XX
- A **Push Session** is anonymous — the relay never learns the sender's identity
- Multiple **Nodes** share a common **Inbox** store in a clustered deployment
- An **Inbox** belongs to exactly one **Device** (identified by public key)
- An **Envelope** sits in exactly one **Inbox** until **Delivery** or TTL expiry
- The **Relay** has no concept of Sync Groups — it sees only independent public keys and Inboxes

## Example dialogue

> **Operator:** "Does the relay know which of my users' devices belong together?"
> **Domain expert:** "No. The relay sees public keys and Inboxes. It has no concept of groups, users, or which devices are paired. Two keys that happen to exchange blobs look identical to two unrelated keys."

> **Operator:** "What happens to blobs if a device is offline for 60 days?"
> **Domain expert:** "They're reaped by TTL. The relay is a delivery buffer, not a durable log. Devices that need history store it locally after receiving it."

> **Operator:** "Can I see what's being synced through my relay?"
> **Domain expert:** "No. Every blob is encrypted with the recipient's public key before it reaches the relay. You operate the infrastructure; you cannot read the content."

> **Developer:** "Can trueseal-relay send push notifications to wake up a sleeping app?"
> **Domain expert:** "No. Push notifications require knowing a device's APNs or FCM token — that's device identity. The relay has no concept of identity beyond public keys. Offline delivery is handled by the relay's Inbox and trueseal-sync's outbox replay when the device reconnects."

> **Operator:** "Can I run multiple relay nodes behind a load balancer?"
> **Domain expert:** "Yes, as long as they share an Inbox store. Any Node can handle any request — there is no node-local state. A Node can be replaced or fail without losing any accepted Envelopes."

## Flagged ambiguities

- "push" — overloaded. In trueseal-sync, Push is a Device sending an Envelope to the relay (via a Push Session). In trueseal-relay, Delivery is the relay forwarding to a Device. Use Push for the client action, Delivery for the relay action.
- "server" — avoided in favour of Relay throughout the trueseal stack to be precise about the zero-knowledge property.
- "ephemeral" — overloaded. In Push Session, ephemeral refers to the keypair (generated per session, immediately discarded). In Inbox, ephemeral refers to the data lifetime (held only until delivered). Never use "ephemeral storage" — it implies in-memory, which contradicts the durable-until-delivered requirement.
- "session" (unqualified) — always specify Receive Session or Push Session. The two have fundamentally different authentication models and lifetimes.
