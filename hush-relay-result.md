# Issue #19 — Error (0x05) frame on rejection in AcceptPush

## Status: Completed ✓

Commit: `feat(session): send Error frame on OnPush rejection (closes #19)`

---

## What was done

### TDD cycle

**RED** — Added `TestAcceptPush_ErrorFrameOnOnPushError` to `push_test.go` first. The test dials a Noise NK Push Session, sends a Push frame with a handler configured to return an error, and asserts that an `Error (0x05)` frame is received. Confirmed build failure:
```
internal/session/push_test.go:548:20: undefined: session.MsgTypeError
```

**GREEN** — Three targeted changes:

1. **`internal/session/frame.go`** — Added `MsgTypeError MsgType = 0x05` with doc comment (permanent rejection, non-retryable). Updated `Parse` to recognise `0x05` in the type switch.

2. **`internal/session/push.go`** — Updated the `OnPush` error path in `AcceptPush`: on error, encrypt and send an `Error` frame via `cs2` (relay→client cipher), then `continue`. The blob is still not stored (no Ack) — the send is best-effort (encrypt failure silently skips send, which would be an internal transport error unrelated to the rejection).

3. **`internal/session/push_test.go`** — Updated `TestAcceptPush_NoAckOnOnPushError`: the old test checked for silence (no data at all), but the correct new behavior is an `Error` frame. Updated it to decrypt the response and assert `MsgTypeError`, not `MsgTypeAck`. Added the new `TestAcceptPush_ErrorFrameOnOnPushError`.

4. **`docs/adr/0008-wire-protocol-framing.md`** — Added `0x05 | Error | relay → client | empty` to the message type table. Added two new sections: **Error semantics** (permanent rejection, non-retryable, body is empty by design — relay is blind) and **Two-layer enforcement model** (hush-sync as primary, relay as backstop for non-compliant clients; 1 MiB default sourced from hush-sync `MAX_ENVELOPE_BYTES`).

**REFACTOR** — No refactoring was necessary; the change was minimal and clean.

---

## Files changed

| File | Change |
|---|---|
| `internal/session/frame.go` | Added `MsgTypeError = 0x05`; updated `Parse` switch |
| `internal/session/push.go` | Error path sends `Error` frame before `continue` |
| `internal/session/push_test.go` | New test `TestAcceptPush_ErrorFrameOnOnPushError`; updated `TestAcceptPush_NoAckOnOnPushError` |
| `docs/adr/0008-wire-protocol-framing.md` | Error row in table; Error semantics section; Two-layer enforcement section |

---

## Test results

```
ok  github.com/julianbonomini/hush-relay/cmd/hush-relay
ok  github.com/julianbonomini/hush-relay/internal/config
ok  github.com/julianbonomini/hush-relay/internal/keypair
ok  github.com/julianbonomini/hush-relay/internal/notify/inprocess
ok  github.com/julianbonomini/hush-relay/internal/relay
ok  github.com/julianbonomini/hush-relay/internal/session
ok  github.com/julianbonomini/hush-relay/internal/store
ok  github.com/julianbonomini/hush-relay/internal/store/sqlite
```

All 9 packages pass. No existing tests broken.

---

## Manifesto compliance

- **Blind**: The relay sends `Error` solely because `OnPush` returned an error — never because of content inspection. The relay sees a size (bytes), not a meaning.
- **Durable until delivered**: Rejected blobs are never stored, so there is nothing to lose. Accepted blobs still get an `Ack` only after `Put` succeeds.
- **Operational simplicity**: No new configuration. No new binaries.
