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

// validProtectiveStopSet returns the Protective-Stop-set that would be
// emitted alongside validCampaignOpened: the same Campaign, the same frozen
// numbers, Level equal to that Campaign's ProtectiveStop, and PreviousLevel
// 0 (a first set).
func validProtectiveStopSet() event.ProtectiveStopSetPayload {
	return event.ProtectiveStopSetPayload{
		CampaignID:    "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:  "AAPL",
		UnitIndex:     1,
		Reason:        event.ProtectiveStopReasonInitial,
		AsOf:          proposalPeriodEnd,
		Level:         campaignEntryPrice - 2*proposalN,
		PreviousLevel: 0,
		EntryPrice:    campaignEntryPrice,
		CampaignN:     proposalN,
		StopMultiple:  2,
		Rule:          event.RuleProtectiveStopSetFromFill,
		ADR:           event.ADRCampaignFrozenAtEntry,
	}
}

// validProtectiveStopSetRaised returns a legitimate Stop Ladder raise
// (#15): Unit 1's stop, raised by half a campaign N because a further Unit
// was added.
func validProtectiveStopSetRaised() event.ProtectiveStopSetPayload {
	previous := campaignEntryPrice - 2*proposalN
	return event.ProtectiveStopSetPayload{
		CampaignID:    "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:  "AAPL",
		UnitIndex:     1,
		Reason:        event.ProtectiveStopReasonAddLadder,
		AsOf:          proposalPeriodEnd,
		Level:         previous + 0.5*proposalN,
		PreviousLevel: previous,
		EntryPrice:    campaignEntryPrice,
		CampaignN:     proposalN,
		StopMultiple:  2,
		Rule:          event.RuleStopLadderRaisedByHalfN,
		ADR:           event.ADRCampaignFrozenAtEntry,
	}
}

func TestProtectiveStopSetPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ProtectiveStopSetPayload)
		wantErr string
	}{
		{name: "valid protective stop set"},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing as of",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.AsOf = time.Time{} },
			wantErr: "as of",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.ADR = "" },
			wantErr: "adr",
		},
		{
			name:    "zero campaign n",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.CampaignN = 0 },
			wantErr: "campaign n must be positive",
		},
		{
			name:    "negative campaign n",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.CampaignN = -1 },
			wantErr: "campaign n must be positive",
		},
		{
			name:    "zero stop multiple",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.StopMultiple = 0 },
			wantErr: "stop multiple must be positive",
		},
		{
			name:    "negative stop multiple",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.StopMultiple = -1 },
			wantErr: "stop multiple must be positive",
		},
		{
			name:    "zero entry price",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.EntryPrice = 0 },
			wantErr: "entry price must be positive",
		},
		{
			name:    "negative entry price",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.EntryPrice = -1 },
			wantErr: "entry price must be positive",
		},
		{
			name:    "level at zero",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.Level = 0 },
			wantErr: "level must be positive",
		},
		{
			name:    "level negative",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.Level = -1 },
			wantErr: "level must be positive",
		},
		{
			name: "level at or above entry price",
			mutate: func(p *event.ProtectiveStopSetPayload) {
				p.Level = p.EntryPrice
			},
			wantErr: "must be below the entry price",
		},
		{
			// Exact float64 equality, for the reason recorded on
			// CampaignOpenedPayload.Validate: the stated level must be the
			// identical value the derivation produces.
			name: "level does not match its derivation",
			mutate: func(p *event.ProtectiveStopSetPayload) {
				p.Level = campaignEntryPrice - 2*proposalN + 0.01
			},
			wantErr: "does not match the derivation",
		},
		{
			name:    "negative previous level",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.PreviousLevel = -1 },
			wantErr: "previous level must not be negative",
		},
		{
			// Forward-looking for #15's Stop Ladder: a non-zero previous
			// level must sit below the new one, since the baseline's stop
			// only ever rises.
			name: "previous level at or above the new level",
			mutate: func(p *event.ProtectiveStopSetPayload) {
				p.PreviousLevel = p.Level
			},
			wantErr: "baseline's stop ladder only ever raises",
		},
		{
			// A positive previous level strictly below the new one, with
			// Reason ProtectiveStopReasonAddLadder and Level matching
			// sizing.RaisedStop's own derivation, is a legitimate Stop
			// Ladder raise (#15).
			name: "a legitimate raised stop",
			mutate: func(p *event.ProtectiveStopSetPayload) {
				p.Reason = event.ProtectiveStopReasonAddLadder
				p.Rule = event.RuleStopLadderRaisedByHalfN
				p.PreviousLevel = p.Level - 0.5*proposalN
			},
		},
		{
			name:    "missing unit index",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.UnitIndex = 0 },
			wantErr: "unit index",
		},
		{
			name:    "unrecognised reason",
			mutate:  func(p *event.ProtectiveStopSetPayload) { p.Reason = "raised" },
			wantErr: "not a recognised protective stop reason",
		},
		{
			name: "initial set with a non-zero previous level",
			mutate: func(p *event.ProtectiveStopSetPayload) {
				p.PreviousLevel = p.Level - 0.5*proposalN
			},
			wantErr: "previous level must be zero for an initial set",
		},
		{
			name: "add-ladder raise with a zero previous level",
			mutate: func(p *event.ProtectiveStopSetPayload) {
				p.Reason = event.ProtectiveStopReasonAddLadder
				p.Rule = event.RuleStopLadderRaisedByHalfN
			},
			wantErr: "previous level must be positive for an add-ladder raise",
		},
		{
			name: "add-ladder raise whose level does not match the raise derivation",
			mutate: func(p *event.ProtectiveStopSetPayload) {
				p.Reason = event.ProtectiveStopReasonAddLadder
				p.Rule = event.RuleStopLadderRaisedByHalfN
				p.PreviousLevel = p.Level - 0.5*proposalN
				p.Level += 0.01
			},
			wantErr: "does not match the derivation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validProtectiveStopSet()
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

func TestProtectiveStopSetPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(p *event.ProtectiveStopSetPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{"campaign n", func(p *event.ProtectiveStopSetPayload, f float64) { p.CampaignN = f }, "campaign n must be finite"},
		{"stop multiple", func(p *event.ProtectiveStopSetPayload, f float64) { p.StopMultiple = f }, "stop multiple must be finite"},
		{"entry price", func(p *event.ProtectiveStopSetPayload, f float64) { p.EntryPrice = f }, "entry price must be finite"},
		{"level", func(p *event.ProtectiveStopSetPayload, f float64) { p.Level = f }, "level must be finite"},
		{"previous level", func(p *event.ProtectiveStopSetPayload, f float64) { p.PreviousLevel = f }, "previous level must be finite"},
	}

	nonFinite := []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	}

	for _, field := range fields {
		for _, nf := range nonFinite {
			t.Run(field.name+" "+nf.name, func(t *testing.T) {
				t.Parallel()
				payload := validProtectiveStopSet()
				field.apply(&payload, nf.value)

				err := payload.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
	}
}

func TestProtectiveStopSetPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.ProtectiveStopSetPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id",
		"instrument id",
		"unit index",
		"reason",
		"as of",
		"rule",
		"adr",
		"campaign n",
		"stop multiple",
		"entry price",
		"level",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestProtectiveStopSetEventConstants(t *testing.T) {
	t.Parallel()

	if event.ProtectiveStopSetEventType != "strategy.protective-stop.set" {
		t.Errorf("ProtectiveStopSetEventType = %q, want %q", event.ProtectiveStopSetEventType, "strategy.protective-stop.set")
	}
	if event.ProtectiveStopSetSchemaVersion != 2 {
		t.Errorf("ProtectiveStopSetSchemaVersion = %d, want 2", event.ProtectiveStopSetSchemaVersion)
	}
	if event.RuleProtectiveStopSetFromFill != "protective-stop.set.from-fill" {
		t.Errorf("RuleProtectiveStopSetFromFill = %q", event.RuleProtectiveStopSetFromFill)
	}
	if event.RuleStopLadderRaisedByHalfN != "stop-ladder.raised-by-half-n" {
		t.Errorf("RuleStopLadderRaisedByHalfN = %q", event.RuleStopLadderRaisedByHalfN)
	}
	if event.ProtectiveStopReasonInitial != "initial" {
		t.Errorf("ProtectiveStopReasonInitial = %q, want %q", event.ProtectiveStopReasonInitial, "initial")
	}
	if event.ProtectiveStopReasonAddLadder != "add-ladder" {
		t.Errorf("ProtectiveStopReasonAddLadder = %q, want %q", event.ProtectiveStopReasonAddLadder, "add-ladder")
	}
}

// TestProtectiveStopSetPayloadRaisedRoundTrip mirrors
// TestProtectiveStopSetPayloadRoundTrip for the add-ladder shape, so the
// raised reason's own required fields (UnitIndex, Reason, a positive
// PreviousLevel) round-trip through JSON cleanly too.
func TestProtectiveStopSetPayloadRaisedRoundTrip(t *testing.T) {
	t.Parallel()

	original := validProtectiveStopSetRaised()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.ProtectiveStopSetPayload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded.Validate() error = %v", err)
	}
}

func TestProtectiveStopSetPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validProtectiveStopSet()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.ProtectiveStopSetPayload
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

func TestProtectiveStopSetPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validProtectiveStopSet())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"campaign_id",
		"instrument_id",
		"unit_index",
		"reason",
		"as_of",
		"level",
		"previous_level",
		"entry_price",
		"campaign_n",
		"stop_multiple",
		"rule",
		"adr",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
