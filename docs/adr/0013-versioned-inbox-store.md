# The inbox store is versioned; a relay Store Reset needs the Operator's opt-in

Status: accepted (decided 2026-09-30 in [trueseal-roadmap#20](https://github.com/julianbonomini/trueseal-roadmap/issues/20); not yet implemented). This is the relay side of trueseal-sync ADR-0032, and it applies to the supported SQLite backend (ADR-0012).

The relay store has no schema version today. Self-hosters upgrade by pulling a new container, so a format change would either break the Node or quietly lose queued Blobs.

## Decision

- **Store Version.** `inbox.db` records the Store Version that wrote it. The relay migrates any older preview Store Version forward, in one transaction, when it starts. A crash leaves the old store intact, and the next start retries.
- **A newer store is refused.** A relay started on a store written by a newer release refuses to start, and leaves the store unchanged. The same goes for a store it can't migrate. Either way it writes one clear log line naming both Store Versions. That line is operational and carries no client metadata, which satisfies ADR-0012.
- **A relay Store Reset needs the Operator's opt-in.** A release may declare itself a relay Store Reset in its release notes. This needs no product-owner approval, because inboxes are a buffer and not a record (ADR-0012), and no Device identity is at stake. On such a release, the relay refuses to start on an older store until the Operator passes an explicit reset flag. With the flag, it drops every inbox and keeps the relay keypair, so clients stay configured.
- **Release gate.** Every relay release candidate must migrate a store fixture from each earlier release since the floor, holding queued Blobs, and deliver them afterwards. It must also refuse a newer store without changing it, and it must honour the reset flag only on a declared Store Reset release.

## Considered alternatives

- **Drop the inboxes automatically on a reset release.** Rejected. It silently loses up to 30 days of offline delivery, and senders won't resend because the relay already acked their pushes. A container that stops with a clear message is safer.
- **Require product-owner approval, as for device resets.** Rejected. The data is temporary and no identity is lost, so the Operator's consent is the right bar.

## Consequences

- The deploying docs describe the upgrade path, the refusal message and the reset flag.
- The Compatibility Table lists the relay's Store Version alongside its Transport Version.
