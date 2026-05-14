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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/julianbonomini/hush-relay/internal/config"
	"github.com/julianbonomini/hush-relay/internal/keypair"
	"github.com/julianbonomini/hush-relay/internal/notify/inprocess"
	"github.com/julianbonomini/hush-relay/internal/relay"
	"github.com/julianbonomini/hush-relay/internal/session"
	sqlitestore "github.com/julianbonomini/hush-relay/internal/store/sqlite"
	"github.com/julianbonomini/hush-relay/internal/store"
)

func main() {
	// Privacy: log only public keys and connection events — never envelope content.
	cfgPath := flag.String("config", "relay.toml", "path to TOML config file")
	// -no-config: skip TOML file loading entirely and use env vars only.
	// Use this in Docker deployments where all settings are passed as environment
	// variables and no config file is mounted into the container.
	noConfig := flag.Bool("no-config", false, "skip config file loading; use env vars only (Docker-friendly)")
	genKey := flag.Bool("genkey", false, "generate a new relay keypair and exit")
	keyOut := flag.String("keyout", "keypair.hex", "keypair output path (used with -genkey)")
	flag.Parse()

	if *genKey {
		if err := keypair.Generate(*keyOut); err != nil {
			log.Fatalf("genkey: %v", err)
		}
		kp, err := keypair.Load(*keyOut)
		if err != nil {
			log.Fatalf("genkey load: %v", err)
		}
		fmt.Printf("keypair written to %s\n", *keyOut)
		fmt.Printf("relay public key (share with clients): %x\n", kp.Public)
		os.Exit(0)
	}

	configPath := *cfgPath
	if *noConfig {
		configPath = ""
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	kp, err := keypair.Load(cfg.Relay.KeypairPath)
	if err != nil {
		log.Fatalf("keypair: %v", err)
	}
	log.Printf("relay public key: %x", kp.Public)

	var inbox store.InboxStore
	switch cfg.Store.Type {
	case "sqlite":
		s, err := sqlitestore.New(cfg.Store.SQLitePath)
		if err != nil {
			log.Fatalf("sqlite store: %v", err)
		}
		defer s.Close()
		inbox = s
	default:
		log.Fatalf("unsupported store type: %s", cfg.Store.Type)
	}

	notifier := inprocess.New()
	router := relay.NewRouter(inbox, notifier, cfg.Relay.TTL, cfg.Relay.MaxEnvelopeBytes)
	reaper := store.NewReaper(inbox, cfg.Relay.ReapInterval)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
				log.Printf("push: connection limit reached (%d) — rejecting %s", max, conn.RemoteAddr())
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
					log.Printf("push session: %v", err)
				}
				if d := time.Since(start); d > 30*time.Second {
					log.Printf("push: long-lived session closed  addr=%s  duration=%s  (no read deadline — see P2)", c.RemoteAddr(), d.Round(time.Second))
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
				log.Printf("recv: connection limit reached (%d) — rejecting %s", max, conn.RemoteAddr())
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
					log.Printf("receive session: %v", err)
				}
				if d := time.Since(start); d > 24*time.Hour {
					log.Printf("recv: very long-lived session closed  duration=%s  (no read deadline — see P2)", d.Round(time.Second))
				}
			}(conn)
		}
	}()

	log.Printf("hush-relay running (push=%s receive=%s health=%s)", cfg.Relay.ListenPush, cfg.Relay.ListenReceive, cfg.Relay.ListenHealth)

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
