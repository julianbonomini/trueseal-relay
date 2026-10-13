package keypair

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/flynn/noise"
	"golang.org/x/crypto/curve25519"
)

// ErrExists is returned by Generate when the keypair file already exists.
// A relay key is never silently replaced: losing or rotating it breaks every
// client configuration (ADR-0012).
var ErrExists = fs.ErrExist

// KeyPair holds a Noise X25519 static keypair.
type KeyPair struct {
	Private []byte
	Public  []byte
	DHKey   noise.DHKey
}

// Load reads a hex-encoded 32-byte X25519 private key from path and derives
// the public key. Fails fast with a clear error if missing or malformed.
func Load(path string) (*KeyPair, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("keypair: read %s: %w", path, err)
	}

	kp, err := ParseHex(string(data))
	if err != nil {
		return nil, fmt.Errorf("keypair: %s: %w", path, err)
	}
	return kp, nil
}

// Generate creates a new random X25519 keypair and writes the hex-encoded
// private key to path (mode 0600). The write is create-exclusive and atomic:
// the key is written to a temp file in the same directory and hard-linked
// into place, so the only observable states are "no file" and "complete
// file" — a crash can never leave a partial keyfile that wedges every later
// start (RELAY-12). If the file already exists, Generate fails with ErrExists
// and leaves it untouched: a stray genkey can never destroy the relay key.
func Generate(path string) error {
	hexKey, err := GenerateHex()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keypair.tmp-*")
	if err != nil {
		return fmt.Errorf("keypair: create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the link succeeds and we remove it
	if _, err := tmp.WriteString(hexKey + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("keypair: write temp for %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("keypair: sync temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("keypair: close temp for %s: %w", path, err)
	}
	// link(2) is atomic and refuses to replace an existing path — the
	// create-exclusive gate. The temp file never outlives this function.
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("keypair: %s already exists — refusing to replace the relay key: %w", path, ErrExists)
		}
		return fmt.Errorf("keypair: install %s: %w", path, err)
	}
	return nil
}

// LoadOrGenerate loads the keypair at path, or generates one at mode 0600 on
// first start. generated reports which happened — including in a race: the
// loser of two racing first starts loads the winner's key and reports
// generated=false, so the startup log line stays truthful (RELAY-12). A
// corrupt or partial file is REFUSED with its underlying error, never
// regenerated over: only the Operator replaces a relay key.
func LoadOrGenerate(path string) (kp *KeyPair, generated bool, err error) {
	kp, err = Load(path)
	if err == nil {
		return kp, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	genErr := Generate(path)
	switch {
	case genErr == nil:
		generated = true
	case errors.Is(genErr, ErrExists):
		generated = false // a racing first start won; fall through and load its key
	default:
		return nil, false, genErr
	}
	kp, err = Load(path)
	if err != nil {
		return nil, false, err
	}
	return kp, generated, nil
}

// ParseHex parses a hex-encoded 32-byte X25519 private key, as supplied via
// TRUESEAL_RELAY_KEYPAIR_HEX (ADR-0012: operators may bring their own key).
func ParseHex(privHex string) (*KeyPair, error) {
	priv, err := hex.DecodeString(strings.TrimSpace(privHex))
	if err != nil {
		return nil, fmt.Errorf("keypair: decode hex: %w", err)
	}
	return fromPrivate(priv)
}

// GenerateHex returns a fresh hex-encoded private key (test helper and the
// genkey subcommand share this with Generate).
func GenerateHex() (string, error) {
	dhKey, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("keypair: generate: %w", err)
	}
	return hex.EncodeToString(dhKey.Private), nil
}

// Resolve returns the keypair the relay must use: the operator-supplied
// TRUESEAL_RELAY_KEYPAIR_HEX when set, otherwise the keypair file at path,
// generated on first start. Serve, pubkey and address all resolve through
// this one path, so the printed key is always the key the handshake uses.
func Resolve(path string) (kp *KeyPair, generated bool, err error) {
	if v := os.Getenv("TRUESEAL_RELAY_KEYPAIR_HEX"); v != "" {
		kp, err := ParseHex(v)
		if err != nil {
			return nil, false, fmt.Errorf("TRUESEAL_RELAY_KEYPAIR_HEX: %w", err)
		}
		return kp, false, nil
	}
	return LoadOrGenerate(path)
}

func fromPrivate(priv []byte) (*KeyPair, error) {
	if len(priv) != 32 {
		return nil, fmt.Errorf("keypair: want 32-byte private key, got %d bytes", len(priv))
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("keypair: derive public key: %w", err)
	}
	dhKey := noise.DHKey{Private: priv, Public: pub}
	return &KeyPair{Private: priv, Public: pub, DHKey: dhKey}, nil
}
