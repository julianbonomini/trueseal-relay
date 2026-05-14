# Relay public key distribution is outside the primitive

The relay has a long-term X25519 keypair (the same identity model as every Device). Connecting devices verify the relay holds its private key via the Noise XX handshake. How the relay's public key reaches end users is entirely the operator's responsibility — trueseal-relay does not model it.

Operators may distribute the relay public key by baking it into a client binary, publishing it on a landing page or QR code, or including it in a self-hosting README. trueseal-relay exposes the relay's public key and nothing else. This is the same contract trueseal-noise has with its callers: the library performs the handshake; key distribution is out of band.

The alternative — building relay discovery or key distribution into the binary — would embed deployment assumptions into infrastructure that has no business making them.
