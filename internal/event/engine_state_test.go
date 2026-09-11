package event_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validEngineState() event.EngineStatePayload {
	return event.EngineStatePayload{
		State:  event.EngineStateHalted,
		Reason: event.EngineStateReasonCampaignWithoutProtectiveStop,
		Detail: "campaign \"campaign:AAPL:x\" has protective stop -1 against entry price 100",
	}
}

func TestEngineStatePayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.EngineStatePayload)
		wantErr string
	}{
		{name: "valid engine state"},
		{
			name:    "missing state",
			mutate:  func(p *event.EngineStatePayload) { p.State = "" },
			wantErr: "state",
		},
		{
			name:    "unrecognised state",
			mutate:  func(p *event.EngineStatePayload) { p.State = "running" },
			wantErr: "state",
		},
		{
			name:    "missing reason",
			mutate:  func(p *event.EngineStatePayload) { p.Reason = "" },
			wantErr: "reason",
		},
		{
			name:    "unrecognised reason",
			mutate:  func(p *event.EngineStatePayload) { p.Reason = "something-else" },
			wantErr: "reason",
		},
		{
			name:    "missing detail",
			mutate:  func(p *event.EngineStatePayload) { p.Detail = "" },
			wantErr: "detail",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validEngineState()
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

func TestEngineStatePayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.EngineStatePayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{"state", "reason", "detail"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestEngineStateEventConstants(t *testing.T) {
	t.Parallel()

	if event.EngineStateEventType != "strategy.engine.state" {
		t.Errorf("EngineStateEventType = %q, want %q", event.EngineStateEventType, "strategy.engine.state")
	}
	if event.EngineStateSchemaVersion != 1 {
		t.Errorf("EngineStateSchemaVersion = %d, want 1", event.EngineStateSchemaVersion)
	}
	if event.EngineStateHalted != "halted" {
		t.Errorf("EngineStateHalted = %q, want %q", event.EngineStateHalted, "halted")
	}
	if event.EngineStateReasonCampaignWithoutProtectiveStop != "campaign-without-protective-stop" {
		t.Errorf("EngineStateReasonCampaignWithoutProtectiveStop = %q", event.EngineStateReasonCampaignWithoutProtectiveStop)
	}
}

func TestEngineStatePayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validEngineState()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.EngineStatePayload
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
}

func TestEngineStatePayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validEngineState())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{"state", "reason", "detail"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
