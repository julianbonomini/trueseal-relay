//go:build hostile

package main

// Hostile-build wiring tests for RELAY-14: the -hostile-control flag
// exists only in this build, and the starter brings up the loopback
// control listener (or stays inert without an address). The control
// protocol itself is covered by the internal/hostile tests.

import (
	"flag"
	"net"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

func TestHostileBuildDefinesTheFlag(t *testing.T) {
	// A fresh FlagSet: registering on the global CommandLine would panic
	// on redefinition under -count=2.
	fs := flag.NewFlagSet("hostile-check", flag.ContinueOnError)
	if got := registerHostileFlag(fs); got == nil {
		t.Fatalf("hostile build must register -hostile-control")
	}
	if fs.Lookup("hostile-control") == nil {
		t.Fatalf("hostile build: -hostile-control not registered")
	}
	if err := fs.Parse([]string{"-hostile-control", "127.0.0.1:0"}); err != nil {
		t.Fatalf("hostile build must accept -hostile-control: %v", err)
	}
}

func TestMaybeStartHostileStartsLoopbackListener(t *testing.T) {
	// Reserve a free loopback port, release it, and hand it to the
	// starter (ListenControl binds synchronously before returning).
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	router := relay.NewRouter(nil, nil, 0, 0)
	stop := maybeStartHostile(router, nil, &addr)
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("control listener not answering on %s: %v", addr, err)
	}
	_ = conn.Close()
}

func TestMaybeStartHostileWithoutAddressStaysInert(t *testing.T) {
	router := relay.NewRouter(nil, nil, 0, 0)
	addr := ""
	stop := maybeStartHostile(router, nil, &addr)
	defer stop()
}
