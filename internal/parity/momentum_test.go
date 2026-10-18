package parity_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

var plan = []string{"2008-11", "2009-06"}

func months() []parity.MomentumMonth {
	return []parity.MomentumMonth{syntheticMonth("2008-11", true), syntheticMonth("2009-06", false)}
}

func compare(ms []parity.MomentumMonth) parity.Report {
	return parity.CompareMomentum(momentum.FCombo(), "SYN", plan, ms)
}

func TestMatchingExportPassesEveryCheck(t *testing.T) {
	ms := months()
	if !ms[0].RiskOff || ms[1].RiskOff {
		t.Fatal("fixture: the first month must be risk-off and the second risk-on")
	}
	r := compare(ms)
	for _, c := range r.Checks {
		if !c.Pass() {
			t.Errorf("%s failed: %+v", c.Name, c)
		}
	}
	if !r.Pass() {
		t.Error("a matching export must pass")
	}
	if len(r.Differences) != 0 {
		t.Errorf("a matching export has no differences, got %+v", r.Differences)
	}
	for _, name := range []string{parity.CheckMonths, parity.CheckExport, parity.CheckRiskOff, parity.CheckRankedOrder,
		parity.CheckCapMarks, parity.CheckWeights, parity.CheckScores, parity.CheckVolatility, parity.CheckHistory,
		parity.CheckDifferences} {
		if c := checkNamed(r, name); c.Run != "SYN" {
			t.Errorf("%s: run %q", name, c.Run)
		}
	}
	if c := checkNamed(r, parity.CheckRankedOrder); c.Compared != 80 {
		t.Errorf("ranked order compared %d names, want 80 (40 a month)", c.Compared)
	}
	if c := checkNamed(r, parity.CheckWeights); c.Compared != 51 {
		t.Errorf("weights compared %d, want 51 (25 + IEF, then 25)", c.Compared)
	}
	if c := checkNamed(r, parity.CheckHistory); c.Compared != 84 {
		t.Errorf("history compared %d sampled names, want 84 (42 a month)", c.Compared)
	}
}

// failsOnly asserts that exactly the named check fails.
func failsOnly(t *testing.T, r parity.Report, name string) {
	t.Helper()
	if r.Pass() {
		t.Fatalf("an export altered for %s must fail", name)
	}
	for _, c := range r.Checks {
		if c.Pass() == (c.Name == name) {
			t.Errorf("%s: pass=%v (failures %d, first %q)", c.Name, c.Pass(), c.Failures, c.First)
		}
	}
}

func TestAMissingPlannedMonthFails(t *testing.T) {
	r := compare(months()[:1])
	failsOnly(t, r, parity.CheckMonths)
	if c := checkNamed(r, parity.CheckMonths); !strings.Contains(c.First, "2009-06") {
		t.Errorf("first failure %q must name the missing month", c.First)
	}
}

func TestAnUnplannedMonthFails(t *testing.T) {
	failsOnly(t, compare(append(months(), syntheticMonth("2010-01", false))), parity.CheckMonths)
}

func TestTwoRankedNamesSwappedFailRankedOrder(t *testing.T) {
	ms := months()
	r := &ms[1].Ranked
	(*r)[30], (*r)[31] = (*r)[31], (*r)[30]
	failsOnly(t, compare(ms), parity.CheckRankedOrder)
}

func TestAChangedCapMarkFails(t *testing.T) {
	ms := months()
	for i := range ms[1].Ranked {
		if ms[1].Ranked[i].Mark == parity.MarkCapped {
			ms[1].Ranked[i].Mark = parity.MarkBeyond
			break
		}
	}
	failsOnly(t, compare(ms), parity.CheckCapMarks)
}

func TestAWeightOffByMoreThanTheToleranceFails(t *testing.T) {
	ms := months()
	ms[1].Weights[ms[1].Ranked[0].Ticker] += 2e-12
	failsOnly(t, compare(ms), parity.CheckWeights)
}

func TestAWeightWithinTheToleranceIsMeasuredNotFailed(t *testing.T) {
	ms := months()
	ms[1].Weights[ms[1].Ranked[0].Ticker] += 5e-13
	r := compare(ms)
	if !r.Pass() {
		t.Fatal("a weight 5e-13 off is within 1e-12")
	}
	if c := checkNamed(r, parity.CheckWeights); c.MaxDiff < 4e-13 || c.MaxDiff > 6e-13 {
		t.Errorf("max weight difference %g, want about 5e-13", c.MaxDiff)
	}
}

func TestAMissingOrExtraTargetFailsWeights(t *testing.T) {
	ms := months()
	delete(ms[0].Weights, "IEF")
	ms[0].Weights["XTRA"] = 0.01
	failsOnly(t, compare(ms), parity.CheckWeights)
}

func TestAScoreOffByMoreThanTheRelativeToleranceFails(t *testing.T) {
	ms := months()
	ms[1].Ranked[39].Score *= 1 + 1e-8 // the last name: its rank and mark stay
	failsOnly(t, compare(ms), parity.CheckScores)
}

func TestAScoreWithinTheRelativeTolerancePasses(t *testing.T) {
	ms := months()
	ms[1].Ranked[39].Score *= 1 + 1e-10
	if r := compare(ms); !r.Pass() {
		t.Errorf("a score 1e-10 off relative is within 1e-9: %+v", checkNamed(r, parity.CheckScores))
	}
}

func TestAVolatilityOffByMoreThanTheRelativeToleranceFails(t *testing.T) {
	ms := months()
	ms[1].Ranked[39].Volatility *= 1 + 1e-8
	failsOnly(t, compare(ms), parity.CheckVolatility)
}

func TestAFlippedRiskOffFlagFails(t *testing.T) {
	ms := months()
	ms[0].RiskOff = false
	markAndWeigh(&ms[0]) // the logged book consistent with the logged flag
	failsOnly(t, compare(ms), parity.CheckRiskOff)
}

func TestATrendMeanOffByMoreThanTheRelativeToleranceFails(t *testing.T) {
	ms := months()
	ms[1].SPYMean *= 1 + 1e-8
	failsOnly(t, compare(ms), parity.CheckRiskOff)
}

func TestAShortNameWithAnotherCloseCountFails(t *testing.T) {
	ms := months()
	ms[1].Short["S1"] = 101
	failsOnly(t, compare(ms), parity.CheckHistory)
}

func TestAMalformedExportFailsIntegrity(t *testing.T) {
	for name, alter := range map[string]func(*parity.MomentumMonth){
		"a close column of the wrong length": func(m *parity.MomentumMonth) { m.Closes["N100"] = m.Closes["N100"][1:] },
		"a pick with no weight": func(m *parity.MomentumMonth) {
			delete(m.Weights, m.Ranked[0].Ticker)
		},
		"a scored count that is not the ranked list's length": func(m *parity.MomentumMonth) { m.Scored++ },
		"a name ranked twice": func(m *parity.MomentumMonth) { m.Ranked[39].Ticker = m.Ranked[38].Ticker },
		"no trend closes":     func(m *parity.MomentumMonth) { delete(m.Closes, "SPY") },
		"no close index":      func(m *parity.MomentumMonth) { m.Index = nil },
		"a non-positive trend close": func(m *parity.MomentumMonth) {
			zero := 0.0
			m.Closes["SPY"][100] = &zero
		},
		"a non-positive close": func(m *parity.MomentumMonth) {
			zero := 0.0
			m.Closes["N139"][100] = &zero
		},
	} {
		t.Run(name, func(t *testing.T) {
			ms := months()
			alter(&ms[1])
			r := compare(ms)
			if r.Pass() || checkNamed(r, parity.CheckExport).Pass() {
				t.Errorf("%s must fail the export check", name)
			}
		})
	}
}

// The differences ADR 0026 point 8 lists are reported with their size and
// do not fail; any other difference fails.

func TestAWindowShiftedAcrossAGapIsAListedDifference(t *testing.T) {
	ms := months()
	m := &ms[1]
	// The backtest dropped the missing close and scored the last 253 it
	// had, from a 260-row frame (defensive_v2.py:286); live leaves it out.
	gapped := m.Ranked[30].Ticker
	col := m.Closes[gapped]
	col[10] = nil
	var px []float64
	for _, p := range col {
		if p != nil {
			px = append(px, *p)
		}
	}
	m.Ranked[30].Score, m.Ranked[30].Volatility = backtestScore(px), backtestVol(px)
	rebook(m)
	r := compare(ms)
	if !r.Pass() {
		t.Fatalf("a gap is on ADR 0026 point 8's list: %+v", r)
	}
	d := onlyDifference(t, r)
	if d.Kind != parity.KindGap || !d.Listed() || !strings.Contains(d.Where, gapped) || d.Size == "" {
		t.Errorf("difference %+v", d)
	}
}

func TestTiedScoresRankedInAnotherOrderAreAListedDifference(t *testing.T) {
	ms := months()
	m := &ms[1]
	// Two names beyond the book with one score: the backtest kept its
	// universe's order (defensive_v2.py:323); Go breaks the tie by ID.
	a, b := m.Ranked[38], m.Ranked[39]
	b.Score = a.Score
	if a.Ticker < b.Ticker {
		a, b = b, a
	}
	m.Ranked[38], m.Ranked[39] = a, b
	// Their logged scores no longer come from their closes.
	delete(m.Closes, m.Ranked[38].Ticker)
	delete(m.Closes, m.Ranked[39].Ticker)
	r := compare(ms)
	if !r.Pass() {
		t.Fatalf("a tie is on the list: %+v", r.Checks)
	}
	for _, d := range r.Differences {
		if d.Kind != parity.KindTie || !d.Listed() {
			t.Errorf("difference %+v", d)
		}
	}
	if len(r.Differences) == 0 {
		t.Error("the reordered tie must be reported")
	}
}

func TestAZeroVolatilityPickIsAListedDifference(t *testing.T) {
	ms := months()
	m := &ms[1]
	// defensive_book weighted a zero volatility as 1 (defensive_v2.py:76);
	// live leaves the name out, which changes the book.
	m.Ranked[0].Volatility = 0
	delete(m.Closes, m.Ranked[0].Ticker)
	markAndWeigh(m)
	r := compare(ms)
	if !r.Pass() {
		t.Fatalf("a zero volatility is on the list: %+v", r.Checks)
	}
	for _, d := range r.Differences {
		if d.Kind != parity.KindZeroVolatility {
			t.Errorf("difference %+v", d)
		}
	}
	if len(r.Differences) == 0 {
		t.Error("the zero volatility must be reported")
	}
}

func TestTooFewTrendClosesIsAListedDifference(t *testing.T) {
	ms := months()
	m := &ms[1]
	col := m.Closes["SPY"]
	for i := 0; i < 100; i++ {
		col[i] = nil
	}
	m.SPYCloses = 160 // the backtest treated fewer than 200 as risk-on
	m.SPYMean = 0
	r := compare(ms)
	if !r.Pass() {
		t.Fatalf("fewer than 200 trend closes is on the list: %+v", r.Checks)
	}
	if d := onlyDifference(t, r); d.Kind != parity.KindTrendHistory || !d.Listed() {
		t.Errorf("difference %+v", d)
	}
}

func TestANameWithNoSectorIsAnUnlistedDifferenceAndFails(t *testing.T) {
	ms := months()
	ms[1].Ranked[0].Sector = "" // defensive_book left it uncapped; live leaves it out
	markAndWeigh(&ms[1])
	r := compare(ms)
	failsOnly(t, r, parity.CheckDifferences)
	found := false
	for _, d := range r.Differences {
		found = found || d.Kind == parity.KindNoSector
		if d.Listed() {
			t.Errorf("no-sector is not on ADR 0026 point 8's list: %+v", d)
		}
	}
	if !found {
		t.Error("the no-sector difference must be reported")
	}
}

func onlyDifference(t *testing.T, r parity.Report) parity.Difference {
	t.Helper()
	if len(r.Differences) != 1 {
		t.Fatalf("want one difference, got %+v", r.Differences)
	}
	return r.Differences[0]
}

func TestAMissingTrendCloseInTheWindowIsAListedDifference(t *testing.T) {
	ms := months()
	ms[1].Closes["SPY"][250] = nil // the backtest dropped it (defensive_v2.py:278)
	r := compare(ms)
	if !r.Pass() {
		t.Fatalf("a gap is on the list: %+v", r.Checks)
	}
	if d := onlyDifference(t, r); d.Kind != parity.KindGap || !strings.Contains(d.Where, "SPY") {
		t.Errorf("difference %+v", d)
	}
}

func TestASampledNameQuantConnectNeitherScoredNorLeftOutFails(t *testing.T) {
	ms := months()
	ms[1].Closes["XTRA"] = ms[1].Closes[ms[1].Ranked[0].Ticker]
	failsOnly(t, compare(ms), parity.CheckHistory)
}

func TestAConstantNameScoresZeroOnBothSides(t *testing.T) {
	ms := months()
	m := &ms[1]
	last := &m.Ranked[len(m.Ranked)-1]
	m.Closes[last.Ticker] = ptrs(make([]float64, len(m.Index)))
	for _, p := range m.Closes[last.Ticker] {
		*p = 50
	}
	last.Score, last.Volatility = 0, 0
	rebook(m)
	r := compare(ms)
	if c := checkNamed(r, parity.CheckScores); !c.Pass() {
		t.Errorf("a zero score on both sides matches: %+v", c)
	}
	if !r.Pass() {
		t.Errorf("a zero volatility is on the list: %+v", r.Checks)
	}
}

func TestANoSectorNameOutsideTheBookLeavesTheWeightsCompared(t *testing.T) {
	ms := months()
	m := &ms[1]
	m.Ranked[39].Sector = "" // beyond the book on both sides
	markAndWeigh(m)
	r := compare(ms)
	if c := checkNamed(r, parity.CheckWeights); c.Compared != 51 || !c.Pass() {
		t.Errorf("the book is the same on both sides, so every weight is compared: %+v", c)
	}
	if d := onlyDifference(t, r); d.Kind != parity.KindNoSector {
		t.Errorf("difference %+v", d)
	}
}
