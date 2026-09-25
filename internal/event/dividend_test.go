package event_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the tests for a dividend (ADR 0004, ADR 0024): the
// dividend kind of market.corporate-action, and the
// strategy.campaign.dividend decision the reducer records for it.

func validDividendAction() event.CorporateActionPayload {
	return event.CorporateActionPayload{
		InstrumentID: "AAPL",
		Kind:         event.CorporateActionKindDividend,
		EffectiveAt:  time.Date(2026, time.March, 2, 4, 0, 0, 0, time.UTC),
		CashAmount:   184.0,
		Currency:     "USD",
	}
}

func TestDividendCorporateActionValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CorporateActionPayload)
		wantErr string
	}{
		{name: "valid dividend"},
		{
			name:    "no cash amount",
			mutate:  func(p *event.CorporateActionPayload) { p.CashAmount = 0 },
			wantErr: "cash amount must be positive",
		},
		{
			name:    "negative cash amount",
			mutate:  func(p *event.CorporateActionPayload) { p.CashAmount = -1 },
			wantErr: "cash amount must be positive",
		},
		{
			name:    "non-finite cash amount",
			mutate:  func(p *event.CorporateActionPayload) { p.CashAmount = math.NaN() },
			wantErr: "cash amount must be positive",
		},
		{
			name:    "no currency",
			mutate:  func(p *event.CorporateActionPayload) { p.Currency = "" },
			wantErr: "currency is required for a dividend",
		},
		{
			name:    "a dividend carrying split terms",
			mutate:  func(p *event.CorporateActionPayload) { p.RawSharesLost = 1 },
			wantErr: "a dividend carries no split or symbol change terms",
		},
		{
			name:    "a dividend carrying a new instrument id",
			mutate:  func(p *event.CorporateActionPayload) { p.NewInstrumentID = "AAPL2" },
			wantErr: "a dividend carries no split or symbol change terms",
		},
		{
			name:    "a delisting carrying a cash amount",
			mutate:  func(p *event.CorporateActionPayload) { *p = validCorporateAction(); p.CashAmount = 1 },
			wantErr: "a delisting carries no symbol change or dividend terms",
		},
		{
			name:    "a split carrying a cash amount",
			mutate:  func(p *event.CorporateActionPayload) { *p = validSplitAction(); p.CashAmount = 1 },
			wantErr: "a split carries no symbol change or dividend terms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validDividendAction()
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

// TestUpcastCorporateActionDividend: schema 3 recognises a dividend, and
// schema 2 (which could not) refuses one.
func TestUpcastCorporateActionDividend(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validDividendAction())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	got, err := event.UpcastCorporateActionPayload(3, encoded)
	if err != nil {
		t.Fatalf("UpcastCorporateActionPayload(3, dividend) error = %v", err)
	}
	if got != validDividendAction() {
		t.Fatalf("decoded = %+v, want %+v", got, validDividendAction())
	}

	// Schema 2's own shape has no cash_amount field at all, so a dividend
	// recorded at schema 2 is refused as an unknown field, not merely an
	// unrecognised kind: it could not genuinely have been produced by a
	// schema-2 build.
	_, err = event.UpcastCorporateActionPayload(2, []byte(`{"instrument_id":"AAPL","kind":"dividend","effective_at":"2026-03-02T04:00:00Z","cash_amount":184,"currency":"USD"}`))
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("UpcastCorporateActionPayload(2, dividend) error = %v, want it refused: schema 2 has no cash_amount field", err)
	}
}

func validCampaignDividend() event.CampaignDividendPayload {
	return event.CampaignDividendPayload{
		CampaignID:        "campaign:AAPL:2026-02-01T20:00:00.000000000Z",
		InstrumentID:      "AAPL",
		CorporateActionID: "run:corporate-action:9",
		EffectiveAt:       time.Date(2026, time.March, 2, 4, 0, 0, 0, time.UTC),
		CashAmount:        184.0,
		Currency:          "USD",
		Rule:              event.RuleDividendCreditedAsCash,
		ADR:               event.ADRDividendCreditedAsCash,
	}
}

func TestCampaignDividendPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CampaignDividendPayload)
		wantErr string
	}{
		{name: "valid dividend"},
		{name: "no campaign", mutate: func(p *event.CampaignDividendPayload) { p.CampaignID = "" }, wantErr: "campaign id"},
		{name: "no instrument", mutate: func(p *event.CampaignDividendPayload) { p.InstrumentID = "" }, wantErr: "instrument id"},
		{name: "no corporate action", mutate: func(p *event.CampaignDividendPayload) { p.CorporateActionID = "" }, wantErr: "corporate action id"},
		{name: "no effective time", mutate: func(p *event.CampaignDividendPayload) { p.EffectiveAt = time.Time{} }, wantErr: "effective at"},
		{name: "no cash amount", mutate: func(p *event.CampaignDividendPayload) { p.CashAmount = 0 }, wantErr: "cash amount"},
		{name: "negative cash amount", mutate: func(p *event.CampaignDividendPayload) { p.CashAmount = -1 }, wantErr: "cash amount"},
		{name: "non-finite cash amount", mutate: func(p *event.CampaignDividendPayload) { p.CashAmount = math.Inf(1) }, wantErr: "cash amount"},
		{name: "no currency", mutate: func(p *event.CampaignDividendPayload) { p.Currency = "" }, wantErr: "currency"},
		{name: "no rule", mutate: func(p *event.CampaignDividendPayload) { p.Rule = "" }, wantErr: "rule"},
		{name: "no adr", mutate: func(p *event.CampaignDividendPayload) { p.ADR = "" }, wantErr: "adr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validCampaignDividend()
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

func TestCampaignDividendPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	err := event.CampaignDividendPayload{}.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want an error for a wholly empty payload")
	}
	for _, want := range []string{"campaign id", "instrument id", "corporate action id", "effective at", "cash amount", "currency", "rule", "adr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want it to mention %q", err, want)
		}
	}
}
