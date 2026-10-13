//go:build !hostile

package main

import (
	"flag"

	"github.com/julianbonomini/trueseal-relay/internal/logging"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

// This file is the release half of the RELAY-14 pair. A normal build has
// no Hostile Relay surface: the -hostile-control flag is never defined
// (passing it fails "flag provided but not defined"), internal/hostile is
// never imported (CI asserts its symbols are absent with go tool nm), and
// maybeStartHostile does nothing.

// registerHostileFlag (release): no flag. A release binary cannot be
// enrolled into hostile behaviour at runtime.
func registerHostileFlag(_ *flag.FlagSet) *string { return nil }

// maybeStartHostile (release): a no-op.
func maybeStartHostile(_ *relay.Router, _ *logging.Logger, _ *string) func() {
	return func() {}
}
