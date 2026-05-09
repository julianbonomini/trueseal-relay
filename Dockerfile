# ── build stage ───────────────────────────────────────────────────────────────
FROM golang:1.26 AS builder

WORKDIR /src

# Cache module downloads separately from source
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Pure Go build — modernc.org/sqlite requires no CGO
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath \
    -o /hush-relay ./cmd/hush-relay

# ── final stage ───────────────────────────────────────────────────────────────
# alpine: smallest image that provides /bin/sh for the entrypoint script
FROM alpine:3.21

RUN apk add --no-cache tzdata ca-certificates

COPY --from=builder /hush-relay /hush-relay
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Push sessions (Noise NK): hush-sync → relay
EXPOSE 7701
# Receive sessions (Noise XX): device long-lived connections
EXPOSE 7700

ENTRYPOINT ["/entrypoint.sh"]
CMD ["-config", "/etc/hush-relay/relay.toml"]
