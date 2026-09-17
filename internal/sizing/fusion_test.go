package sizing_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// Go permits fusing `x + a*b` into a single operation that keeps the
// full-precision product, and arm64 does while amd64 does not. An
// accumulator is the worst place for it: the divergence compounds with every
// term instead of appearing once.
//
// These tests pin the two accumulators to the rounded answer using inputs
// that actually tell the two apart, so an unrounded implementation fails
// them on a fused architecture rather than only when a golden journal
// happens to notice.

// TestProductRoundsItsResult states the barrier itself: the product as
// float64 arithmetic computes it, and never the exact product a fused
// instruction would carry into a following addition.
//
// The operands are variables, not untyped constants: constant arithmetic is
// evaluated at arbitrary precision by the compiler, so a constant expression
// would compare the wrong two things.
func TestProductRoundsItsResult(t *testing.T) {
	t.Parallel()

	a, b := 1.4213-1.1, 42000.0
	if got, want := sizing.Product(a, b), a*b; got != want {
		t.Fatalf("Product(%v, %v) = %v, want %v", a, b, got, want)
	}
	if fused := math.FMA(a, b, 0); sizing.Product(a, b) == fused && a*b != fused {
		t.Fatal("Product returned the fused result")
	}
}

// TestAggregateOpenRiskRoundsEachProductBeforeAddingIt: the aggregate open
// risk is the figure .greptile/rules.md's "risk multiplication when
// pyramiding" failure mode is checked against, summed over Units. The
// contract multiplier here is Faith's Heating Oil 42,000 rather than an
// equity's 1, because multiplying by 1 is exact and so cannot tell a fused
// accumulation from a rounded one at all.
func TestAggregateOpenRiskRoundsEachProductBeforeAddingIt(t *testing.T) {
	t.Parallel()

	const dollarsPerPoint = 42000.0
	units := []sizing.UnitOpenRisk{
		{EntryPrice: 1.4213, ProtectiveStop: 1.1, Quantity: 11},
		{EntryPrice: 1.5913, ProtectiveStop: 1.2100000000000002, Quantity: 13},
	}

	var rounded, fused float64
	for _, u := range units {
		risk := (u.EntryPrice - u.ProtectiveStop) * float64(u.Quantity)
		rounded += float64(risk * dollarsPerPoint)
		fused = math.FMA(risk, dollarsPerPoint, fused)
	}
	if rounded == fused {
		t.Fatalf("these inputs accumulate to %v either way; the test cannot tell a fused implementation from a rounded one", rounded)
	}

	got, err := sizing.AggregateOpenRisk(units, dollarsPerPoint)
	if err != nil {
		t.Fatalf("AggregateOpenRisk() error = %v", err)
	}
	if got != rounded {
		t.Fatalf("AggregateOpenRisk() = %v, want %v (each product rounded before it is added; fusing them gives %v)", got, rounded, fused)
	}
}
