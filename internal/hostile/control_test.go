//go:build hostile

package hostile

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/julianbonomini/trueseal-relay/internal/relay"
)

// controlClient dials the listener and exchanges one command per call.
type controlClient struct {
	t    *testing.T
	conn net.Conn
	enc  *json.Encoder
	sc   *bufio.Scanner
}

func dialControl(t *testing.T, addr string) *controlClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	c := &controlClient{t: t, conn: conn, enc: json.NewEncoder(conn), sc: bufio.NewScanner(conn)}
	return c
}

func (c *controlClient) do(cmd string) response {
	c.t.Helper()
	if err := c.enc.Encode(json.RawMessage(cmd)); err != nil {
		c.t.Fatalf("send: %v", err)
	}
	if !c.sc.Scan() {
		c.t.Fatalf("no response: %v", c.sc.Err())
	}
	var r response
	if err := json.Unmarshal(c.sc.Bytes(), &r); err != nil {
		c.t.Fatalf("bad response JSON: %v", err)
	}
	return r
}

func (c *controlClient) close() { c.conn.Close() }

func TestControlRefusesNonLoopbackBind(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	for _, addr := range []string{"0.0.0.0:0", ":0"} {
		if _, err := e.ListenControl(addr); err == nil {
			t.Fatalf("a non-loopback bind (%q) must be refused", addr)
		}
	}
}

func TestControlRoundTrip(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	addr, err := e.ListenControl("127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	c := dialControl(t, addr.String())
	defer c.close()

	if r := c.do(`{"cmd":"set","device":"*","drop":true}`); !r.OK {
		t.Fatalf("set: %s", r.Error)
	}
	if got := e.rulesFor(testKey); !got.Drop {
		t.Fatalf("rules not applied")
	}
	if r := c.do(`{"cmd":"clear","device":"*"}`); !r.OK {
		t.Fatalf("clear: %s", r.Error)
	}
	if got := e.rulesFor(testKey); got.Drop {
		t.Fatalf("rules not cleared")
	}
	if r := c.do(`{"cmd":"nonsense"}`); r.OK {
		t.Fatalf("unknown command must fail")
	}
	if r := c.do(`{"cmd":"set","device":"zzz","drop":true}`); r.OK {
		t.Fatalf("bad device selector must fail")
	}
}

func TestControlInjectAndReplayDriveALiveSession(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	addr, err := e.ListenControl("127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	c := dialControl(t, addr.String())
	defer c.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan relay.DeliveryBlob, 4)
	out := e.WrapSession(ctx, testKey, in)

	// Deliver a real blob first (the replay source).
	in <- blob(21, "real")
	if got := recv(t, out); got.BlobID != 21 {
		t.Fatalf("setup: got %d", got.BlobID)
	}

	// Inject via the control surface.
	harness := []byte("injected-sync-envelope")
	cmd := fmt.Sprintf(`{"cmd":"inject","device":%q,"envelope":%q}`, hexKey(testKey), hex.EncodeToString(harness))
	if r := c.do(cmd); !r.OK {
		t.Fatalf("inject: %s", r.Error)
	}
	got := recv(t, out)
	if got.BlobID < syntheticBlobIDBase || string(got.Envelope) != string(harness) {
		t.Fatalf("injected blob wrong: %d %q", got.BlobID, got.Envelope)
	}

	// Replay via the control surface: the last delivered blob (the
	// injected one) comes again.
	if r := c.do(fmt.Sprintf(`{"cmd":"replay","device":%q,"count":1}`, hexKey(testKey))); !r.OK {
		t.Fatalf("replay: %s", r.Error)
	}
	if got := recv(t, out); got.BlobID < syntheticBlobIDBase || string(got.Envelope) != string(harness) {
		t.Fatalf("replayed blob wrong: %d %q", got.BlobID, got.Envelope)
	}

	// Replay on a device with no session fails cleanly.
	var none relay.RecipientKey
	if r := c.do(fmt.Sprintf(`{"cmd":"replay","device":%q,"count":1}`, hexKey(none))); r.OK {
		t.Fatalf("replay without a live session must fail")
	}
}

func TestControlSetThenSessionAppliesRules(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	addr, err := e.ListenControl("127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	c := dialControl(t, addr.String())
	defer c.close()

	if r := c.do(`{"cmd":"set","device":"*","duplicate":true}`); !r.OK {
		t.Fatalf("set: %s", r.Error)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan relay.DeliveryBlob, 4)
	out := e.WrapSession(ctx, testKey, in)
	in <- blob(31, "x")
	recv(t, out)
	recv(t, out) // the duplicate
	recvNone(t, out)
}
