# Noise XX and NK over TCP for device-to-relay connections

Two session types exist, each using a different Noise handshake pattern over raw TCP. Both reuse the single cryptographic vocabulary of the hush stack — everything is keypairs, everything is Noise.

**Receive Sessions use Noise XX** — mutual authentication. The Device and the relay verify each other's static public keys. The relay learns the Device's stable identity (its static public key) and registers it as online for immediate delivery. Forward secrecy is provided by the handshake.

**Push Sessions use Noise NK** — one-sided authentication. The Device verifies the relay's static public key (confirming it is talking to the correct relay), but the relay never learns the sender's identity. A fresh ephemeral X25519 keypair is generated for each Push Session and discarded immediately after. The relay sees an unlinkable anonymous peer. This is what makes sender identity structurally unknowable — not policy-withheld.

The alternative for Push Sessions was reusing Noise XX with the Device's stable keypair. Rejected because it would expose sender identity to the relay on every push, breaking the blind principle. Noise NK is the correct pattern when one party must remain anonymous.

The alternative for the transport layer was WebSocket + TLS + challenge-response auth. Rejected because it introduces a second cryptographic vocabulary, requires a separate credential system, and provides no benefit — the relay is infrastructure we control so CDN/load-balancer compatibility is not a constraint.
