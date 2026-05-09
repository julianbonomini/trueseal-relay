# Relay is always in the path; no P2P, no TURN

Remote sync always routes through the relay. Devices connect outbound to the relay and never attempt direct connections to each other (no STUN, no TURN, no NAT traversal). The relay is a permanent, zero-knowledge intermediary — it stores and forwards ciphertext blobs and never sees plaintext.

This simplifies the protocol significantly: devices only need outbound TCP to the relay, which works from any network. The tradeoff (relay is always in the path) is acceptable because the relay is zero-knowledge — if it is compromised, the attacker gets encrypted blobs only. LAN sync is out of scope — the relay has no concept of network topology.
