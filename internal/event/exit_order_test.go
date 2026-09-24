package event_test

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// exitOrderAsOf is when this fixture's exit order came into force.
var exitOrderAsOf = time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC)

// validExitOrderSet returns Unit 1 of the Campaign validCampaignOpened
// describes with its exit order at its own Protective Stop: no exit
// condition applies, so the Exit Channel level is zero.
func validExitOrderSet() event.ExitOrderSetPayload {
	return event.ExitOrderSetPayload{
		CampaignID:       "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:     "AAPL",
		UnitIndex:        1,
		Level:            190.25,
		Quantity:         133,
		Source:           event.ExitOrderSourceProtectiveStop,
		ProtectiveStop:   190.25,
		ExitChannelLevel: 0,
		AsOf:             exitOrderAsOf,
		Rule:             event.RuleExitOrderHigherOfStopAndExitChannel,
		ADR:              event.ADRExitOrderRestsAtTheLevel,
	}
}

// validExitOrderSetAtExitChannel is the same Unit once an exit condition
// applies at a level above its stop: the Exit Channel level governs.
func validExitOrderSetAtExitChannel() event.ExitOrderSetPayload {
	p := validExitOrderSet()
	p.ExitChannelLevel = 195.5
	p.Level = 195.5
	p.Source = event.ExitOrderSourceExitChannel
	return p
}

func TestExitOrderSetPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		base    func() event.ExitOrderSetPayload
		mutate  func(*event.ExitOrderSetPayload)
		wantErr string
	}{
		{name: "valid, no exit condition applies", base: validExitOrderSet},
		{name: "valid, exit channel level above the stop", base: validExitOrderSetAtExitChannel},
		{
			name: "valid, exit channel level below the stop: the stop governs",
			base: validExitOrderSet,
			mutate: func(p *event.ExitOrderSetPayload) {
				p.ExitChannelLevel = 185
			},
		},
		{
			name: "valid, a tie names the protective stop",
			base: validExitOrderSet,
			mutate: func(p *event.ExitOrderSetPayload) {
				p.ExitChannelLevel = p.ProtectiveStop
			},
		},
		{
			name: "a tie naming the exit channel is refused",
			base: validExitOrderSet,
			mutate: func(p *event.ExitOrderSetPayload) {
				p.ExitChannelLevel = p.ProtectiveStop
				p.Source = event.ExitOrderSourceExitChannel
			},
			wantErr: "source",
		},
		{
			name: "the lower level stated when the exit channel is higher",
			base: validExitOrderSetAtExitChannel,
			mutate: func(p *event.ExitOrderSetPayload) {
				p.Level = p.ProtectiveStop
			},
			wantErr: "does not match",
		},
		{
			name: "the exit channel stated when the stop is higher",
			base: validExitOrderSet,
			mutate: func(p *event.ExitOrderSetPayload) {
				p.ExitChannelLevel = 185
				p.Level = 185
				p.Source = event.ExitOrderSourceExitChannel
			},
			wantErr: "does not match",
		},
		{
			name:    "source protective-stop while the exit channel governs",
			base:    validExitOrderSetAtExitChannel,
			mutate:  func(p *event.ExitOrderSetPayload) { p.Source = event.ExitOrderSourceProtectiveStop },
			wantErr: "source",
		},
		{
			name:    "unrecognised source",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.Source = "trailing" },
			wantErr: "not a recognised exit order source",
		},
		{
			name:    "missing campaign id",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "zero unit index",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.UnitIndex = 0 },
			wantErr: "unit index",
		},
		{
			name:    "zero quantity",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.Quantity = 0 },
			wantErr: "quantity",
		},
		{
			name:    "missing as of",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.AsOf = time.Time{} },
			wantErr: "as of is required",
		},
		{
			name:    "unwritable as of",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.AsOf = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
			wantErr: "RFC 3339",
		},
		{
			name:    "zero protective stop",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.ProtectiveStop = 0 },
			wantErr: "protective stop must be positive",
		},
		{
			name:    "non-finite protective stop",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.ProtectiveStop = math.Inf(1) },
			wantErr: "protective stop must be finite",
		},
		{
			name:    "negative exit channel level",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.ExitChannelLevel = -1 },
			wantErr: "exit channel level must not be negative",
		},
		{
			name:    "non-finite exit channel level",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.ExitChannelLevel = math.NaN() },
			wantErr: "exit channel level must be finite",
		},
		{
			name:    "zero level",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.Level = 0 },
			wantErr: "level must be positive",
		},
		{
			name:    "non-finite level",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.Level = math.NaN() },
			wantErr: "level must be finite",
		},
		{
			name:    "missing rule",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			base:    validExitOrderSet,
			mutate:  func(p *event.ExitOrderSetPayload) { p.ADR = "" },
			wantErr: "adr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := tt.base()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}
			// The two "valid" mutations above move the Exit Channel level
			// without restating Level; they rely on the stop still governing.
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

func TestExitOrderSetPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.ExitOrderSetPayload
	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id", "instrument id", "unit index", "quantity", "source",
		"as of", "protective stop", "level", "rule", "adr",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestExitOrderSetEventConstants(t *testing.T) {
	t.Parallel()

	if event.ExitOrderSetEventType != "strategy.exit-order.set" {
		t.Errorf("ExitOrderSetEventType = %q", event.ExitOrderSetEventType)
	}
	if event.ExitOrderSetSchemaVersion != 1 {
		t.Errorf("ExitOrderSetSchemaVersion = %d, want 1", event.ExitOrderSetSchemaVersion)
	}
	if event.ExitOrderSourceProtectiveStop != "protective-stop" {
		t.Errorf("ExitOrderSourceProtectiveStop = %q", event.ExitOrderSourceProtectiveStop)
	}
	if event.ExitOrderSourceExitChannel != "exit-channel" {
		t.Errorf("ExitOrderSourceExitChannel = %q", event.ExitOrderSourceExitChannel)
	}
	if event.RuleExitOrderHigherOfStopAndExitChannel != "exit-order.higher-of-stop-and-exit-channel" {
		t.Errorf("RuleExitOrderHigherOfStopAndExitChannel = %q", event.RuleExitOrderHigherOfStopAndExitChannel)
	}
	// ADR 0005: a Protective Stop and an Exit-Channel exit are both resting
	// orders at their level.
	if event.ADRExitOrderRestsAtTheLevel != "0005" {
		t.Errorf("ADRExitOrderRestsAtTheLevel = %q, want 0005", event.ADRExitOrderRestsAtTheLevel)
	}
}

func TestExitOrderSetPayloadRoundTripAndJSONTags(t *testing.T) {
	t.Parallel()

	original := validExitOrderSetAtExitChannel()
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded event.ExitOrderSetPayload
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

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	keys := []string{
		"campaign_id", "instrument_id", "unit_index", "level", "quantity",
		"source", "protective_stop", "exit_channel_level", "as_of", "rule", "adr",
	}
	for _, key := range keys {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload is missing key %q: %s", key, encoded)
		}
	}
	if len(asMap) != len(keys) {
		t.Errorf("encoded payload has %d keys, want %d: %s", len(asMap), len(keys), encoded)
	}
}
