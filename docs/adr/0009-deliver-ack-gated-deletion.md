# Blob deletion is gated on DeliverAck from the Device

The relay previously deleted blobs from the InboxStore at Flush time — before writing the Deliver frame to the TCP connection. A TCP drop after Flush but before the write permanently lost those blobs. This violated the manifesto principle: *an accepted blob is never lost before delivery*.

The fix: blobs are deleted only after the recipient Device sends a `DeliverAck (0x06)` frame confirming receipt. Until that Ack arrives the blob remains in the store. The InboxStore interface gains `Peek` (fetch without delete) and `DeleteByIDs` to support this model. `Flush` (fetch+delete atomically) is retained for TTL reaping.

If a Receive Session closes before all DeliverAcks are received, unacknowledged blobs stay in the store and are re-delivered on the next session. Deduplication across sessions is the client's responsibility — this was already true and is documented in the InboxStore interface.

**Considered options:**

- *TCP write success as the deletion signal* — relay-internal, no protocol change. Rejected: TCP write success means bytes left the relay's OS buffer, not that the client persisted them. Does not satisfy the guaranteed delivery principle.
- *Client DeliverAck* — chosen. The client confirms durable receipt. Requires a new `DeliverAck (0x06)` frame (client→relay) and an 8-byte opaque blob_id prefix on the Deliver frame body. Breaking change to the wire protocol — acceptable because nothing is live yet.
