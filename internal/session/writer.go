package session

import (
	"net"
	"time"

	"github.com/flynn/noise"
)

// writerQueueCapacity bounds plaintext frames buffered between producers
// (the delivery goroutine, the Heartbeat echo path) and the writer. Small
// on purpose: backpressure should reach the routing loop quickly so blobs
// stay Peek-able in the Inbox rather than piling up per session.
const writerQueueCapacity = 16

// sessionWriter is the single owner of a Receive Session's outbound
// CipherState and net.Conn (RELAY-2, issue #39).
//
// flynn/noise CipherState is not safe for concurrent use — racing Encrypt
// calls corrupt the nonce sequence and the peer can no longer decrypt.
// Framing must also not interleave on the wire. Channelling every outbound
// frame through one goroutine serialises both by construction: nonce order
// is call order, and each frame is written with a single Write.
type sessionWriter struct {
	queue chan []byte
	done  chan struct{}
}

// newSessionWriter starts the writer goroutine. It exits when stop is
// closed or when encrypt/write fails.
//
// writeTimeout is the per-write deadline (RELAY-6; the RELAY-2 review
// follow-up): a blocked conn.Write does not select on stop, so without a
// deadline a client that never reads pins the writer goroutine and its
// queue forever. On ANY exit the writer closes the conn — that unblocks
// the session's read loop promptly (deadline or not), which is what
// closes stop and releases every blocked send().
func newSessionWriter(conn net.Conn, cs *noise.CipherState, stop <-chan struct{}, writeTimeout time.Duration) *sessionWriter {
	w := &sessionWriter{
		queue: make(chan []byte, writerQueueCapacity),
		done:  make(chan struct{}),
	}
	go w.run(conn, cs, stop, writeTimeout)
	return w
}

func (w *sessionWriter) run(conn net.Conn, cs *noise.CipherState, stop <-chan struct{}, writeTimeout time.Duration) {
	defer close(w.done)
	// A failed or stopped writer must tear the session down: closing the
	// conn makes the read loop fail, which closes stop and frees every
	// goroutine blocked in send(). The session's own defer conn.Close()
	// makes the normal path a harmless double close.
	defer conn.Close()
	for {
		select {
		case <-stop:
			return
		case frame := <-w.queue:
			encrypted, err := cs.Encrypt(nil, nil, frame)
			if err != nil {
				return
			}
			if writeTimeout > 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			}
			if err := writeNoiseMsg(conn, encrypted); err != nil {
				return
			}
		}
	}
}

// send enqueues a frame, blocking until the writer takes it or the session
// stops. Deliver frames use send: they must never be dropped — anything not
// written is simply not DeliverAck'd, so it stays in the Inbox and is
// re-delivered (ADR-0009). Blocking here is the backpressure signal.
// Returns false if the session stopped first.
func (w *sessionWriter) send(frame []byte, stop <-chan struct{}) bool {
	select {
	case w.queue <- frame:
		return true
	case <-stop:
		return false
	}
}

// trySend enqueues a frame without blocking, dropping it if the writer is
// behind. Heartbeat echoes use trySend: a dropped echo is harmless — the
// client heartbeats every 25 s (ADR-0012), and a session whose writer is
// persistently saturated is unhealthy anyway. The read loop must never
// stall behind a slow conn: it also serves DeliverAck, and stalling it
// would block inbox deletion (ADR-0009).
func (w *sessionWriter) trySend(frame []byte) {
	select {
	case w.queue <- frame:
	default:
	}
}
