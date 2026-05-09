package session_test

import (
	"testing"

	"github.com/julianbonomini/hush-relay/internal/session"
)

// All four MsgTypes round-trip cleanly.
func TestFrame_AllTypes(t *testing.T) {
	for _, tt := range []session.MsgType{
		session.MsgTypePush,
		session.MsgTypeDeliver,
		session.MsgTypeHeartbeat,
		session.MsgTypeAck,
	} {
		raw := session.Frame(tt, []byte("x"))
		got, _, ok := session.Parse(raw)
		if !ok || got != tt {
			t.Errorf("type %02x: round-trip failed", tt)
		}
	}
}

// Empty body is valid.
func TestFrame_EmptyBody(t *testing.T) {
	raw := session.Frame(session.MsgTypeHeartbeat, []byte{})
	_, body, ok := session.Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false for empty body")
	}
	if len(body) != 0 {
		t.Errorf("want empty body, got %d bytes", len(body))
	}
}

// Unknown type tag returns ok=false.
func TestParse_UnknownType(t *testing.T) {
	raw := session.Frame(0xFF, []byte("x"))
	raw[0] = 0xFF
	_, _, ok := session.Parse(raw)
	if ok {
		t.Error("want ok=false for unknown type, got true")
	}
}

// Truncated frame returns ok=false.
func TestParse_Truncated(t *testing.T) {
	raw := session.Frame(session.MsgTypePush, []byte("hello"))
	_, _, ok := session.Parse(raw[:3])
	if ok {
		t.Error("want ok=false for truncated frame, got true")
	}
}

// Frame then Parse returns the original type and body.
func TestFrame_RoundTrip(t *testing.T) {
	body := []byte("hello")
	raw := session.Frame(session.MsgTypePush, body)

	got, gotBody, ok := session.Parse(raw)
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if got != session.MsgTypePush {
		t.Errorf("want MsgTypePush, got %v", got)
	}
	if string(gotBody) != string(body) {
		t.Errorf("want %q, got %q", body, gotBody)
	}
}
