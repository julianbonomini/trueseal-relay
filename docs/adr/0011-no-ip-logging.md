# No IP address logging

The relay does not log IP addresses of connecting peers — neither for Receive Sessions nor Push Sessions.

Push Sessions are anonymous by design: the relay never learns the sender's identity (ADR-0002). Logging the sender's IP undermines this structurally — a public log entry linking an IP to a recipient key and timestamp is a correlation attack vector. Receive Session IP logging similarly links a device's network location to its public key.

This was made explicit as part of making relay logs publicly readable (via Dozzle) so any operator's users can audit that the relay is behaving as promised. Public logs are only a credible trust signal if they prove the relay is blind — logs containing IPs would instead prove the opposite.

IP-level debugging was the only meaningful trade-off. Connection counters, key prefixes, and error messages are sufficient for operator diagnostics without needing addresses.

## Consequences

- Operators cannot use logs to investigate abuse by IP. Rate limiting or IP-level blocking must be handled at the network/firewall layer, not in relay logs.
- This is a trust promise to relay users. Reversing it (re-adding IP logging) would require a changelog notice and a new ADR.
