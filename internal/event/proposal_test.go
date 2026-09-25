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

// proposalPeriodEnd is the completed bar the fixtures below belong to.
var proposalPeriodEnd = time.Date(2026, time.February, 27, 0, 0, 0, 0, time.UTC)

// proposalN is the N the internal/strategy breakout fixture produces on its
// breakout bar. Reusing it here keeps the payload fixtures and the event-seam
// fixtures arithmetically continuous rather than independently invented.
//
// It is deliberately a float64 variable rather than an untyped constant.
// Go evaluates untyped constant expressions in arbitrary precision and rounds
// once at the end, so a constant `200 - 3*proposalN` can differ in the last
// bit from the float64 arithmetic a producer actually performs — and
// Validate's derivation checks are exact by design. A fixture built from
// constants would therefore be testing constant folding rather than the
// producer's arithmetic.
var proposalN = 37.57779214788228

// validTradeProposal returns a Baseline (volatility-normalised, ADR 0003)
// trade proposal that satisfies every validation rule, so each table row only
// has to describe its one deviation.
//
// Hand-worked: a $1,000,000 Notional Account at the Baseline's 0.5 % Unit
// Volatility Fraction gives a 1N budget of $5,000; 5,000 / 37.5777... =
// 133.07..., truncated to 133 shares (The Turtle Rules p.14). Risk at Stop is
// derived, never configured: 0.005 x 2 = 0.01. The realised figure is what
// those 133 whole shares actually risk, 133 x (2 x 37.5777...) / 1,000,000 =
// 0.00999..., a little under the budget because of the truncation. The
// Protective Stop intent is the entry level less two N: 200 - 2 x 37.5777...
// = 124.84...
func validTradeProposal() event.TradeProposalPayload {
	return event.TradeProposalPayload{
		InstrumentID:           "AAPL",
		PeriodEnd:              proposalPeriodEnd,
		SignalID:               "signal:AAPL:2026-02-27T00:00:00.000000000Z",
		Rule:                   event.RuleUnitSizingVolatilityNormalised,
		ADR:                    event.ADRUnitSizing,
		Direction:              event.DirectionLong,
		EntryLevel:             200,
		Quantity:               133,
		N:                      proposalN,
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2,
		RiskAtStop:             0.005 * 2,
		RealisedRiskAtStop:     133 * (2 * proposalN * 1) / 1_000_000,
		DollarsPerPoint:        1,
		NotionalAccount:        1_000_000,
		ProtectiveStopIntent:   200 - float64(2*proposalN),
	}
}

// validFixedRiskTradeProposal is the Sublime Variant's counterpart [M p.56]:
// a 2 % Risk at Stop with a 3N stop gives 1,000,000 x 0.02 / (3 x 37.5777...)
// = 177.4... -> 177 shares, and Risk at Stop is the configured input rather
// than a derivation from the Unit Volatility Fraction.
func validFixedRiskTradeProposal() event.TradeProposalPayload {
	p := validTradeProposal()
	p.Rule = event.RuleUnitSizingFixedRiskAtStop
	p.SizingMode = event.SizingModeFixedRiskAtStop
	p.StopMultiple = 3
	p.RiskAtStop = 0.02
	p.Quantity = 177
	p.RealisedRiskAtStop = 177 * (3 * proposalN * 1) / 1_000_000
	p.ProtectiveStopIntent = 200 - float64(3*proposalN)
	return p
}

func TestTradeProposalPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// base defaults to validTradeProposal; rows that exercise the
		// Sublime Variant set it to validFixedRiskTradeProposal.
		base    func() event.TradeProposalPayload
		mutate  func(*event.TradeProposalPayload)
		wantErr string
	}{
		{name: "valid baseline proposal"},
		{name: "valid fixed-risk-at-stop proposal", base: validFixedRiskTradeProposal},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.TradeProposalPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing period end",
			mutate:  func(p *event.TradeProposalPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			// The proposal is the audit link back to the decision that
			// caused it. Without the Signal's envelope ID a journal reader
			// cannot tell which Signal produced which position.
			name:    "missing signal id",
			mutate:  func(p *event.TradeProposalPayload) { p.SignalID = "" },
			wantErr: "signal id",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.TradeProposalPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.TradeProposalPayload) { p.ADR = "" },
			wantErr: "adr",
		},
		{
			name:    "unrecognised direction",
			mutate:  func(p *event.TradeProposalPayload) { p.Direction = "short" },
			wantErr: "direction",
		},
		{
			name:    "unrecognised sizing mode",
			mutate:  func(p *event.TradeProposalPayload) { p.SizingMode = "risk-parity" },
			wantErr: "sizing mode",
		},
		{
			// A proposal is an instruction to buy. Zero shares is not a
			// smaller instruction, it is the absence of one, and must be
			// recorded as a decline instead (ProposalDeclinedPayload).
			name:    "zero quantity",
			mutate:  func(p *event.TradeProposalPayload) { p.Quantity = 0 },
			wantErr: "quantity must be a positive whole number",
		},
		{
			name:    "negative quantity",
			mutate:  func(p *event.TradeProposalPayload) { p.Quantity = -122 },
			wantErr: "quantity must be a positive whole number",
		},
		{
			name:    "zero n",
			mutate:  func(p *event.TradeProposalPayload) { p.N = 0 },
			wantErr: "n must be positive",
		},
		{
			name:    "zero entry level",
			mutate:  func(p *event.TradeProposalPayload) { p.EntryLevel = 0 },
			wantErr: "entry level must be positive",
		},
		{
			name:    "zero dollars per point",
			mutate:  func(p *event.TradeProposalPayload) { p.DollarsPerPoint = 0 },
			wantErr: "dollars per point must be positive",
		},
		{
			name:    "zero notional account",
			mutate:  func(p *event.TradeProposalPayload) { p.NotionalAccount = 0 },
			wantErr: "notional account must be positive",
		},
		{
			name:    "zero unit volatility fraction",
			mutate:  func(p *event.TradeProposalPayload) { p.UnitVolatilityFraction = 0 },
			wantErr: "unit volatility fraction",
		},
		{
			name:    "zero stop multiple",
			mutate:  func(p *event.TradeProposalPayload) { p.StopMultiple = 0 },
			wantErr: "stop multiple must be positive",
		},
		{
			name:    "zero risk at stop",
			mutate:  func(p *event.TradeProposalPayload) { p.RiskAtStop = 0 },
			wantErr: "risk at stop",
		},
		{
			name:    "risk at stop above the whole account",
			mutate:  func(p *event.TradeProposalPayload) { p.RiskAtStop = 1.5 },
			wantErr: "risk at stop",
		},
		{
			// The ticket's headline payload invariant: the stated Risk at
			// Stop must be the value the derivation produces (ADR 0003:
			// derived, never configured). 0.005 x 2 is 0.01, so 0.015 is a
			// number nothing in this payload justifies.
			name:    "stated risk at stop contradicts the derivation",
			mutate:  func(p *event.TradeProposalPayload) { p.RiskAtStop = 0.015 },
			wantErr: "does not match the derivation",
		},
		{
			// The same check must not fire on a fixed-risk-at-stop proposal,
			// where Risk at Stop is the input and is deliberately unrelated
			// to Unit Volatility Fraction x Stop Multiple (0.005 x 3 =
			// 0.015, not the configured 0.02).
			name:    "fixed-risk-at-stop does not derive from the unit volatility fraction",
			base:    validFixedRiskTradeProposal,
			wantErr: "",
		},
		{
			name:    "protective stop intent above the entry level",
			mutate:  func(p *event.TradeProposalPayload) { p.ProtectiveStopIntent = 210 },
			wantErr: "protective stop intent",
		},
		{
			name:    "protective stop intent equal to the entry level",
			mutate:  func(p *event.TradeProposalPayload) { p.ProtectiveStopIntent = 200 },
			wantErr: "protective stop intent",
		},
		{
			// A long equity cannot fall below zero, so a Protective Stop at
			// or below zero is unreachable: the Unit would in fact risk the
			// whole position, not the derived fraction.
			name: "protective stop intent at or below zero",
			mutate: func(p *event.TradeProposalPayload) {
				p.EntryLevel = 50
				p.N = 30
				p.ProtectiveStopIntent = 50 - 2*30
				p.Quantity = 1
			},
			wantErr: "protective stop intent must be positive",
		},
		{
			// The Protective Stop intent is derived too (entry less Stop
			// Multiple x N), so a stated value that is merely "below entry"
			// but not the derived level is still invalid.
			name:    "protective stop intent contradicts the derivation",
			mutate:  func(p *event.TradeProposalPayload) { p.ProtectiveStopIntent = 150 },
			wantErr: "does not match the derivation",
		},
		{
			// Truncation may only ever risk LESS than the budget. 134 shares
			// at 37.5777... is 5,035.4..., above the $5,000 1N budget, so
			// this payload claims a Unit larger than its own parameters
			// permit. (The realised-risk check below catches it too; both
			// are asserted because each is exact in its own mode's terms.)
			name: "quantity exceeds the volatility-normalised budget",
			mutate: func(p *event.TradeProposalPayload) {
				p.Quantity = 134
				p.RealisedRiskAtStop = 134 * (2 * proposalN * 1) / 1_000_000
			},
			wantErr: "exceeds the",
		},
		{
			// The same overshoot in the other mode: 178 shares at a 3N stop
			// is 20,066.5..., above the $20,000 Risk-at-Stop budget.
			name: "quantity exceeds the fixed-risk-at-stop budget",
			base: validFixedRiskTradeProposal,
			mutate: func(p *event.TradeProposalPayload) {
				p.Quantity = 178
				p.RealisedRiskAtStop = 178 * (3 * proposalN * 1) / 1_000_000
			},
			wantErr: "exceeds the",
		},
		{
			// The realised figure is derived from the payload's own fields
			// exactly as Risk at Stop and the Protective Stop intent are, so
			// a stated value the quantity does not support is rejected. 0.005
			// is what 133 shares would risk only if N were half what the
			// payload says it is.
			name:    "stated realised risk does not match the derivation",
			mutate:  func(p *event.TradeProposalPayload) { p.RealisedRiskAtStop = 0.005 },
			wantErr: "does not match the derivation",
		},
		{
			name:    "stated realised risk does not match the derivation under fixed-risk-at-stop",
			base:    validFixedRiskTradeProposal,
			mutate:  func(p *event.TradeProposalPayload) { p.RealisedRiskAtStop = 0.01 },
			wantErr: "does not match the derivation",
		},
		{
			// A realised figure above the declared budget contradicts the
			// direction truncation can only ever move in. Contrived here by
			// shrinking the declared budget rather than the quantity, so
			// that the derivation check still passes and this check is the
			// one that fires.
			name: "realised risk exceeds the declared budget",
			mutate: func(p *event.TradeProposalPayload) {
				p.UnitVolatilityFraction = 0.001
				p.RiskAtStop = 0.001 * 2
			},
			wantErr: "exceeds the declared risk at stop",
		},
		{
			name:    "zero realised risk at stop",
			mutate:  func(p *event.TradeProposalPayload) { p.RealisedRiskAtStop = 0 },
			wantErr: "realised risk at stop",
		},
		{
			name:    "negative realised risk at stop",
			mutate:  func(p *event.TradeProposalPayload) { p.RealisedRiskAtStop = -0.01 },
			wantErr: "realised risk at stop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			base := tt.base
			if base == nil {
				base = validTradeProposal
			}
			payload := base()
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

func TestTradeProposalPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(p *event.TradeProposalPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{"entry level", func(p *event.TradeProposalPayload, f float64) { p.EntryLevel = f }, "entry level must be finite"},
		{"n", func(p *event.TradeProposalPayload, f float64) { p.N = f }, "n must be finite"},
		{"unit volatility fraction", func(p *event.TradeProposalPayload, f float64) { p.UnitVolatilityFraction = f }, "unit volatility fraction must be finite"},
		{"stop multiple", func(p *event.TradeProposalPayload, f float64) { p.StopMultiple = f }, "stop multiple must be finite"},
		{"risk at stop", func(p *event.TradeProposalPayload, f float64) { p.RiskAtStop = f }, "risk at stop must be finite"},
		{"realised risk at stop", func(p *event.TradeProposalPayload, f float64) { p.RealisedRiskAtStop = f }, "realised risk at stop must be finite"},
		{"dollars per point", func(p *event.TradeProposalPayload, f float64) { p.DollarsPerPoint = f }, "dollars per point must be finite"},
		{"notional account", func(p *event.TradeProposalPayload, f float64) { p.NotionalAccount = f }, "notional account must be finite"},
		{"protective stop intent", func(p *event.TradeProposalPayload, f float64) { p.ProtectiveStopIntent = f }, "protective stop intent must be finite"},
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
				payload := validTradeProposal()
				field.apply(&payload, nf.value)

				err := payload.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
	}
}

func TestTradeProposalPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.TradeProposalPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"instrument id",
		"period end",
		"signal id",
		"rule",
		"adr",
		"direction",
		"sizing mode",
		"quantity",
		"entry level",
		"n must be positive",
		"unit volatility fraction",
		"stop multiple",
		"risk at stop",
		"realised risk at stop",
		"dollars per point",
		"notional account",
		"protective stop intent",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestTradeProposalEventConstants(t *testing.T) {
	t.Parallel()

	if event.TradeProposalEventType != "strategy.trade.proposed" {
		t.Errorf("TradeProposalEventType = %q, want %q", event.TradeProposalEventType, "strategy.trade.proposed")
	}
	if event.TradeProposalSchemaVersion != 1 {
		t.Errorf("TradeProposalSchemaVersion = %d, want 1 (a new payload starts at 1)", event.TradeProposalSchemaVersion)
	}
	// The rule names say what the rule computes, not which vendor's system
	// it resembles — #9's finding, applied to sizing: a Variant that changes
	// the Stop Multiple is still volatility-normalised sizing, and the name
	// must not imply otherwise.
	if event.RuleUnitSizingVolatilityNormalised != "unit.sizing.volatility-normalised" {
		t.Errorf("RuleUnitSizingVolatilityNormalised = %q", event.RuleUnitSizingVolatilityNormalised)
	}
	if event.RuleUnitSizingFixedRiskAtStop != "unit.sizing.fixed-risk-at-stop" {
		t.Errorf("RuleUnitSizingFixedRiskAtStop = %q", event.RuleUnitSizingFixedRiskAtStop)
	}
	// ADR 0003 is the decision that defines volatility-normalised sizing and
	// makes the Sizing Mode explicit; it governs both modes.
	if event.ADRUnitSizing != "0003" {
		t.Errorf("ADRUnitSizing = %q, want %q", event.ADRUnitSizing, "0003")
	}
}

func TestTradeProposalPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validTradeProposal()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.TradeProposalPayload
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

func TestTradeProposalPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validTradeProposal())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"instrument_id",
		"period_end",
		"signal_id",
		"rule",
		"adr",
		"direction",
		"entry_level",
		"quantity",
		"n",
		"sizing_mode",
		"unit_volatility_fraction",
		"stop_multiple",
		"risk_at_stop",
		"realised_risk_at_stop",
		"dollars_per_point",
		"notional_account",
		"protective_stop_intent",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}

// validProposalDeclined returns a decline that satisfies every validation
// rule. A decline is not an error: it is the journal record of a Signal that
// fired and deliberately produced no position, so that "nothing happened" is
// never indistinguishable from "the sizing step silently dropped it".
func validProposalDeclined() event.ProposalDeclinedPayload {
	return event.ProposalDeclinedPayload{
		InstrumentID: "AAPL",
		PeriodEnd:    proposalPeriodEnd,
		Kind:         event.ProposalDeclinedKindEntry,
		SignalID:     "signal:AAPL:2026-02-27T00:00:00.000000000Z",
		Reason:       event.DeclineReasonQuantityBelowOneUnit,
		Detail:       "notional account 100.00 at unit volatility fraction 0.005000 sizes 0 shares at n 40.698903",
	}
}

// validProposalDeclinedInsufficientCash mirrors validProposalDeclined for
// the add-kind, insufficient-cash shape: no SignalID, a CampaignID instead,
// and RequiredCash strictly above AvailableCash.
func validProposalDeclinedInsufficientCash() event.ProposalDeclinedPayload {
	return event.ProposalDeclinedPayload{
		InstrumentID:  "AAPL",
		PeriodEnd:     proposalPeriodEnd,
		Kind:          event.ProposalDeclinedKindAdd,
		CampaignID:    "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		Reason:        event.DeclineReasonInsufficientCash,
		Detail:        "unit cost 26,700.00 (200 shares x 133.50 x 1) exceeds available cash 10,000.00",
		RequiredCash:  26_700,
		AvailableCash: 10_000,
	}
}

// validProposalDeclinedUnitCapExceeded mirrors validProposalDeclined for the
// entry-kind, unit-cap-exceeded shape (ADR 0008, schema 5): PostTradeExposure
// strictly above CapLimit, exactly the comparison that makes the Unit
// excessive.
func validProposalDeclinedUnitCapExceeded() event.ProposalDeclinedPayload {
	return event.ProposalDeclinedPayload{
		InstrumentID:      "AAPL",
		PeriodEnd:         proposalPeriodEnd,
		Kind:              event.ProposalDeclinedKindEntry,
		SignalID:          "signal:AAPL:2026-02-27T00:00:00.000000000Z",
		Reason:            event.DeclineReasonUnitCapExceeded,
		Detail:            "unclassified-group post-trade exposure 11 exceeds the cap 10",
		Cap:               event.CapUnclassifiedGroup,
		CapLimit:          10,
		PostTradeExposure: 11,
	}
}

func TestProposalDeclinedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ProposalDeclinedPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "n not ready reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Reason = event.DeclineReasonNNotReady },
			wantErr: "",
		},
		{
			name:    "stop intent not positive reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Reason = event.DeclineReasonStopIntentNotPositive },
			wantErr: "",
		},
		{
			// The cost is the one figure this reason cannot state, so both
			// cash fields stay zero and the zero rule above applies to it.
			name:    "unit cost not representable reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Reason = event.DeclineReasonUnitCostNotRepresentable },
			wantErr: "",
		},
		{
			name: "unit cost not representable reason carrying a cash figure",
			mutate: func(p *event.ProposalDeclinedPayload) {
				p.Reason = event.DeclineReasonUnitCostNotRepresentable
				p.RequiredCash = math.Inf(1)
			},
			wantErr: "required cash must be zero",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing period end",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			name:    "missing signal id",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.SignalID = "" },
			wantErr: "signal id",
		},
		{
			name:    "unrecognised kind",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Kind = "because" },
			wantErr: "is not a recognised proposal declined kind",
		},
		{
			name:    "missing kind",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Kind = "" },
			wantErr: "is not a recognised proposal declined kind",
		},
		{
			name: "entry kind with a campaign id",
			mutate: func(p *event.ProposalDeclinedPayload) {
				p.CampaignID = "campaign:AAPL:2026-02-27T00:00:00.000000000Z"
			},
			wantErr: "campaign id must be empty for an entry-kind decline",
		},
		{
			// The reason is an enumerated constant, not free text: a
			// journal that can be queried for "how often was a Signal
			// declined because the account was too small" needs a closed
			// set, and Detail carries the specifics.
			name:    "unrecognised reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Reason = "because" },
			wantErr: "is not a recognised decline reason",
		},
		{
			name:    "missing reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Reason = "" },
			wantErr: "is not a recognised decline reason",
		},
		{
			name:    "missing detail",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Detail = "" },
			wantErr: "detail is required",
		},
		{
			name:    "required cash set for a non-cash reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.RequiredCash = 1 },
			wantErr: "required cash must be zero",
		},
		{
			name:    "available cash set for a non-cash reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.AvailableCash = 1 },
			wantErr: "available cash must be zero",
		},
		{
			// NaN and infinity are not zero, and the zero rule is what keeps
			// a field that means nothing for this reason from carrying a
			// number at all — so a non-finite value must be caught by it
			// rather than waved through as "not a finite non-zero".
			name:    "nan required cash for a non-cash reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.RequiredCash = math.NaN() },
			wantErr: "required cash must be zero",
		},
		{
			name:    "infinite required cash for a non-cash reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.RequiredCash = math.Inf(1) },
			wantErr: "required cash must be zero",
		},
		{
			name:    "nan available cash for a non-cash reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.AvailableCash = math.NaN() },
			wantErr: "available cash must be zero",
		},
		{
			name:    "infinite available cash for a non-cash reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.AvailableCash = math.Inf(-1) },
			wantErr: "available cash must be zero",
		},
		{
			name:    "cap set for a non-cap reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Cap = event.CapInstrument },
			wantErr: "cap must be empty",
		},
		{
			name:    "cap limit set for a non-cap reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.CapLimit = 4 },
			wantErr: "cap limit must be zero",
		},
		{
			name:    "post-trade exposure set for a non-cap reason",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.PostTradeExposure = 5 },
			wantErr: "post-trade exposure must be zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validProposalDeclined()
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

// TestProposalDeclinedPayloadValidateInsufficientCash covers the add-kind,
// insufficient-cash shape separately: it is the one combination the entry-kind
// table above cannot exercise (a required SignalID and a required CampaignID
// are mutually exclusive), and it pins the cash-figure invariants
// (DeclineReasonInsufficientCash's own doc comment).
func TestProposalDeclinedPayloadValidateInsufficientCash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ProposalDeclinedPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.CampaignID = "" },
			wantErr: "campaign id is required",
		},
		{
			name: "add kind with a signal id",
			mutate: func(p *event.ProposalDeclinedPayload) {
				p.SignalID = "signal:AAPL:2026-02-27T00:00:00.000000000Z"
			},
			wantErr: "signal id must be empty for an add-kind decline",
		},
		{
			name:    "nan required cash",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.RequiredCash = math.NaN() },
			wantErr: "required cash must be finite",
		},
		{
			name:    "negative required cash",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.RequiredCash = -1 },
			wantErr: "required cash must not be negative",
		},
		{
			name:    "nan available cash",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.AvailableCash = math.NaN() },
			wantErr: "available cash must be finite",
		},
		{
			// ADR 0020 floors the basis, never the fills taken from it, so
			// the balance left after a fill the basis could not fund is
			// recorded as it is.
			name:   "negative available cash",
			mutate: func(p *event.ProposalDeclinedPayload) { p.AvailableCash = -1 },
		},
		{
			name:    "infinite available cash",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.AvailableCash = math.Inf(-1) },
			wantErr: "available cash must be finite",
		},
		{
			// Reject an insufficient-cash claim at the affordable boundary:
			// cost exactly equal to available cash is AFFORDABLE, so a
			// decline claiming insufficient-cash at that figure is internally
			// inconsistent and must be rejected.
			name:    "required cash equal to available cash",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.RequiredCash = p.AvailableCash },
			wantErr: "does not exceed available cash",
		},
		{
			name:    "required cash below available cash",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.RequiredCash = p.AvailableCash - 1 },
			wantErr: "does not exceed available cash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validProposalDeclinedInsufficientCash()
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

// TestProposalDeclinedPayloadValidateUnitCapExceeded pins
// DeclineReasonUnitCapExceeded's own figure invariants (ADR 0008, schema 5):
// Cap must be one of the five recognised identities, CapLimit must be
// positive, and PostTradeExposure must strictly exceed it — the comparison
// that makes the decline auditable, mirroring
// TestProposalDeclinedPayloadValidateInsufficientCash's own shape for
// DeclineReasonInsufficientCash.
func TestProposalDeclinedPayloadValidateUnitCapExceeded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ProposalDeclinedPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "unrecognised cap",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Cap = "because" },
			wantErr: "is not a recognised cap identity",
		},
		{
			name:    "missing cap",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.Cap = "" },
			wantErr: "is not a recognised cap identity",
		},
		{
			name:    "zero cap limit",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.CapLimit = 0 },
			wantErr: "cap limit must be a positive integer",
		},
		{
			name:    "negative cap limit",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.CapLimit = -1 },
			wantErr: "cap limit must be a positive integer",
		},
		{
			// Reject a unit-cap-exceeded claim at the boundary: exposure
			// exactly equal to the cap is WITHIN it, so a decline claiming
			// unit-cap-exceeded at that figure is internally inconsistent.
			name:    "post-trade exposure equal to cap limit",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.PostTradeExposure = p.CapLimit },
			wantErr: "does not exceed the cap limit",
		},
		{
			name:    "post-trade exposure below cap limit",
			mutate:  func(p *event.ProposalDeclinedPayload) { p.PostTradeExposure = p.CapLimit - 1 },
			wantErr: "does not exceed the cap limit",
		},
		{
			name: "every recognised cap identity is accepted",
			mutate: func(p *event.ProposalDeclinedPayload) {
				p.Cap = event.CapTotalLong
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validProposalDeclinedUnitCapExceeded()
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

func TestProposalDeclinedEventConstants(t *testing.T) {
	t.Parallel()

	if event.ProposalDeclinedEventType != "strategy.proposal.declined" {
		t.Errorf("ProposalDeclinedEventType = %q, want %q", event.ProposalDeclinedEventType, "strategy.proposal.declined")
	}
	if event.ProposalDeclinedSchemaVersion != 5 {
		t.Errorf("ProposalDeclinedSchemaVersion = %d, want 5", event.ProposalDeclinedSchemaVersion)
	}
	for _, reason := range []string{
		event.DeclineReasonNNotReady,
		event.DeclineReasonQuantityBelowOneUnit,
		event.DeclineReasonStopIntentNotPositive,
		event.DeclineReasonInsufficientCash,
		event.DeclineReasonUnitCostNotRepresentable,
		event.DeclineReasonUnitCapExceeded,
	} {
		if reason == "" {
			t.Error("every decline reason constant must be a non-empty enumerated value")
		}
	}
	for _, kind := range []string{event.ProposalDeclinedKindEntry, event.ProposalDeclinedKindAdd} {
		if kind == "" {
			t.Error("every proposal declined kind constant must be a non-empty enumerated value")
		}
	}
	for _, cap := range []string{
		event.CapInstrument, event.CapIndustry, event.CapSector, event.CapUnclassifiedGroup, event.CapTotalLong,
	} {
		if cap == "" {
			t.Error("every cap identity constant must be a non-empty enumerated value")
		}
	}
}

func TestProposalDeclinedPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validProposalDeclined()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.ProposalDeclinedPayload
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

// TestProposalDeclinedUnitCapExceededPayloadRoundTrip is
// TestProposalDeclinedPayloadRoundTrip for the unit-cap-exceeded shape:
// schema 5's Cap/CapLimit/PostTradeExposure fields must survive
// marshal/unmarshal exactly, the same property the cash-shaped decline is
// already pinned for.
func TestProposalDeclinedUnitCapExceededPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validProposalDeclinedUnitCapExceeded()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.ProposalDeclinedPayload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded.Validate() error = %v", err)
	}
	if decoded != original {
		t.Fatalf("round trip changed the payload:\n  original: %+v\n  decoded:  %+v", original, decoded)
	}

	reEncoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-Marshal() error = %v", err)
	}
	if !bytes.Equal(encoded, reEncoded) {
		t.Fatalf("round trip not stable:\n  first:  %s\n  second: %s", encoded, reEncoded)
	}
}

func TestProposalDeclinedPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validProposalDeclined())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{"instrument_id", "period_end", "kind", "signal_id", "campaign_id", "reason", "detail", "required_cash", "available_cash", "cap", "cap_limit", "post_trade_exposure"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}

// --- #11: a proposal that was never filled expires with its bar ---

// expiredAt is the period end of the bar that supersedes the proposal above:
// the next completed bar for the same instrument.
var expiredAt = proposalPeriodEnd.AddDate(0, 0, 1)

// earliestFillAt is the period end of the bar BEFORE the proposal's own
// decision bar — the earliest instant at which an order for it could have
// executed (see event.ProposalExpiredPayload.EarliestFillAt's own doc
// comment).
var earliestFillAt = proposalPeriodEnd.AddDate(0, 0, -1)

// validProposalExpired returns the expiry of validTradeProposal, superseded by
// the next bar without a fill ever arriving for it (ADR 0011: a Signal belongs
// to one bar and expires with it, so the proposal derived from it does too).
func validProposalExpired() event.ProposalExpiredPayload {
	return event.ProposalExpiredPayload{
		InstrumentID:   "AAPL",
		Kind:           event.ProposalKindEntry,
		ProposalID:     "proposal:AAPL:2026-02-27T00:00:00.000000000Z",
		SignalID:       "signal:AAPL:2026-02-27T00:00:00.000000000Z",
		PeriodEnd:      proposalPeriodEnd,
		ExpiredAt:      expiredAt,
		EarliestFillAt: earliestFillAt,
		Rule:           event.RuleSignalExpiresWithItsBar,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByNextBar,
		Quantity:       133,
		Level:          200,
	}
}

// validExitProposalExpired returns #13's other Kind: the expiry of an exit
// proposal (strategy.exit.proposed) that superseded, itself superseded by the
// next bar without an exit fill ever arriving. Unlike an entry-kind expiry, it
// names no Signal at all: an exit proposal is not sized from one (see
// event.ExitProposalPayload).
func validExitProposalExpired() event.ProposalExpiredPayload {
	return event.ProposalExpiredPayload{
		InstrumentID:   "AAPL",
		Kind:           event.ProposalKindExit,
		ProposalID:     "exit-proposal:AAPL:2026-02-27T00:00:00.000000000Z",
		SignalID:       "",
		PeriodEnd:      proposalPeriodEnd,
		ExpiredAt:      expiredAt,
		EarliestFillAt: earliestFillAt,
		Rule:           event.RuleExitProposalExpiresWithItsBar,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByNextBar,
		Quantity:       133,
		Level:          180,
	}
}

func TestProposalExpiredPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ProposalExpiredPayload)
		wantErr string
	}{
		{name: "valid expiry"},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.ProposalExpiredPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			// Without the proposal's id the journal cannot say which proposal
			// expired, which is the whole reason this event exists: a fill
			// arriving after it is rejected, and a reviewer must be able to see
			// why.
			name:    "missing proposal id",
			mutate:  func(p *event.ProposalExpiredPayload) { p.ProposalID = "" },
			wantErr: "proposal id",
		},
		{
			name:    "missing signal id",
			mutate:  func(p *event.ProposalExpiredPayload) { p.SignalID = "" },
			wantErr: "signal id",
		},
		{
			name:    "missing kind",
			mutate:  func(p *event.ProposalExpiredPayload) { p.Kind = "" },
			wantErr: "kind",
		},
		{
			name:    "unrecognised kind",
			mutate:  func(p *event.ProposalExpiredPayload) { p.Kind = "bogus" },
			wantErr: "kind",
		},
		{
			// An exit-kind expiry names no Signal at all (see
			// event.ExitProposalPayload's doc comment): an exit proposal is not
			// sized from one, so a signal id on it would claim a decision chain
			// this expiry never had.
			name: "exit-kind expiry names a signal id",
			mutate: func(p *event.ProposalExpiredPayload) {
				p.Kind = event.ProposalKindExit
				p.SignalID = "signal:AAPL:2026-02-27T00:00:00.000000000Z"
			},
			wantErr: "signal id",
		},
		{
			name:    "missing period end",
			mutate:  func(p *event.ProposalExpiredPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			// Reject expiry at the execution window's excluded lower bound:
			// ExpiredAt must be strictly after EarliestFillAt for EVERY
			// Reason, including the ordinary next-bar one.
			name:    "expired at at the earliest fill at",
			mutate:  func(p *event.ProposalExpiredPayload) { p.ExpiredAt = p.EarliestFillAt },
			wantErr: "must be after the earliest instant",
		},
		{
			name:    "expired at before the earliest fill at",
			mutate:  func(p *event.ProposalExpiredPayload) { p.ExpiredAt = p.EarliestFillAt.AddDate(0, 0, -1) },
			wantErr: "must be after the earliest instant",
		},
		{
			// A next-bar expiry must still be strictly after PeriodEnd, even
			// though it is now comfortably after EarliestFillAt too.
			name:    "next-bar expiry at the period end",
			mutate:  func(p *event.ProposalExpiredPayload) { p.ExpiredAt = p.PeriodEnd },
			wantErr: "a proposal is superseded by a later bar",
		},
		{
			name:    "next-bar expiry before the period end",
			mutate:  func(p *event.ProposalExpiredPayload) { p.ExpiredAt = p.PeriodEnd.AddDate(0, 0, -1) },
			wantErr: "a proposal is superseded by a later bar",
		},
		{
			// Reject a stop-superseded entry expiry: superseded-by-stop
			// applies only to an Add proposal, not an entry or exit proposal.
			name:    "superseded-by-stop reason on an entry-kind expiry",
			mutate:  func(p *event.ProposalExpiredPayload) { p.Reason = event.ExpiryReasonSupersededByStop },
			wantErr: "only valid for kind",
		},
		{
			// An ENTRY proposal is precisely the kind that can be outstanding
			// when a delisting arrives, because no Campaign is open then: the
			// proposal must reach a terminal event there, or a later fill for
			// it would open a Campaign in an instrument that has stopped
			// trading (ADR 0009).
			name: "entry-kind expiry superseded by a delisting is legitimate",
			mutate: func(p *event.ProposalExpiredPayload) {
				p.Reason = event.ExpiryReasonSupersededByDelisting
				p.Rule = event.RuleEntryProposalSupersededByDelisting
			},
		},
		{
			// ExpiredAt equal to PeriodEnd is legitimate for a
			// delisting-superseded entry expiry, exactly as for the exit- and
			// Add-kind ones: a delisting can take effect at the SAME bar that
			// raised the proposal.
			name: "delisting-superseded entry expiry at the period end is legitimate",
			mutate: func(p *event.ProposalExpiredPayload) {
				p.Reason = event.ExpiryReasonSupersededByDelisting
				p.Rule = event.RuleEntryProposalSupersededByDelisting
				p.ExpiredAt = p.PeriodEnd
			},
		},
		{
			name:    "missing expired at",
			mutate:  func(p *event.ProposalExpiredPayload) { p.ExpiredAt = time.Time{} },
			wantErr: "expired at",
		},
		{
			// A proposal is superseded by a LATER bar. An expiry stamped at or
			// before the bar that produced the proposal would be describing an
			// impossible ordering.
			name:    "expired at equal to period end",
			mutate:  func(p *event.ProposalExpiredPayload) { p.ExpiredAt = p.PeriodEnd },
			wantErr: "must be after",
		},
		{
			name:    "expired at before period end",
			mutate:  func(p *event.ProposalExpiredPayload) { p.ExpiredAt = p.PeriodEnd.AddDate(0, 0, -1) },
			wantErr: "must be after",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.ProposalExpiredPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.ProposalExpiredPayload) { p.ADR = "" },
			wantErr: "adr",
		},
		{
			name:    "unrecognised reason",
			mutate:  func(p *event.ProposalExpiredPayload) { p.Reason = "changed our mind" },
			wantErr: "reason",
		},
		{
			name:    "zero quantity",
			mutate:  func(p *event.ProposalExpiredPayload) { p.Quantity = 0 },
			wantErr: "quantity",
		},
		{
			name:    "zero level",
			mutate:  func(p *event.ProposalExpiredPayload) { p.Level = 0 },
			wantErr: "level",
		},
		{
			name:    "non-finite level",
			mutate:  func(p *event.ProposalExpiredPayload) { p.Level = math.NaN() },
			wantErr: "level must be finite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validProposalExpired()
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

// TestExitProposalExpiredPayloadValidate covers the exit-kind fixture's own
// pairing rule (no signal id), mirroring how TestStopFillPayloadValidate
// pins the stop-kind fixture in fill_test.go.
func TestExitProposalExpiredPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ProposalExpiredPayload)
		wantErr string
	}{
		{name: "valid exit-kind expiry"},
		{
			name:    "exit-kind expiry names a signal id",
			mutate:  func(p *event.ProposalExpiredPayload) { p.SignalID = "signal:AAPL:2026-02-27T00:00:00.000000000Z" },
			wantErr: "signal id",
		},
		{
			// An outstanding exit proposal is cancelled the instant a
			// delisting forces the same Campaign closed, rather than waiting
			// for ADR 0011's ordinary next-bar expiry (which a delisted
			// instrument will never produce).
			name: "exit-kind expiry superseded by a delisting is legitimate",
			mutate: func(p *event.ProposalExpiredPayload) {
				p.Reason = event.ExpiryReasonSupersededByDelisting
				p.Rule = event.RuleExitProposalSupersededByDelisting
			},
		},
		{
			// Like the stop-superseded case, ExpiredAt equal to PeriodEnd is
			// legitimate here too: a delisting can take effect at the SAME
			// bar that raised the exit proposal.
			name: "delisting-superseded expiry at the period end is legitimate",
			mutate: func(p *event.ProposalExpiredPayload) {
				p.Reason = event.ExpiryReasonSupersededByDelisting
				p.Rule = event.RuleExitProposalSupersededByDelisting
				p.ExpiredAt = p.PeriodEnd
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validExitProposalExpired()
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

// validAddProposalExpired returns #14's third Kind: the expiry of an add
// proposal (strategy.add.proposed) that superseded, itself superseded by the
// next bar without an add fill ever arriving. Like an exit-kind expiry, it
// names no Signal at all: an add proposal is not sized from one (see
// event.AddProposalPayload).
func validAddProposalExpired() event.ProposalExpiredPayload {
	return event.ProposalExpiredPayload{
		InstrumentID:   "AAPL",
		Kind:           event.ProposalKindAdd,
		ProposalID:     "add-proposal-unit-2:AAPL:2026-02-27T00:00:00.000000000Z",
		SignalID:       "",
		PeriodEnd:      proposalPeriodEnd,
		ExpiredAt:      expiredAt,
		EarliestFillAt: earliestFillAt,
		Rule:           event.RuleAddProposalExpiresWithItsBar,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByNextBar,
		Quantity:       133,
		Level:          220.04,
	}
}

// TestAddProposalExpiredPayloadValidate mirrors
// TestExitProposalExpiredPayloadValidate for the add-kind fixture.
func TestAddProposalExpiredPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.ProposalExpiredPayload)
		wantErr string
	}{
		{name: "valid add-kind expiry"},
		{
			name:    "add-kind expiry names a signal id",
			mutate:  func(p *event.ProposalExpiredPayload) { p.SignalID = "signal:AAPL:2026-02-27T00:00:00.000000000Z" },
			wantErr: "signal id",
		},
		{
			// Accept an Add expiry caused by a partial stop fill: an Add
			// proposal can be cancelled while its Campaign remains open,
			// without waiting for the next bar.
			name: "add-kind expiry superseded by a partial stop is legitimate",
			mutate: func(p *event.ProposalExpiredPayload) {
				p.Reason = event.ExpiryReasonSupersededByStop
				p.Rule = event.RuleAddProposalSupersededByStop
			},
		},
		{
			// Do not apply the next-bar expiry's strict PeriodEnd bound to
			// a stop expiry: a resting stop can fill INSIDE the bar that
			// proposed the Add it cancels (ADR 0005) — ExpiredAt EQUAL to
			// PeriodEnd is legitimate for this Reason, unlike the next-bar one.
			name: "stop-superseded expiry at the period end is legitimate",
			mutate: func(p *event.ProposalExpiredPayload) {
				p.Reason = event.ExpiryReasonSupersededByStop
				p.Rule = event.RuleAddProposalSupersededByStop
				p.ExpiredAt = p.PeriodEnd
			},
		},
		{
			// But never before EarliestFillAt — that rule holds for every
			// Reason.
			name: "stop-superseded expiry before earliest fill at is rejected",
			mutate: func(p *event.ProposalExpiredPayload) {
				p.Reason = event.ExpiryReasonSupersededByStop
				p.Rule = event.RuleAddProposalSupersededByStop
				p.ExpiredAt = p.EarliestFillAt
			},
			wantErr: "must be after the earliest instant",
		},
		{
			// The add-kind mirror of the exit-kind case above: a
			// delisting cancels an outstanding Add proposal too.
			name: "add-kind expiry superseded by a delisting is legitimate",
			mutate: func(p *event.ProposalExpiredPayload) {
				p.Reason = event.ExpiryReasonSupersededByDelisting
				p.Rule = event.RuleAddProposalSupersededByDelisting
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validAddProposalExpired()
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

// TestAddProposalExpiredPayloadRoundTrip mirrors
// TestExitProposalExpiredPayloadRoundTrip for the add-kind fixture.
func TestAddProposalExpiredPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validAddProposalExpired()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.ProposalExpiredPayload
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

func TestProposalExpiredEventConstants(t *testing.T) {
	t.Parallel()

	if event.ProposalExpiredEventType != "strategy.proposal.expired" {
		t.Errorf("ProposalExpiredEventType = %q, want %q", event.ProposalExpiredEventType, "strategy.proposal.expired")
	}
	// ADR 0015 requires explicit schema versions for incompatible payloads.
	// Version 2 added required Kind (entry|exit), reusing this expiry
	// mechanism for exit proposals; add is another recognised Kind value
	// and needed no further bump. Version 3 added EarliestFillAt: accepting
	// a version-2 record's absent bound as zero would silently weaken the
	// chronology check (see ProposalExpiredSchemaVersion's doc comment).
	if event.ProposalExpiredSchemaVersion != 3 {
		t.Errorf("ProposalExpiredSchemaVersion = %d, want 3", event.ProposalExpiredSchemaVersion)
	}
	if event.ProposalKindEntry != "entry" {
		t.Errorf("ProposalKindEntry = %q, want %q", event.ProposalKindEntry, "entry")
	}
	if event.ProposalKindExit != "exit" {
		t.Errorf("ProposalKindExit = %q, want %q", event.ProposalKindExit, "exit")
	}
	if event.ProposalKindAdd != "add" {
		t.Errorf("ProposalKindAdd = %q, want %q", event.ProposalKindAdd, "add")
	}
	if event.RuleSignalExpiresWithItsBar != "signal.expires.with-its-bar" {
		t.Errorf("RuleSignalExpiresWithItsBar = %q", event.RuleSignalExpiresWithItsBar)
	}
	if event.RuleExitProposalExpiresWithItsBar != "exit-proposal.expires.with-its-bar" {
		t.Errorf("RuleExitProposalExpiresWithItsBar = %q", event.RuleExitProposalExpiresWithItsBar)
	}
	if event.RuleAddProposalExpiresWithItsBar != "add-proposal.expires.with-its-bar" {
		t.Errorf("RuleAddProposalExpiresWithItsBar = %q", event.RuleAddProposalExpiresWithItsBar)
	}
	// ADR 0011 is the decision that a Signal belongs to one bar and expires
	// with it; the proposal a Signal produced inherits that lifetime. #13
	// cites the same ADR for an exit proposal's expiry: the Baseline holds no
	// persistent proposal memory of any kind, entry or exit alike.
	if event.ADRSignalExpiry != "0011" {
		t.Errorf("ADRSignalExpiry = %q, want %q", event.ADRSignalExpiry, "0011")
	}
	if event.ExpiryReasonSupersededByNextBar == "" {
		t.Error("the expiry reason constant must be a non-empty enumerated value")
	}
	if event.ExpiryReasonSupersededByStop != "superseded-by-stop" {
		t.Errorf("ExpiryReasonSupersededByStop = %q, want %q", event.ExpiryReasonSupersededByStop, "superseded-by-stop")
	}
	if event.RuleAddProposalSupersededByStop != "add-proposal.superseded-by-stop" {
		t.Errorf("RuleAddProposalSupersededByStop = %q", event.RuleAddProposalSupersededByStop)
	}
	if event.ExpiryReasonSupersededByDelisting != "superseded-by-delisting" {
		t.Errorf("ExpiryReasonSupersededByDelisting = %q, want %q", event.ExpiryReasonSupersededByDelisting, "superseded-by-delisting")
	}
	if event.RuleExitProposalSupersededByDelisting != "exit-proposal.superseded-by-delisting" {
		t.Errorf("RuleExitProposalSupersededByDelisting = %q", event.RuleExitProposalSupersededByDelisting)
	}
	if event.RuleAddProposalSupersededByDelisting != "add-proposal.superseded-by-delisting" {
		t.Errorf("RuleAddProposalSupersededByDelisting = %q", event.RuleAddProposalSupersededByDelisting)
	}
	if event.RuleEntryProposalSupersededByDelisting != "entry-proposal.superseded-by-delisting" {
		t.Errorf("RuleEntryProposalSupersededByDelisting = %q", event.RuleEntryProposalSupersededByDelisting)
	}
}

func TestProposalExpiredPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validProposalExpired()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.ProposalExpiredPayload
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

func TestProposalExpiredPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validProposalExpired())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"instrument_id",
		"kind",
		"proposal_id",
		"signal_id",
		"period_end",
		"expired_at",
		"earliest_fill_at",
		"rule",
		"adr",
		"reason",
		"quantity",
		"level",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}

// TestExitProposalExpiredPayloadRoundTrip mirrors
// TestProposalExpiredPayloadRoundTrip for the exit-kind fixture.
func TestExitProposalExpiredPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validExitProposalExpired()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.ProposalExpiredPayload
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
