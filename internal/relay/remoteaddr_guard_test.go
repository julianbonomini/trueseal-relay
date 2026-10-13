package relay_test

// RELAY-9 completion criterion (issue #29): non-test code has no
// RemoteAddr() call — enforced here, at the source. The relay never logs,
// stores or uses a client's IP address in ANY mode (ADR-0012 "IP
// addresses"; trueseal-roadmap#26), and RemoteAddr is the one standard
// library call that surfaces it. Dev mode must not reintroduce it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoRemoteAddrInNonTestCode(t *testing.T) {
	root := filepath.Join("..", "..") // repo root, from internal/relay
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name := info.Name()
		if info.IsDir() {
			switch name {
			case ".git", ".worktrees", "graft", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "RemoteAddr(") {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(hits) > 0 {
		t.Errorf("RemoteAddr() in non-test code (the relay never reads a peer address, any mode): %v", hits)
	}
}
