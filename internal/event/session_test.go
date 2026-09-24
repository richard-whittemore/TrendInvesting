package event_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validSessionClosed() event.SessionClosedPayload {
	return event.SessionClosedPayload{
		PeriodEnd:     time.Date(2026, time.September, 8, 20, 0, 0, 0, time.UTC),
		InstrumentIDs: []string{"AAPL", "MSFT"},
	}
}

func TestSessionClosedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.SessionClosedPayload)
		wantErr string
	}{
		{name: "valid", mutate: func(*event.SessionClosedPayload) {}},
		{name: "one instrument", mutate: func(p *event.SessionClosedPayload) { p.InstrumentIDs = []string{"AAPL"} }},
		{
			name:    "zero period end",
			mutate:  func(p *event.SessionClosedPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end is required",
		},
		{
			name:    "unwritable period end",
			mutate:  func(p *event.SessionClosedPayload) { p.PeriodEnd = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
			wantErr: "period end cannot be written",
		},
		{
			name:    "no instruments",
			mutate:  func(p *event.SessionClosedPayload) { p.InstrumentIDs = nil },
			wantErr: "at least one instrument",
		},
		{
			name:    "empty instrument id",
			mutate:  func(p *event.SessionClosedPayload) { p.InstrumentIDs = []string{"", "AAPL"} },
			wantErr: "instrument id 1 is empty",
		},
		{
			name:    "unsorted",
			mutate:  func(p *event.SessionClosedPayload) { p.InstrumentIDs = []string{"MSFT", "AAPL"} },
			wantErr: "strictly ascending",
		},
		{
			name:    "duplicate",
			mutate:  func(p *event.SessionClosedPayload) { p.InstrumentIDs = []string{"AAPL", "AAPL"} },
			wantErr: "strictly ascending",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validSessionClosed()
			tt.mutate(&payload)
			err := payload.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestSessionClosedPayloadRoundTripAndJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validSessionClosed())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded event.SessionClosedPayload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded.Validate() error = %v", err)
	}
	reEncoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-Marshal() error = %v", err)
	}
	if !bytes.Equal(encoded, reEncoded) {
		t.Fatalf("round trip not stable:\n  first:  %s\n  second: %s", encoded, reEncoded)
	}
	const want = `{"period_end":"2026-09-08T20:00:00Z","instrument_ids":["AAPL","MSFT"]}`
	if string(encoded) != want {
		t.Fatalf("encoded = %s, want %s", encoded, want)
	}
}

func TestSessionClosedEventConstants(t *testing.T) {
	t.Parallel()

	if event.SessionClosedEventType != "market.session.closed" {
		t.Errorf("SessionClosedEventType = %q, want %q", event.SessionClosedEventType, "market.session.closed")
	}
	if event.SessionClosedSchemaVersion != 1 {
		t.Errorf("SessionClosedSchemaVersion = %d, want 1", event.SessionClosedSchemaVersion)
	}
}
