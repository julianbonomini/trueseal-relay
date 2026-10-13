//go:build hostile

package main

import (
	"flag"
	"log"
	"os"

	"github.com/julianbonomini/trueseal-relay/internal/hostile"
	"github.com/julianbonomini/trueseal-relay/internal/logging"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

// This file exists ONLY in `hostile` tag builds (RELAY-14; e2e ADR-0001
// gate G3). It is the only place the binary can gain the Hostile Relay
// surface: the -hostile-control flag is registered here, and without the
// tag neither the flag nor internal/hostile exists.

// registerHostileFlag defines the -hostile-control flag on fs (hostile
// builds only). TRUESEAL_RELAY_HOSTILE_CONTROL overrides it. Takes the
// FlagSet so tests use a fresh one — redefining a flag on the global
// CommandLine panics under -count=2.
func registerHostileFlag(fs *flag.FlagSet) *string {
	return fs.String("hostile-control", "", "hostile test build only: loopback address for the e2e control listener (e.g. 127.0.0.1:7703); also TRUESEAL_RELAY_HOSTILE_CONTROL")
}

// maybeStartHostile installs the hostile engine on the router and starts
// the loopback-only control listener. With no control address configured
// the hooks stay inert (the engine still wraps sessions, but with no rules
// every blob passes through unchanged). It returns a stop function for
// shutdown.
func maybeStartHostile(router *relay.Router, logg *logging.Logger, flagVal *string) func() {
	addr := ""
	if flagVal != nil {
		addr = *flagVal
	}
	if env := os.Getenv("TRUESEAL_RELAY_HOSTILE_CONTROL"); env != "" {
		addr = env
	}
	engine := hostile.NewEngine(logg)
	router.SetDeliveryWrapper(engine)
	if addr == "" {
		log.Printf("HOSTILE BUILD — injection hooks compiled in but no -hostile-control address given; delivery passes through until a listener is required")
		log.Printf("*** this is an e2e test build — never publish it as a release artifact or image ***")
		return func() { engine.Close() }
	}
	bound, err := engine.ListenControl(addr)
	if err != nil {
		log.Fatalf("hostile control: %v", err)
	}
	log.Printf("HOSTILE BUILD — injection hooks active, control listener on %s", bound)
	log.Printf("*** this is an e2e test build — never publish it as a release artifact or image ***")
	return func() { engine.Close() }
}
