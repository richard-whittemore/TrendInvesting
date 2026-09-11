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

var unitsStoppedAt = time.Date(2026, time.April, 10, 0, 0, 0, 0, time.UTC)

// validCampaignUnitsStopped returns a stop fill closing Unit 4 alone (the
// gap case, #15), leaving 3 Units open.
func validCampaignUnitsStopped() event.CampaignUnitsStoppedPayload {
	const (
		fillPrice       = 195.0
		entryPrice      = 200.0
		quantityClosed  = 133
		campaignN       = 3.0
		dollarsPerPoint = 1.0
	)
	realisedResult := float64(quantityClosed) * (fillPrice - entryPrice) * dollarsPerPoint
	return event.CampaignUnitsStoppedPayload{
		CampaignID:             "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:           "AAPL",
		FillID:                 "sim-fill-stop-4",
		UnitIndexes:            []int{4},
		FillPrice:              fillPrice,
		QuantityClosed:         quantityClosed,
		EntryPrice:             entryPrice,
		CampaignN:              campaignN,
		DollarsPerPoint:        dollarsPerPoint,
		RealisedResult:         realisedResult,
		StoppedAt:              unitsStoppedAt,
		RemainingUnits:         3,
		AggregateOpenRiskAfter: 500.0,
		Rule:                   event.RuleCampaignUnitsStoppedByStop,
		ADR:                    event.ADRCampaignExitRecordsTheFill,
	}
}

func TestCampaignUnitsStoppedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CampaignUnitsStoppedPayload)
		wantErr string
	}{
		{name: "valid units stopped"},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing fill id",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.FillID = "" },
			wantErr: "fill id",
		},
		{
			name:    "empty unit indexes",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.UnitIndexes = nil },
			wantErr: "unit indexes is required",
		},
		{
			name:    "unit index below 1",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.UnitIndexes = []int{0} },
			wantErr: "must be at least 1",
		},
		{
			name:    "unit indexes out of order",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.UnitIndexes = []int{2, 1} },
			wantErr: "strictly ascending",
		},
		{
			name:    "duplicate unit indexes",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.UnitIndexes = []int{1, 1} },
			wantErr: "strictly ascending",
		},
		{
			name: "several unit indexes ascending is legitimate",
			mutate: func(p *event.CampaignUnitsStoppedPayload) {
				p.UnitIndexes = []int{1, 2, 3}
				p.QuantityClosed = 399
				p.RealisedResult = float64(p.QuantityClosed) * (p.FillPrice - p.EntryPrice) * p.DollarsPerPoint
			},
		},
		{
			name:    "zero fill price",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.FillPrice = 0 },
			wantErr: "fill price must be positive",
		},
		{
			name:    "zero quantity closed",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.QuantityClosed = 0 },
			wantErr: "quantity closed must be a positive whole number",
		},
		{
			name:    "negative quantity closed",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.QuantityClosed = -1 },
			wantErr: "quantity closed must be a positive whole number",
		},
		{
			name:    "zero entry price",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.EntryPrice = 0 },
			wantErr: "entry price must be positive",
		},
		{
			name:    "zero campaign n",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.CampaignN = 0 },
			wantErr: "campaign n must be positive",
		},
		{
			name:    "zero dollars per point",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.DollarsPerPoint = 0 },
			wantErr: "dollars per point must be positive",
		},
		{
			// Exact float64 equality, the same discipline every derived
			// result in this package uses.
			name: "realised result does not match its derivation",
			mutate: func(p *event.CampaignUnitsStoppedPayload) {
				p.RealisedResult += 0.01
			},
			wantErr: "does not match the derivation",
		},
		{
			name:    "missing stopped at",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.StoppedAt = time.Time{} },
			wantErr: "stopped at",
		},
		{
			name:    "negative remaining units",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.RemainingUnits = -1 },
			wantErr: "remaining units must not be negative",
		},
		{
			name:    "negative aggregate open risk after",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.AggregateOpenRiskAfter = -1 },
			wantErr: "aggregate open risk after must not be negative",
		},
		{
			// The required shape: exactly zero once nothing remains.
			name: "aggregate open risk after nonzero while nothing remains",
			mutate: func(p *event.CampaignUnitsStoppedPayload) {
				p.RemainingUnits = 0
			},
			wantErr: "aggregate open risk after must be zero when remaining units is 0",
		},
		{
			name: "aggregate open risk after zero while nothing remains is legitimate",
			mutate: func(p *event.CampaignUnitsStoppedPayload) {
				p.RemainingUnits = 0
				p.AggregateOpenRiskAfter = 0
			},
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.CampaignUnitsStoppedPayload) { p.ADR = "" },
			wantErr: "adr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validCampaignUnitsStopped()
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

func TestCampaignUnitsStoppedPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(p *event.CampaignUnitsStoppedPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{"fill price", func(p *event.CampaignUnitsStoppedPayload, f float64) { p.FillPrice = f }, "fill price must be finite"},
		{"entry price", func(p *event.CampaignUnitsStoppedPayload, f float64) { p.EntryPrice = f }, "entry price must be finite"},
		{"campaign n", func(p *event.CampaignUnitsStoppedPayload, f float64) { p.CampaignN = f }, "campaign n must be finite"},
		{"dollars per point", func(p *event.CampaignUnitsStoppedPayload, f float64) { p.DollarsPerPoint = f }, "dollars per point must be finite"},
		{"realised result", func(p *event.CampaignUnitsStoppedPayload, f float64) { p.RealisedResult = f }, "realised result must be finite"},
		{"aggregate open risk after", func(p *event.CampaignUnitsStoppedPayload, f float64) { p.AggregateOpenRiskAfter = f }, "aggregate open risk after must be finite"},
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
				payload := validCampaignUnitsStopped()
				field.apply(&payload, nf.value)

				err := payload.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
	}
}

func TestCampaignUnitsStoppedPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.CampaignUnitsStoppedPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id",
		"instrument id",
		"fill id",
		"unit indexes is required",
		"fill price",
		"quantity closed",
		"entry price",
		"campaign n",
		"dollars per point",
		"stopped at",
		"rule",
		"adr",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestCampaignUnitsStoppedEventConstants(t *testing.T) {
	t.Parallel()

	if event.CampaignUnitsStoppedEventType != "strategy.campaign.units-stopped" {
		t.Errorf("CampaignUnitsStoppedEventType = %q, want %q", event.CampaignUnitsStoppedEventType, "strategy.campaign.units-stopped")
	}
	if event.CampaignUnitsStoppedSchemaVersion != 1 {
		t.Errorf("CampaignUnitsStoppedSchemaVersion = %d, want 1", event.CampaignUnitsStoppedSchemaVersion)
	}
	if event.RuleCampaignUnitsStoppedByStop != "campaign.units-stopped.by-stop" {
		t.Errorf("RuleCampaignUnitsStoppedByStop = %q", event.RuleCampaignUnitsStoppedByStop)
	}
}

func TestCampaignUnitsStoppedPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validCampaignUnitsStopped()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CampaignUnitsStoppedPayload
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

func TestCampaignUnitsStoppedPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validCampaignUnitsStopped())
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
		"fill_id",
		"unit_indexes",
		"fill_price",
		"quantity_closed",
		"entry_price",
		"campaign_n",
		"dollars_per_point",
		"realised_result",
		"stopped_at",
		"remaining_units",
		"aggregate_open_risk_after",
		"rule",
		"adr",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
