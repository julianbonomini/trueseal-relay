# Security Policy

TrueSeal is a **developer preview**. Everything here is `0.x`, published and installable,
and openly allowed to break. This policy is the same across every `trueseal-*` repository.

## Reporting a vulnerability

**Please report security issues privately through
[GitHub Security Advisories](../../security/advisories/new) (private vulnerability
reporting) — never in a public issue, discussion, or pull request.**

Include what you can: the affected component and version or commit, the scenario, a
reproduction or failing test if you have one, and which property you believe is broken
(confidentiality, integrity, authenticity, delivery, availability, or metadata privacy —
they are different claims).

If private vulnerability reporting is unavailable for some reason, open a public issue
describing only that you have something to report privately, and we will arrange a channel.

## What to expect

- **Best effort, no SLA.** We will acknowledge and investigate as time allows. There is no
  response-time commitment.
- **No bounty.** There is no paid program.
- **Fixes land only in the next 0.x release.** Nothing is backported to earlier versions
  (see [sync ADR-0033: preview lifecycle](https://github.com/julianbonomini/trueseal-sync/blob/main/docs/adr/0033-preview-lifecycle-and-1.0-criteria.md)).
  There is no notice period: any 0.x release may change the API, the wire format or stored
  data, and every breaking release carries Upgrade Notes.
- **Disclosure.** Confirmed findings are published as GitHub Security Advisories, crediting
  the reporter unless you ask otherwise. We ask that you give us a reasonable window to
  ship a fix before public disclosure.

## Scope: what is a security finding

The scope of this repository is the **relay binary and its published GHCR image** — the
software a self-hoster runs.

In scope for a report:

- Anything that breaks a claim the relay actually makes — see the [README](README.md) and
  the Threat Model page at [trueseal.dev](https://trueseal.dev), where every claim names
  the tests that prove it. Examples: the relay decrypting or forging a message, a push
  landing in an inbox other than the envelope recipient's, a DeliverAck deleting a blob
  from a device that did not ack it, an acknowledged blob being redelivered as new, or
  an unacked blob being lost before its TTL.
- **Metadata-privacy breaks in the relay software:** client IP addresses (or any client
  identifying metadata) being logged, stored, or used — in normal mode *or* dev mode
  ([ADR-0011](docs/adr/0011-no-ip-logging.md), [ADR-0012](docs/adr/0012-self-hosted-preview-baseline.md)).
- **Abuse-limit bypasses:** exceeding the per-inbox quota, the per-recipient rate limit,
  the connection cap, or the per-device Receive Session cap; circumventing the handshake,
  session, or write deadlines; the relay accepting a size limit or TTL above the protocol
  ceilings at startup.
- **Wire-protocol breaks:** the Transport Version exchange being skipped or mis-bound
  into the Noise prologue, or a typed refusal being silently treated as acceptance.
- **Release-integrity breaks:** the hostile-relay injection hooks being reachable in a
  release build, or the shipped image not matching the attested candidate bytes.

Not this repository's scope: findings about the protocol itself, the client, or the SDKs
belong to [trueseal-sync](https://github.com/julianbonomini/trueseal-sync) (or the
respective SDK repo). **Operator configuration is your side of the line:** a self-hosting
choice (running the experimental Postgres cluster, publishing the health port, adding your
own reverse proxy or log pipeline that captures client metadata, running an old image) is
not a vulnerability in the relay — though reports that the shipped defaults make such a
mistake easy are welcome.

The following are **documented limitations, not findings** — they are stated publicly, and
reporting them as vulnerabilities will be closed as known:

- **What the relay can see:** device keys, online times, recipient keys, blob sizes,
  timing, fan-out shape (likely co-membership), the End-to-End Version, and often which
  device pushed (sync ADR-0024 accepts push fan-out linkability by design).
- **The relay never logs, stores or uses IP addresses** — but the server it runs on still
  sees the connection, as with any internet service. Hiding your IP from the host needs a
  VPN or Tor (post-preview work).
- **Availability:** any holder of a public key can consume that inbox's quota; quotas and
  rate limits bound but cannot eliminate denial of service.
- **No relay key rotation** — changing the relay key breaks every client configuration.
- **Postgres clustering is experimental** ([ADR-0010](docs/adr/0010-postgres-adapter-for-clustered-deployment.md)) —
  not versioned, not the supported deployment.
- **Not independently audited.**

If you are unsure whether something is a limitation or a finding, report it privately —
we would rather hear it twice than miss it once.
