package event_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the tests for a split's cash in lieu (ADR 0023): the split
// kind of market.corporate-action, its schema-1 upcaster, and the
// strategy.campaign.cash-in-lieu decision the reducer records for it.

// validSplitAction is the rounded 7-for-1 of ADR 0023's Context in the
// engine's own terms: one raw share lost, four split-adjusted shares a raw
// share after the split, and the 0.99 of a share LEAN kept paid as cash.
func validSplitAction() event.CorporateActionPayload {
	return event.CorporateActionPayload{
		InstrumentID:            "AAPL",
		Kind:                    event.CorporateActionKindSplit,
		EffectiveAt:             time.Date(2014, time.June, 9, 4, 0, 0, 0, time.UTC),
		NewShares:               7,
		OldShares:               1,
		EngineSharesPerRawShare: 4,
		RawSharesLost:           1,
		CashInLieu:              91.45,
		Currency:                "USD",
	}
}

func TestSplitCorporateActionValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CorporateActionPayload)
		wantErr string
	}{
		{name: "valid split with cash in lieu"},
		{
			// The 2005 2-for-1: no share lost, a fraction paid as cash.
			name:   "cash with no share lost",
			mutate: func(p *event.CorporateActionPayload) { p.RawSharesLost = 0; p.CashInLieu = 2.75 },
		},
		{
			name:   "an exact split: nothing lost and nothing paid",
			mutate: func(p *event.CorporateActionPayload) { p.RawSharesLost = 0; p.CashInLieu = 0 },
		},
		{
			// ADR 0023 admits any ratio; only the adapter restricts it.
			name:   "a reverse split",
			mutate: func(p *event.CorporateActionPayload) { p.NewShares, p.OldShares = 1, 10 },
		},
		{
			name:    "no new shares",
			mutate:  func(p *event.CorporateActionPayload) { p.NewShares = 0 },
			wantErr: "new shares",
		},
		{
			name:    "no old shares",
			mutate:  func(p *event.CorporateActionPayload) { p.OldShares = -1 },
			wantErr: "old shares",
		},
		{
			name:    "a one-for-one split changes no share",
			mutate:  func(p *event.CorporateActionPayload) { p.NewShares, p.OldShares = 3, 3 },
			wantErr: "changes no share",
		},
		{
			name:    "no engine shares per raw share",
			mutate:  func(p *event.CorporateActionPayload) { p.EngineSharesPerRawShare = 0 },
			wantErr: "engine shares per raw share",
		},
		{
			name:    "negative shares lost",
			mutate:  func(p *event.CorporateActionPayload) { p.RawSharesLost = -1 },
			wantErr: "raw shares lost",
		},
		{
			name:    "negative cash",
			mutate:  func(p *event.CorporateActionPayload) { p.CashInLieu = -0.01 },
			wantErr: "cash in lieu",
		},
		{
			name:    "non-finite cash",
			mutate:  func(p *event.CorporateActionPayload) { p.CashInLieu = math.Inf(1) },
			wantErr: "cash in lieu",
		},
		{
			// ADR 0023: a share taken with nothing paid is not cash in lieu.
			name:    "a share lost with no cash paid",
			mutate:  func(p *event.CorporateActionPayload) { p.CashInLieu = 0 },
			wantErr: "no cash",
		},
		{
			name:    "no currency",
			mutate:  func(p *event.CorporateActionPayload) { p.Currency = "" },
			wantErr: "currency",
		},
		{
			name:    "a delisting carrying split terms",
			mutate:  func(p *event.CorporateActionPayload) { p.Kind = event.CorporateActionKindDelisting },
			wantErr: "a delisting carries no split terms",
		},
		{
			name:    "a delisting carrying only a currency",
			mutate:  func(p *event.CorporateActionPayload) { *p = validCorporateAction(); p.Currency = "USD" },
			wantErr: "a delisting carries no split terms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validSplitAction()
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

// TestUpcastCorporateActionPayload is ADR 0015's upcaster for the schema-2
// bump: a schema-1 record could only be a delisting, so it reads as the
// schema-2 delisting it is; anything a schema-1 record could not have said,
// and any schema this build does not know, is refused.
func TestUpcastCorporateActionPayload(t *testing.T) {
	t.Parallel()

	v1Delisting := []byte(`{"instrument_id":"AAPL","kind":"delisting","effective_at":"2026-02-27T00:00:00Z"}`)
	got, err := event.UpcastCorporateActionPayload(1, v1Delisting)
	if err != nil {
		t.Fatalf("UpcastCorporateActionPayload(1, delisting) error = %v", err)
	}
	if got != validCorporateAction() {
		t.Fatalf("upcast = %+v, want %+v", got, validCorporateAction())
	}

	split, err := json.Marshal(validSplitAction())
	if err != nil {
		t.Fatal(err)
	}
	got, err = event.UpcastCorporateActionPayload(2, split)
	if err != nil {
		t.Fatalf("UpcastCorporateActionPayload(2, split) error = %v", err)
	}
	if got != validSplitAction() {
		t.Fatalf("decoded = %+v, want %+v", got, validSplitAction())
	}

	for _, tt := range []struct {
		name    string
		version uint32
		payload []byte
		wantErr string
	}{
		{"a schema-1 split", 1, []byte(`{"instrument_id":"AAPL","kind":"split","effective_at":"2026-02-27T00:00:00Z"}`), "schema-1"},
		{"a schema-1 record carrying schema-2 fields", 1, split, "schema-1"},
		{"a schema-1 record that is not JSON", 1, []byte(`{`), "decode"},
		{"a schema-1 record that is invalid", 1, []byte(`{"instrument_id":"","kind":"delisting","effective_at":"2026-02-27T00:00:00Z"}`), "instrument id"},
		{"a schema-2 record followed by another", 2, []byte(`{"instrument_id":"AAPL","kind":"delisting","effective_at":"2026-02-27T00:00:00Z"} {}`), "trailing"},
		{"a schema-2 record with an unknown field", 2, []byte(`{"instrument_id":"AAPL","kind":"delisting","effective_at":"2026-02-27T00:00:00Z","price":1}`), "decode"},
		{"a schema-2 record that is invalid", 2, []byte(`{"instrument_id":"AAPL","kind":"merger","effective_at":"2026-02-27T00:00:00Z"}`), "can only be"},
		{"schema 0", 0, v1Delisting, "schema version 0"},
		{"a newer schema", 4, v1Delisting, "newer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := event.UpcastCorporateActionPayload(tt.version, tt.payload)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("UpcastCorporateActionPayload(%d) error = %v, want substring %q", tt.version, err, tt.wantErr)
			}
		})
	}
}

// validCampaignCashInLieu is two raw shares lost at four split-adjusted
// shares a raw share: Units 4 and 3, the two most recent, lose one raw share
// each (ADR 0023).
func validCampaignCashInLieu() event.CampaignCashInLieuPayload {
	return event.CampaignCashInLieuPayload{
		CampaignID:              "campaign:AAPL:2014-05-01T20:00:00.000000000Z",
		InstrumentID:            "AAPL",
		CorporateActionID:       "run:corporate-action:9",
		EffectiveAt:             time.Date(2014, time.June, 9, 4, 0, 0, 0, time.UTC),
		NewShares:               7,
		OldShares:               1,
		EngineSharesPerRawShare: 4,
		RawSharesLost:           2,
		EngineSharesLost:        8,
		CashInLieu:              180.5,
		Currency:                "USD",
		Reductions: []event.UnitReduction{
			{UnitIndex: 4, QuantityBefore: 24612, QuantityAfter: 24608},
			{UnitIndex: 3, QuantityBefore: 24612, QuantityAfter: 24608},
		},
		QuantityBefore: 98448,
		QuantityAfter:  98440,
		Rule:           event.RuleCashInLieuMostRecentUnitsFirst,
		ADR:            event.ADRSplitCashInLieu,
	}
}

func TestCampaignCashInLieuPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CampaignCashInLieuPayload)
		wantErr string
	}{
		{name: "two Units reduced, most recent first"},
		{
			name: "cash with no share lost reduces no Unit",
			mutate: func(p *event.CampaignCashInLieuPayload) {
				p.RawSharesLost, p.EngineSharesLost, p.Reductions = 0, 0, nil
				p.QuantityAfter = p.QuantityBefore
			},
		},
		{name: "no campaign", mutate: func(p *event.CampaignCashInLieuPayload) { p.CampaignID = "" }, wantErr: "campaign id"},
		{name: "no instrument", mutate: func(p *event.CampaignCashInLieuPayload) { p.InstrumentID = "" }, wantErr: "instrument id"},
		{name: "no corporate action", mutate: func(p *event.CampaignCashInLieuPayload) { p.CorporateActionID = "" }, wantErr: "corporate action id"},
		{name: "no effective time", mutate: func(p *event.CampaignCashInLieuPayload) { p.EffectiveAt = time.Time{} }, wantErr: "effective at"},
		{name: "no ratio", mutate: func(p *event.CampaignCashInLieuPayload) { p.NewShares = 0 }, wantErr: "new shares"},
		{name: "no old shares", mutate: func(p *event.CampaignCashInLieuPayload) { p.OldShares = 0 }, wantErr: "old shares"},
		{name: "no conversion", mutate: func(p *event.CampaignCashInLieuPayload) { p.EngineSharesPerRawShare = 0 }, wantErr: "engine shares per raw share"},
		{name: "negative shares lost", mutate: func(p *event.CampaignCashInLieuPayload) { p.RawSharesLost = -1 }, wantErr: "raw shares lost"},
		{
			// A decision is recorded only when the split paid something.
			name:    "no cash",
			mutate:  func(p *event.CampaignCashInLieuPayload) { p.CashInLieu = 0 },
			wantErr: "cash in lieu must be positive",
		},
		{name: "non-finite cash", mutate: func(p *event.CampaignCashInLieuPayload) { p.CashInLieu = math.NaN() }, wantErr: "cash in lieu"},
		{name: "no currency", mutate: func(p *event.CampaignCashInLieuPayload) { p.Currency = "" }, wantErr: "currency"},
		{
			name:    "one reduction per raw share lost",
			mutate:  func(p *event.CampaignCashInLieuPayload) { p.Reductions = p.Reductions[:1] },
			wantErr: "one reduction per raw share lost",
		},
		{
			name: "the most recent Unit first",
			mutate: func(p *event.CampaignCashInLieuPayload) {
				p.Reductions[0], p.Reductions[1] = p.Reductions[1], p.Reductions[0]
			},
			wantErr: "most recent Unit first",
		},
		{
			name:    "a Unit index of zero",
			mutate:  func(p *event.CampaignCashInLieuPayload) { p.Reductions[1].UnitIndex = 0 },
			wantErr: "unit index",
		},
		{
			name:    "a reduction of other than one raw share",
			mutate:  func(p *event.CampaignCashInLieuPayload) { p.Reductions[0].QuantityAfter = 24604 },
			wantErr: "exactly one raw share",
		},
		{
			name: "a Unit reduced to nothing",
			mutate: func(p *event.CampaignCashInLieuPayload) {
				p.Reductions[0] = event.UnitReduction{UnitIndex: 4, QuantityBefore: 4, QuantityAfter: 0}
			},
			wantErr: "keeps at least one share",
		},
		{
			name:    "engine shares lost disagreeing with the reductions",
			mutate:  func(p *event.CampaignCashInLieuPayload) { p.EngineSharesLost = 4 },
			wantErr: "engine shares lost",
		},
		{
			name:    "a holding after disagreeing with the shares lost",
			mutate:  func(p *event.CampaignCashInLieuPayload) { p.QuantityAfter = 98444 },
			wantErr: "quantity after",
		},
		{
			name:    "a holding reduced to nothing",
			mutate:  func(p *event.CampaignCashInLieuPayload) { p.QuantityBefore, p.QuantityAfter = 8, 0 },
			wantErr: "quantity after must be positive",
		},
		{name: "no rule", mutate: func(p *event.CampaignCashInLieuPayload) { p.Rule = "" }, wantErr: "rule"},
		{name: "no adr", mutate: func(p *event.CampaignCashInLieuPayload) { p.ADR = "" }, wantErr: "adr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validCampaignCashInLieu()
			payload.Reductions = append([]event.UnitReduction(nil), payload.Reductions...)
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

// TestCampaignCashInLieuRefusesOverflowingShareCounts keeps the exact-sum
// checks exact at the int64 boundary: a reduction or a holding that would
// wrap is refused, never accepted as a small number.
func TestCampaignCashInLieuRefusesOverflowingShareCounts(t *testing.T) {
	t.Parallel()

	payload := validCampaignCashInLieu()
	payload.EngineSharesPerRawShare = math.MaxInt64
	payload.Reductions = []event.UnitReduction{
		{UnitIndex: 4, QuantityBefore: math.MaxInt64, QuantityAfter: 0},
		{UnitIndex: 3, QuantityBefore: math.MaxInt64, QuantityAfter: 0},
	}
	if err := payload.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want a refusal for share counts past int64")
	}

	payload = validCampaignCashInLieu()
	payload.EngineSharesPerRawShare = math.MaxInt64 - 1
	payload.Reductions = []event.UnitReduction{
		{UnitIndex: 4, QuantityBefore: math.MaxInt64, QuantityAfter: 1},
		{UnitIndex: 3, QuantityBefore: math.MaxInt64, QuantityAfter: 1},
	}
	payload.EngineSharesLost = 2
	err := payload.Validate()
	if err == nil || !strings.Contains(err.Error(), "engine shares lost") {
		t.Fatalf("Validate() error = %v, want the wrapped sum refused", err)
	}
}

func TestCampaignCashInLieuEventConstants(t *testing.T) {
	t.Parallel()

	if event.CampaignCashInLieuEventType != "strategy.campaign.cash-in-lieu" {
		t.Errorf("CampaignCashInLieuEventType = %q", event.CampaignCashInLieuEventType)
	}
	if event.CampaignCashInLieuSchemaVersion != 1 {
		t.Errorf("CampaignCashInLieuSchemaVersion = %d, want 1", event.CampaignCashInLieuSchemaVersion)
	}
	if event.ADRSplitCashInLieu != "0023" {
		t.Errorf("ADRSplitCashInLieu = %q, want 0023", event.ADRSplitCashInLieu)
	}
}
