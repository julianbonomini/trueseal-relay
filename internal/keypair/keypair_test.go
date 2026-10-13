package keypair

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RED (RELAY-12): Generate must never overwrite an existing keypair. The old
// implementation truncated the file, so a stray genkey destroyed the relay key.
func TestGenerate_RefusesToOverwriteExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	if err := Generate(path); err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}

	err = Generate(path)
	if err == nil {
		t.Fatal("second Generate succeeded — existing keypair would be silently replaced")
	}
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Generate error = %v, want ErrExists", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(original) {
		t.Fatal("existing keypair file was modified by the refused Generate")
	}
}

// RELAY-12: first start with no keypair generates one at mode 0600.
func TestLoadOrGenerate_Generates0600WhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	kp, generated, err := LoadOrGenerate(path)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if !generated {
		t.Fatal("generated = false for a missing keypair file")
	}
	if len(kp.Public) != 32 || len(kp.Private) != 32 {
		t.Fatalf("key sizes: pub %d priv %d, want 32/32", len(kp.Public), len(kp.Private))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("keypair mode = %o, want 600", got)
	}
}

// RELAY-12: an existing keypair is reused, never rotated — the public key a
// restart prints is the same one clients configured.
func TestLoadOrGenerate_ReusesExistingAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	first, generated, err := LoadOrGenerate(path)
	if err != nil {
		t.Fatalf("first LoadOrGenerate: %v", err)
	}
	if !generated {
		t.Fatal("first call should report generated")
	}
	second, generated, err := LoadOrGenerate(path)
	if err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}
	if generated {
		t.Fatal("second call reported generated — an existing keypair was replaced")
	}
	if string(first.Public) != string(second.Public) {
		t.Fatal("public key changed across restarts — every client configuration would break")
	}
}

// RELAY-12: two racing first starts must not clobber each other; the loser
// loads the winner's key.
func TestLoadOrGenerate_ConcurrentFirstStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			kp, _, err := LoadOrGenerate(path)
			if err != nil {
				errs <- err
				return
			}
			results <- string(kp.Public)
		}()
	}
	var pubs []string
	for i := 0; i < 2; i++ {
		select {
		case p := <-results:
			pubs = append(pubs, p)
		case err := <-errs:
			t.Fatalf("concurrent LoadOrGenerate: %v", err)
		}
	}
	if pubs[0] != pubs[1] {
		t.Fatal("racing first starts produced two different relay keys")
	}
}

// TRUESEAL_RELAY_KEYPAIR_HEX: an operator-supplied key parses like a file key.
func TestParseHex_RoundTripsWithLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	if err := Generate(path); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fromHex, err := ParseHex(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("ParseHex: %v", err)
	}
	fromFile, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(fromHex.Public) != string(fromFile.Public) {
		t.Fatal("ParseHex and Load derived different public keys from the same material")
	}
}
