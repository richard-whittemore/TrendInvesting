package event_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestEnvelopeValidateRequiresCompactPayload rejects bytes a journal encoder
// would rewrite, preserving ADR 0017's exact-payload hash invariant at ingress.
func TestEnvelopeValidateRequiresCompactPayload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		payload string
		valid   bool
	}{
		{"internal whitespace", `{"a":  1}`, false},
		{"trailing newline", "{\"a\":1}\n", false},
		{"leading whitespace", "\t{\"a\":1}", false},
		{"compact", `{"a":1}`, true},
		{"whitespace inside string", `{"a":"  1\n"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := validEnvelope()
			envelope.Payload = []byte(tc.payload)
			envelope.PayloadHash = event.HashPayload(envelope.Payload)
			err := envelope.Validate()
			if tc.valid && err != nil {
				t.Errorf("Validate() = %v, want compact payload accepted", err)
			}
			if !tc.valid && (err == nil || !strings.Contains(err.Error(), "payload must contain compact JSON")) {
				t.Errorf("Validate() = %v, want payload must contain compact JSON", err)
			}
			if !bytes.Equal(envelope.Payload, []byte(tc.payload)) || envelope.PayloadHash != event.HashPayload([]byte(tc.payload)) {
				t.Fatal("Validate rewrote the payload or its hash")
			}
		})
	}
}
