package momentum_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// heldLot is lot with the account holding the given shares.
func heldLot(prices map[string]float64, held map[string]int64) momentum.MinimumLot {
	l := lot(prices)
	l.Held = held
	return l
}

// TestMinimumLotKeepsAHeldPickOneLotShort holds 2 shares of C and prices
// its one share at $150 against its $140 target: C is held, so it is not
// skipped. It keeps its place in the book, its sector slot and its weight,
// and is recorded held-one-lot-short with the one-share price, the target
// value and the holding it keeps: 2 shares worth $300, 30% of the $1,000
// account (ADR 0026, point 2, "D7-L", amended 2026-10-18). F, which would
// have taken C's place, stays beyond the book.
func TestMinimumLotKeepsAHeldPickOneLotShort(t *testing.T) {
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, heldLot(map[string]float64{"C": 150}, map[string]int64{"C": 2}))
	if err != nil {
		t.Fatal(err)
	}
	s := statusOf(book)
	weight := 0.98 * (1 / 0.04) / 175
	mustEqual(t, "C", s["C"].Status, momentum.StatusHeldOneLotShort)
	mustEqual(t, "C weight", s["C"].Weight, weight)
	mustEqual(t, "C one share", s["C"].OneShare, 150.0)
	mustEqual(t, "C target value", s["C"].TargetValue, float64(weight*1000))
	mustEqual(t, "C held quantity", s["C"].HeldQuantity, int64(2))
	mustEqual(t, "C held value", s["C"].HeldValue, 300.0)
	mustEqual(t, "C held share", s["C"].HeldShare, 0.3)
	mustEqual(t, "F", s["F"].Status, momentum.StatusBeyondTopN)
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), "A,C,E")
	for _, r := range book.Ranked {
		if r.Status == momentum.StatusPicked && (r.OneShare != 0 || r.HeldQuantity != 0 || r.HeldValue != 0 || r.HeldShare != 0) {
			t.Errorf("%s picked with one-lot figures %+v", r.InstrumentID, r)
		}
	}
}

// TestMinimumLotKeepsAHeldPicksSectorSlot holds A, dear by the share: A
// keeps sector X's one slot, so B stays capped behind it, where an A not
// held would have freed the slot for B (TestMinimumLotFreesTheSectorSlot).
func TestMinimumLotKeepsAHeldPicksSectorSlot(t *testing.T) {
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, heldLot(map[string]float64{"A": 300}, map[string]int64{"A": 1}))
	if err != nil {
		t.Fatal(err)
	}
	s := statusOf(book)
	mustEqual(t, "A", s["A"].Status, momentum.StatusHeldOneLotShort)
	mustEqual(t, "B", s["B"].Status, momentum.StatusSectorCapped)
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), "A,C,E")
}

// TestMinimumLotStillSkipsAPickNotHeld: the exception is for a held pick
// only. With E held and C dear but not held, C is skipped and F takes its
// place exactly as before the amendment, and an affordable held pick (E)
// is an ordinary pick with no one-lot figures.
func TestMinimumLotStillSkipsAPickNotHeld(t *testing.T) {
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, heldLot(map[string]float64{"C": 150}, map[string]int64{"E": 3}))
	if err != nil {
		t.Fatal(err)
	}
	s := statusOf(book)
	mustEqual(t, "C", s["C"].Status, momentum.StatusUnaffordable)
	mustEqual(t, "C held quantity", s["C"].HeldQuantity, int64(0))
	mustEqual(t, "F", s["F"].Status, momentum.StatusPicked)
	mustEqual(t, "E", s["E"].Status, momentum.StatusPicked)
	mustEqual(t, "E held value", s["E"].HeldValue, 0.0)
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), "A,E,F")
}

// invVolWeight is the weight SelectBook gives a pick of volatility vol in a
// book of picks with volatilities vols, in pick order, out of invested,
// computed in the order SelectBook computes it so it matches to the bit.
func invVolWeight(invested, vol float64, vols ...float64) float64 {
	total := 0.0
	for _, v := range vols {
		total += 1 / v
	}
	return invested * (1 / vol) / total
}

// TestMinimumLotKeepsAHeldPickThatBecomesDearOnALaterPass holds one A at
// $270, affordable on the first pass at its $280 target, and prices C, not
// held, at $150: C is skipped, F takes its place, and A's weight falls to
// about 0.267, so on the second pass A's one share exceeds its target. A is
// held, so it is kept one lot short with that pass's figures, not skipped as
// TestMinimumLotIteratesUntilEveryPickIsAffordable skips it; a decided book
// then has both a kept and an unaffordable name.
func TestMinimumLotKeepsAHeldPickThatBecomesDearOnALaterPass(t *testing.T) {
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, heldLot(map[string]float64{"C": 150, "A": 270}, map[string]int64{"A": 1}))
	if err != nil {
		t.Fatal(err)
	}
	s := statusOf(book)
	weight := invVolWeight(0.98, 0.02, 0.02, 0.01, 0.03)
	mustEqual(t, "A", s["A"].Status, momentum.StatusHeldOneLotShort)
	mustEqual(t, "A weight", s["A"].Weight, weight)
	mustEqual(t, "A target value", s["A"].TargetValue, float64(weight*1000))
	mustEqual(t, "A one share", s["A"].OneShare, 270.0)
	mustEqual(t, "A held value", s["A"].HeldValue, 270.0)
	mustEqual(t, "A held share", s["A"].HeldShare, 0.27)
	mustEqual(t, "C", s["C"].Status, momentum.StatusUnaffordable)
	mustEqual(t, "C held quantity", s["C"].HeldQuantity, int64(0))
	mustEqual(t, "B", s["B"].Status, momentum.StatusSectorCapped)
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), "A,E,F")
}

// TestMinimumLotPicksAHeldNameThatBecomesAffordable holds 2 C at $200, dear
// on the first pass at its $140 target, and prices E, not held, at $600
// against its $560: E is skipped and F takes its place, which raises C's
// weight to about 0.226, a $226 target its one share fits. C ends an
// ordinary pick, its one-lot and holding figures from the first pass
// cleared, since only the final pass marks a held name kept one lot short.
func TestMinimumLotPicksAHeldNameThatBecomesAffordable(t *testing.T) {
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, heldLot(map[string]float64{"C": 200, "E": 600}, map[string]int64{"C": 2}))
	if err != nil {
		t.Fatal(err)
	}
	s := statusOf(book)
	mustEqual(t, "E", s["E"].Status, momentum.StatusUnaffordable)
	mustEqual(t, "C", s["C"].Status, momentum.StatusPicked)
	c := s["C"]
	if c.OneShare != 0 || c.TargetValue != 0 || c.HeldQuantity != 0 || c.HeldValue != 0 || c.HeldShare != 0 {
		t.Errorf("C picked with one-lot figures %+v", c)
	}
	mustEqual(t, "C weight", c.Weight, invVolWeight(0.98, 0.04, 0.02, 0.04, 0.03))
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), "A,C,F")
}

// TestMinimumLotRefusesABadHolding: a holding is whole shares, at least one.
func TestMinimumLotRefusesABadHolding(t *testing.T) {
	if _, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, heldLot(nil, map[string]int64{"C": 0})); err == nil || !strings.Contains(err.Error(), "held") {
		t.Errorf("a holding of no shares: %v", err)
	}
}

// TestDecideKeepsAHeldPickOneLotShort holds 3 AAA at $900 a share, above
// its target: the decision is decided with AAA kept at its weight beside
// BBB, both targeted, and a detail naming AAA's holding, its value and its
// share of net liquidation, the target value and the one-share price, so
// the concentration a kept holding carries is on the record.
func TestDecideKeepsAHeldPickOneLotShort(t *testing.T) {
	in := decideInput()
	in.MinimumLot = &momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"AAA": 900, "BBB": 40}, Held: map[string]int64{"AAA": 3}}
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
	aaa := d.Names[0]
	mustEqual(t, "AAA", aaa.Status, momentum.StatusHeldOneLotShort)
	mustEqual(t, "AAA held value", aaa.HeldValue, 2700.0)
	mustEqual(t, "AAA held share", aaa.HeldShare, 2.7)
	mustEqual(t, "AAA held quantity", aaa.HeldQuantity, int64(3))
	if !(aaa.Weight > 0 && aaa.TargetValue == float64(aaa.Weight*1000) && aaa.OneShare == 900) {
		t.Errorf("AAA %+v", aaa)
	}
	mustEqual(t, "targets", len(d.Targets), 2)
	for _, want := range []string{"AAA holds 3 shares worth 2700.00 (270% of net liquidation 1000.00)", "one share 900.00", "no order"} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("detail %q does not name %q", d.Detail, want)
		}
	}
}

// TestDecideWithholdsABookWithNoAffordablePickThoughOneIsHeld: AAA is held
// and kept one lot short, its $1,200 share above even the $980 it would get
// alone, and BBB is dear and not held. No pick is affordable,
// so the decision is withheld as all-unaffordable and the whole book kept:
// a bad net liquidation or bad raw closes would look the same, and deciding
// a book of kept holdings alone would sell every other stock held on it
// (ADR 0026, point 5, "fail closed").
func TestDecideWithholdsABookWithNoAffordablePickThoughOneIsHeld(t *testing.T) {
	in := decideInput()
	in.MinimumLot = &momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"AAA": 1200, "BBB": 2000}, Held: map[string]int64{"AAA": 3}}
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeWithheld)
	mustEqual(t, "reason", d.Reason, momentum.ReasonAllUnaffordable)
	mustEqual(t, "targets", len(d.Targets), 0)
	mustEqual(t, "AAA", d.Names[0].Status, momentum.StatusHeldOneLotShort)
	mustEqual(t, "BBB", d.Names[1].Status, momentum.StatusUnaffordable)
	mustEqual(t, "detail", d.Detail, "2 candidates valued, every one unaffordable (one share at its previous raw close exceeds its target value: weight x momentum scale 1 x net liquidation 1000.00), 1 of them held; none left out; the book is kept: check the net liquidation and the raw closes")
}
