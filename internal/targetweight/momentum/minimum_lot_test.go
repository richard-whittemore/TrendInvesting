package momentum_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// lot values every capCandidates pick at a $1,000 account, the momentum
// sleeve at full scale, with prices that leave each pick affordable unless a
// test raises one.
func lot(prices map[string]float64) momentum.MinimumLot {
	all := map[string]float64{"A": 100, "B": 100, "C": 100, "D": 100, "E": 100, "F": 50, "G": 100}
	for id, p := range prices {
		all[id] = p
	}
	return momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: all}
}

func statusOf(book momentum.Book) map[string]momentum.RankedName {
	out := map[string]momentum.RankedName{}
	for _, r := range book.Ranked {
		out[r.InstrumentID] = r
	}
	return out
}

func targetIDs(book momentum.Book) []string {
	var out []string
	for _, t := range book.Targets {
		out = append(out, t.InstrumentID)
	}
	return out
}

// TestMinimumLotSkipsAPickWhoseOneShareExceedsItsTarget prices C's one share
// at $150 against its $140 target (0.98 x 25/175 of $1,000): C is recorded
// unaffordable and F, the next eligible ranked name, takes its place (ADR
// 0026, point 2, "D7-L"; plan.md lines 380-383).
func TestMinimumLotSkipsAPickWhoseOneShareExceedsItsTarget(t *testing.T) {
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), capCandidates(), false, lot(map[string]float64{"C": 150}))
	if err != nil {
		t.Fatal(err)
	}
	s := statusOf(book)
	mustEqual(t, "C", s["C"].Status, momentum.StatusUnaffordable)
	mustEqual(t, "C one share", s["C"].OneShare, 150.0)
	mustEqual(t, "C target value", s["C"].TargetValue, float64(float64(0.98*(1/0.04)/175)*1000))
	mustEqual(t, "C weight", s["C"].Weight, 0.0)
	mustEqual(t, "F", s["F"].Status, momentum.StatusPicked)
	mustEqual(t, "B", s["B"].Status, momentum.StatusSectorCapped)
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), "A,E,F")
	for _, r := range book.Ranked {
		if r.Status == momentum.StatusPicked && r.OneShare != 0 {
			t.Errorf("%s picked with a one-share price recorded", r.InstrumentID)
		}
	}
	// The backtest, which had no minimum lot, would have picked C.
	mustEqual(t, "C backtest pick", s["C"].BacktestWouldPick, true)
}

// TestMinimumLotFreesTheSectorSlot makes A unaffordable: its slot in sector
// X goes to B, which the cap had kept out, so the cap still holds (one per
// sector) and counts only the names held.
func TestMinimumLotFreesTheSectorSlot(t *testing.T) {
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), capCandidates(), false, lot(map[string]float64{"A": 300}))
	if err != nil {
		t.Fatal(err)
	}
	s := statusOf(book)
	mustEqual(t, "A", s["A"].Status, momentum.StatusUnaffordable)
	mustEqual(t, "B", s["B"].Status, momentum.StatusPicked)
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), "B,C,E")
}

// TestMinimumLotIteratesUntilEveryPickIsAffordable: skipping C and taking F
// lowers A's weight from 0.28 to about 0.267, so A at $270 becomes
// unaffordable on the second pass, and B takes its sector's slot. Selection
// repeats until no pick's one share exceeds its target; each pass skips at
// least one more name, so it ends.
func TestMinimumLotIteratesUntilEveryPickIsAffordable(t *testing.T) {
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), capCandidates(), false, lot(map[string]float64{"C": 150, "A": 270}))
	if err != nil {
		t.Fatal(err)
	}
	s := statusOf(book)
	mustEqual(t, "A", s["A"].Status, momentum.StatusUnaffordable)
	mustEqual(t, "C", s["C"].Status, momentum.StatusUnaffordable)
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), "B,E,F")
	for _, r := range book.Ranked {
		if r.Status == momentum.StatusPicked {
			target := float64(float64(r.Weight*1) * 1000)
			if lot(map[string]float64{"C": 150, "A": 270}).Prices[r.InstrumentID] > target {
				t.Errorf("%s picked at a target of %v", r.InstrumentID, target)
			}
		}
	}
}

// TestMinimumLotUsesTheMomentumSleevesScale halves the sleeve (b = 0.5):
// E's $560 target becomes $280, below its $300 share.
func TestMinimumLotUsesTheMomentumSleevesScale(t *testing.T) {
	l := lot(map[string]float64{"E": 300})
	full, err := momentum.SelectBookWithMinimumLot(smallBook(), capCandidates(), false, l)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "E at full scale", statusOf(full)["E"].Status, momentum.StatusPicked)
	l.Scale = 0.5
	half, err := momentum.SelectBookWithMinimumLot(smallBook(), capCandidates(), false, l)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "E at half scale", statusOf(half)["E"].Status, momentum.StatusUnaffordable)
}

func TestMinimumLotWithEveryPickAffordableIsTheBook(t *testing.T) {
	plain, err := momentum.SelectBook(smallBook(), capCandidates(), true)
	if err != nil {
		t.Fatal(err)
	}
	// Risk-off halves the book, so C's target is $70 a $1,000 account: at
	// $2,000 every pick is affordable.
	affordable := lot(nil)
	affordable.NetLiquidation = 2000
	withLot, err := momentum.SelectBookWithMinimumLot(smallBook(), capCandidates(), true, affordable)
	if err != nil {
		t.Fatal(err)
	}
	for i := range plain.Ranked {
		if plain.Ranked[i] != withLot.Ranked[i] {
			t.Errorf("rank %d: %+v, want %+v", i, withLot.Ranked[i], plain.Ranked[i])
		}
	}
	mustEqual(t, "targets", strings.Join(targetIDs(withLot), ","), strings.Join(targetIDs(plain), ","))
}

func TestMinimumLotWithNoAffordablePickPicksNothing(t *testing.T) {
	expensive := map[string]float64{}
	for _, id := range []string{"A", "B", "C", "D", "E", "F", "G"} {
		expensive[id] = 5000
	}
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), capCandidates(), false, lot(expensive))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range book.Ranked {
		if r.Status == momentum.StatusPicked {
			t.Errorf("%s picked", r.InstrumentID)
		}
	}
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), "IEF")
}

func TestMinimumLotRefusesBadInputs(t *testing.T) {
	for _, c := range []struct {
		name string
		lot  momentum.MinimumLot
		want string
	}{
		{"no net liquidation", momentum.MinimumLot{Scale: 1, Prices: lot(nil).Prices}, "net liquidation"},
		{"scale above 1", momentum.MinimumLot{NetLiquidation: 1000, Scale: 1.5, Prices: lot(nil).Prices}, "scale"},
		{"no price", momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"A": 1}}, "price"},
		{"zero price", lot(map[string]float64{"E": 0}), "price"},
	} {
		if _, err := momentum.SelectBookWithMinimumLot(smallBook(), capCandidates(), false, c.lot); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// TestDecideRecordsTheMinimumLot runs Decide under D7-L: with AAA's one
// share above its target the decision records it unaffordable with its
// figures and holds BBB alone; with every share above its target the
// decision is made, every name recorded unaffordable and the whole of
// Invested in the risk-off instrument, so no stock is held.
func TestDecideRecordsTheMinimumLot(t *testing.T) {
	in := decideInput()
	in.MinimumLot = &momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"AAA": 900, "BBB": 40}}
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
	aaa := d.Names[0]
	mustEqual(t, "AAA", aaa.Status, momentum.StatusUnaffordable)
	mustEqual(t, "AAA one share", aaa.OneShare, 900.0)
	if !(aaa.TargetValue > 0 && aaa.TargetValue < 900) {
		t.Errorf("AAA target value %v", aaa.TargetValue)
	}
	mustEqual(t, "BBB", d.Names[1].Status, momentum.StatusPicked)
	mustEqual(t, "targets", len(d.Targets), 1)

	in.MinimumLot.Prices["BBB"] = 2000
	d, err = momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
	mustEqual(t, "AAA every share dear", d.Names[0].Status, momentum.StatusUnaffordable)
	mustEqual(t, "BBB every share dear", d.Names[1].Status, momentum.StatusUnaffordable)
	mustEqual(t, "risk-off only", fmt.Sprint(d.Targets), fmt.Sprint([]momentum.Target{{InstrumentID: "IEF", Weight: 0.98}}))

	in.MinimumLot.Prices = map[string]float64{}
	if _, err := momentum.Decide(momentum.FCombo(), in); err == nil {
		t.Error("a minimum lot without prices was accepted")
	}
}

// TestMinimumLotLeavesAFullyRiskOffBookAsSelectBookMakesIt: with
// RiskOffFraction 1 a risk-off book gives every pick a zero target, which
// is no target value for D7-L to compare a share against, so no pick is
// skipped and the book is SelectBook's: the whole of Invested in the
// risk-off instrument.
func TestMinimumLotLeavesAFullyRiskOffBookAsSelectBookMakesIt(t *testing.T) {
	cfg := smallBook()
	cfg.RiskOffFraction = 1
	plain, err := momentum.SelectBook(cfg, capCandidates(), true)
	if err != nil {
		t.Fatal(err)
	}
	book, err := momentum.SelectBookWithMinimumLot(cfg, capCandidates(), true, lot(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range book.Ranked {
		if r.Status == momentum.StatusUnaffordable {
			t.Errorf("%s unaffordable at a zero target", r.InstrumentID)
		}
	}
	mustEqual(t, "targets", strings.Join(targetIDs(book), ","), strings.Join(targetIDs(plain), ","))
	last := book.Targets[len(book.Targets)-1]
	mustEqual(t, "risk-off target", last, momentum.Target{InstrumentID: cfg.RiskOffInstrument, Weight: cfg.Invested})
}

// TestDecideMovesAnUnaffordablePartialRiskOffBookToTheRiskOffInstrument:
// with RiskOffFraction 0.5 a risk-off book keeps a positive target for each
// pick, and with every candidate's one share above it D7-L skips them all.
// The decision is made with the whole of Invested in the risk-off
// instrument, every candidate recorded unaffordable, so the planner sells
// every stock held rather than keeping the old book through the month.
func TestDecideMovesAnUnaffordablePartialRiskOffBookToTheRiskOffInstrument(t *testing.T) {
	in := decideInput()
	prices := trending(300, 1.001)
	prices[299] = 50
	in.Closes["SPY"] = series(in.Sessions, prices)
	in.MinimumLot = &momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"AAA": 900, "BBB": 900}}
	cfg := momentum.FCombo()
	mustEqual(t, "partial risk-off", cfg.RiskOffFraction, 0.5)
	d, err := momentum.Decide(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "risk-off", d.Trend.RiskOff, true)
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
	for _, n := range d.Names[:2] {
		mustEqual(t, n.InstrumentID, n.Status, momentum.StatusUnaffordable)
		if !(n.TargetValue > 0 && n.TargetValue < n.OneShare) {
			t.Errorf("%s target value %v against one share %v", n.InstrumentID, n.TargetValue, n.OneShare)
		}
	}
	mustEqual(t, "risk-off only", fmt.Sprint(d.Targets), fmt.Sprint([]momentum.Target{{InstrumentID: "IEF", Weight: cfg.Invested}}))
}

// TestDecideWithNoCandidateUnderTheLotIsStillWithheld: D7-L decides an
// all-unaffordable book, but a book with no pick for any other reason (here
// every member unlabelled) is still withheld as no-eligible-names.
func TestDecideWithNoCandidateUnderTheLotIsStillWithheld(t *testing.T) {
	in := decideInput()
	for i := range in.Universe.Rows {
		in.Universe.Rows[i].Sector, in.Universe.Rows[i].SectorSource = "", ""
	}
	in.MinimumLot = &momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"AAA": 1, "BBB": 1}}
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeWithheld)
	mustEqual(t, "reason", d.Reason, momentum.ReasonNoEligibleNames)
}
