# Noise XX over TCP for device-to-relay connections

Devices connect to the relay using Noise XX over raw TCP — the same protocol and keypair infrastructure as hush-noise. There is no P2P; devices never connect directly to each other.

The alternative was WebSocket + TLS + challenge-response auth. We chose Noise XX because: it reuses the single cryptographic vocabulary of the whole stack (everything is keypairs, everything is Noise XX), it gives mutual authentication and forward secrecy without a separate credential system, and the relay is infrastructure we control so CDN/load-balancer compatibility is not a constraint.

The consequence: the relay is just another Noise responder (`noise.Accept()`). Devices dial it exactly as they would dial another device. No special auth scheme, no API keys, no bearer tokens.
