package event_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the tests for AdapterRunStoppedEventType (#169): the
// adapter's own record of a deliberate stop, distinguishing it from
// RunCompletedEventType's silent "the stream ended".

func validAdapterRunStopped() event.AdapterRunStoppedPayload {
	return event.AdapterRunStoppedPayload{
		Reason:       event.AdapterRunStoppedReasonDelisted,
		InstrumentID: "AAPL",
		Detail:       "LEAN reports AAPL DELISTED at 2026-02-27T00:00:00Z; its delisting signal carries no reason",
	}
}

func TestAdapterRunStoppedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.AdapterRunStoppedPayload)
		wantErr string
	}{
		{name: "valid delisted stop"},
		{
			name:    "missing reason",
			mutate:  func(p *event.AdapterRunStoppedPayload) { p.Reason = "" },
			wantErr: "reason is required",
		},
		{
			// Reason is a closed set: only a delisting stop is recognised
			// today, so a future reason (an invalid-startup path that later
			// learns to report itself, say) must be a new value added
			// deliberately, never a string a producer can send today and
			// have silently misread as delisted.
			name:    "unrecognised reason",
			mutate:  func(p *event.AdapterRunStoppedPayload) { p.Reason = "invalid-startup" },
			wantErr: "not a recognised run stop reason",
		},
		{
			name:    "missing instrument id for a delisted stop",
			mutate:  func(p *event.AdapterRunStoppedPayload) { p.InstrumentID = "" },
			wantErr: `instrument id is required for reason "delisted"`,
		},
		{
			name:    "missing detail",
			mutate:  func(p *event.AdapterRunStoppedPayload) { p.Detail = "" },
			wantErr: "detail is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validAdapterRunStopped()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}

			err := payload.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestAdapterRunStoppedPayloadValidateAggregatesEveryField mirrors this
// package's own convention (e.g.
// TestCorporateActionPayloadValidateAggregatesEveryField): every field's own
// error is reported at once, not just the first.
func TestAdapterRunStoppedPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	err := event.AdapterRunStoppedPayload{}.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want an error for a wholly empty payload")
	}
	for _, want := range []string{"reason is required", "detail is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want it to mention %q", err, want)
		}
	}
}

func TestAdapterRunStoppedPayloadRoundTripAndJSONTags(t *testing.T) {
	t.Parallel()

	original := validAdapterRunStopped()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded event.AdapterRunStoppedPayload
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

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	wantKeys := map[string]bool{"reason": true, "instrument_id": true, "detail": true}
	if len(fields) != len(wantKeys) {
		t.Fatalf("got %d field(s) %v, want exactly %v", len(fields), fields, wantKeys)
	}
	for key := range wantKeys {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing expected field %q", key)
		}
	}
}

func TestAdapterRunStoppedEventConstants(t *testing.T) {
	t.Parallel()

	if event.AdapterRunStoppedEventType != "adapter.run.stopped" {
		t.Errorf("AdapterRunStoppedEventType = %q, want %q", event.AdapterRunStoppedEventType, "adapter.run.stopped")
	}
	if event.AdapterRunStoppedSchemaVersion != 1 {
		t.Errorf("AdapterRunStoppedSchemaVersion = %d, want 1", event.AdapterRunStoppedSchemaVersion)
	}
	if event.AdapterRunStoppedReasonDelisted != "delisted" {
		t.Errorf("AdapterRunStoppedReasonDelisted = %q, want %q", event.AdapterRunStoppedReasonDelisted, "delisted")
	}
}
