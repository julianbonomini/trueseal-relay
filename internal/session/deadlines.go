package session

import "time"

// Deadlines carries the ADR-0012 liveness deadlines (RELAY-6). Every value
// is operator-configurable with short values so e2e can exercise the paths
// quickly; a zero value disables its deadline (the pre-RELAY-6 behaviour
// existing test callers rely on).
type Deadlines struct {
	// Handshake bounds the version-prefix read plus the Noise handshake on
	// both listeners (ADR-0012: 10 s). It covers the prefix read too — a
	// client that connects and sends nothing (slow-loris on the prefix)
	// is closed here.
	Handshake time.Duration
	// PushSession bounds the whole Push Session, accept to close
	// (ADR-0012: 30 s in total).
	PushSession time.Duration
	// ReceiveIdle closes a Receive Session after this much silence
	// (ADR-0012: 75 s with no frame received). ANY inbound frame —
	// Heartbeat, DeliverAck, even a malformed one — resets it, matching
	// the client's any-frame liveness (SYNC-16).
	ReceiveIdle time.Duration
	// Write bounds a single outbound write inside the sessionWriter
	// (the RELAY-6 follow-up the RELAY-2 review recorded: a blocked
	// conn.Write doesn't select on stop, so without a deadline a stalled
	// client pins the writer goroutine and its queue forever).
	Write time.Duration
}
