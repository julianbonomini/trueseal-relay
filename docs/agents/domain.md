# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root — relay-specific domain language (Session, Inbox, Delivery, TTL, Operator)
- **`docs/adr/`** — relay infrastructure decisions (transport, storage, key distribution)
- Also read **`hush-sync/CONTEXT.md`** for protocol-level terms (Envelope, Blob, Device, Keypair, Pairing) that hush-relay implements but does not own

If any of these files don't exist, **proceed silently**.

## File structure

Single-context repo:

```
/
├── CONTEXT.md
├── docs/
│   └── adr/
│       ├── 0001-relay-always-in-path.md
│       └── ...
```

## Use the glossary's vocabulary

When your output names a domain concept, use the term as defined in `CONTEXT.md`. For protocol-level terms not defined here, defer to `hush-sync/CONTEXT.md`.

Don't drift to synonyms the glossary explicitly avoids (e.g. "server" instead of "Relay", "push" for what the relay does — that's "Delivery").

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0002 (Noise XX over TCP) — but worth reopening because…_
