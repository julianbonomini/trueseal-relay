# Persistent TCP connections scale to millions; file descriptor limits are not a constraint

Receive Sessions are long-lived TCP connections. Each connected Device holds one open connection to the relay for the duration of its session. This is the correct model for server-push delivery — the relay delivers blobs immediately to online devices without polling.

File descriptor limits are not a meaningful constraint. Linux defaults to 1024 FDs per process but this is trivially raised via `ulimit` or `systemd` config. The OS supports millions of concurrent connections. Go's goroutine-per-connection model costs approximately 4KB of stack per connection — 100K concurrent connections consumes ~400MB RAM, well within a modest VPS.

The real operational concern is NAT/firewall timeouts. Idle TCP connections are silently killed by NAT tables (typically 30–300 seconds). The relay must send application-level heartbeats on idle Receive Sessions to prevent silent disconnection. Clients that do not receive a heartbeat within a defined window should reconnect.

The alternative — short-lived polling connections (HTTP long-poll or periodic reconnect) — was rejected because it trades a solved problem (FD limits) for a real one (delivery latency and reconnect overhead).
