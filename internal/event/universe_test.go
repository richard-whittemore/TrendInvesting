package event_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the tests for ADR 0009's universe events: the declared
// classification fact (market.instrument-classification) and the monthly
// eligibility decision (strategy.universe.eligibility).

func validInstrumentClassification() event.InstrumentClassificationPayload {
	return event.InstrumentClassificationPayload{
		InstrumentID:      "AAPL",
		SecurityType:      event.SecurityTypeCommonStock,
		USPrimaryExchange: true,
		EffectiveAt:       time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestInstrumentClassificationPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.InstrumentClassificationPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.InstrumentClassificationPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing security type",
			mutate:  func(p *event.InstrumentClassificationPayload) { p.SecurityType = "" },
			wantErr: "not a recognised security type",
		},
		{
			name:    "unrecognised security type",
			mutate:  func(p *event.InstrumentClassificationPayload) { p.SecurityType = "reit" },
			wantErr: "not a recognised security type",
		},
		{
			name:    "missing effective at",
			mutate:  func(p *event.InstrumentClassificationPayload) { p.EffectiveAt = time.Time{} },
			wantErr: "effective at",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validInstrumentClassification()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}
			err := payload.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func validUniverseEligibility() event.UniverseEligibilityPayload {
	return event.UniverseEligibilityPayload{
		InstrumentID:           "AAPL",
		PeriodEnd:              time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC),
		Eligible:               true,
		ClassificationEligible: true,
		Price:                  50,
		PriceEligible:          true,
		DollarVolume:           10_000_000,
		DollarVolumeEligible:   true,
		CompletedBars:          300,
		HistoryEligible:        true,
		Rule:                   event.RuleUniverseEligibility,
		ADR:                    event.ADRUniverseEligibility,
	}
}

func TestUniverseEligibilityPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.UniverseEligibilityPayload)
		wantErr string
	}{
		{name: "valid eligible"},
		{
			name: "valid ineligible on one criterion",
			mutate: func(p *event.UniverseEligibilityPayload) {
				p.Eligible = false
				p.PriceEligible = false
			},
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.UniverseEligibilityPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing period end",
			mutate:  func(p *event.UniverseEligibilityPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			name:    "negative price",
			mutate:  func(p *event.UniverseEligibilityPayload) { p.Price = -1 },
			wantErr: "price",
		},
		{
			name:    "negative dollar volume",
			mutate:  func(p *event.UniverseEligibilityPayload) { p.DollarVolume = -1 },
			wantErr: "dollar volume",
		},
		{
			name:    "negative completed bars",
			mutate:  func(p *event.UniverseEligibilityPayload) { p.CompletedBars = -1 },
			wantErr: "completed bars",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.UniverseEligibilityPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.UniverseEligibilityPayload) { p.ADR = "" },
			wantErr: "adr",
		},
		{
			name:    "eligible true but a criterion false",
			mutate:  func(p *event.UniverseEligibilityPayload) { p.HistoryEligible = false },
			wantErr: "does not match its own four criteria",
		},
		{
			name: "eligible false but every criterion true",
			mutate: func(p *event.UniverseEligibilityPayload) {
				p.Eligible = false
			},
			wantErr: "does not match its own four criteria",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validUniverseEligibility()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}
			err := payload.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
