//go:build !hostile

package main

// Release-build proof for RELAY-14: without the `hostile` tag there is no
// runtime enrollment path — the -hostile-control flag is never defined and
// the starter is a no-op. (CI additionally asserts with go tool nm that no
// internal/hostile symbol reaches the release binary.)

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func TestReleaseBuildDefinesNoHostileFlag(t *testing.T) {
	// A fresh FlagSet mirrors serve()'s registration without touching the
	// global CommandLine (a redefinition there would panic under -count=2).
	fs := flag.NewFlagSet("release-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if got := registerHostileFlag(fs); got != nil {
		t.Fatalf("release build must not register -hostile-control")
	}
	if fs.Lookup("hostile-control") != nil {
		t.Fatalf("release build must not define -hostile-control")
	}
	// The runtime enrollment path is absent: passing the flag fails loudly.
	err := fs.Parse([]string{"-hostile-control", "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("release build must reject -hostile-control as undefined, got: %v", err)
	}
}

func TestReleaseMaybeStartHostileIsANoOp(t *testing.T) {
	stop := maybeStartHostile(nil, nil, nil)
	if stop == nil {
		t.Fatalf("maybeStartHostile must return a stop func")
	}
	stop() // must not panic
}
