package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/julianbonomini/trueseal-relay/internal/logging"
	"github.com/julianbonomini/trueseal-relay/internal/notify"
	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
	"github.com/julianbonomini/trueseal-relay/internal/session"
	"github.com/julianbonomini/trueseal-relay/internal/store"
	pgstore "github.com/julianbonomini/trueseal-relay/internal/store/postgres"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

// refusalCounters counts connection-limit refusals per listener (RELAY-6).
// ADR-0012: no refusal is logged in normal mode — a port scan must not be
// able to flood the operator's logs, and there is no client metadata to
// log anyway. The counters exist so RELAY-9's dev mode and any future
// aggregate metrics can surface them without touching the accept loops.
var refusalCounters struct {
	push, recv atomic.Int64
}

// sessionErrorLog rate-limits per-connection session error lines (RELAY-6).
// Normal mode may log errors (ADR-0012), but a scanner sending garbage at
// the listener must not produce one line per connection: at most one line
// per listener kind per minute is emitted, carrying the suppressed count.
type sessionErrorLog struct {
	mu         sync.Mutex
	last       map[string]time.Time
	suppressed map[string]int
	now        func() time.Time
}

func newSessionErrorLog(now func() time.Time) *sessionErrorLog {
	return &sessionErrorLog{last: map[string]time.Time{}, suppressed: map[string]int{}, now: now}
}

const sessionErrorLogInterval = time.Minute

// logf emits "kind session error (class)" at most once per interval per
// kind. Later lines in a window report how many were suppressed since the
// last emitted line. Field-free: the underlying error is never printed —
// it can wrap net.OpError, whose string contains the peer address
// (ADR-0012). RELAY-9: the one-word CLASS recovers operator signal without
// fields — timeout / reset / closed / internal — derived by errors.As/Is
// inspection, never by formatting the error.
func (l *sessionErrorLog) logf(kind string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	last, seen := l.last[kind]
	l.suppressed[kind]++
	if seen && now.Sub(last) < sessionErrorLogInterval {
		return
	}
	n := l.suppressed[kind]
	l.suppressed[kind] = 0
	l.last[kind] = now
	class := classifySessionErr(err)
	if n > 1 {
		log.Printf("%s session error (%s, %d since last report)", kind, class, n)
	} else {
		log.Printf("%s session error (%s)", kind, class)
	}
}

// classifySessionErr reduces a session error to one field-free word
// (RELAY-9; the C3 review's classified-error follow-up). It INSPECTS the
// error chain but never formats it: net.OpError's string carries the peer
// address, which must never reach a log line in any mode.
func classifySessionErr(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return "reset"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return "closed"
	}
	return "internal"
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
	// -dev: dev logging mode (RELAY-9, ADR-0012). Verbose per-message
	// detail — key prefixes, sizes, Blob IDs, session events — but NEVER
	// payload bytes or peer addresses, in any mode.
	devFlag := flag.Bool("dev", false, "dev logging mode: verbose per-message logs (never IP addresses); also TRUESEAL_RELAY_DEV=1")
	// RELAY-14: -hostile-control exists ONLY in `hostile` tag builds; in a
	// release build no such flag is defined and this returns nil.
	hostileControl := registerHostileFlag()
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
	if *devFlag {
		cfg.Relay.Dev = true
	}

	// RELAY-9 (ADR-0012 "Logging"): one logging seam for the session,
	// relay and store packages. Normal mode: lifecycle + field-free errors
	// only. Dev mode adds per-message detail — and still never logs a
	// peer address or payload bytes, in any mode.
	relayLog := logging.New(os.Stderr, cfg.Relay.Dev)
	session.SetLogger(relayLog)
	if cfg.Relay.Dev {
		log.Printf("*** DEV MODE ENABLED — verbose logging (key prefixes, sizes, blob IDs, session events) ***")
		log.Printf("*** dev mode never logs IP addresses or payload bytes; do not run it against real users ***")
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

	// RELAY-6 (ADR-0012): the abuse limits and liveness deadlines, all
	// operator-configurable (short values for e2e). Every limit is keyed on
	// the recipient Inbox, the Device key or a global count — never on a
	// peer address, which this process never reads, stores or logs.
	router := relay.NewRouterWithLimits(inbox, notifier, cfg.Relay.TTL, cfg.Relay.MaxEnvelopeBytes, relay.Limits{
		MaxBlobsPerInbox:            cfg.Relay.MaxBlobsPerInbox,
		MaxBytesPerInbox:            cfg.Relay.MaxBytesPerInbox,
		PushRatePerSecond:           cfg.Relay.PushRatePerSecond,
		PushRateBurst:               float64(cfg.Relay.PushRateBurst),
		MaxReceiveSessionsPerDevice: cfg.Relay.MaxReceiveSessionsPerDevice,
	})
	router.SetLogger(relayLog)
	// RELAY-14: in `hostile` tag builds, install the injection engine and
	// start its loopback-only control listener. A no-op in release builds.
	stopHostile := maybeStartHostile(router, relayLog, hostileControl)
	defer stopHostile()
	reaper := store.NewReaper(inbox, cfg.Relay.ReapInterval)
	reaper.SetLogger(relayLog)

	// RELAY-11: the listener, accept and drain lifecycle lives in a
	// testable server type; main only wires dependencies and waits for the
	// signal.
	srv := newServer(cfg.Relay, kp.DHKey, inbox, router, reaper, relayLog)
	if err := srv.start(); err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("trueseal-relay running (push=%s receive=%s health=%s)", cfg.Relay.ListenPush, cfg.Relay.ListenReceive, cfg.Relay.ListenHealth)

	<-ctx.Done()
	srv.shutdown()
}

// healthProbeTimeout bounds the store probe in /healthz (RELAY-10): a wedged
// store must fail the health check fast, not hang the probe until the HTTP
// server timeout. Well under the Docker HEALTHCHECK interval.
const healthProbeTimeout = 2 * time.Second

// healthHandler answers GET /healthz (ADR-0012 "Health and metrics"). It
// runs one cheap store read, bounded by healthProbeTimeout: 200 when the
// store answers, 503 when it errors or stalls. The body is the same small
// additive JSON in both outcomes — status, dev_mode and the Transport
// Version from the same constant the handshake prefix uses (RELAY-5;
// sync ADR-0022). It carries no client data and no error internals: the
// probe result is a binary healthy/unavailable, nothing more.
func healthHandler(inbox store.InboxStore, devMode bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCtx, cancel := context.WithTimeout(r.Context(), healthProbeTimeout)
		defer cancel()
		status, code := "ok", http.StatusOK
		if err := inbox.Ping(probeCtx); err != nil {
			status, code = "unavailable", http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, "{\"status\":%q,\"dev_mode\":%t,\"transport_version\":%d}\n",
			status, devMode, session.TransportVersion)
	})
}
