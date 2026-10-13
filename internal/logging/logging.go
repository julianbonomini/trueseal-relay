// Package logging is the relay's single logging seam (RELAY-9, ADR-0012
// "Logging"). The session, relay and store packages take a *Logger instead
// of the global log package, so the normal/dev mode decision lives in
// exactly one place.
//
// Two levels:
//
//   - Printf — lifecycle events and field-free errors, emitted in every
//     mode. A line passed here never contains an IP address, a key or key
//     prefix, a size, a Blob ID, or any other per-client or per-message
//     field (ADR-0012 normal mode).
//   - Debugf — per-message detail, emitted only in dev mode: key prefixes,
//     sizes, Blob IDs and session lifecycle detail. Even here, payload
//     bytes and peer addresses are never logged — the relay never logs,
//     stores or uses a client's IP address in ANY mode (ADR-0012 "IP
//     addresses"; trueseal-roadmap#26).
//
// The address/payload rules are conventions enforced by review plus the
// binary-level dev-mode privacy test (cmd/trueseal-relay), which runs a
// full session over real TCP and fails if any address or payload byte
// reaches the log.
package logging

import (
	"io"
	"log"
	"sync/atomic"
)

// Logger routes relay log lines. It is safe for concurrent use.
type Logger struct {
	out *log.Logger
	dev atomic.Bool
}

// New returns a Logger writing to w. dev selects dev mode: Debugf lines
// are emitted, and the caller is expected to have printed the prominent
// startup warning ADR-0012 requires.
func New(w io.Writer, dev bool) *Logger {
	l := &Logger{out: log.New(w, "", log.LstdFlags)}
	l.dev.Store(dev)
	return l
}

// Nop returns a Logger that drops everything. It is the zero-value default
// for packages constructed without a logger (unit tests that do not
// exercise logging), so a nil *Logger is never dereferenced.
func Nop() *Logger {
	return New(io.Discard, false)
}

// Dev reports whether dev mode is on.
func (l *Logger) Dev() bool { return l.dev.Load() }

// Printf emits a lifecycle or field-free error line in every mode.
func (l *Logger) Printf(format string, args ...any) {
	l.out.Printf(format, args...)
}

// Debugf emits a per-message detail line in dev mode only. In normal mode
// the line is dropped before formatting — a stray expensive format never
// runs for a line nobody sees.
func (l *Logger) Debugf(format string, args ...any) {
	if !l.dev.Load() {
		return
	}
	l.out.Printf(format, args...)
}
