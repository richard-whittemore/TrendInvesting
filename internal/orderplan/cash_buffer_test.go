package orderplan_test

import (
	"reflect"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// ADR 0026, owner decision 2026-10-31: subtract f × V before the existing
// cost/scale rule, clamp at zero, and never change the requested basket.
// All monetary inputs are synthetic.
func TestCashBufferBudget(t *testing.T) {
	for _, fractional := range []bool{false, true} {
		t.Run(map[bool]string{false: "whole", true: "fractional"}[fractional], func(t *testing.T) {
			buy := event.OrderPlannedPayload{Quantity: 98, LimitPrice: 100, Side: event.OrderSideBuy}
			costs := 2.0
			if fractional {
				buy.Quantity = 980000
				buy.QuantityScale = 10000
				buy.FractionalIncrement = "0.0001"
				buy.RoundingSlack = 0.10
				costs = 2.10
			}
			buys := []event.OrderPlannedPayload{buy}
			for _, tc := range []struct {
				name                                   string
				cash, fraction, wantBudget, wantBuffer float64
			}{
				{"zero_exact_legacy", 9900 + costs, 0, 9900 + costs, 0},
				{"one_percent_unscaled", 9900 + costs, 0.01, 9800 + costs, 100},
				{"below_reserve_skipped", 50, 0.01, 0, 100},
			} {
				t.Run(tc.name, func(t *testing.T) {
					buffer := orderplan.CashBuffer(10000, tc.fraction)
					budget := max(0, tc.cash-buffer) // the withholding's floor at zero
					if budget != tc.wantBudget || buffer != tc.wantBuffer {
						t.Errorf("budget/buffer = %v/%v; want %v/%v", budget, buffer, tc.wantBudget, tc.wantBuffer)
					}
					kept, skipped, scale := orderplan.ScaleBuys(buys, budget, 2)
					if tc.fraction == 0 {
						old, oldSkipped, oldScale := orderplan.ScaleBuys(buys, tc.cash, 2)
						if !reflect.DeepEqual(kept, old) || !reflect.DeepEqual(skipped, oldSkipped) || scale != oldScale {
							t.Fatal("zero buffer changes legacy plan")
						}
					}
					if tc.wantBudget == 0 {
						if len(kept) != 0 || len(skipped) != 1 || skipped[0].Reason == "" || scale != 0 {
							t.Fatalf("want skipped buy with reason, got %v %v %v", kept, skipped, scale)
						}
					} else if !reflect.DeepEqual(kept, buys) || len(skipped) != 0 || scale != 1 {
						t.Fatalf("buy should fit unscaled: %v %v %v", kept, skipped, scale)
					}
				})
			}
		})
	}
}

// ADR 0026, owner decision 2026-10-31: the reserve is a balance held aside,
// not a charge on every buy-phase evaluation. Proceeds cannot erase a
// shortfall, and a terminal buy releases only its unused commitment.
func TestCashBufferBudgetWithheldOnce(t *testing.T) {
	b := orderplan.NewBudget(50)
	b.WithholdCashBuffer(100)
	if got := b.Remaining(); got != 0 {
		t.Fatalf("below reserve: %v", got)
	}
	b.AddProceeds(100)
	if got := b.Remaining(); got != 50 {
		t.Fatalf("after proceeds: %v", got)
	}
	b.WithholdCashBuffer(100)
	if got := b.Remaining(); got != 50 {
		t.Fatalf("reserve charged twice: %v", got)
	}
	b.Release("buy", 30)
	if got := b.Remaining(); got != 20 {
		t.Fatalf("working reservation: %v", got)
	}
	if !b.Fill("buy", 20) || !b.Close("buy") {
		t.Fatal("buy not tracked")
	}
	if got := b.Remaining(); got != 30 {
		t.Fatalf("terminal buy released reserve: %v", got)
	}
	b.Remove(40)
	if got := b.Remaining(); got != 0 {
		t.Fatalf("cash drop left negative budget: %v", got)
	}
}
