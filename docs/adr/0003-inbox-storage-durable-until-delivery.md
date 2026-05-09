# Inbox storage is durable until delivery; TTL on undelivered blobs

The relay must persist accepted blobs durably — across crashes, restarts, and node replacements — until they are delivered or reaped by TTL. An accepted blob that is lost before delivery is a data loss event. This is non-negotiable.

A blob is deleted from the Inbox only after the recipient Device sends a DeliverAck (frame `0x06`) confirming receipt. Until that Ack arrives, the blob remains in the store. If the Receive Session closes before the Ack is received, the blob is re-delivered on the next session — deduplication is the client's responsibility. Storage exists only for undelivered blobs — the relay is a delivery buffer, not a durable log.

Undelivered blobs are subject to a TTL (exact value operator-configured, suggested 30 days). Blobs exceeding the TTL without delivery are reaped. Reaping is policy, not data loss — it handles abandoned and permanently offline devices without unbounded storage growth.

Inbox state must be stored in a shared external store (not node-local). This is required for clustering: any Node must be able to serve any Inbox, and a failed Node must not take undelivered blobs with it. A single-Node deployment may use an embedded store; a clustered deployment requires an external one.

The alternative — tracking per-recipient delivery cursors — was rejected. Immediate post-delivery deletion is simpler, leaks no relationship information to the relay, and is consistent with the relay being a temporary buffer.

Callers that need history store it locally after receiving it. The relay is not the source of truth.
