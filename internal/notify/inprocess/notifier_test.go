package inprocess_test

import (
	"context"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/notify/inprocess"
)

func key(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

// Notify with no subscribers → no error.
func TestNotifier_NotifyNoSubscribers(t *testing.T) {
	n := inprocess.New()
	if err := n.Notify(key(0xBB)); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

// Multiple subscribers for same key → all notified.
func TestNotifier_MultipleSubscribers(t *testing.T) {
	n := inprocess.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch1, _ := n.Subscribe(ctx, key(0xCC))
	ch2, _ := n.Subscribe(ctx, key(0xCC))

	n.Notify(key(0xCC)) //nolint:errcheck

	for _, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Error("want signal on both subscribers, timed out")
		}
	}
}

// ctx cancel → channel closed.
func TestNotifier_CtxCancelClosesChannel(t *testing.T) {
	n := inprocess.New()
	ctx, cancel := context.WithCancel(context.Background())

	ch, _ := n.Subscribe(ctx, key(0xDD))
	cancel()

	select {
		case _, ok := <-ch:
			if ok {
				t.Error("want channel closed, got value")
			}
		case <-time.After(time.Second):
			t.Error("want channel closed within 1s")
	}
}

// Subscribe then Notify → signal received on channel.
func TestNotifier_NotifySignalsSubscriber(t *testing.T) {
	n := inprocess.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := n.Subscribe(ctx, key(0xAA))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := n.Notify(key(0xAA)); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Error("want signal within 1s, got none")
	}
}
