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

// TestDrawdownStepAppliedPayloadValidateAllowsFloatTolerance documents and
// tests the justified float64 tolerance: a chained multiplication (several
// Drawdown Steps applied from one account snapshot,
// internal/strategy.NotionalAccount.Observe) can differ from a fresh
// 0.8 x NotionalBefore in the last bit without being a defect.
func TestDrawdownStepAppliedPayloadValidateAllowsFloatTolerance(t *testing.T) {
	t.Parallel()

	payload := validDrawdownStepApplied()
	payload.NotionalAfter = 800_000 + 1e-7 // far below the 1e-6 x 1,000,000 tolerance
	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil (within the justified float tolerance)", err)
	}
}

// TestDrawdownStepAppliedPayloadValidateRejectsBeyondTolerance confirms the
// tolerance is not so wide that it stops catching a real defect (here, a
// wrong drawdown percentage: 0.9x instead of 0.8x).
func TestDrawdownStepAppliedPayloadValidateRejectsBeyondTolerance(t *testing.T) {
	t.Parallel()

	payload := validDrawdownStepApplied()
	payload.NotionalAfter = 900_000 // 0.9 x 1,000,000, not 0.8x
	if err := payload.Validate(); err == nil || !strings.Contains(err.Error(), "does not match the derivation") {
		t.Fatalf("Validate() error = %v, want it to name a derivation mismatch", err)
	}
}
