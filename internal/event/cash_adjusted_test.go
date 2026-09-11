package event_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validNotionalAccountCashAdjusted is #17's hand-derived deposit fixture
// (also used directly by sizing_test.go's
// TestCashMovementScaledFigureDepositExample): after one Drawdown Step at
// equity 890,000 (S 1,000,000, B 900,000, A 800,000), a deposit of 200,000
// scales every figure by 1,090,000/890,000.
func validNotionalAccountCashAdjusted() event.NotionalAccountCashAdjustedPayload {
	return event.NotionalAccountCashAdjustedPayload{
		AsOf:                 time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
		Amount:               200_000,
		EquityBefore:         890_000,
		EquityAfter:          1_090_000,
		StartingFigureBefore: 1_000_000,
		StartingFigureAfter:  1_224_719.1011235956,
		NotionalBefore:       800_000,
		NotionalAfter:        979_775.2808988765,
		Rule:                 event.RuleNotionalAccountCashAdjustment,
		ADR:                  event.ADRNotionalAccountCashAdjustment,
	}
}

func TestNotionalAccountCashAdjustedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.NotionalAccountCashAdjustedPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing as of",
			mutate:  func(p *event.NotionalAccountCashAdjustedPayload) { p.AsOf = time.Time{} },
			wantErr: "as of is required",
		},
		{
			name:    "zero amount",
			mutate:  func(p *event.NotionalAccountCashAdjustedPayload) { p.Amount = 0 },
			wantErr: "amount must be non-zero",
		},
		{
			name:    "zero equity before",
			mutate:  func(p *event.NotionalAccountCashAdjustedPayload) { p.EquityBefore = 0 },
			wantErr: "equity before must be positive",
		},
		{
			name: "equity after does not match equity before plus amount",
			mutate: func(p *event.NotionalAccountCashAdjustedPayload) {
				p.EquityAfter = 1_090_000.01
			},
			wantErr: "equity after",
		},
		{
			name:    "zero starting figure before",
			mutate:  func(p *event.NotionalAccountCashAdjustedPayload) { p.StartingFigureBefore = 0 },
			wantErr: "starting figure before must be positive",
		},
		{
			name: "starting figure after does not match the derivation",
			mutate: func(p *event.NotionalAccountCashAdjustedPayload) {
				p.StartingFigureAfter++
			},
			wantErr: "starting figure after",
		},
		{
			name:    "zero notional before",
			mutate:  func(p *event.NotionalAccountCashAdjustedPayload) { p.NotionalBefore = 0 },
			wantErr: "notional before must be positive",
		},
		{
			name: "notional after does not match the derivation",
			mutate: func(p *event.NotionalAccountCashAdjustedPayload) {
				p.NotionalAfter = 979_775.2808988766 // one ULP off
			},
			wantErr: "notional after",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.NotionalAccountCashAdjustedPayload) { p.Rule = "" },
			wantErr: "rule is required",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.NotionalAccountCashAdjustedPayload) { p.ADR = "" },
			wantErr: "adr is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validNotionalAccountCashAdjusted()
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
