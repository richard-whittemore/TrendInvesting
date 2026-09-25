package event_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the tests for a symbol change (ADR 0024): the
// symbol-change kind of market.corporate-action, and the
// strategy.instrument.symbol-changed decision the reducer records for it.

// validSymbolChangeAction is an instrument continuing under a new instrument
// id, with no other terms.
func validSymbolChangeAction() event.CorporateActionPayload {
	return event.CorporateActionPayload{
		InstrumentID:    "AAPL",
		Kind:            event.CorporateActionKindSymbolChange,
		EffectiveAt:     time.Date(2026, time.March, 2, 4, 0, 0, 0, time.UTC),
		NewInstrumentID: "AAPL2",
	}
}

func TestSymbolChangeCorporateActionValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CorporateActionPayload)
		wantErr string
	}{
		{name: "valid symbol change"},
		{
			name:    "no new instrument id",
			mutate:  func(p *event.CorporateActionPayload) { p.NewInstrumentID = "" },
			wantErr: "new instrument id is required",
		},
		{
			name:    "new instrument id same as instrument id",
			mutate:  func(p *event.CorporateActionPayload) { p.NewInstrumentID = p.InstrumentID },
			wantErr: "same as instrument id",
		},
		{
			name:    "a symbol change carrying split terms",
			mutate:  func(p *event.CorporateActionPayload) { p.NewShares, p.OldShares, p.EngineSharesPerRawShare = 2, 1, 1 },
			wantErr: "a symbol change carries no split or dividend terms",
		},
		{
			name:    "a symbol change carrying a currency",
			mutate:  func(p *event.CorporateActionPayload) { p.Currency = "USD" },
			wantErr: "a symbol change carries no split or dividend terms",
		},
		{
			name:    "a symbol change carrying a cash amount",
			mutate:  func(p *event.CorporateActionPayload) { p.CashAmount = 1 },
			wantErr: "a symbol change carries no split or dividend terms",
		},
		{
			name:    "a delisting carrying a new instrument id",
			mutate:  func(p *event.CorporateActionPayload) { *p = validCorporateAction(); p.NewInstrumentID = "AAPL2" },
			wantErr: "a delisting carries no symbol change or dividend terms",
		},
		{
			name:    "a split carrying a new instrument id",
			mutate:  func(p *event.CorporateActionPayload) { *p = validSplitAction(); p.NewInstrumentID = "AAPL2" },
			wantErr: "a split carries no symbol change or dividend terms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validSymbolChangeAction()
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

func validInstrumentSymbolChanged() event.InstrumentSymbolChangedPayload {
	return event.InstrumentSymbolChangedPayload{
		InstrumentID:      "AAPL",
		NewInstrumentID:   "AAPL2",
		CorporateActionID: "run:corporate-action:9",
		EffectiveAt:       time.Date(2026, time.March, 2, 4, 0, 0, 0, time.UTC),
		CampaignID:        "campaign:AAPL:2026-02-01T20:00:00.000000000Z",
		Rule:              event.RuleSymbolChangeCarriesInstrumentState,
		ADR:               event.ADRSymbolChangeCarriesInstrumentState,
	}
}

func TestInstrumentSymbolChangedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.InstrumentSymbolChangedPayload)
		wantErr string
	}{
		{name: "valid, with an open campaign"},
		{name: "valid, no campaign (a Setup)", mutate: func(p *event.InstrumentSymbolChangedPayload) { p.CampaignID = "" }},
		{name: "no instrument id", mutate: func(p *event.InstrumentSymbolChangedPayload) { p.InstrumentID = "" }, wantErr: "instrument id"},
		{name: "no new instrument id", mutate: func(p *event.InstrumentSymbolChangedPayload) { p.NewInstrumentID = "" }, wantErr: "new instrument id"},
		{
			name:    "new instrument id same as instrument id",
			mutate:  func(p *event.InstrumentSymbolChangedPayload) { p.NewInstrumentID = p.InstrumentID },
			wantErr: "same as instrument id",
		},
		{name: "no corporate action id", mutate: func(p *event.InstrumentSymbolChangedPayload) { p.CorporateActionID = "" }, wantErr: "corporate action id"},
		{name: "no effective at", mutate: func(p *event.InstrumentSymbolChangedPayload) { p.EffectiveAt = time.Time{} }, wantErr: "effective at"},
		{name: "no rule", mutate: func(p *event.InstrumentSymbolChangedPayload) { p.Rule = "" }, wantErr: "rule"},
		{name: "no adr", mutate: func(p *event.InstrumentSymbolChangedPayload) { p.ADR = "" }, wantErr: "adr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validInstrumentSymbolChanged()
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

// TestUpcastCorporateActionSymbolChange: schema 3 recognises symbol-change,
// and schema 2 (which could not) refuses one.
func TestUpcastCorporateActionSymbolChange(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validSymbolChangeAction())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	got, err := event.UpcastCorporateActionPayload(3, encoded)
	if err != nil {
		t.Fatalf("UpcastCorporateActionPayload(3, symbol change) error = %v", err)
	}
	if got != validSymbolChangeAction() {
		t.Fatalf("decoded = %+v, want %+v", got, validSymbolChangeAction())
	}

	_, err = event.UpcastCorporateActionPayload(2, encoded)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("UpcastCorporateActionPayload(2, symbol change) error = %v, want a decode failure: schema 2 has no new_instrument_id field", err)
	}
}
