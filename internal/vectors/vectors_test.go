package vectors_test

import (
	"strings"
	"testing"

	"github.com/julianbonomini/trueseal-relay/internal/vectors"
)

// The pin (G8): the vendored file parses, matches its checksum (enforced by
// Load), and carries the expected format identity.
func TestLoad_PinIntegrity(t *testing.T) {
	v, rejections, err := vectors.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v.Format != "trueseal-sync-vectors" {
		t.Fatalf("format = %q", v.Format)
	}
	if v.FormatVersion != vectors.SyncV1FormatVersion {
		t.Fatalf("formatVersion = %d, want %d", v.FormatVersion, vectors.SyncV1FormatVersion)
	}
	if !strings.Contains(vectors.SyncV1Source, "trueseal-sync@") {
		t.Fatalf("SyncV1Source %q does not name an exact upstream commit", vectors.SyncV1Source)
	}
	if len(rejections) == 0 {
		t.Fatal("no rejections parsed")
	}
}

// The refusal example in the vectors is a FORMAT example; the relay's actual
// line uses its real supported range (min == max == TransportVersion, since
// the relay speaks exactly one version — ADR-0022).
func TestRefusalExample_Format(t *testing.T) {
	v, _, err := vectors.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	line := v.TransportVersionPrefix.RefusalExample.Line
	if !strings.HasPrefix(line, "unsupported ") || !strings.HasSuffix(line, "\n") || !strings.Contains(line, "..") {
		t.Fatalf("refusal example %q is not the unsupported <min>..<max>\\n shape", line)
	}
}

// The relay-spoken rejection layers must be present in the vectors.
func TestRejections_CoverTransportAndSizeLayers(t *testing.T) {
	_, rejections, err := vectors.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var transport, size int
	for _, r := range rejections {
		switch r.Layer {
		case "transport":
			transport++
		case "size":
			size++
		}
	}
	if transport < 2 {
		t.Fatalf("want ≥2 transport rejections, got %d", transport)
	}
	if size < 1 {
		t.Fatalf("want ≥1 size rejection, got %d", size)
	}
}
