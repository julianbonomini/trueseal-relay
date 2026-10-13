package sqlite

// RELAY-10: Ping is the store liveness read behind /healthz (ADR-0012).

import (
	"context"
	"testing"
)

func TestPing_HealthyStoreAnswers(t *testing.T) {
	s, err := New(t.TempDir() + "/inbox.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on a healthy store: %v", err)
	}
}

func TestPing_ClosedStoreFails(t *testing.T) {
	s, err := New(t.TempDir() + "/inbox.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("Ping on a closed store must fail — /healthz relies on it for the 503")
	}
}
