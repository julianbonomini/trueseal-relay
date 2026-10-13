# Vendored sync test vectors (G5 / G8)

`sync-v1.json` is a byte-exact copy of `trueseal-sync`'s
`docs/test-vectors/sync-v1.json` — the preview wire format, pinned so the
relay's Go tests can prove they agree with the Rust core (release gate G5,
sync ADR-0022). The consumption contract is upstream:
`trueseal-sync/docs/test-vectors/README.md`.

## The pin (G8)

Every cross-repo input is pinned to an exact version and checksum — no
floating references. The pin lives in `vectors.go`:

- `SyncV1Source` — the upstream repo + commit + path this copy came from.
- `SyncV1FormatVersion` — the vectors' own format version.
- `SyncV1SHA256` — the SHA-256 of the vendored file. `Load()` refuses to
  parse bytes that don't match, so an accidental edit fails every
  vector-consuming test.

## Updating (deliberate protocol changes only)

The vectors change only when the wire format deliberately changes (a version
bump per ADR-0022). To update:

1. Land the upstream change in trueseal-sync (its CI regenerates and
   drift-checks `sync-v1.json`).
2. Copy the new file here:
   `cp ../trueseal-sync/docs/test-vectors/sync-v1.json internal/vectors/`
3. Update `SyncV1Source` and `SyncV1SHA256` in `vectors.go`
   (`shasum -a 256 internal/vectors/sync-v1.json`).
4. Review the vector diff as wire-format evidence in the same PR.

An unexpected checksum failure means someone edited the vendored copy by
hand — treat it as a bug, not a prompt to re-pin.
