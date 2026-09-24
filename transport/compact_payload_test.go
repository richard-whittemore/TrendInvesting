package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// TestSocketRejectsNonCompactPayloadBeforeDecider checks ADR 0017's payload
// byte invariant before a decider can record an input for a later journal write.
func TestSocketRejectsNonCompactPayloadBeforeDecider(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	decide := func(ctx context.Context, envelope event.Envelope) (event.Envelope, error) {
		calls.Add(1)
		return echoDecider(ctx, envelope)
	}
	_, path := startServer(t, decide, transport.ServerConfig{})
	conn, err := net.DialTimeout("unix", path, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`{"a":  1}`, `{"a":1}`} {
		bar := newBar(t, "bar-1", 1)
		bar.Payload = nil
		bar.PayloadHash = event.HashPayload([]byte(payload))
		frame, err := json.Marshal(bar)
		if err != nil {
			t.Fatal(err)
		}
		// Splice raw bytes after marshaling: RawMessage itself would compact
		// the malformed input before it ever reached the socket.
		frame = bytes.Replace(frame, []byte(`"payload":null`), []byte(`"payload":`+payload), 1)
		if _, err := conn.Write(append(frame, '\n')); err != nil {
			t.Fatal(err)
		}
		reply := readResponse(t, conn)
		if payload == `{"a":  1}` {
			if reply.Error == nil || reply.Error.Code != transport.CodeInvalidEnvelope ||
				!strings.Contains(reply.Error.Message, "payload must contain compact JSON") || reply.Error.CausationID != bar.ID {
				t.Errorf("reply = %+v, want invalid_envelope naming compact JSON and causation %s", reply, bar.ID)
			}
			if got := calls.Load(); got != 0 {
				t.Errorf("decider called %d times for non-compact payload, want 0", got)
			}
		} else {
			if reply.Error != nil || reply.Envelope == nil {
				t.Fatalf("compact retry on same socket failed: %+v", reply)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("decider called %d times, want only the compact retry", got)
			}
		}
	}
}
