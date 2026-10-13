//go:build hostile

package hostile

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

// The control surface (RELAY-14; e2e ADR-0001): a loopback-only TCP
// listener speaking newline-delimited JSON, one command per line, one
// response per line. It exists so the e2e harness drives hostile behaviour
// mid-scenario without restarting the relay.
//
// Commands (device is a 64-char hex device key, or "*" for every device):
//
//	{"cmd":"set","device":"*","drop":true,"corrupt":true,"duplicate":true,"reorder":2}
//	{"cmd":"clear","device":"*"}
//	{"cmd":"inject","device":"<hex>","envelope":"<hex>"}
//	{"cmd":"replay","device":"<hex>","count":1}      count 0 = whole history
//
// Responses: {"ok":true} or {"ok":false,"error":"..."}.
//
// The listener refuses any non-loopback bind address: a hostile control
// surface reachable off the host would be a security hole, build tag or
// not. Logs carry no peer addresses in any mode (ADR-0012).

// command is one decoded control line.
type command struct {
	Cmd       string `json:"cmd"`
	Device    string `json:"device"`
	Drop      bool   `json:"drop"`
	Corrupt   bool   `json:"corrupt"`
	Duplicate bool   `json:"duplicate"`
	Reorder   int    `json:"reorder"`
	Envelope  string `json:"envelope"`
	Count     int    `json:"count"`
}

// response is one control reply line.
type response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ListenControl starts the control listener on addr, which MUST be
// loopback (127.0.0.1, ::1 or localhost). It returns the bound address
// (the port may be 0 for tests) or an error; a non-loopback bind is
// refused outright.
func (e *Engine) ListenControl(addr string) (net.Addr, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("hostile control address %q: %w", addr, err)
	}
	if err := requireLoopback(host); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("hostile control listen: %w", err)
	}
	e.logg.Printf("hostile: control listener on %s (loopback only; this build is a test tool — never publish it)", ln.Addr())
	go e.serveControl(ln)
	return ln.Addr(), nil
}

// requireLoopback refuses any host that does not resolve to a loopback
// address.
func requireLoopback(host string) error {
	if host == "" {
		return fmt.Errorf("hostile control listener must bind to a loopback address (127.0.0.1 or ::1), not the wildcard")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("hostile control listener: cannot resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("hostile control listener must be loopback-only — %q resolves to a non-loopback address", host)
		}
	}
	return nil
}

// serveControl accepts control connections until Close.
func (e *Engine) serveControl(ln net.Listener) {
	go func() {
		<-e.done
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (Close) — ordinary teardown
		}
		go e.handleControlConn(conn)
	}
}

// Close shuts the control listener. Idempotent.
func (e *Engine) Close() {
	e.closeOnce.Do(func() { close(e.done) })
}

// handleControlConn serves one control connection: JSON lines in,
// JSON lines out, until EOF. The peer address is never logged.
func (e *Engine) handleControlConn(conn net.Conn) {
	defer conn.Close()
	e.logg.Debugf("hostile: control connection accepted")
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024) // injected envelopes can be large
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c command
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			_ = enc.Encode(response{OK: false, Error: "bad JSON: " + err.Error()})
			continue
		}
		_ = enc.Encode(e.apply(c))
	}
}

// apply executes one command and builds its response.
func (e *Engine) apply(c command) response {
	switch c.Cmd {
	case "set":
		err := e.SetRules(c.Device, Rules{
			Drop:          c.Drop,
			Corrupt:       c.Corrupt,
			Duplicate:     c.Duplicate,
			ReorderWindow: c.Reorder,
		})
		if err != nil {
			return response{OK: false, Error: err.Error()}
		}
		return response{OK: true}
	case "clear":
		e.ClearRules(c.Device)
		return response{OK: true}
	case "inject":
		key, err := parseDeviceKey(c.Device)
		if err != nil {
			return response{OK: false, Error: err.Error()}
		}
		env, err := hex.DecodeString(c.Envelope)
		if err != nil {
			return response{OK: false, Error: "envelope must be hex: " + err.Error()}
		}
		if len(env) == 0 {
			return response{OK: false, Error: "envelope must not be empty"}
		}
		if err := e.Inject(key, env); err != nil {
			return response{OK: false, Error: err.Error()}
		}
		return response{OK: true}
	case "replay":
		key, err := parseDeviceKey(c.Device)
		if err != nil {
			return response{OK: false, Error: err.Error()}
		}
		if err := e.Replay(key, c.Count); err != nil {
			return response{OK: false, Error: err.Error()}
		}
		return response{OK: true}
	default:
		return response{OK: false, Error: fmt.Sprintf("unknown cmd %q (want set|clear|inject|replay)", c.Cmd)}
	}
}

// parseDeviceKey decodes a 64-char hex device key. The wildcard is not
// valid here: inject and replay target exactly one device's sessions.
func parseDeviceKey(s string) (relay.RecipientKey, error) {
	var key relay.RecipientKey
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return key, fmt.Errorf("device must be a 64-char hex device key for this command")
	}
	copy(key[:], raw)
	return key, nil
}
