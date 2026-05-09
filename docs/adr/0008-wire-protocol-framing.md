# Wire protocol: raw framing with Ack and Heartbeat control messages

## Framing

All messages over a Noise session use the framing already defined in hush-sync/src/relay.rs:

```
[type: u8][len: u32 BE][body: bytes]
```

Heartbeat and Ack have zero-byte bodies — the type tag is the entire message minus the length prefix. `frame(MsgType::Heartbeat, &[])` is five bytes. No protobuf is used for control messages — proto buys nothing when there are no fields to name.

## Message types

Owned by hush-sync as the protocol authority (ADR-0005). hush-relay implements against them.

| Tag | Name | Direction | Body |
|---|---|---|---|
| `0x01` | Push | client → relay | encoded Envelope bytes |
| `0x02` | Deliver | relay → client | encoded Envelope bytes |
| `0x03` | Heartbeat | bidirectional | empty |
| `0x04` | Ack | relay → client | 8 bytes: u64 BE sequence of confirmed Envelope |

## Ack semantics

Ack means **"I persisted this blob to the InboxStore."** It does not mean "I received your bytes" — that is already guaranteed by Noise transport.

The distinction matters for crash safety. A relay that crashes between receiving a Push and writing to the InboxStore has lost the blob. Without an explicit Ack, the client (hush-sync outbox) cannot distinguish "relay persisted" from "bytes were written to TCP." With Ack, the outbox only marks a blob as relay-confirmed after receiving the Ack for its sequence number.

Sequence for Push Sessions:
1. Client opens Noise NK Push Session
2. Client sends one or more `Push` frames
3. For each Push: relay writes to InboxStore, then sends `Ack` with the Envelope's sequence number
4. Client reads Acks — session stays open until all Acks received
5. Client closes session

A Push Session that closes before all Acks are received is treated as unconfirmed. hush-sync replays unconfirmed blobs on the next Push Session.

## Heartbeat

Sent by the relay on idle Receive Sessions to prevent NAT/firewall timeout (see ADR-0006). Client echoes back a Heartbeat. Either side may initiate. Interval is operator-configured.
