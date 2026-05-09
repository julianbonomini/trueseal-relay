#!/bin/sh
set -e

KEYPAIR_PATH="${HUSH_RELAY_KEYPAIR_PATH:-/data/keypair.hex}"

if [ ! -f "$KEYPAIR_PATH" ]; then
    echo "No keypair found at $KEYPAIR_PATH — generating..."
    /hush-relay -genkey -keyout "$KEYPAIR_PATH"
    echo "Keypair generated. Share the public key above with your clients."
fi

# Enforce 0600 on the keypair file even if the umask was permissive at generation time.
chmod 600 "$KEYPAIR_PATH"

exec /hush-relay "$@"
