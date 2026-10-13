# ── build stage ───────────────────────────────────────────────────────────────
FROM golang:1.26 AS builder

WORKDIR /src

# Cache module downloads separately from source
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Cross-build args must precede every RUN that uses them.
ARG TARGETOS TARGETARCH

# RELAY-14: a release image must never carry the Hostile Relay surface.
# The `hostile` build tag is never passed here; fail the image build if
# hook code somehow survived anyway (RELAY-15 carries the same assertion
# into the candidate/promote workflows).
#
# The check runs against an UNSTRIPPED intermediate build: the shipped
# binary is built with -ldflags="-s -w", and -s strips the symbol table
# go tool nm reads — an nm check on the final artifact would succeed
# vacuously even with the tag set (proven locally: a stripped
# hostile-tagged build shows zero internal/hostile symbols, the
# unstripped intermediate shows 30 with the tag and 0 without).
# RELAY-15: copy THIS pattern (intermediate build, then nm), never an
# nm check on a stripped artifact.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -trimpath -o /tmp/nm-check ./cmd/trueseal-relay && \
  ! go tool nm /tmp/nm-check | grep -q "internal/hostile"

# Pure Go build — modernc.org/sqlite requires no CGO
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -ldflags="-s -w" -trimpath \
  -o /trueseal-relay ./cmd/trueseal-relay

# ── final stage ───────────────────────────────────────────────────────────────
# alpine: smallest image that provides /bin/sh for the entrypoint script
FROM alpine:3.21

RUN apk add --no-cache tzdata ca-certificates

COPY --from=builder /trueseal-relay /trueseal-relay
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Push sessions (Noise NK): trueseal-sync → relay
EXPOSE 7701
# Receive sessions (Noise XX): device long-lived connections
EXPOSE 7700

# ADR-0012 "Health and metrics": the health listener stays private to the
# container network — Docker's own HEALTHCHECK uses it (RELAY-10). A Node
# whose store stops answering goes unhealthy and gets restarted instead of
# showing green while every push fails. busybox wget exits non-zero on 503.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:7702/healthz || exit 1

ENTRYPOINT ["/entrypoint.sh"]
# No -config flag: the image ships no config file, and a -config pointing at
# a missing path silently falls back to defaults (the pre-RELAY-16 shape
# hid that). Under Docker the documented configuration path is env vars —
# see .env.example and docker-compose.yml. Operators who want a file mount
# one and pass -config themselves.
CMD []
