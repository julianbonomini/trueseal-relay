package keypair

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/flynn/noise"
	"golang.org/x/crypto/curve25519"
)

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

	privHex := strings.TrimSpace(string(data))
	priv, err := hex.DecodeString(privHex)
	if err != nil {
		return nil, fmt.Errorf("keypair: decode hex from %s: %w", path, err)
	}
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

// Generate creates a new random X25519 keypair and writes the hex-encoded
// private key to path (mode 0600). Use once at deployment setup.
func Generate(path string) error {
	dhKey, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return fmt.Errorf("keypair: generate: %w", err)
	}
	data := []byte(hex.EncodeToString(dhKey.Private) + "\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("keypair: write %s: %w", path, err)
	}
	return nil
}
