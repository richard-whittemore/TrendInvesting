package sizing_test

import (
	"math"
	"math/rand"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// baselineCommission is ADR 0013's Baseline schedule as the backtest
// fixture configures it: $0.005 per share, a $1.00 floor, capped at 1 % of
// trade value.
var baselineCommission = sizing.CommissionSchedule{PerShare: 0.005, MinimumPerOrder: 1, MaximumFractionOfTradeValue: 0.01}

// TestCommissionAppliesRateThenFloorThenCeiling pins ADR 0013's order: the
// ceiling caps the charge, so it overrides the floor.
func TestCommissionAppliesRateThenFloorThenCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		quantity int64
		price    float64
		want     float64
	}{
		{"the rate", 500, 50, 2.5},
		{"the floor", 50, 50, 1},
		{"the ceiling over the floor", 100, 0.5, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := sizing.Commission(tt.quantity, tt.price, 1, baselineCommission)
			if !ok || got != tt.want {
				t.Errorf("Commission(%d, %v) = %v, %v; want %v, true", tt.quantity, tt.price, got, ok, tt.want)
			}
		})
	}
}

// TestCommissionNeverFallsAsPriceRises is the property a hold relies on: the
// charge at the worst-case price bounds the charge on any fill below it (ADR
// 0020, as amended 2026-09-24).
func TestCommissionNeverFallsAsPriceRises(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(20260924))
	for range 10_000 {
		quantity := int64(1 + rng.Intn(10_000))
		low := 0.01 + float64(rng.Float64()*500)
		high := low + float64(rng.Float64()*50)
		atLow, okLow := sizing.Commission(quantity, low, 1, baselineCommission)
		atHigh, okHigh := sizing.Commission(quantity, high, 1, baselineCommission)
		if !okLow || !okHigh {
			t.Fatalf("Commission(%d) at %v or %v is not finite", quantity, low, high)
		}
		if atLow > atHigh {
			t.Fatalf("Commission(%d) at %v = %v exceeds the charge at the higher price %v = %v", quantity, low, atLow, high, atHigh)
		}
	}
}

// TestPriceCapIsTheLevelPlusTheGapBufferInN pins ADR 0005's cap, level +
// k x N, computed with the product rounded before the addition.
func TestPriceCapIsTheLevelPlusTheGapBufferInN(t *testing.T) {
	t.Parallel()

	level, k, n := 120.0, 1.0, 2.5
	got, ok := sizing.PriceCap(level, k, n)
	if want := level + float64(k*n); !ok || got != want {
		t.Fatalf("PriceCap(%v, %v, %v) = %v, %v; want %v, true", level, k, n, got, ok, want)
	}
	if got, ok := sizing.PriceCap(level, 0, n); !ok || got != level {
		t.Fatalf("PriceCap with a zero gap buffer = %v, %v; want the level %v", got, ok, level)
	}
	if _, ok := sizing.PriceCap(math.MaxFloat64, 2, math.MaxFloat64); ok {
		t.Fatal("PriceCap reported an overflowing cap as representable")
	}
}

// TestWorstCaseBuyCostBoundsEveryFillUpToTheCap is the invariant ADR 0020's
// amendment rests on: a buy that executes at any price up to the cap, plus
// the slippage internal/fills adds, costs no more than the hold computed
// from the cap. The fill's cost is computed the way the reducer debits it:
// quantity x price x dollars per point, rounded, plus commission.
func TestWorstCaseBuyCostBoundsEveryFillUpToTheCap(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(7))
	for range 10_000 {
		quantity := int64(1 + rng.Intn(50_000))
		n := 0.01 + float64(rng.Float64()*10)
		level := 1 + float64(rng.Float64()*400)
		k := rng.Float64() * 2
		slippageN := 0.01 + float64(rng.Float64()*0.25)
		priceCap, ok := sizing.PriceCap(level, k, n)
		if !ok {
			t.Fatal("cap not finite")
		}
		hold, ok := sizing.WorstCaseBuyCost(quantity, priceCap, slippageN, n, 1, baselineCommission)
		if !ok {
			t.Fatal("hold not finite")
		}
		execution := level + float64(rng.Float64()*(priceCap-level))
		if rng.Intn(4) == 0 {
			execution = priceCap
		}
		price := execution + float64(slippageN*n)
		commission, _ := sizing.Commission(quantity, price, 1, baselineCommission)
		cost := float64(float64(quantity)*price*1) + commission
		if cost > hold {
			t.Fatalf("a fill at %v (cap %v, slippage %v) costs %v, more than its hold %v", price, priceCap, float64(slippageN*n), cost, hold)
		}
	}
}

// TestWorstCaseBuyCostIsTheCapPlusSlippageTimesQuantityPlusCommission pins
// the arithmetic on one hand-checkable case: 100 shares, cap 110, slippage
// 0.05 x N 2 = 0.1, so 110.1 a share, 11,010 of trade value and a 1.00
// commission floor (100 x 0.005 = 0.50).
func TestWorstCaseBuyCostIsTheCapPlusSlippageTimesQuantityPlusCommission(t *testing.T) {
	t.Parallel()

	got, ok := sizing.WorstCaseBuyCost(100, 110, 0.05, 2, 1, baselineCommission)
	price := 110 + float64(0.05*2.0)
	if want := float64(100*price) + 1; !ok || got != want {
		t.Fatalf("WorstCaseBuyCost = %v, %v; want %v, true", got, ok, want)
	}
	if _, ok := sizing.WorstCaseBuyCost(math.MaxInt64, math.MaxFloat64, 0.05, 2, 1, baselineCommission); ok {
		t.Fatal("WorstCaseBuyCost reported an overflowing cost as representable")
	}
}
