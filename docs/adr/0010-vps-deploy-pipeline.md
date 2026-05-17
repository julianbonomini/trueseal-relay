# VPS hosting and git-tag deploy pipeline

The relay is self-hosted on a commodity VPS (Hetzner/Hostinger) behind a Cloudflare DNS-only A record. A managed cloud (AWS ECS, Fargate) was considered but rejected — it introduces ceremony, cost, and operator lock-in that contradict the manifesto's "operational simplicity" and "self-hostable by anyone with a VPS and five minutes" principle. If the VPS is compromised the attacker gains nothing useful: the relay is blind by design.

Images are built in GitHub Actions on `git tag v*.*.*`, pushed to GHCR (private), then deployed via SSH: `docker compose pull && docker compose up -d`. The VPS holds a GitHub PAT scoped to `read:packages` only — source code is unreachable. Watchtower was considered but rejected in favour of explicit, observable deploys tied to a release tag.

The canonical operator artifact is a `docker-compose.yml` + `.env.example` downloadable from the GitHub release. The compose file includes the relay, Caddy (automatic HTTPS + reverse proxy for HTTP services), and Dozzle (public read-only log viewer). Operator prerequisites: DNS A record, open ports 80/443/7700/7701, two env vars filled in.

## Considered Options

- **AWS ECS/Fargate** — rejected: managed infra complexity contradicts operational simplicity principle
- **Watchtower** — rejected: timer-based, no observable deploy log, fuzzy timing
- **Build on VPS** — rejected: requires Go toolchain on VPS; GHCR keeps the VPS lean

## Consequences

- Cloudflare proxy (orange cloud) cannot be used for ports 7700/7701 — raw TCP bypasses HTTP proxying. DNS-only (grey cloud) A record required.
- Dozzle uses agent mode in v2 cluster deployments — no migration, compose file gains one agent service per Node.
