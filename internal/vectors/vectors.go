// Package vectors vendors the trueseal-sync JSON test vectors so the relay's
// Go tests can prove byte-level agreement with the Rust core — release gate
// G5 (sync ADR-0022).
//
// The file is vendored, not fetched: tests must not touch the network, and
// gate G8 requires every cross-repo input pinned to an exact version and
// checksum. Update procedure: see README.md in this directory.
package vectors

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// The pin (G8): the exact upstream source and the content checksum of the
// vendored file. Load fails if the bytes drift from this checksum.
const (
	// SyncV1Source identifies the exact upstream artifact this copy pins.
	SyncV1Source = "trueseal-sync@aa535b4 docs/test-vectors/sync-v1.json"
	// SyncV1FormatVersion is the vectors' own format version field.
	SyncV1FormatVersion = 1
	// SyncV1SHA256 is the SHA-256 of the vendored sync-v1.json, hex.
	SyncV1SHA256 = "0506d5a80a9485f612d4d38a2530b70296a1e022e0539f3a001b6298a3dc3b37"
)

//go:embed sync-v1.json
var syncV1 []byte

// Vectors is the subset of the sync vector schema the relay checks against
// (the transport and size layers; the end-to-end layers are the client's).
type Vectors struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"formatVersion"`
	Protocol      struct {
		TransportVersion       int `json:"transportVersion"`
		ProtocolSizeLimit      int `json:"protocolSizeLimit"`
		MaxEnvelopeBytes       int `json:"maxEnvelopeBytes"`
		NoiseFramePlaintextMax int `json:"noiseFramePlaintextMax"`
	} `json:"protocol"`
	TransportVersionPrefix struct {
		ClientPrefixHex string `json:"clientPrefixHex"`
		AcceptReplyHex  string `json:"acceptReplyHex"`
		RefusalExample  struct {
			Line string `json:"line"`
			Min  int    `json:"min"`
			Max  int    `json:"max"`
		} `json:"refusalExample"`
	} `json:"transportVersionPrefix"`
	Frames map[string]json.RawMessage `json:"frames"`
}

// FrameVector is one entry of the frames section.
type FrameVector struct {
	Type       int    `json:"type"`
	BodyLayout string `json:"bodyLayout"`
	BlobIDHex  string `json:"blobIdHex"`
	FrameHex   string `json:"frameHex"`
}

// Rejection is one entry of the rejections section.
type Rejection struct {
	Layer  string          `json:"layer"`
	Case   string          `json:"case"`
	Input  json.RawMessage `json:"input"`
	Expect json.RawMessage `json:"expect"`
}

// Load parses the vendored vectors, verifying the pinned checksum first.
func Load() (*Vectors, []Rejection, error) {
	sum := sha256.Sum256(syncV1)
	if got := hex.EncodeToString(sum[:]); got != SyncV1SHA256 {
		return nil, nil, fmt.Errorf("vectors: checksum mismatch: file %s, pinned %s — the vendored copy drifted from the pin (G8)", got, SyncV1SHA256)
	}
	var full struct {
		Vectors
		Rejections []Rejection `json:"rejections"`
	}
	if err := json.Unmarshal(syncV1, &full); err != nil {
		return nil, nil, fmt.Errorf("vectors: parse: %w", err)
	}
	v := full.Vectors
	if v.Format != "trueseal-sync-vectors" || v.FormatVersion != SyncV1FormatVersion {
		return nil, nil, fmt.Errorf("vectors: unsupported format %q v%d (pin expects v%d)", v.Format, v.FormatVersion, SyncV1FormatVersion)
	}
	return &v, full.Rejections, nil
}

// Frame decodes one frames-section entry.
func (v *Vectors) Frame(name string) (*FrameVector, error) {
	raw, ok := v.Frames[name]
	if !ok {
		return nil, fmt.Errorf("vectors: no frame %q", name)
	}
	var fv FrameVector
	if err := json.Unmarshal(raw, &fv); err != nil {
		return nil, fmt.Errorf("vectors: frame %q: %w", name, err)
	}
	return &fv, nil
}
