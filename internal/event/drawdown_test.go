package event_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validDrawdownStepApplied returns a payload that satisfies every validation
// rule (Faith's first step, The Turtle Rules p.17: 1,000,000 -> 900,000
// threshold -> 800,000), so each table test only needs to describe its one
// deviation.
func validDrawdownStepApplied() event.DrawdownStepAppliedPayload {
	return event.DrawdownStepAppliedPayload{
		AsOf:           time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC),
		Equity:         900_000,
		Threshold:      900_000,
		NotionalBefore: 1_000_000,
		NotionalAfter:  800_000,
		StepNumber:     1,
		Rule:           event.RuleNotionalAccountDrawdownStep,
		ADR:            event.ADRNotionalAccountDrawdownStep,
	}
}

func TestDrawdownStepAppliedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.DrawdownStepAppliedPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing as of",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.AsOf = time.Time{} },
			wantErr: "as of is required",
		},
		{
			name:    "zero equity",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.Equity = 0 },
			wantErr: "equity must be positive",
		},
		{
			name:    "nan equity",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.Equity = math.NaN() },
			wantErr: "equity must be finite",
		},
		{
			name:    "zero threshold",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.Threshold = 0 },
			wantErr: "threshold must be positive",
		},
		{
			name:    "nan threshold",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.Threshold = math.NaN() },
			wantErr: "threshold must be finite",
		},
		{
			name:    "zero notional before",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.NotionalBefore = 0 },
			wantErr: "notional before must be positive",
		},
		{
			name:    "zero notional after",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.NotionalAfter = 0 },
			wantErr: "notional after must be positive",
		},
		{
			name: "notional after not below notional before",
			mutate: func(p *event.DrawdownStepAppliedPayload) {
				p.NotionalAfter = p.NotionalBefore
			},
			wantErr: "must be strictly below notional before",
		},
		{
			name: "notional after above notional before",
			mutate: func(p *event.DrawdownStepAppliedPayload) {
				p.NotionalAfter = p.NotionalBefore + 1
			},
			wantErr: "must be strictly below notional before",
		},
		{
			name: "notional after does not match the 0.8x derivation",
			mutate: func(p *event.DrawdownStepAppliedPayload) {
				p.NotionalAfter = 900_000 // not 0.8 x 1,000,000
			},
			wantErr: "does not match the derivation",
		},
		{
			name: "equity above threshold",
			mutate: func(p *event.DrawdownStepAppliedPayload) {
				p.Equity = 950_000 // above the 900,000 threshold
			},
			wantErr: "exceeds threshold",
		},
		{
			name: "equity exactly at threshold is accepted",
			mutate: func(p *event.DrawdownStepAppliedPayload) {
				p.Equity = 900_000
				p.Threshold = 900_000
			},
			wantErr: "",
		},
		{
			name:    "zero step number",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.StepNumber = 0 },
			wantErr: "step number must be a positive",
		},
		{
			name:    "negative step number",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.StepNumber = -1 },
			wantErr: "step number must be a positive",
		},
		{
			name:    "second step number is accepted",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.StepNumber = 2 },
			wantErr: "",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.Rule = "" },
			wantErr: "rule is required",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.DrawdownStepAppliedPayload) { p.ADR = "" },
			wantErr: "adr is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validDrawdownStepApplied()
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

// TestDrawdownStepAppliedPayloadValidateRequiresExactDerivation replaces an
// earlier, mistaken tolerance: each Drawdown Step event carries its OWN
// NotionalBefore/NotionalAfter pair, and both the reducer
// (internal/strategy.NotionalAccount.Observe, via
// sizing.DrawdownSteppedNotional) and this Validate call the identical
// shared function on the identical NotionalBefore — nothing is chained
// across the check, so there is nothing for a tolerance to excuse. Even a
// last-bit difference (here, 1e-7 on an 800,000 figure) must be rejected,
// the same exact-equality discipline #10 established for
// TradeProposalPayload (see #65, which tracks this discipline generally).
func TestDrawdownStepAppliedPayloadValidateRequiresExactDerivation(t *testing.T) {
	t.Parallel()

	payload := validDrawdownStepApplied()
	payload.NotionalAfter = 800_000 + 1e-7 // not bit-identical to sizing.DrawdownSteppedNotional(1_000_000)
	if err := payload.Validate(); err == nil || !strings.Contains(err.Error(), "does not match the derivation") {
		t.Fatalf("Validate() error = %v, want it to name a derivation mismatch (exact equality, no tolerance)", err)
	}
}

// TestDrawdownStepAppliedPayloadValidateRejectsWrongPercentage confirms a
// real defect — a wrong drawdown percentage, 0.9x instead of ADR 0007's
// 0.8x — is rejected.
func TestDrawdownStepAppliedPayloadValidateRejectsWrongPercentage(t *testing.T) {
	t.Parallel()

	payload := validDrawdownStepApplied()
	payload.NotionalAfter = 900_000 // 0.9 x 1,000,000, not 0.8x
	if err := payload.Validate(); err == nil || !strings.Contains(err.Error(), "does not match the derivation") {
		t.Fatalf("Validate() error = %v, want it to name a derivation mismatch", err)
	}
}

// TestDrawdownStepAppliedPayloadValidateRejectsTransposedFigures confirms a
// step whose NotionalBefore and NotionalAfter have been swapped (a plausible
// transcription defect: the smaller figure recorded as "before") is
// rejected.
func TestDrawdownStepAppliedPayloadValidateRejectsTransposedFigures(t *testing.T) {
	t.Parallel()

	payload := validDrawdownStepApplied() // NotionalBefore 1,000,000, NotionalAfter 800,000
	payload.NotionalBefore, payload.NotionalAfter = payload.NotionalAfter, payload.NotionalBefore
	if err := payload.Validate(); err == nil || !strings.Contains(err.Error(), "must be strictly below notional before") {
		t.Fatalf("Validate() error = %v, want it to reject notional after exceeding notional before", err)
	}
}

// TestDrawdownStepAppliedPayloadValidateAcceptsAWholeMultiStepChain builds
// the three payloads a single large-drop account snapshot would produce
// (Faith's ladder plus its derived third step: The Turtle Rules p.17,
// 1,000,000 -> 800,000 -> 640,000 -> 512,000) and confirms every one
// validates exactly on its own, not only the first.
func TestDrawdownStepAppliedPayloadValidateAcceptsAWholeMultiStepChain(t *testing.T) {
	t.Parallel()

	chain := []event.DrawdownStepAppliedPayload{
		{
			AsOf: validDrawdownStepApplied().AsOf, Equity: 750_000, Threshold: 900_000,
			NotionalBefore: 1_000_000, NotionalAfter: 800_000, StepNumber: 1,
			Rule: event.RuleNotionalAccountDrawdownStep, ADR: event.ADRNotionalAccountDrawdownStep,
		},
		{
			AsOf: validDrawdownStepApplied().AsOf, Equity: 750_000, Threshold: 820_000,
			NotionalBefore: 800_000, NotionalAfter: 640_000, StepNumber: 2,
			Rule: event.RuleNotionalAccountDrawdownStep, ADR: event.ADRNotionalAccountDrawdownStep,
		},
		{
			AsOf: validDrawdownStepApplied().AsOf, Equity: 750_000, Threshold: 756_000,
			NotionalBefore: 640_000, NotionalAfter: 512_000, StepNumber: 3,
			Rule: event.RuleNotionalAccountDrawdownStep, ADR: event.ADRNotionalAccountDrawdownStep,
		},
	}
	for i, step := range chain {
		if err := step.Validate(); err != nil {
			t.Errorf("chain step %d (StepNumber %d) fails Validate(): %v", i, step.StepNumber, err)
		}
	}
}
