#!/bin/sh
set -e

KEYPAIR_PATH="${TRUESEAL_RELAY_KEYPAIR_PATH:-/data/keypair.hex}"

# Operator-supplied keypair takes precedence over auto-generation.
if [ -n "$TRUESEAL_RELAY_KEYPAIR_HEX" ]; then
    echo "$TRUESEAL_RELAY_KEYPAIR_HEX" > "$KEYPAIR_PATH"
    chmod 600 "$KEYPAIR_PATH"
fi

if [ ! -f "$KEYPAIR_PATH" ]; then
    echo "No keypair found at $KEYPAIR_PATH — generating..."
    /trueseal-relay -genkey -keyout "$KEYPAIR_PATH"
    echo "Keypair generated. Share the public key above with your clients."
fi

# Enforce 0600 on the keypair file even if the umask was permissive at generation time.
chmod 600 "$KEYPAIR_PATH"

exec /trueseal-relay "$@"
