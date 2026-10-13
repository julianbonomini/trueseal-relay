//go:build hostile

// Package hostile implements the Hostile Relay injection hooks (RELAY-14;
// e2e ADR-0001 gate G3): the real relay, plus a loopback-only control
// surface that makes its delivery path misbehave on demand — drop,
// duplicate, reorder, replay, inject and corrupt — so the e2e harness can
// prove every SDK rejects hostile relay input with the typed error or
// delivery issue, and never acks or applies it.
//
// The whole package compiles ONLY under the `hostile` build tag. Nothing
// in a normal build imports it, so a released binary carries no hook code
// and no control listener (CI proves this with go tool nm, and the
// -hostile-control flag simply does not exist without the tag). The
// hostile binary is a test tool: it is never published as a release
// artifact or image.
//
// Logging follows ADR-0012 exactly: lifecycle lines and field-free errors
// in normal mode, device-key prefixes (never full keys) in dev mode, and
// never a peer address or payload bytes in any mode.
package hostile

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/julianbonomini/trueseal-relay/internal/logging"
	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

// Rules are the per-Device hostile behaviours, applied to each blob as it
// flows to that Device's Receive Session, in this fixed order: drop →
// corrupt → duplicate → reorder.
type Rules struct {
	// Drop withholds blobs: the client never sees them. They stay in the
	// Inbox un-acked, so clearing the rule and reconnecting re-delivers
	// them (peek-and-ack survives hostility).
	Drop bool
	// Corrupt flips one byte in the middle of the envelope before it is
	// framed. The blob ID is unchanged; the client's signature/AEAD
	// verification must fail.
	Corrupt bool
	// Duplicate emits every blob twice, back to back, with the same blob
	// ID and identical bytes. The client's dedup must absorb the repeat.
	Duplicate bool
	// ReorderWindow, when > 1, holds blobs until Window have accumulated
	// and then emits them in reverse arrival order (window 2 swaps each
	// adjacent pair). Held blobs flush in reverse order when the session
	// ends. 0 or 1 disables reordering.
	ReorderWindow int
}

// deviceSelector keys rules: a full lowercase hex device key, or "*" for
// every device without an exact entry.
const wildcardSelector = "*"

// syntheticBlobIDBase starts the blob-ID range the engine assigns to
// injected blobs. Store IDs are SQLite rowids counting from 1, so nothing
// real ever collides with this range. A client DeliverAck naming a
// synthetic ID is a silent store no-op (RELAY-1: DeleteForRecipient
// ignores unknown IDs), so injections never corrupt the Inbox.
const syntheticBlobIDBase = int64(1) << 62

// historyCap bounds each session's delivery history (the replay source).
// The hostile relay is a test tool; 256 delivered blobs per session is far
// beyond any scenario, and the bound keeps a pathological run from growing
// memory without limit.
const historyCap = 256

// Engine is the hostile-behaviour controller. It implements
// relay.DeliveryWrapper: each Receive Session's blob channel is wrapped in
// a pipe that applies the session's Rules and accepts inject/replay
// commands. Safe for concurrent use.
type Engine struct {
	logg *logging.Logger

	mu       sync.Mutex
	rules    map[string]Rules
	sessions map[relay.RecipientKey]map[*pipe]struct{}

	synthID atomic.Int64

	// control listener (control.go); closed by Close.
	closeOnce sync.Once
	done      chan struct{}
}

// NewEngine returns an Engine with no rules (delivery passes through
// unchanged until the control surface installs some).
func NewEngine(l *logging.Logger) *Engine {
	if l == nil {
		l = logging.Nop()
	}
	return &Engine{
		logg:     l,
		rules:    map[string]Rules{},
		sessions: map[relay.RecipientKey]map[*pipe]struct{}{},
		done:     make(chan struct{}),
	}
}

// SetRules replaces the rules for a selector (hex device key or "*").
// The zero Rules value restores honest delivery for that selector.
func (e *Engine) SetRules(selector string, r Rules) error {
	if selector != wildcardSelector {
		if _, err := hex.DecodeString(selector); err != nil || len(selector) != 64 {
			return fmt.Errorf("device selector must be %q or a 64-char hex device key", wildcardSelector)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules[selector] = r
	e.logg.Debugf("hostile: rules set selector=%s drop=%t corrupt=%t duplicate=%t reorder=%d",
		selectorLabel(selector), r.Drop, r.Corrupt, r.Duplicate, r.ReorderWindow)
	return nil
}

// ClearRules removes the rules for a selector (honest delivery resumes).
func (e *Engine) ClearRules(selector string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.rules, selector)
	e.logg.Debugf("hostile: rules cleared selector=%s", selectorLabel(selector))
}

// rulesFor returns the rules for a device key: an exact hex entry wins,
// else the wildcard, else honest delivery.
func (e *Engine) rulesFor(key relay.RecipientKey) Rules {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r, ok := e.rules[hex.EncodeToString(key[:])]; ok {
		return r
	}
	return e.rules[wildcardSelector]
}

// selectorLabel shortens a hex device key to its 4-byte prefix for dev-mode
// logs (ADR-0012: a prefix, never the full key).
func selectorLabel(selector string) string {
	if len(selector) > 8 {
		return selector[:8]
	}
	return selector
}

// WrapSession implements relay.DeliveryWrapper (RELAY-14). It registers a
// pipe for the session and returns the channel the session layer reads.
func (e *Engine) WrapSession(ctx context.Context, key relay.RecipientKey, in <-chan relay.DeliveryBlob) <-chan relay.DeliveryBlob {
	p := &pipe{
		key:       key,
		out:       make(chan relay.DeliveryBlob, 256),
		injectCh:  make(chan relay.DeliveryBlob, 16),
		replayCh:  make(chan int, 4),
		replayAll: make(chan struct{}, 1),
	}
	e.mu.Lock()
	if e.sessions[key] == nil {
		e.sessions[key] = map[*pipe]struct{}{}
	}
	e.sessions[key][p] = struct{}{}
	e.mu.Unlock()
	e.logg.Debugf("hostile: session wrapped key=%x", key[:4])

	go p.run(ctx, e, in)
	return p.out
}

// Inject pushes a blob carrying envelope (harness-supplied bytes — the
// relay cannot sign, so injected Manifests or Syncs come from the Adversary
// Client) into every live Receive Session of the device, under a synthetic
// blob ID. An error is returned if the device has no live session — the
// harness connects first, then injects.
func (e *Engine) Inject(key relay.RecipientKey, envelope []byte) error {
	blob := relay.DeliveryBlob{
		BlobID:   syntheticBlobIDBase + e.synthID.Add(1),
		Envelope: envelope,
	}
	e.mu.Lock()
	pipes := e.sessions[key]
	var targets []*pipe
	for p := range pipes {
		targets = append(targets, p)
	}
	e.mu.Unlock()
	if len(targets) == 0 {
		return fmt.Errorf("no live Receive Session for device %x", key[:4])
	}
	for _, p := range targets {
		select {
		case p.injectCh <- blob:
		default:
			return fmt.Errorf("inject queue full for device %x", key[:4])
		}
	}
	e.logg.Debugf("hostile: inject key=%x envelope=%d bytes", key[:4], len(envelope))
	return nil
}

// Replay re-delivers the last count delivered blobs (original blob IDs and
// bytes) on every live Receive Session of the device. count <= 0 replays
// the whole recorded history. The client's dedup must absorb the repeats;
// its DeliverAck of an already-deleted store blob is a silent no-op.
func (e *Engine) Replay(key relay.RecipientKey, count int) error {
	e.mu.Lock()
	pipes := e.sessions[key]
	var targets []*pipe
	for p := range pipes {
		targets = append(targets, p)
	}
	e.mu.Unlock()
	if len(targets) == 0 {
		return fmt.Errorf("no live Receive Session for device %x", key[:4])
	}
	for _, p := range targets {
		if count <= 0 {
			select {
			case p.replayAll <- struct{}{}:
			default:
			}
			continue
		}
		select {
		case p.replayCh <- count:
		default:
			return fmt.Errorf("replay queue full for device %x", key[:4])
		}
	}
	e.logg.Debugf("hostile: replay key=%x count=%d", key[:4], count)
	return nil
}

// pipe is one wrapped Receive Session: it applies the device's Rules to
// every blob from the routing loop and interleaves injected and replayed
// blobs, preserving the routing loop's close as the session teardown
// signal.
type pipe struct {
	key relay.RecipientKey

	out       chan relay.DeliveryBlob
	injectCh  chan relay.DeliveryBlob
	replayCh  chan int
	replayAll chan struct{}

	// run-loop state (single goroutine, no locking):
	rules   Rules
	held    []relay.DeliveryBlob // reorder window
	history []relay.DeliveryBlob // replay source
}

// emit sends one blob to the session, honouring the reorder window, and
// records it in the replay history. Returns false when ctx is done.
func (p *pipe) emit(ctx context.Context, b relay.DeliveryBlob) bool {
	if p.rules.Drop {
		return true // withheld — stays in the Inbox, un-acked
	}
	if p.rules.Corrupt {
		b = corruptCopy(b)
	}
	out := []relay.DeliveryBlob{b}
	if p.rules.Duplicate {
		out = append(out, duplicateCopy(b))
	}
	for _, ob := range out {
		if p.rules.ReorderWindow > 1 {
			p.held = append(p.held, ob)
			if len(p.held) < p.rules.ReorderWindow {
				continue
			}
			// Window full: emit in reverse arrival order.
			for i := len(p.held) - 1; i >= 0; i-- {
				if !p.send(ctx, p.held[i]) {
					return false
				}
			}
			p.held = p.held[:0]
			continue
		}
		if !p.send(ctx, ob) {
			return false
		}
	}
	return true
}

// send writes one blob to the session channel and records it in history.
func (p *pipe) send(ctx context.Context, b relay.DeliveryBlob) bool {
	select {
	case p.out <- b:
		p.history = append(p.history, b)
		if len(p.history) > historyCap {
			p.history = p.history[len(p.history)-historyCap:]
		}
		return true
	case <-ctx.Done():
		return false
	}
}

// run is the pipe's single goroutine: routing-loop blobs, injected blobs
// and replay commands are serialized here, so per-session state needs no
// locks.
func (p *pipe) run(ctx context.Context, e *Engine, in <-chan relay.DeliveryBlob) {
	defer func() {
		e.mu.Lock()
		delete(e.sessions[p.key], p)
		if len(e.sessions[p.key]) == 0 {
			delete(e.sessions, p.key)
		}
		e.mu.Unlock()
		close(p.out) // propagate the session teardown signal (router contract)
		e.logg.Debugf("hostile: session unwrapped key=%x", p.key[:4])
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-in:
			if !ok {
				// Routing loop ended: flush the reorder window (reverse
				// arrival order — a held blob must not be silently lost).
				for i := len(p.held) - 1; i >= 0; i-- {
					p.send(ctx, p.held[i])
				}
				return
			}
			p.rules = e.rulesFor(p.key)
			if !p.emit(ctx, b) {
				return
			}
		case b := <-p.injectCh:
			// Injected blobs bypass drop/reorder (the harness asked for
			// this exact blob now) but are recorded for replay.
			if !p.send(ctx, b) {
				return
			}
		case count := <-p.replayCh:
			if !p.replay(ctx, count) {
				return
			}
		case <-p.replayAll:
			if !p.replay(ctx, 0) {
				return
			}
		}
	}
}

// replay re-sends the last count history entries (all when count <= 0),
// oldest first, with their original blob IDs and bytes.
func (p *pipe) replay(ctx context.Context, count int) bool {
	start := 0
	if count > 0 && len(p.history) > count {
		start = len(p.history) - count
	}
	for _, b := range p.history[start:] {
		select {
		case p.out <- b:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// corruptCopy returns a copy of the blob with one byte flipped in the
// middle of the envelope. The copy keeps the store's bytes immutable.
func corruptCopy(b relay.DeliveryBlob) relay.DeliveryBlob {
	env := make([]byte, len(b.Envelope))
	copy(env, b.Envelope)
	if len(env) > 0 {
		env[len(env)/2] ^= 0xFF
	}
	return relay.DeliveryBlob{BlobID: b.BlobID, Envelope: env}
}

// duplicateCopy returns a byte-for-byte copy of the blob (same blob ID) so
// the duplicate owns its backing array.
func duplicateCopy(b relay.DeliveryBlob) relay.DeliveryBlob {
	env := make([]byte, len(b.Envelope))
	copy(env, b.Envelope)
	return relay.DeliveryBlob{BlobID: b.BlobID, Envelope: env}
}
