package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
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
	router := relay.NewRouter(inbox, notifier, cfg.Relay.TTL)
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

	go func() {
		for {
			conn, err := pushLn.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				if err := session.AcceptPush(c, kp.DHKey, router); err != nil {
					log.Printf("push session: %v", err)
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
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				if err := session.AcceptReceive(c, kp.DHKey, router); err != nil {
					log.Printf("receive session: %v", err)
				}
			}(conn)
		}
	}()

	log.Printf("hush-relay running (push=%s receive=%s)", cfg.Relay.ListenPush, cfg.Relay.ListenReceive)
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
