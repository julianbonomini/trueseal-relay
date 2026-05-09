package keypair_test

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/julianbonomini/hush-relay/internal/keypair"
)

// Valid hex keypair file loads successfully and public key is derived.
func TestLoad_ValidKey(t *testing.T) {
	// Generate a known 32-byte private key
	priv := make([]byte, 32)
	for i := range priv {
		priv[i] = byte(i)
	}
	path := filepath.Join(t.TempDir(), "keypair.hex")
	os.WriteFile(path, []byte(hex.EncodeToString(priv)+"\n"), 0600) //nolint:errcheck

	kp, err := keypair.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(kp.Private) != 32 {
		t.Errorf("want 32-byte private key, got %d", len(kp.Private))
	}
	if len(kp.Public) != 32 {
		t.Errorf("want 32-byte public key, got %d", len(kp.Public))
	}
}

// Missing file returns a clear error.
func TestLoad_MissingFile(t *testing.T) {
	_, err := keypair.Load("/nonexistent/keypair.hex")
	if err == nil {
		t.Error("want error for missing file, got nil")
	}
}

// Wrong key length returns a clear error.
func TestLoad_WrongLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	os.WriteFile(path, []byte("deadbeef"), 0600) //nolint:errcheck — 4 bytes, not 32

	_, err := keypair.Load(path)
	if err == nil {
		t.Error("want error for wrong key length, got nil")
	}
}

// Generate produces a valid 32-byte keypair and writes it to disk.
func TestGenerate_WritesValidKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")

	if err := keypair.Generate(path); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	kp, err := keypair.Load(path)
	if err != nil {
		t.Fatalf("Load after Generate: %v", err)
	}
	if len(kp.Private) != 32 || len(kp.Public) != 32 {
		t.Errorf("want 32-byte keys, got priv=%d pub=%d", len(kp.Private), len(kp.Public))
	}
}
