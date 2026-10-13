package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/flynn/noise"
	"github.com/julianbonomini/trueseal-relay/internal/config"
	"github.com/julianbonomini/trueseal-relay/internal/logging"
	"github.com/julianbonomini/trueseal-relay/internal/session"
	"github.com/julianbonomini/trueseal-relay/internal/store"
)

// server owns the relay's listeners, accept loops, session bookkeeping and
// shutdown drain. It exists so the lifecycle that used to be wired inline
// in main() is constructible and testable (RELAY-11): tests build a server
// against a real store and loopback listeners and drive shutdown directly.
//
// Shutdown ordering follows ADR-0012 "Graceful shutdown":
//  1. stop the health endpoint (an orchestrator stops routing first);
//  2. stop accepting on every listener;
//  3. drain in-flight work up to the configured drain budget;
//  4. stop the reaper.
//
// The store is closed by the caller after shutdown returns — the ordering
// guarantee "the store closes only after every session has stopped" holds
// because shutdown does not return until session bookkeeping is done (or
// the drain budget has been force-exited past).
type server struct {
	log          *logging.Logger
	handler      session.Handler
	relayKey     noise.DHKey
	deadlines    session.Deadlines
	drain        time.Duration
	inbox        store.InboxStore
	reaper       *store.Reaper
	devMode      bool
	listenPush   string
	listenRecv   string
	listenHealth string

	pushSem chan struct{} // nil = unlimited (max_connections 0)
	recvSem chan struct{}

	pushLn, recvLn net.Listener
	healthSrv      *http.Server
	reaperCancel   context.CancelFunc

	acceptWG sync.WaitGroup // accept loops
	sessWG   sync.WaitGroup // session goroutines
	errLog   *sessionErrorLog
}

func newServer(cfg config.RelayConfig, relayKey noise.DHKey, inbox store.InboxStore, handler session.Handler, reaper *store.Reaper, logg *logging.Logger) *server {
	if logg == nil {
		logg = logging.Nop()
	}
	s := &server{
		log:      logg,
		handler:  handler,
		relayKey: relayKey,
		deadlines: session.Deadlines{
			Handshake:   cfg.HandshakeDeadline,
			PushSession: cfg.PushSessionDeadline,
			ReceiveIdle: cfg.ReceiveIdleDeadline,
			Write:       cfg.WriteDeadline,
		},
		drain:        cfg.ShutdownDrain,
		inbox:        inbox,
		reaper:       reaper,
		devMode:      cfg.Dev,
		listenPush:   cfg.ListenPush,
		listenRecv:   cfg.ListenReceive,
		listenHealth: cfg.ListenHealth,
		errLog:       newSessionErrorLog(time.Now),
	}
	// max_connections 0 keeps its documented "no limit" meaning (config);
	// a nil semaphore acquires unconditionally.
	if cfg.MaxConnections > 0 {
		s.pushSem = make(chan struct{}, cfg.MaxConnections)
		s.recvSem = make(chan struct{}, cfg.MaxConnections)
	}
	return s
}

// start opens the listeners and launches the accept loops, the health
// endpoint and the TTL reaper.
func (s *server) start() error {
	var err error
	s.pushLn, err = net.Listen("tcp", s.listenPush)
	if err != nil {
		return err
	}
	log.Printf("push listener on %s", s.pushLn.Addr())
	s.recvLn, err = net.Listen("tcp", s.listenRecv)
	if err != nil {
		s.pushLn.Close()
		return err
	}
	log.Printf("receive listener on %s", s.recvLn.Addr())

	s.healthSrv = &http.Server{
		Addr:         s.listenHealth,
		Handler:      healthHandler(s.inbox, s.devMode),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	go func() {
		if err := s.healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health: server error: %v", err)
		}
	}()

	reaperCtx, reaperCancel := context.WithCancel(context.Background())
	s.reaperCancel = reaperCancel
	go s.reaper.Run(reaperCtx)

	s.acceptWG.Add(2)
	go s.acceptLoop(s.pushLn, "push", s.pushSem, session.AcceptPushWithDeadlines)
	go s.acceptLoop(s.recvLn, "receive", s.recvSem, session.AcceptReceiveWithDeadlines)
	return nil
}

// acceptFn is one session-kind entry point (push or receive).
type acceptFn func(net.Conn, noise.DHKey, session.Handler, session.Deadlines) error

// acceptLoop accepts until the listener is closed, then returns. Each
// accepted connection is capped by the listener's semaphore (a nil
// semaphore never refuses) and served on its own goroutine tracked by
// sessWG. ADR-0012: a refused connection is closed silently — counted,
// surfaced only in dev mode (RELAY-9); a session error is rate-limited and
// field-free (it can wrap a peer address).
func (s *server) acceptLoop(ln net.Listener, kind string, sem chan struct{}, accept acceptFn) {
	defer s.acceptWG.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if sem != nil {
			select {
			case sem <- struct{}{}:
			default:
				n := refusalCounters.push.Add(1)
				s.log.Debugf("%s: connection limit reached — connection refused (total %d)", kind, n)
				conn.Close()
				continue
			}
		}
		s.sessWG.Add(1)
		go func(c net.Conn) {
			defer s.sessWG.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			if err := accept(c, s.relayKey, s.handler, s.deadlines); err != nil {
				s.errLog.logf(kind, err)
			}
		}(conn)
	}
}

// shutdown drains the server. It does not return until every session
// goroutine has finished or the drain budget has been exceeded.
func (s *server) shutdown() {
	// 1. Stop the health endpoint first: an orchestrator (the Docker
	// HEALTHCHECK, a load balancer) must see the Node leave service before
	// sessions are disturbed.
	healthCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.healthSrv.Shutdown(healthCtx)
	cancel()

	// 2. Stop accepting.
	s.pushLn.Close()
	s.recvLn.Close()
	s.acceptWG.Wait()

	// 3. Drain in-flight sessions, bounded by the configured budget.
	log.Printf("shutting down — draining active sessions (%s timeout)", s.drain)
	done := make(chan struct{})
	go func() { s.sessWG.Wait(); close(done) }()
	select {
	case <-done:
		log.Printf("all sessions drained cleanly")
	case <-time.After(s.drain):
		log.Printf("shutdown timeout — forcing exit")
	}

	// 4. Stop the reaper. The caller closes the store after shutdown
	// returns.
	s.reaperCancel()
}
