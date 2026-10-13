#!/bin/sh
set -e

# All keypair handling lives in the binary (RELAY-12):
#   - first start generates the keypair at mode 0600 (create-exclusive),
#   - TRUESEAL_RELAY_KEYPAIR_HEX supplies an operator key directly,
#   - `trueseal-relay pubkey` / `address --host H` print what clients need.
# The entrypoint only execs, and "$@" goes straight to the binary — the old
# doubled-path shape (`exec /trueseal-relay "$@"` receiving
# "/trueseal-relay -genkey ...") made flag parsing silently drop -genkey.
#
# RELAY_BINARY is a test seam; the image always uses the default.
exec "${RELAY_BINARY:-/trueseal-relay}" "$@"
