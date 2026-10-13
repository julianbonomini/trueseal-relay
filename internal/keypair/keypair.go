package keypair

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
// private key to path (mode 0600). The write is create-exclusive: if the
// file already exists, Generate fails with ErrExists and leaves it untouched,
// so a stray genkey can never destroy the relay key (RELAY-12).
func Generate(path string) error {
	dhKey, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return fmt.Errorf("keypair: generate: %w", err)
	}
	data := []byte(hex.EncodeToString(dhKey.Private) + "\n")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("keypair: %s already exists — refusing to replace the relay key: %w", path, ErrExists)
		}
		return fmt.Errorf("keypair: write %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("keypair: write %s: %w", path, err)
	}
	return nil
}

// LoadOrGenerate loads the keypair at path, or generates one at mode 0600 on
// first start. generated reports which happened. If two first starts race,
// the loser's create-exclusive write fails with ErrExist and it loads the
// winner's key — both end up with the same relay keypair.
func LoadOrGenerate(path string) (kp *KeyPair, generated bool, err error) {
	kp, err = Load(path)
	if err == nil {
		return kp, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	if genErr := Generate(path); genErr != nil && !errors.Is(genErr, ErrExists) {
		return nil, false, genErr
	}
	// Either we just generated it, or a racing process beat us to it.
	kp, err = Load(path)
	if err != nil {
		return nil, false, err
	}
	return kp, true, nil
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
