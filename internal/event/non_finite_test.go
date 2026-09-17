package event_test

import (
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// A payload's own Validate is the last thing standing between a NaN and the
// journal. NaN compares false against every threshold, so a figure that
// reached a comparison-only check would pass "must be positive", "must be
// below the fill price" and every cross-derivation in this package at once,
// and would then poison every figure derived from it for the rest of the
// run. The guards are therefore written as an explicit finiteness test ahead
// of the range test; these are the cases that prove each one is wired to the
// field it names.
//
// Infinities are tested alongside NaN because they arrive by a different
// route — an overflowing product or a division by a figure that underflowed
// to zero, rather than 0/0 — and a guard written as `x != x` would catch only
// the first.

// validatable is the one method every payload in this package shares.
type validatable interface{ Validate() error }

func TestNonFiniteFiguresAreRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload validatable
		wantErr string
	}{
		{
			name: "unit added fill price is nan",
			payload: mutateUnitAdded(func(p *event.CampaignUnitAddedPayload) {
				p.FillPrice = math.NaN()
			}),
			wantErr: "fill price must be finite",
		},
		{
			name: "unit added campaign n is positive infinity",
			payload: mutateUnitAdded(func(p *event.CampaignUnitAddedPayload) {
				p.CampaignN = math.Inf(1)
			}),
			wantErr: "campaign n must be finite",
		},
		{
			name: "unit added stop multiple is negative infinity",
			payload: mutateUnitAdded(func(p *event.CampaignUnitAddedPayload) {
				p.StopMultiple = math.Inf(-1)
			}),
			wantErr: "stop multiple must be finite",
		},
		{
			name: "unit added protective stop is nan",
			payload: mutateUnitAdded(func(p *event.CampaignUnitAddedPayload) {
				p.ProtectiveStop = math.NaN()
			}),
			wantErr: "protective stop must be finite",
		},

		{
			name: "campaign evaluated unit index below one",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.Units[0].UnitIndex = 0
			}),
			wantErr: "unit_index = 0, must be at least 1",
		},
		{
			name: "campaign evaluated unit entry price is nan",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.Units[0].EntryPrice = math.NaN()
			}),
			wantErr: "units[0]: entry price must be finite",
		},
		{
			name: "campaign evaluated unit entry price is not positive",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.Units[0].EntryPrice = 0
			}),
			wantErr: "units[0]: entry price must be positive",
		},
		{
			// #78 wires this check through sizing.ValidStopLevel, which
			// wraps the reason with the predicate's own name — the unit
			// index and the reason are both still present, just no longer
			// contiguous as one substring.
			name: "campaign evaluated unit protective stop is positive infinity",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.Units[0].ProtectiveStop = math.Inf(1)
			}),
			wantErr: "units[0]: sizing: invalid protective stop level: protective stop level must be finite",
		},
		{
			name: "campaign evaluated unit protective stop is not positive",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.Units[0].ProtectiveStop = -1
			}),
			wantErr: "units[0]: sizing: invalid protective stop level: protective stop level must be positive",
		},
		{
			name: "campaign evaluated unit quantity is not positive",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.Units[0].Quantity = 0
			}),
			wantErr: "units[0]: quantity must be a positive whole number",
		},
		{
			name: "campaign evaluated dollars per point is nan",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.DollarsPerPoint = math.NaN()
			}),
			wantErr: "dollars per point must be finite",
		},
		{
			name: "campaign evaluated notional account is positive infinity",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.NotionalAccount = math.Inf(1)
			}),
			wantErr: "notional account must be finite",
		},
		{
			name: "campaign evaluated aggregate open risk is nan",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.AggregateOpenRisk = math.NaN()
			}),
			wantErr: "aggregate open risk must be finite",
		},
		{
			name: "campaign evaluated aggregate open risk fraction is nan",
			payload: mutateEvaluated(func(p *event.CampaignEvaluatedPayload) {
				p.AggregateOpenRiskFraction = math.NaN()
			}),
			wantErr: "aggregate open risk fraction must be finite",
		},

		{
			name: "cash adjusted amount is nan",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.Amount = math.NaN()
			}),
			wantErr: "amount must be finite",
		},
		{
			name: "cash adjusted equity before is nan",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.EquityBefore = math.NaN()
			}),
			wantErr: "equity before must be finite",
		},
		{
			name: "cash adjusted equity after is positive infinity",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.EquityAfter = math.Inf(1)
			}),
			wantErr: "equity after must be finite",
		},
		{
			// A withdrawal that empties the account: the figure is finite and
			// arithmetically consistent with the amount, and is refused for
			// being non-positive rather than for being unreadable.
			name: "cash adjusted equity after is not positive",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.Amount = -p.EquityBefore
				p.EquityAfter = 0
			}),
			wantErr: "equity after must be positive",
		},
		{
			name: "cash adjusted starting figure before is nan",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.StartingFigureBefore = math.NaN()
			}),
			wantErr: "starting figure before must be finite",
		},
		{
			name: "cash adjusted starting figure after is negative infinity",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.StartingFigureAfter = math.Inf(-1)
			}),
			wantErr: "starting figure after must be finite",
		},
		{
			name: "cash adjusted starting figure after is not positive",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.StartingFigureAfter = 0
			}),
			wantErr: "starting figure after must be positive",
		},
		{
			name: "cash adjusted notional before is nan",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.NotionalBefore = math.NaN()
			}),
			wantErr: "notional before must be finite",
		},
		{
			name: "cash adjusted notional after is nan",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.NotionalAfter = math.NaN()
			}),
			wantErr: "notional after must be finite",
		},
		{
			name: "cash adjusted notional after is not positive",
			payload: mutateCashAdjusted(func(p *event.NotionalAccountCashAdjustedPayload) {
				p.NotionalAfter = -1
			}),
			wantErr: "notional after must be positive",
		},

		{
			name: "cash movement equity before is nan",
			payload: mutateCashMovement(func(p *event.CashMovementPayload) {
				p.EquityBefore = math.NaN()
			}),
			wantErr: "equity before must be finite",
		},

		{
			name: "drawdown step notional before is nan",
			payload: mutateDrawdownStep(func(p *event.DrawdownStepAppliedPayload) {
				p.NotionalBefore = math.NaN()
			}),
			wantErr: "notional before must be finite",
		},
		{
			name: "drawdown step notional after is positive infinity",
			payload: mutateDrawdownStep(func(p *event.DrawdownStepAppliedPayload) {
				p.NotionalAfter = math.Inf(1)
			}),
			wantErr: "notional after must be finite",
		},

		{
			name: "rebase previous starting figure is nan",
			payload: mutateRebased(func(p *event.NotionalAccountRebasedPayload) {
				p.PreviousStartingFigure = math.NaN()
			}),
			wantErr: "previous starting figure must be finite",
		},
		{
			name: "rebase new starting figure is nan",
			payload: mutateRebased(func(p *event.NotionalAccountRebasedPayload) {
				p.NewStartingFigure = math.NaN()
			}),
			wantErr: "new starting figure must be finite",
		},
		{
			name: "rebase equity is positive infinity",
			payload: mutateRebased(func(p *event.NotionalAccountRebasedPayload) {
				p.Equity = math.Inf(1)
			}),
			wantErr: "equity must be finite",
		},

		{
			name: "recovery equity is nan",
			payload: mutateRecovered(func(p *event.NotionalAccountRecoveredPayload) {
				p.Equity = math.NaN()
			}),
			wantErr: "equity must be finite",
		},
		{
			name: "recovery starting figure is nan",
			payload: mutateRecovered(func(p *event.NotionalAccountRecoveredPayload) {
				p.StartingFigure = math.NaN()
			}),
			wantErr: "starting figure must be finite",
		},
		{
			name: "recovery notional before is negative infinity",
			payload: mutateRecovered(func(p *event.NotionalAccountRecoveredPayload) {
				p.NotionalBefore = math.Inf(-1)
			}),
			wantErr: "notional before must be finite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.payload.Validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want an error naming %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %v, want it to name %q", err, tt.wantErr)
			}
		})
	}
}

func mutateUnitAdded(mutate func(*event.CampaignUnitAddedPayload)) event.CampaignUnitAddedPayload {
	payload := validCampaignUnitAdded()
	mutate(&payload)
	return payload
}

func mutateEvaluated(mutate func(*event.CampaignEvaluatedPayload)) event.CampaignEvaluatedPayload {
	payload := validCampaignEvaluated()
	mutate(&payload)
	return payload
}

func mutateCashAdjusted(mutate func(*event.NotionalAccountCashAdjustedPayload)) event.NotionalAccountCashAdjustedPayload {
	payload := validNotionalAccountCashAdjusted()
	mutate(&payload)
	return payload
}

func mutateCashMovement(mutate func(*event.CashMovementPayload)) event.CashMovementPayload {
	payload := validCashMovement()
	mutate(&payload)
	return payload
}

func mutateDrawdownStep(mutate func(*event.DrawdownStepAppliedPayload)) event.DrawdownStepAppliedPayload {
	payload := validDrawdownStepApplied()
	mutate(&payload)
	return payload
}

func mutateRebased(mutate func(*event.NotionalAccountRebasedPayload)) event.NotionalAccountRebasedPayload {
	payload := validNotionalAccountRebased()
	mutate(&payload)
	return payload
}

func mutateRecovered(mutate func(*event.NotionalAccountRecoveredPayload)) event.NotionalAccountRecoveredPayload {
	payload := validNotionalAccountRecovered()
	mutate(&payload)
	return payload
}
