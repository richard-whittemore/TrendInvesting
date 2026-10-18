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

// lotCandidates is capCandidates without D, its unlabelled name, which the
// rule picks uncapped as the third pick: these tests value the book of A, C
// and E and the names that replace them.
func lotCandidates() []momentum.Candidate {
	var out []momentum.Candidate
	for _, c := range capCandidates() {
		if c.InstrumentID != "D" {
			out = append(out, c)
		}
	}
	return out
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
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, lot(map[string]float64{"C": 150}))
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
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, lot(map[string]float64{"A": 300}))
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
	book, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, lot(map[string]float64{"C": 150, "A": 270}))
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
	full, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, l)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "E at full scale", statusOf(full)["E"].Status, momentum.StatusPicked)
	l.Scale = 0.5
	half, err := momentum.SelectBookWithMinimumLot(smallBook(), lotCandidates(), false, l)
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
// decision is withheld as all-unaffordable, every name recorded unaffordable
// with the figures that valued it and a detail naming the one-share lot and
// the net liquidation, so the book is kept rather than sold on a valuation
// that may be wrong (ADR 0026, point 5, "fail closed").
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
	mustEqual(t, "detail of a book with a pick", d.Detail, "")

	in.MinimumLot.Prices["BBB"] = 2000
	d, err = momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustAllUnaffordable(t, d, 2)
	mustEqual(t, "detail", d.Detail, "2 candidates valued, every one unaffordable (one share at its previous raw close exceeds its target value: weight x momentum scale 1 x net liquidation 1000.00); none left out; the book is kept: check the net liquidation and the raw closes")

	in.MinimumLot.Prices = map[string]float64{}
	if _, err := momentum.Decide(momentum.FCombo(), in); err == nil {
		t.Error("a minimum lot without prices was accepted")
	}
}

// mustAllUnaffordable checks a decision D7-L withheld as all-unaffordable:
// no targets, the first n names unaffordable with the figures that valued
// them, and a detail naming the one-share lot and the $1,000 net liquidation.
func mustAllUnaffordable(t *testing.T, d momentum.Decision, n int) {
	t.Helper()
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeWithheld)
	mustEqual(t, "reason", d.Reason, momentum.ReasonAllUnaffordable)
	mustEqual(t, "targets", len(d.Targets), 0)
	for _, name := range d.Names[:n] {
		mustEqual(t, name.InstrumentID, name.Status, momentum.StatusUnaffordable)
		if !(name.TargetValue > 0 && name.TargetValue < name.OneShare) {
			t.Errorf("%s target value %v against one share %v", name.InstrumentID, name.TargetValue, name.OneShare)
		}
	}
	for _, want := range []string{"one share", "net liquidation 1000.00"} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("detail %q does not name %q", d.Detail, want)
		}
	}
}

// TestDecideSubstitutesAnUnlabelledNameForADearOne leaves BBB without a
// sector label and prices AAA's one share above its target: BBB is ranked
// and picked uncapped, as the backtest picked such a name (ADR 0026 point
// 8, the owner's decision of 2026-10-18), so it takes AAA's place and the
// decision is decided, not withheld. One unlabelled member in four is above
// the default unlabelled-share ceiling, so the ceiling is raised to isolate
// the substitution.
func TestDecideSubstitutesAnUnlabelledNameForADearOne(t *testing.T) {
	in := decideInput()
	in.Universe.Rows[1].Sector, in.Universe.Rows[1].SectorSource = "", ""
	in.MinimumLot = &momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"AAA": 2000, "BBB": 1}}
	cfg := momentum.FCombo()
	cfg.MaxUnlabelledShare = 0.5
	d, err := momentum.Decide(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
	mustEqual(t, "AAA", d.Names[0].Status, momentum.StatusUnaffordable)
	mustEqual(t, "BBB", d.Names[1].Status, momentum.StatusPicked)
}

// TestDecideNamesTheZeroVolatilityLeftOut flattens BBB, so its volatility is
// zero and it is left out as zero-volatility, while AAA's one share is above
// its target: the detail counts the one valued name and names the reason the
// other was left out, not every candidate as valued.
func TestDecideNamesTheZeroVolatilityLeftOut(t *testing.T) {
	in := decideInput()
	in.Closes["BBB"] = series(in.Sessions, constant(300, 40))
	in.MinimumLot = &momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"AAA": 2000, "BBB": 1}}
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustAllUnaffordable(t, d, 1)
	mustEqual(t, "BBB", d.Names[1].Status, momentum.StatusZeroVolatility)
	mustEqual(t, "detail", d.Detail, "1 candidate valued, every one unaffordable (one share at its previous raw close exceeds its target value: weight x momentum scale 1 x net liquidation 1000.00); 1 left out (1 zero volatility); the book is kept: check the net liquidation and the raw closes")
}

// TestDecideExplainsAFullyRiskOffLiquidation: with RiskOffFraction 1 in a
// risk-off month every pick's target weight is zero, so the decision is made
// with the whole of Invested in the risk-off instrument and carries a detail
// saying every stock held is sold, with the lot and without it.
func TestDecideExplainsAFullyRiskOffLiquidation(t *testing.T) {
	for name, lot := range map[string]*momentum.MinimumLot{
		"plain":       nil,
		"minimum lot": {NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"AAA": 2000, "BBB": 2000}},
	} {
		t.Run(name, func(t *testing.T) {
			in := decideInput()
			prices := trending(300, 1.001)
			prices[299] = 50
			in.Closes["SPY"] = series(in.Sessions, prices)
			in.MinimumLot = lot
			cfg := momentum.FCombo()
			cfg.RiskOffFraction = 1
			d, err := momentum.Decide(cfg, in)
			if err != nil {
				t.Fatal(err)
			}
			mustEqual(t, "outcome", d.Outcome, momentum.OutcomeDecided)
			mustEqual(t, "targets", fmt.Sprint(d.Targets), fmt.Sprint([]momentum.Target{{InstrumentID: "AAA"}, {InstrumentID: "BBB"}, {InstrumentID: "IEF", Weight: cfg.Invested}}))
			for _, want := range []string{"every pick's target weight is zero", "IEF", "every stock held is sold"} {
				if !strings.Contains(d.Detail, want) {
					t.Errorf("detail %q does not name %q", d.Detail, want)
				}
			}
		})
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

// TestDecideWithholdsAnUnaffordablePartialRiskOffBook: with RiskOffFraction
// 0.5 a risk-off book keeps a positive target for each pick, and with every
// candidate's one share above it D7-L skips them all. The decision is
// withheld as all-unaffordable and the old book kept, since a systematic
// valuation error (a bad net liquidation or bad raw closes) looks the same.
func TestDecideWithholdsAnUnaffordablePartialRiskOffBook(t *testing.T) {
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
	mustAllUnaffordable(t, d, 2)
}

// TestDecideWithNoCandidateUnderTheLotIsStillWithheld: a book with no pick
// and none skipped as unaffordable (here every scored member with zero
// volatility) is withheld as no-eligible-names, not all-unaffordable.
func TestDecideWithNoCandidateUnderTheLotIsStillWithheld(t *testing.T) {
	in := decideInput()
	in.Closes["AAA"] = series(in.Sessions, constant(300, 50))
	in.Closes["BBB"] = series(in.Sessions, constant(300, 40))
	in.MinimumLot = &momentum.MinimumLot{NetLiquidation: 1000, Scale: 1, Prices: map[string]float64{"AAA": 1, "BBB": 1}}
	d, err := momentum.Decide(momentum.FCombo(), in)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "outcome", d.Outcome, momentum.OutcomeWithheld)
	mustEqual(t, "reason", d.Reason, momentum.ReasonNoEligibleNames)
}
