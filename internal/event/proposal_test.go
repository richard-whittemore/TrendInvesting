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
var proposalN = 40.69890254048816

// validTradeProposal returns a Baseline (volatility-normalised, ADR 0003)
// trade proposal that satisfies every validation rule, so each table row only
// has to describe its one deviation.
//
// Hand-worked: a $1,000,000 Notional Account at the Baseline's 0.5 % Unit
// Volatility Fraction gives a 1N budget of $5,000; 5,000 / 40.6989... =
// 122.85..., truncated to 122 shares (The Turtle Rules p.14). Risk at Stop is
// derived, never configured: 0.005 x 2 = 0.01. The Protective Stop intent is
// the entry level less two N: 200 - 2 x 40.6989... = 118.60...
func validTradeProposal() event.TradeProposalPayload {
	return event.TradeProposalPayload{
		InstrumentID:           "AAPL",
		PeriodEnd:              proposalPeriodEnd,
		SignalID:               "signal:AAPL:2026-02-27T00:00:00.000000000Z",
		Rule:                   event.RuleUnitSizingVolatilityNormalised,
		ADR:                    event.ADRUnitSizing,
		Direction:              event.DirectionLong,
		EntryLevel:             200,
		Quantity:               122,
		N:                      proposalN,
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2,
		RiskAtStop:             0.005 * 2,
		DollarsPerPoint:        1,
		NotionalAccount:        1_000_000,
		ProtectiveStopIntent:   200 - 2*proposalN,
	}
}

// validFixedRiskTradeProposal is the Sublime Variant's counterpart [M p.56]:
// a 2 % Risk at Stop with a 3N stop gives 1,000,000 x 0.02 / (3 x 40.6989...)
// = 163.8... -> 163 shares, and Risk at Stop is the configured input rather
// than a derivation from the Unit Volatility Fraction.
func validFixedRiskTradeProposal() event.TradeProposalPayload {
	p := validTradeProposal()
	p.Rule = event.RuleUnitSizingFixedRiskAtStop
	p.SizingMode = event.SizingModeFixedRiskAtStop
	p.StopMultiple = 3
	p.RiskAtStop = 0.02
	p.Quantity = 163
	p.ProtectiveStopIntent = 200 - 3*proposalN
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
			// Truncation may only ever risk LESS than the budget. 123 shares
			// at 40.6989... is 5,006.0..., above the $5,000 1N budget, so
			// this payload claims a Unit larger than its own parameters
			// permit.
			name:    "quantity exceeds the volatility-normalised budget",
			mutate:  func(p *event.TradeProposalPayload) { p.Quantity = 123 },
			wantErr: "exceeds the",
		},
		{
			// The same overshoot in the other mode: 164 shares at a 3N stop
			// is 20,023.8..., above the $20,000 Risk-at-Stop budget.
			name:    "quantity exceeds the fixed-risk-at-stop budget",
			base:    validFixedRiskTradeProposal,
			mutate:  func(p *event.TradeProposalPayload) { p.Quantity = 164 },
			wantErr: "exceeds the",
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
		SignalID:     "signal:AAPL:2026-02-27T00:00:00.000000000Z",
		Reason:       event.DeclineReasonQuantityBelowOneUnit,
		Detail:       "notional account 100.00 at unit volatility fraction 0.005000 sizes 0 shares at n 40.698903",
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

func TestProposalDeclinedEventConstants(t *testing.T) {
	t.Parallel()

	if event.ProposalDeclinedEventType != "strategy.proposal.declined" {
		t.Errorf("ProposalDeclinedEventType = %q, want %q", event.ProposalDeclinedEventType, "strategy.proposal.declined")
	}
	if event.ProposalDeclinedSchemaVersion != 1 {
		t.Errorf("ProposalDeclinedSchemaVersion = %d, want 1", event.ProposalDeclinedSchemaVersion)
	}
	for _, reason := range []string{
		event.DeclineReasonNNotReady,
		event.DeclineReasonQuantityBelowOneUnit,
		event.DeclineReasonStopIntentNotPositive,
	} {
		if reason == "" {
			t.Error("every decline reason constant must be a non-empty enumerated value")
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

	for _, key := range []string{"instrument_id", "period_end", "signal_id", "reason", "detail"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
