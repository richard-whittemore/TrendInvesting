package sizing_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file is the arithmetic seam of the equity-native Golden Scenario: the
// Baseline's sizing and ladder rules worked at a contract multiplier of ONE
// (CONTEXT.md: "Unit"; ADR 0003; ADR 0008). Its event-seam counterpart, the
// same numbers driven through the real reducer and the real fill simulator,
// is internal/fills/equity_golden_test.go, and every constant below is
// repeated there so the two seams cannot drift apart silently.
//
// Faith's own worked examples are futures: Heating Oil carries a 42,000
// multiplier (The Turtle Rules p.15), so they prove the arithmetic without
// ever exercising the equity path. These constants exercise it — a multiplier
// of one, and a whole-share truncation that a reader can check on paper.
//
// # The derivation, in full
//
// The scenario's instrument has N = 2.5 exactly, by construction: every bar
// of the event-seam fixture's warm-up has True Range exactly 2.5, so the
// 20-bar simple average that seeds N is 2.5 (The Turtle Rules p.13), and
// Wilder's own step holds it there — (19 x 2.5 + 2.5) / 20 = 50 / 20 = 2.5,
// exact on paper and exact in float64.
//
//	Notional Account            8,400          (ADR 0007: a configured figure)
//	Unit Volatility Fraction    0.005          (ADR 0003: the Baseline's 0.5 %)
//	Stop Multiple               2              (ADR 0003; The Turtle Rules p.22)
//	N                           2.5            (see above)
//	Dollars per point           1              (equities: one point is one dollar a share)
//
//	budget            = 8,400 x 0.005              = 42
//	cost per unit per N = 2.5 x 1                  = 2.5
//	quantity          = floor(42 / 2.5) = floor(16.8) = 16 shares
//
// # Why 16.8 is the constant and not a rounder one
//
// The Turtle rule truncates (The Turtle Rules p.14-15: 16.88 contracts
// becomes 16, and Faith does not round). A fixture whose quotient sat just
// above a whole share — 16.05, say — would truncate to 16 and ALSO round to
// 16, so it could not fail if the truncation were ever replaced by rounding:
// it would test nothing. 16.8 rounds to 17 and truncates to 16, so the
// expectation below is only satisfied by truncation.
//
// Seventeen shares is not merely a different answer, it is an unsafe one: 17
// x 2.5 = 42.5 exceeds the 42 the Unit Volatility Fraction budgets, so the
// realised risk at the Protective Stop would be 17 x 5 / 8,400 = 1.0119 %
// against a declared 1 %, breaking the RealisedRiskAtStop <= RiskAtStop
// invariant sizing.Unit documents. A truncation boundary at the wrong end of
// the rule costs capital.
//
// The current implementation answers 16 twice over, and the constant pins
// both readings agreeing: the floor itself, and the budget correction that
// follows it, which lowers any quantity whose cost exceeds the budget. Either
// alone gives 16 here, and a reimplementation carrying only a rounding step
// gives 17 — which is what this constant, and not one that rounds the same
// either way, is able to say.
//
// # Why a multiplier of one is load-bearing here
//
// At Faith's Heating Oil multiplier the SAME budget and the SAME N size
// 42 / (2.5 x 42,000) = 0.0004 of a contract — zero whole contracts, and a
// recorded decline instead of a position (The Turtle Rules p.15 names that
// outcome for small accounts). So a fixture that left the multiplier at
// 42,000 could not accidentally pass: the equity path is what produces 16.
const (
	// equityNotionalAccount is the Notional Account these fixtures size
	// against (CONTEXT.md; ADR 0007), chosen so the Unit quotient lands at
	// the whole-share boundary described above rather than near it.
	equityNotionalAccount = 8_400.0
	// equityUnitVolatilityFraction is ADR 0003's Baseline 0.5 % per N.
	equityUnitVolatilityFraction = 0.005
	// equityStopMultiple is the Baseline's 2 N (ADR 0003; The Turtle Rules
	// p.22).
	equityStopMultiple = 2.0
	// equityN is the volatility reading in force, exact by the fixture's
	// construction (see this file's own derivation above).
	equityN = 2.5
	// equityDollarsPerPoint is the equity multiplier: one point of price is
	// one dollar a share.
	equityDollarsPerPoint = 1.0
	// equityHeatingOilDollarsPerPoint is Faith's Heating Oil contract
	// multiplier (The Turtle Rules p.15), used here only as the negative
	// control that shows the multiplier is load-bearing.
	equityHeatingOilDollarsPerPoint = 42_000.0

	// equityUnitQuantity is floor(42 / 2.5) = floor(16.8): the whole shares
	// the rule buys.
	equityUnitQuantity int64 = 16
	// equityRoundedUnitQuantity is what rounding 16.8 would buy instead, and
	// is asserted NOT to be the answer.
	equityRoundedUnitQuantity int64 = 17

	// equityEntryLevel is the Entry Channel high the event-seam fixture's
	// breakout exceeds — the level a resting buy-stop sits at (ADR 0005),
	// never the breakout bar's own high.
	equityEntryLevel = 156.0
	// equityEntryFill is that level plus ADR 0013's 0.05 N of slippage
	// against the trader: 156 + 0.05 x 2.5 = 156 + 0.125.
	equityEntryFill = 156.125
	// equityUnitOneStop is the Protective Stop of the Unit that filled at
	// equityEntryFill: 156.125 - 2 x 2.5 (The Turtle Rules p.22).
	equityUnitOneStop = 151.125
	// equityRungTwo is the second Add Ladder rung, measured from the ACTUAL
	// fill and not from the intended level (The Turtle Rules p.19; ADR
	// 0013): 156.125 + 0.5 x 2.5.
	equityRungTwo = 157.375
	// equityRungThree and equityRungFour continue the intended ladder at the
	// same half-N spacing, assuming each Unit lands exactly on its own rung.
	equityRungThree = 158.625
	equityRungFour  = 159.875
)

// equityTolerance is the margin every price comparison here uses:
// .greptile/rules.md forbids comparing prices with ==. It is far tighter
// than the smallest difference any assertion below turns on (a cent), so a
// wrong answer cannot hide inside it.
const equityTolerance = 1e-9

func equityClose(got, want float64) bool {
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff <= equityTolerance
}

// TestEquityUnitTruncatesAtTheWholeShareBoundary is the truncation boundary
// itself: 42 / 2.5 is 16.8, and the Turtle rule buys 16 shares, not 17 (The
// Turtle Rules p.14-15).
//
// The rounded answer is asserted explicitly as the wrong one, because the
// whole value of this constant is that the two differ — see this file's own
// note on why 16.8 rather than a quotient that rounds the same either way.
func TestEquityUnitTruncatesAtTheWholeShareBoundary(t *testing.T) {
	t.Parallel()

	got, err := sizing.UnitQuantity(equityNotionalAccount, equityUnitVolatilityFraction, equityN, equityDollarsPerPoint)
	if err != nil {
		t.Fatalf("UnitQuantity() error = %v", err)
	}
	if got == equityRoundedUnitQuantity {
		t.Fatalf("UnitQuantity() = %d, which is 16.8 ROUNDED; The Turtle Rules p.14-15 truncates, so the answer is %d", got, equityUnitQuantity)
	}
	if got != equityUnitQuantity {
		t.Fatalf("UnitQuantity() = %d, want %d (floor(8,400 x 0.005 / (2.5 x 1)) = floor(16.8))", got, equityUnitQuantity)
	}
}

// TestEquityUnitSizesAtAMultiplierOfOne is the equity path end to end at the
// arithmetic seam: the quantity, the declared Risk at Stop (ADR 0003:
// derived, never configured) and what those whole shares actually risk.
//
//	RiskAtStop         = 0.005 x 2                       = 1 %
//	RealisedRiskAtStop = 16 x 2 x 2.5 x 1 / 8,400 = 80/8,400 = 0.9523809...%
//
// The gap between them IS the truncation, and it always points the same way:
// a truncated Unit risks less than the budget, never more. Faith's own Unit
// shows the same gap at a 42,000 multiplier — a declared 2 % realised as
// 1.895 % once 16.88 contracts become 16 (The Turtle Rules p.15).
func TestEquityUnitSizesAtAMultiplierOfOne(t *testing.T) {
	t.Parallel()

	unit, err := sizing.SizeUnit(sizing.Inputs{
		Mode:                   sizing.ModeVolatilityNormalised,
		NotionalAccount:        equityNotionalAccount,
		UnitVolatilityFraction: equityUnitVolatilityFraction,
		StopMultiple:           equityStopMultiple,
		N:                      equityN,
		DollarsPerPoint:        equityDollarsPerPoint,
	})
	if err != nil {
		t.Fatalf("SizeUnit() error = %v", err)
	}

	if unit.Quantity != equityUnitQuantity {
		t.Errorf("Quantity = %d, want %d", unit.Quantity, equityUnitQuantity)
	}
	// 0.005 x 2 on paper; stated as the product of the two declared
	// parameters rather than as 0.01 so the assertion says which rule it
	// checks.
	if want := equityUnitVolatilityFraction * equityStopMultiple; !equityClose(unit.RiskAtStop, want) {
		t.Errorf("RiskAtStop = %v, want %v (unit volatility fraction x stop multiple, ADR 0003)", unit.RiskAtStop, want)
	}
	// 80 / 8,400 written as the hand-derived rational, so the expectation is
	// not a restatement of the producer's own expression.
	if want := 80.0 / 8_400.0; !equityClose(unit.RealisedRiskAtStop, want) {
		t.Errorf("RealisedRiskAtStop = %v, want %v (16 shares x 2 N x 2.5 x 1 / 8,400)", unit.RealisedRiskAtStop, want)
	}
	if unit.RealisedRiskAtStop >= unit.RiskAtStop {
		t.Errorf("RealisedRiskAtStop %v is not below the declared budget %v: truncation must always leave a Unit risking less, never more",
			unit.RealisedRiskAtStop, unit.RiskAtStop)
	}
}

// TestEquityUnitAtFaithsContractMultiplierBuysNothing is the negative that
// makes the multiplier load-bearing: the identical account, fraction and N at
// Heating Oil's 42,000 multiplier (The Turtle Rules p.15) size 0.0004 of a
// contract, which truncates to nothing at all.
//
// Zero is returned without an error — a fact about the account, not a
// failure; The Turtle Rules p.15 names it directly, and the reducer journals
// it as a decline rather than a position.
func TestEquityUnitAtFaithsContractMultiplierBuysNothing(t *testing.T) {
	t.Parallel()

	got, err := sizing.UnitQuantity(equityNotionalAccount, equityUnitVolatilityFraction, equityN, equityHeatingOilDollarsPerPoint)
	if err != nil {
		t.Fatalf("UnitQuantity() error = %v", err)
	}
	if got != 0 {
		t.Fatalf("UnitQuantity() at a 42,000 multiplier = %d, want 0: 42 / (2.5 x 42,000) is 0.0004 of a contract", got)
	}
}

// TestEquityCampaignLaddersFromTheSlippedFill pins the three levels a
// Campaign opened by this fixture's entry carries, all measured from the
// ACTUAL fill of 156.125 rather than from the 156 the order rested at (The
// Turtle Rules p.19: "measured from the actual fill of the previous unit ...
// slippage on the first fill pushes later adds out accordingly"; ADR 0013).
//
// Measuring from the resting level instead would put the stop at 151 and the
// second rung at 157.25 — a quarter of a point and an eighth of a point out
// respectively, and enough for the event-seam fixture's own bar 57 to reach a
// rung it must not reach.
func TestEquityCampaignLaddersFromTheSlippedFill(t *testing.T) {
	t.Parallel()

	stop, err := sizing.ProtectiveStopLevel(equityEntryFill, equityN, equityStopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel() error = %v", err)
	}
	if !equityClose(stop, equityUnitOneStop) {
		t.Errorf("ProtectiveStopLevel() = %v, want %v (156.125 - 2 x 2.5)", stop, equityUnitOneStop)
	}

	rung, err := sizing.NextAddLevel(equityEntryFill, equityN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}
	if !equityClose(rung, equityRungTwo) {
		t.Errorf("NextAddLevel() = %v, want %v (156.125 + 0.5 x 2.5)", rung, equityRungTwo)
	}

	// The same two derivations from the level the order RESTED at, which is
	// what makes "measured from the actual fill" load-bearing rather than
	// decorative: both come out a fraction lower, and the event-seam
	// fixture's bar 57 turns on the difference.
	fromLevel, err := sizing.ProtectiveStopLevel(equityEntryLevel, equityN, equityStopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel(from the resting level) error = %v", err)
	}
	if equityClose(fromLevel, equityUnitOneStop) {
		t.Errorf("a stop measured from the resting level %v is indistinguishable from one measured from the fill %v: this fixture's slippage no longer separates them",
			equityEntryLevel, equityEntryFill)
	}

	ladder, err := sizing.AddLadder(equityEntryFill, equityN, 4, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("AddLadder() error = %v", err)
	}
	want := []float64{equityEntryFill, equityRungTwo, equityRungThree, equityRungFour}
	if len(ladder) != len(want) {
		t.Fatalf("AddLadder() returned %d rungs, want %d (ADR 0008 caps a Campaign at 4 Units)", len(ladder), len(want))
	}
	for i := range want {
		if !equityClose(ladder[i], want[i]) {
			t.Errorf("AddLadder()[%d] = %v, want %v", i, ladder[i], want[i])
		}
	}
}
