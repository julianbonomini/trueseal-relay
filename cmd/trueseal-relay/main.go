package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/config"
	"github.com/julianbonomini/trueseal-relay/internal/keypair"
	"github.com/julianbonomini/trueseal-relay/internal/notify"
	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
	"github.com/julianbonomini/trueseal-relay/internal/store"
	pgstore "github.com/julianbonomini/trueseal-relay/internal/store/postgres"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

// logConnectionRefused reports a connection-limit refusal.
// ADR-0012: no client metadata — the peer address is never logged.
func logConnectionRefused(kind string, max int) {
	log.Printf("%s: connection limit reached (%d) — rejecting connection", kind, max)
}

// usage lists the subcommands. RELAY-12: keypair operations are subcommands,
// so they cannot be silently swallowed by flag parsing the way the doubled
// Docker entrypoint path swallowed -genkey.
const usage = `trueseal-relay — blind ciphertext relay

Usage:
  trueseal-relay [flags]                 run the relay (generates the keypair on first start)
  trueseal-relay pubkey [flags]          print the relay public key (hex) that clients configure
  trueseal-relay address --host H [flags] print the Relay Address (sync ADR-0028 grammar)
  trueseal-relay genkey [flags]          generate a keypair file and exit (never overwrites)

Flags (run): -config <path> (default relay.toml), -no-config (env vars only)
             -reset-store  DESTRUCTIVE: drop every inbox and start fresh —
                           honoured only on a declared Store Reset release
                           (ADR-0013); refused on any other release
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "pubkey":
			runPubkey(os.Args[2:])
			return
		case "address":
			runAddress(os.Args[2:])
			return
		case "genkey":
			runGenkey(os.Args[2:])
			return
		case "help", "-h", "--help":
			fmt.Print(usage)
			return
		}
	}
	serve(os.Args[1:])
}

// configFlags parses the shared -config/-no-config flags used by the relay
// and its subcommands. Stray positional arguments are a caller-error —
// usually the doubled Docker path shape — so they fail loudly (RELAY-12).
func configFlags(args []string) (configPath string) {
	fs := flag.NewFlagSet("trueseal-relay", flag.ExitOnError)
	cfgPath := fs.String("config", "relay.toml", "path to TOML config file")
	noConfig := fs.Bool("no-config", false, "skip config file loading; use env vars only (Docker-friendly)")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		log.Fatalf("unexpected argument %q — did you mean a subcommand?\n\n%s", fs.Arg(0), usage)
	}
	if *noConfig {
		return ""
	}
	return *cfgPath
}

// runPubkey prints the hex public key the Noise handshake uses — resolved
// through the same path as serve (TRUESEAL_RELAY_KEYPAIR_HEX, then the file).
func runPubkey(args []string) {
	cfg, err := config.Load(configFlags(args))
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	kp, _, err := keypair.Resolve(cfg.Relay.KeypairPath)
	if err != nil {
		log.Fatalf("keypair: %v", err)
	}
	fmt.Printf("%x\n", kp.Public)
}

// runAddress prints the Relay Address, trueseal://<pub hex>@host[:receivePort][?push=pushPort]
// (sync ADR-0028). The relay cannot know its public host, so --host is
// required. Ports always print explicitly — a port is never dropped silently.
func runAddress(args []string) {
	fs := flag.NewFlagSet("address", flag.ExitOnError)
	host := fs.String("host", "", "public host name or address clients dial (required)")
	cfgPath := fs.String("config", "relay.toml", "path to TOML config file")
	noConfig := fs.Bool("no-config", false, "skip config file loading; use env vars only (Docker-friendly)")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		log.Fatalf("unexpected argument %q\n\n%s", fs.Arg(0), usage)
	}
	if *host == "" {
		log.Fatalf("address: --host is required (the relay cannot know its public host)")
	}
	if err := validateHost(*host); err != nil {
		log.Fatalf("address: %v", err)
	}
	configPath := *cfgPath
	if *noConfig {
		configPath = ""
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	kp, _, err := keypair.Resolve(cfg.Relay.KeypairPath)
	if err != nil {
		log.Fatalf("keypair: %v", err)
	}
	receivePort, err := listenPort(cfg.Relay.ListenReceive)
	if err != nil {
		log.Fatalf("address: %v", err)
	}
	pushPort, err := listenPort(cfg.Relay.ListenPush)
	if err != nil {
		log.Fatalf("address: %v", err)
	}
	fmt.Printf("trueseal://%x@%s:%s?push=%s\n", kp.Public, *host, receivePort, pushPort)
}

// validateHost rejects hosts that would corrupt the ADR-0028 grammar.
// Follow-up for SYNC-15's Rust parser (recorded for BIBLE): this grammar
// deliberately does NOT validate port range (a numeric port is accepted
// as-is), leading zeros, or IPv6 literals (which would need brackets the
// grammar doesn't define) — the core parser decides those, and this check
// only guarantees the printed address parses back into its parts.
func validateHost(host string) error {
	if strings.ContainsAny(host, ":@?") {
		return fmt.Errorf("invalid --host %q: must not contain ':', '@' or '?' (it would corrupt the Relay Address grammar)", host)
	}
	return nil
}

// listenPort extracts the port from a listen address like ":7700" or
// "0.0.0.0:7700".
func listenPort(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("listen address %q has no port: %w", addr, err)
	}
	if port == "" || port == "0" {
		return "", fmt.Errorf("listen address %q has no fixed port to advertise", addr)
	}
	return port, nil
}

// runGenkey generates a keypair file and exits. Create-exclusive: it refuses
// to touch an existing file, so a stray genkey can never destroy the relay
// key (RELAY-12).
func runGenkey(args []string) {
	fs := flag.NewFlagSet("genkey", flag.ExitOnError)
	keyOut := fs.String("keyout", "keypair.hex", "keypair output path")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		log.Fatalf("unexpected argument %q\n\n%s", fs.Arg(0), usage)
	}
	if err := keypair.Generate(*keyOut); err != nil {
		log.Fatalf("genkey: %v", err)
	}
	kp, err := keypair.Load(*keyOut)
	if err != nil {
		log.Fatalf("genkey load: %v", err)
	}
	fmt.Printf("keypair written to %s\n", *keyOut)
	fmt.Printf("relay public key (share with clients): %x\n", kp.Public)
}

func serve(args []string) {
	// Privacy (ADR-0012): logs carry no client metadata — no IP addresses, no
	// keys or prefixes, no sizes, no Blob IDs, no per-session events. Only
	// lifecycle events (startup, listener addresses, the relay's own public
	// key, shutdown) and field-free errors. Dev mode (RELAY-9) never logs
	// addresses either.
	cfgPath := flag.String("config", "relay.toml", "path to TOML config file")
	// -no-config: skip TOML file loading entirely and use env vars only.
	// Use this in Docker deployments where all settings are passed as environment
	// variables and no config file is mounted into the container.
	noConfig := flag.Bool("no-config", false, "skip config file loading; use env vars only (Docker-friendly)")
	// -reset-store: the operator's opt-in for a declared relay Store Reset
	// (ADR-0013). On any other release it is refused before the store opens.
	resetStore := flag.Bool("reset-store", false, "DESTRUCTIVE: drop every inbox and start fresh (declared Store Reset releases only, ADR-0013)")
	flag.CommandLine.Parse(args)

	// A positional argument means the caller's intent was lost — the doubled
	// Docker entrypoint path turned '-genkey' into a silently-ignored arg and
	// started a relay instead (RELAY-12). Fail loudly with guidance.
	if flag.NArg() > 0 {
		log.Fatalf("unexpected argument %q — did you mean a subcommand?\n\n%s", flag.Arg(0), usage)
	}

	configPath := *cfgPath
	if *noConfig {
		configPath = ""
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// RELAY-12: first start generates the keypair (mode 0600) instead of
	// failing; an operator-supplied TRUESEAL_RELAY_KEYPAIR_HEX wins. The
	// private key material is never logged — the public key is a startup
	// lifecycle field ADR-0012 explicitly allows.
	kp, generated, err := keypair.Resolve(cfg.Relay.KeypairPath)
	if err != nil {
		log.Fatalf("keypair: %v", err)
	}
	if generated {
		log.Printf("no keypair found at %s — generated a new relay keypair (mode 0600)", cfg.Relay.KeypairPath)
	}
	log.Printf("relay public key: %x", kp.Public)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var inbox store.InboxStore
	var notifier interface {
		notify.Notifier
	}
	switch cfg.Store.Type {
	case "sqlite":
		// ADR-0013: versioned open. Fresh stores record Store Version 1;
		// older stores migrate forward in one transaction; unversioned
		// stores with today's schema are adopted; newer or unmigratable
		// stores are refused unchanged with one clear line naming both
		// versions (no client metadata). The reset flag is honoured only
		// on a declared Store Reset release.
		s, err := sqlitestore.NewWithOptions(cfg.Store.SQLitePath, sqlitestore.Options{
			ResetRequested: *resetStore,
			ResetDeclared:  sqlitestore.StoreResetDeclared(),
		})
		if err != nil {
			log.Fatalf("sqlite store: %v", err)
		}
		defer s.Close()
		if *resetStore {
			log.Printf("store reset: dropped every inbox at the operator's -reset-store (ADR-0013); keypair untouched")
		}
		log.Printf("inbox store: sqlite, store version %d", s.StoreVersion())
		inbox = s
		notifier = inprocess.New()
	case "postgres":
		// Experimental (ADR-0012); store versioning is scoped to the
		// supported SQLite backend (ADR-0013).
		s, err := pgstore.New(ctx, cfg.Store.PostgresDSN)
		if err != nil {
			log.Fatalf("postgres store: %v", err)
		}
		defer s.Close()
		inbox = s
		notifier = s
	default:
		log.Fatalf("unsupported store type: %s", cfg.Store.Type)
	}

	router := relay.NewRouter(inbox, notifier, cfg.Relay.TTL, cfg.Relay.MaxEnvelopeBytes)
	reaper := store.NewReaper(inbox, cfg.Relay.ReapInterval)

	// Start TTL reaper
	go reaper.Run(ctx)

	var wg sync.WaitGroup

	// Push listener — NK sessions
	pushLn, err := net.Listen("tcp", cfg.Relay.ListenPush)
	if err != nil {
		log.Fatalf("push listen %s: %v", cfg.Relay.ListenPush, err)
	}
	log.Printf("push listener on %s", cfg.Relay.ListenPush)

	// Receive listener — XX sessions
	receiveLn, err := net.Listen("tcp", cfg.Relay.ListenReceive)
	if err != nil {
		log.Fatalf("receive listen %s: %v", cfg.Relay.ListenReceive, err)
	}
	log.Printf("receive listener on %s", cfg.Relay.ListenReceive)

	go func() {
		<-ctx.Done()
		pushLn.Close()
		receiveLn.Close()
	}()

	max := cfg.Relay.MaxConnections
	pushSem := make(chan struct{}, max)
	recvSem := make(chan struct{}, max)
	var pushActive, recvActive atomic.Int64
	watermark := int64(max) * 75 / 100 // warn at 75% capacity

	go func() {
		for {
			conn, err := pushLn.Accept()
			if err != nil {
				return
			}
			select {
			case pushSem <- struct{}{}:
			default:
				logConnectionRefused("push", max)
				conn.Close()
				continue
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { <-pushSem }()
				n := pushActive.Add(1)
				defer pushActive.Add(-1)
				if n >= watermark {
					log.Printf("push: active connections at %d/%d (%.0f%% capacity) — consider investigating stalled connections (P2)", n, max, float64(n)/float64(max)*100)
				}
				start := time.Now()
				if err := session.AcceptPush(c, kp.DHKey, router); err != nil {
					// Field-free: session errors can wrap net.OpError, whose
					// string contains the peer address (ADR-0012).
					log.Printf("push session error")
				}
				if d := time.Since(start); d > 30*time.Second {
					log.Printf("push: long-lived session closed (no read deadline — see P2)")
				}
			}(conn)
		}
	}()

	go func() {
		for {
			conn, err := receiveLn.Accept()
			if err != nil {
				return
			}
			select {
			case recvSem <- struct{}{}:
			default:
				logConnectionRefused("recv", max)
				conn.Close()
				continue
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { <-recvSem }()
				n := recvActive.Add(1)
				defer recvActive.Add(-1)
				if n >= watermark {
					log.Printf("recv: active connections at %d/%d (%.0f%% capacity) — consider investigating stalled connections (P2)", n, max, float64(n)/float64(max)*100)
				}
				start := time.Now()
				if err := session.AcceptReceive(c, kp.DHKey, router); err != nil {
					// Field-free: session errors can wrap net.OpError, whose
					// string contains the peer address (ADR-0012).
					log.Printf("receive session error")
				}
				if d := time.Since(start); d > 24*time.Hour {
					log.Printf("recv: very long-lived session closed (no read deadline — see P2)")
				}
			}(conn)
		}
	}()

	log.Printf("trueseal-relay running (push=%s receive=%s health=%s)", cfg.Relay.ListenPush, cfg.Relay.ListenReceive, cfg.Relay.ListenHealth)

	// Health endpoint — GET /healthz returns 200 OK while the relay is running.
	// Useful for container health checks and load balancer probes.
	healthSrv := &http.Server{
		Addr: cfg.Relay.ListenHealth,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	go func() {
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health: server error: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = healthSrv.Shutdown(context.Background())
	}()
	<-ctx.Done()
	log.Printf("shutting down — draining active sessions (30s timeout)")
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		log.Printf("all sessions drained cleanly")
	case <-time.After(30 * time.Second):
		log.Printf("shutdown timeout — forcing exit")
	}
}
