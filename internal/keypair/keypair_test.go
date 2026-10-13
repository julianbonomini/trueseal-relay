package keypair_test

import (
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/julianbonomini/trueseal-relay/internal/keypair"
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

// Missing file returns a clear error wrapping fs.ErrNotExist (LoadOrGenerate
// depends on that to distinguish "generate" from "refuse").
func TestLoad_MissingFile(t *testing.T) {
	_, err := keypair.Load("/nonexistent/keypair.hex")
	if err == nil {
		t.Error("want error for missing file, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing-file error = %v, want it to wrap fs.ErrNotExist", err)
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

// ParseHex rejects a key that is not 32 bytes.
func TestParseHex_WrongLength(t *testing.T) {
	if _, err := keypair.ParseHex("deadbeef"); err == nil {
		t.Error("want error for 4-byte hex, got nil")
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

// RELAY-12: Generate must never overwrite an existing keypair. The old
// implementation truncated the file, so a stray genkey destroyed the relay key.
func TestGenerate_RefusesToOverwriteExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	if err := keypair.Generate(path); err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}

	err = keypair.Generate(path)
	if err == nil {
		t.Fatal("second Generate succeeded — existing keypair would be silently replaced")
	}
	if !errors.Is(err, keypair.ErrExists) {
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

// RELAY-12 (review P2.1): a crash between create and write used to leave a
// partial keyfile that wedged every later start. Generate now writes a temp
// file and links it into place atomically, so the only observable states are
// "no file" and "complete file". A corrupt file is REFUSED, never
// regenerated over — only the Operator may replace a relay key.
func TestLoadOrGenerate_RefusesPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keypair.hex")
	// The wedging state: a partially written keyfile.
	if err := os.WriteFile(path, []byte("ab"), 0600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	_, _, err := keypair.LoadOrGenerate(path)
	if err == nil {
		t.Fatal("LoadOrGenerate over a partial file succeeded — it must refuse, not regenerate")
	}
	if errors.Is(err, keypair.ErrExists) {
		t.Fatalf("partial file reported as ErrExists — want the underlying corrupt-file error, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "ab" {
		t.Fatal("partial file was modified — a relay key must only be replaced by the Operator")
	}
}

// RELAY-12 (review P2.1): a successful Generate leaves no temp-file debris —
// the only observable artifact is the complete keypair itself.
func TestGenerate_LeavesNoDebris(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keypair.hex")
	if err := keypair.Generate(path); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "keypair.hex" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory contains %v, want only keypair.hex", names)
	}
}

// RELAY-12: first start with no keypair generates one at mode 0600.
func TestLoadOrGenerate_Generates0600WhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	kp, generated, err := keypair.LoadOrGenerate(path)
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
	first, generated, err := keypair.LoadOrGenerate(path)
	if err != nil {
		t.Fatalf("first LoadOrGenerate: %v", err)
	}
	if !generated {
		t.Fatal("first call should report generated")
	}
	second, generated, err := keypair.LoadOrGenerate(path)
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

// RELAY-12 (review P2.2/P2.5): two racing first starts must not clobber each
// other, and the race loser reports generated=false so the startup log line
// stays truthful on multi-replica starts. The start barrier makes the
// collision as deterministic as scheduling allows; the ErrExists path itself
// is pinned directly by TestGenerate_RefusesToOverwriteExisting.
func TestLoadOrGenerate_ConcurrentFirstStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan string, 2)
	generatedFlags := make(chan bool, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release both goroutines together
			kp, generated, err := keypair.LoadOrGenerate(path)
			if err != nil {
				errs <- err
				return
			}
			results <- string(kp.Public)
			generatedFlags <- generated
		}()
	}
	close(start)
	wg.Wait()
	var pubs []string
	var gens []bool
	for i := 0; i < 2; i++ {
		select {
		case p := <-results:
			pubs = append(pubs, p)
		case err := <-errs:
			t.Fatalf("concurrent LoadOrGenerate: %v", err)
		}
		gens = append(gens, <-generatedFlags)
	}
	if pubs[0] != pubs[1] {
		t.Fatal("racing first starts produced two different relay keys")
	}
	trueCount := 0
	for _, g := range gens {
		if g {
			trueCount++
		}
	}
	if trueCount > 1 {
		t.Fatal("both racers reported generated=true — the loser's log line would lie")
	}
}

// TRUESEAL_RELAY_KEYPAIR_HEX: an operator-supplied key parses like a file key.
func TestParseHex_RoundTripsWithLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keypair.hex")
	if err := keypair.Generate(path); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fromHex, err := keypair.ParseHex(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("ParseHex: %v", err)
	}
	fromFile, err := keypair.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(fromHex.Public) != string(fromFile.Public) {
		t.Fatal("ParseHex and Load derived different public keys from the same material")
	}
}
