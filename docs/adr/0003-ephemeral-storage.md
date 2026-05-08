# Relay stores blobs only until delivery; TTL on undelivered blobs

The relay deletes a blob immediately after it is successfully delivered to the recipient's active Session. Storage is only needed for offline recipients — blobs queue until the recipient reconnects.

Undelivered blobs are subject to a TTL (exact value TBD at implementation time, suggested 30 days). Blobs that exceed the TTL without delivery are reaped. This handles abandoned devices and extended offline periods without unbounded storage growth.

The alternative — tracking per-recipient delivery cursors — was rejected because it is unnecessary complexity. Immediate post-delivery deletion is simpler, leaks no group membership information to the relay, and is consistent with the relay being a temporary buffer rather than a durable log.

Callers that need durable history (e.g. hush-clip's local clipboard history) store it locally after receiving it. The relay is not the source of truth — it is a delivery mechanism.
