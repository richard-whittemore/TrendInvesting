package event_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// addProposalPeriodEnd is when validAddProposal's rung was reached: the
// second Unit's opportunity, some days after validCampaignOpened's own
// opening fill (2026-02-27).
var addProposalPeriodEnd = proposalPeriodEnd.AddDate(0, 0, 6)

// validAddProposal returns an Add proposal for Unit 2 of
// validCampaignOpened's Campaign: the next rung, measured from Unit 1's
// actual fill (campaignEntryPrice, 201.25) plus half the campaign N.
func validAddProposal() event.AddProposalPayload {
	previousFill := campaignEntryPrice
	n := proposalN
	return event.AddProposalPayload{
		CampaignID:       "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:     "AAPL",
		PeriodEnd:        addProposalPeriodEnd,
		UnitIndex:        2,
		Level:            previousFill + 0.5*n,
		Quantity:         133,
		PreviousUnitFill: previousFill,
		CampaignN:        n,
		Rule:             event.RuleAddLadderHalfN,
		ADR:              event.ADRCampaignFrozenAtEntry,
	}
}

func TestAddProposalPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.AddProposalPayload)
		wantErr string
	}{
		{name: "valid add proposal"},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.AddProposalPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.AddProposalPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing period end",
			mutate:  func(p *event.AddProposalPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			// Unit 1 is the campaign's own opening fill, never an add.
			name:    "unit index 1",
			mutate:  func(p *event.AddProposalPayload) { p.UnitIndex = 1 },
			wantErr: "unit index",
		},
		{
			name:    "unit index 0",
			mutate:  func(p *event.AddProposalPayload) { p.UnitIndex = 0 },
			wantErr: "unit index",
		},
		{
			name:    "zero quantity",
			mutate:  func(p *event.AddProposalPayload) { p.Quantity = 0 },
			wantErr: "quantity",
		},
		{
			name:    "negative quantity",
			mutate:  func(p *event.AddProposalPayload) { p.Quantity = -1 },
			wantErr: "quantity",
		},
		{
			name:    "zero previous unit fill",
			mutate:  func(p *event.AddProposalPayload) { p.PreviousUnitFill = 0 },
			wantErr: "previous unit fill must be positive",
		},
		{
			name:    "zero campaign n",
			mutate:  func(p *event.AddProposalPayload) { p.CampaignN = 0 },
			wantErr: "campaign n must be positive",
		},
		{
			name:    "zero level",
			mutate:  func(p *event.AddProposalPayload) { p.Level = 0 },
			wantErr: "level must be positive",
		},
		{
			// The headline invariant: Level must be EXACTLY
			// PreviousUnitFill + 0.5 x CampaignN.
			name:    "level does not match its derivation",
			mutate:  func(p *event.AddProposalPayload) { p.Level += 0.01 },
			wantErr: "does not match the derivation",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.AddProposalPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.AddProposalPayload) { p.ADR = "" },
			wantErr: "adr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validAddProposal()
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

func TestAddProposalPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(p *event.AddProposalPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{"level", func(p *event.AddProposalPayload, f float64) { p.Level = f }, "level must be finite"},
		{"previous unit fill", func(p *event.AddProposalPayload, f float64) { p.PreviousUnitFill = f }, "previous unit fill must be finite"},
		{"campaign n", func(p *event.AddProposalPayload, f float64) { p.CampaignN = f }, "campaign n must be finite"},
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
				payload := validAddProposal()
				field.apply(&payload, nf.value)

				err := payload.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
	}
}

func TestAddProposalPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.AddProposalPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id",
		"instrument id",
		"period end",
		"unit index",
		"quantity",
		"previous unit fill",
		"campaign n",
		"rule",
		"adr",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestAddProposalEventConstants(t *testing.T) {
	t.Parallel()

	if event.AddProposalEventType != "strategy.add.proposed" {
		t.Errorf("AddProposalEventType = %q, want %q", event.AddProposalEventType, "strategy.add.proposed")
	}
	if event.AddProposalSchemaVersion != 1 {
		t.Errorf("AddProposalSchemaVersion = %d, want 1", event.AddProposalSchemaVersion)
	}
	if event.RuleAddLadderHalfN != "add.ladder.half-n" {
		t.Errorf("RuleAddLadderHalfN = %q, want %q", event.RuleAddLadderHalfN, "add.ladder.half-n")
	}
}

func TestAddProposalPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validAddProposal()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.AddProposalPayload
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

func TestAddProposalPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validAddProposal())
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
		"period_end",
		"unit_index",
		"level",
		"quantity",
		"previous_unit_fill",
		"campaign_n",
		"rule",
		"adr",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}

// --- CampaignUnitAddedPayload -----------------------------------------

// validCampaignUnitAdded returns the unit-added record for the Add fill that
// would execute validAddProposal.
func validCampaignUnitAdded() event.CampaignUnitAddedPayload {
	fillPrice := campaignEntryPrice + 0.5*proposalN
	n := proposalN
	stopMultiple := 2.0
	return event.CampaignUnitAddedPayload{
		CampaignID:     "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:   "AAPL",
		UnitIndex:      2,
		FillID:         "sim-fill-0004",
		FillPrice:      fillPrice,
		Quantity:       133,
		CampaignN:      n,
		StopMultiple:   stopMultiple,
		ProtectiveStop: fillPrice - stopMultiple*n,
		Units:          2,
		AddedAt:        addProposalPeriodEnd,
		Rule:           event.RuleAddLadderHalfN,
		ADR:            event.ADRCampaignFrozenAtEntry,
	}
}

func TestCampaignUnitAddedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CampaignUnitAddedPayload)
		wantErr string
	}{
		{name: "valid unit added"},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "unit index 1",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.UnitIndex = 1; p.Units = 1 },
			wantErr: "unit index",
		},
		{
			name:    "missing fill id",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.FillID = "" },
			wantErr: "fill id",
		},
		{
			name:    "zero fill price",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.FillPrice = 0 },
			wantErr: "fill price must be positive",
		},
		{
			name:    "zero quantity",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.Quantity = 0 },
			wantErr: "quantity",
		},
		{
			name:    "zero campaign n",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.CampaignN = 0 },
			wantErr: "campaign n must be positive",
		},
		{
			name:    "zero stop multiple",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.StopMultiple = 0 },
			wantErr: "stop multiple must be positive",
		},
		{
			name:    "protective stop at zero",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.ProtectiveStop = 0 },
			wantErr: "protective stop must be positive",
		},
		{
			name:    "protective stop at or above fill price",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.ProtectiveStop = p.FillPrice },
			wantErr: "must be below the fill price",
		},
		{
			// Exact float64 equality, same discipline as every other derived
			// field in this package.
			name:    "protective stop does not match its derivation",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.ProtectiveStop += 0.01 },
			wantErr: "does not match the derivation",
		},
		{
			// units is the count AFTER this add, which must equal the index
			// of the unit just added.
			name:    "units does not match unit index",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.Units = 3 },
			wantErr: "units",
		},
		{
			name:    "missing added at",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.AddedAt = time.Time{} },
			wantErr: "added at",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.CampaignUnitAddedPayload) { p.ADR = "" },
			wantErr: "adr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validCampaignUnitAdded()
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

// TestCampaignUnitAddedPayloadThirdAndFourthUnit covers Units 3 and 4
// directly, since the table above only ever exercises Unit 2.
func TestCampaignUnitAddedPayloadThirdAndFourthUnit(t *testing.T) {
	t.Parallel()

	for _, unitIndex := range []int{3, 4} {
		t.Run(fmt.Sprintf("unit %d", unitIndex), func(t *testing.T) {
			t.Parallel()

			payload := validCampaignUnitAdded()
			payload.UnitIndex = unitIndex
			payload.Units = unitIndex
			if err := payload.Validate(); err != nil {
				t.Fatalf("Validate() error = %v, want nil for unit %d", err, unitIndex)
			}
		})
	}
}

func TestCampaignUnitAddedPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.CampaignUnitAddedPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id",
		"instrument id",
		"unit index",
		"fill id",
		"fill price",
		"quantity",
		"campaign n",
		"stop multiple",
		"protective stop",
		"added at",
		"rule",
		"adr",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestCampaignUnitAddedEventConstants(t *testing.T) {
	t.Parallel()

	if event.CampaignUnitAddedEventType != "strategy.campaign.unit-added" {
		t.Errorf("CampaignUnitAddedEventType = %q, want %q", event.CampaignUnitAddedEventType, "strategy.campaign.unit-added")
	}
	if event.CampaignUnitAddedSchemaVersion != 1 {
		t.Errorf("CampaignUnitAddedSchemaVersion = %d, want 1", event.CampaignUnitAddedSchemaVersion)
	}
}

func TestCampaignUnitAddedPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validCampaignUnitAdded()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CampaignUnitAddedPayload
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

func TestCampaignUnitAddedPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validCampaignUnitAdded())
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
		"fill_id",
		"fill_price",
		"quantity",
		"campaign_n",
		"stop_multiple",
		"protective_stop",
		"units",
		"added_at",
		"rule",
		"adr",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
