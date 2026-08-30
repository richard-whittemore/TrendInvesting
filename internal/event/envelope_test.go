package event_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validEnvelope() event.Envelope {
	now := time.Date(2026, time.August, 29, 20, 0, 0, 0, time.UTC)
	return event.Envelope{
		ID:            "evt-1",
		Type:          "market.bar.completed",
		SchemaVersion: 1,
		EventTime:     now,
		RecordedAt:    now,
		Sequence:      1,
		Payload:       []byte(`{"symbol":"AAPL"}`),
	}
}

func TestEnvelopeValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.Envelope)
		wantErr string
	}{
		{name: "valid"},
		{name: "missing id", mutate: func(e *event.Envelope) { e.ID = "" }, wantErr: "event id"},
		{name: "missing type", mutate: func(e *event.Envelope) { e.Type = "" }, wantErr: "event type"},
		{name: "zero schema", mutate: func(e *event.Envelope) { e.SchemaVersion = 0 }, wantErr: "schema version"},
		{name: "zero sequence", mutate: func(e *event.Envelope) { e.Sequence = 0 }, wantErr: "sequence"},
		{name: "invalid payload", mutate: func(e *event.Envelope) { e.Payload = []byte(`{"symbol":`) }, wantErr: "valid JSON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			envelope := validEnvelope()
			if tt.mutate != nil {
				tt.mutate(&envelope)
			}
			err := envelope.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
