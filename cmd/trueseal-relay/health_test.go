package main

// RELAY-10 (#30): /healthz must check the store and report the Transport
// Version. The defect: the handler always answered 200, so a relay with a
// broken inbox.db looked healthy to orchestrators (ADR-0012 "Health and
// metrics").

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/session"
	"github.com/julianbonomini/trueseal-relay/internal/store"
	sqlitestore "github.com/julianbonomini/trueseal-relay/internal/store/sqlite"
)

// blockingStore is a store.InboxStore whose Ping never returns on its own —
// it waits for the probe's context to expire. Only Ping is exercised.
type blockingStore struct{ err error }

func (b *blockingStore) Ping(ctx context.Context) error {
	if b.err != nil {
		return b.err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingStore) Put(context.Context, []byte, []byte, time.Duration) error {
	panic("unused")
}
func (b *blockingStore) PutWithLimits(context.Context, []byte, []byte, time.Duration, store.InboxLimits) error {
	panic("unused")
}
func (b *blockingStore) Peek(context.Context, []byte) ([]store.InboxBlob, error) {
	panic("unused")
}
func (b *blockingStore) PeekAfter(context.Context, []byte, int64) ([]store.InboxBlob, error) {
	panic("unused")
}
func (b *blockingStore) DeleteForRecipient(context.Context, []byte, []int64) error {
	panic("unused")
}
func (b *blockingStore) Reap(context.Context) error { panic("unused") }

type healthBody struct {
	Status           string `json:"status"`
	DevMode          bool   `json:"dev_mode"`
	TransportVersion uint16 `json:"transport_version"`
}

func getHealth(t *testing.T, h http.Handler) (int, healthBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body healthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return rec.Code, body
}

func TestHealthz_HealthyStoreReportsStatusAndTransportVersion(t *testing.T) {
	s, err := sqlitestore.New(t.TempDir() + "/inbox.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	code, body := getHealth(t, healthHandler(s, false))
	if code != http.StatusOK {
		t.Fatalf("healthy store: status = %d, want 200", code)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.TransportVersion != session.TransportVersion {
		t.Errorf("transport_version = %d, want the handshake constant %d",
			body.TransportVersion, session.TransportVersion)
	}
	if body.DevMode {
		t.Errorf("dev_mode = true, want false")
	}
}

// The regression test for the defect: before RELAY-10, /healthz answered
// 200 while the store was closed.
func TestHealthz_BrokenStoreReturns503(t *testing.T) {
	s, err := sqlitestore.New(t.TempDir() + "/inbox.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	code, body := getHealth(t, healthHandler(s, false))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("closed store: status = %d, want 503", code)
	}
	if body.Status != "unavailable" {
		t.Errorf("status = %q, want unavailable", body.Status)
	}
	// The body stays additive and client-data-free: no error detail, no
	// internals — just the same fields with a failing status.
	if body.TransportVersion != session.TransportVersion {
		t.Errorf("transport_version = %d, want %d even when unavailable",
			body.TransportVersion, session.TransportVersion)
	}
}

func TestHealthz_ProbeTimeoutReturns503(t *testing.T) {
	st := &blockingStore{}
	start := time.Now()
	code, body := getHealth(t, healthHandler(st, false))
	elapsed := time.Since(start)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("blocked probe: status = %d, want 503", code)
	}
	if body.Status != "unavailable" {
		t.Errorf("status = %q, want unavailable", body.Status)
	}
	if elapsed > 2*healthProbeTimeout {
		t.Fatalf("blocked probe took %v — the handler must bound the probe at %v", elapsed, healthProbeTimeout)
	}
}

func TestHealthz_ProbeErrorReturns503(t *testing.T) {
	st := &blockingStore{err: errors.New("store: disk on fire")}
	code, body := getHealth(t, healthHandler(st, true))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("failing probe: status = %d, want 503", code)
	}
	if body.Status != "unavailable" {
		t.Errorf("status = %q, want unavailable", body.Status)
	}
	if !body.DevMode {
		t.Errorf("dev_mode = false, want true (dev flag reflected in both outcomes)")
	}
}
