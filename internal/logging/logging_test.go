package logging_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/julianbonomini/trueseal-relay/internal/logging"
)

// The seam's two levels (RELAY-9, ADR-0012): Printf in every mode,
// Debugf only in dev mode.
func TestLogger_Levels(t *testing.T) {
	var buf bytes.Buffer

	normal := logging.New(&buf, false)
	normal.Printf("lifecycle line")
	normal.Debugf("debug line")
	if out := buf.String(); !strings.Contains(out, "lifecycle line") {
		t.Errorf("normal mode dropped a Printf line: %q", out)
	}
	if strings.Contains(buf.String(), "debug line") {
		t.Errorf("normal mode emitted a Debugf line: %q", buf.String())
	}

	buf.Reset()
	dev := logging.New(&buf, true)
	if !dev.Dev() {
		t.Error("Dev() should report dev mode")
	}
	dev.Printf("lifecycle line")
	dev.Debugf("debug line")
	out := buf.String()
	if !strings.Contains(out, "lifecycle line") || !strings.Contains(out, "debug line") {
		t.Errorf("dev mode should emit both levels: %q", out)
	}
}

// Nop never panics and never writes — the zero-value default for packages
// constructed without a logger.
func TestNop(t *testing.T) {
	l := logging.Nop()
	if l.Dev() {
		t.Error("Nop should not be dev mode")
	}
	l.Printf("dropped")
	l.Debugf("dropped")
}
