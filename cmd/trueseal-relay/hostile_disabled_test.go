//go:build !hostile

package main

// Release-build proof for RELAY-14: without the `hostile` tag there is no
// runtime enrollment path — the -hostile-control flag is never defined and
// the starter is a no-op. (CI additionally asserts with go tool nm that no
// internal/hostile symbol reaches the release binary.)

import (
	"flag"
	"testing"
)

func TestReleaseBuildDefinesNoHostileFlag(t *testing.T) {
	if got := registerHostileFlag(); got != nil {
		t.Fatalf("release build must not register -hostile-control")
	}
	if flag.CommandLine.Lookup("hostile-control") != nil {
		t.Fatalf("release build must not define -hostile-control")
	}
}

func TestReleaseMaybeStartHostileIsANoOp(t *testing.T) {
	stop := maybeStartHostile(nil, nil, nil)
	if stop == nil {
		t.Fatalf("maybeStartHostile must return a stop func")
	}
	stop() // must not panic
}
