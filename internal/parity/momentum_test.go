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
		"a name ranked twice":                          func(m *parity.MomentumMonth) { m.Ranked[39].Ticker = m.Ranked[38].Ticker },
		"no trend closes":                              func(m *parity.MomentumMonth) { delete(m.Closes, "SPY") },
		"no close index":                               func(m *parity.MomentumMonth) { m.Index = nil },
		"an index row repeated":                        func(m *parity.MomentumMonth) { m.Index[5] = m.Index[4] },
		"an index out of order":                        func(m *parity.MomentumMonth) { m.Index[4], m.Index[5] = m.Index[5], m.Index[4] },
		"a member count that is not ranked plus short": func(m *parity.MomentumMonth) { m.Members++ },
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
	// Its score, and the book that leaves it out.
	classified(t, r, parity.KindGap, gapped, 2)
}

func TestTiedScoresRankedInAnotherOrderAreAListedDifference(t *testing.T) {
	ms := months()
	m := &ms[1]
	// Two names beyond the book with one score: the backtest kept its
	// universe's order (defensive_v2.py:323); Go breaks the tie by ID.
	a, b := m.Ranked[38], m.Ranked[39]
	tieNames(m, &a, &b)
	if a.Ticker < b.Ticker {
		a, b = b, a
	}
	m.Ranked[38], m.Ranked[39] = a, b
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
	zeroVolatility(m, &m.Ranked[0])
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
	// The backtest treated fewer than 200 as risk-on; QuantConnect logged
	// the mean of the closes it had.
	sum := 0.0
	for _, p := range col[100:] {
		sum += *p
	}
	m.SPYCloses, m.SPYMean = 160, sum/160
	r := compare(ms)
	if !r.Pass() {
		t.Fatalf("fewer than 200 trend closes is on the list: %+v", r.Checks)
	}
	if d := onlyDifference(t, r); d.Kind != parity.KindTrendHistory || !d.Listed() {
		t.Errorf("difference %+v", d)
	}
}

// TestANameWithNoSectorIsNoDifference: defensive_book left a name with no
// sector uncapped, and so does the live rule (ADR 0026 point 8, the owner's
// decision of 2026-10-18), so the comparison passes with no departure.
func TestANameWithNoSectorIsNoDifference(t *testing.T) {
	ms := months()
	ms[1].Ranked[0].Sector = ""
	markAndWeigh(&ms[1])
	r := compare(ms)
	if !r.Pass() {
		t.Errorf("a name with no sector is handled as the backtest handled it: %+v", r.Checks)
	}
	if len(r.Differences) != 0 {
		t.Errorf("differences %+v, want none", r.Differences)
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
	m := &ms[1]
	col := m.Closes["SPY"]
	col[250] = nil // the backtest dropped it (defensive_v2.py:278)
	// QuantConnect logged the mean of the last 200 closes it had.
	var had []float64
	for _, p := range col {
		if p != nil {
			had = append(had, *p)
		}
	}
	sum := 0.0
	for _, p := range had[len(had)-200:] {
		sum += p
	}
	m.SPYCloses, m.SPYMean = len(had), sum/200
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
	if len(r.Differences) != 0 {
		t.Errorf("differences %+v, want none", r.Differences)
	}
}

// tieBeyondTheBook gives the last two names of the second month one score,
// in the order Go would not choose: a listed difference beyond the book.
func tieBeyondTheBook(ms []parity.MomentumMonth) {
	m := &ms[1]
	a, b := m.Ranked[38], m.Ranked[39]
	tieNames(m, &a, &b)
	if a.Ticker < b.Ticker {
		a, b = b, a
	}
	m.Ranked[38], m.Ranked[39] = a, b
}

// A listed cause is a live departure, not an excuse: every mark and
// weight is still compared with the backtest-compatible book.

func TestATieBeyondTheBookDoesNotExcuseAChangedCapMark(t *testing.T) {
	ms := months()
	tieBeyondTheBook(ms)
	for i := range ms[1].Ranked {
		if ms[1].Ranked[i].Mark == parity.MarkCapped {
			ms[1].Ranked[i].Mark = parity.MarkBeyond
			break
		}
	}
	failsOnly(t, compare(ms), parity.CheckCapMarks)
}

func TestATieBeyondTheBookDoesNotExcuseAWeightOffByMoreThanTheTolerance(t *testing.T) {
	ms := months()
	tieBeyondTheBook(ms)
	ms[1].Weights[ms[1].Ranked[0].Ticker] += 2e-12
	failsOnly(t, compare(ms), parity.CheckWeights)
}

func TestANoSectorNameBeyondTheBookDoesNotExcuseAWeightOffByMoreThanTheTolerance(t *testing.T) {
	ms := months()
	m := &ms[1]
	m.Ranked[39].Sector = ""
	markAndWeigh(m)
	m.Weights[m.Ranked[0].Ticker] += 2e-12
	r := compare(ms)
	if c := checkNamed(r, parity.CheckWeights); c.Pass() {
		t.Errorf("the picked set is the same on both sides, so the weight must be compared and fail: %+v", c)
	}
}

func TestANoSectorNameBeyondTheBookDoesNotExcuseALaterChangedMark(t *testing.T) {
	ms := months()
	m := &ms[1]
	m.Ranked[37].Sector = "" // beyond the book on both sides
	markAndWeigh(m)
	if m.Ranked[37].Mark != parity.MarkBeyond || m.Ranked[39].Mark != parity.MarkBeyond {
		t.Fatal("fixture: ranks 38 and 40 must be beyond the book")
	}
	m.Ranked[39].Mark = parity.MarkCapped // Go has it beyond the book
	r := compare(ms)
	if c := checkNamed(r, parity.CheckCapMarks); c.Pass() {
		t.Errorf("a mark after a cause outside the book must be compared and fail: %+v", c)
	}
}

func TestEveryNameAndTargetIsCompared(t *testing.T) {
	ms := months()
	tieBeyondTheBook(ms)
	r := compare(ms)
	if c := checkNamed(r, parity.CheckRankedOrder); c.Compared != 80 || !c.Pass() {
		t.Errorf("the tied names are compared in the backtest's order with the other 78: %+v", c)
	}
	if c := checkNamed(r, parity.CheckWeights); c.Compared != 51 || !c.Pass() {
		t.Errorf("every target is compared: %+v", c)
	}
}

func TestTooFewTrendClosesThatQuantConnectDidNotCountFails(t *testing.T) {
	ms := months()
	m := &ms[1]
	for i := 0; i < 100; i++ {
		m.Closes["SPY"][i] = nil
	}
	// QuantConnect counted 260 closes, so it was not short of history.
	failsOnly(t, compare(ms), parity.CheckRiskOff)
}

// A sampled name whose close column is missing cannot be recomputed, so its
// score and volatility would go unchecked; the history check fails naming
// it, and the month's count shows the shortfall.

func TestAPickedNameWithNoCloseColumnFailsHistory(t *testing.T) {
	ms := months()
	var pick string
	for _, e := range ms[1].Ranked {
		if e.Mark == parity.MarkPicked {
			pick = e.Ticker
			break
		}
	}
	delete(ms[1].Closes, pick)
	r := compare(ms)
	failsOnly(t, r, parity.CheckHistory)
	if c := checkNamed(r, parity.CheckHistory); !strings.Contains(c.First, pick) || !strings.Contains(c.First, "2009-06") {
		t.Errorf("first failure %q must name the month and the missing column %s", c.First, pick)
	}
	counts := map[string]int{}
	for _, m := range r.Months {
		counts[m.Month] = m.Counts[parity.CheckHistory]
	}
	if counts["2008-11"] != 42 || counts["2009-06"] != 41 {
		t.Errorf("history compared per month %v, want 42 and 41", counts)
	}
}

func TestEverySampleTheExportPromisesMustHaveAColumn(t *testing.T) {
	// The logging template samples up to 5 each of the capped names, names
	// beyond the book and names too short to score.
	for name, mark := range map[string]parity.Mark{"capped": parity.MarkCapped, "beyond the book": parity.MarkBeyond} {
		t.Run(name, func(t *testing.T) {
			ms := months()
			var of []string
			for _, e := range ms[0].Ranked {
				if e.Mark == mark {
					of = append(of, e.Ticker)
				}
			}
			if len(of) < 6 {
				t.Fatalf("fixture: %d %s names, want more than the sample of 5", len(of), name)
			}
			for _, tk := range of[1:] {
				delete(ms[0].Closes, tk) // one column left: fewer than 5 sampled
			}
			failsOnly(t, compare(ms), parity.CheckHistory)
		})
	}
	t.Run("too short", func(t *testing.T) {
		ms := months()
		delete(ms[0].Closes, "S1")
		delete(ms[0].Closes, "S2")
		failsOnly(t, compare(ms), parity.CheckHistory)
	})
}

// The sample is the names the template chose, found from the export itself:
// the first 5 capped and the first 5 beyond the book in ranked order, and
// the first 5 too-short names in the order the export lists them. Any other
// name's column does not stand in for one of them.

func TestTheSampledNamesThemselvesMustHaveColumns(t *testing.T) {
	for name, mark := range map[string]parity.Mark{"capped": parity.MarkCapped, "beyond the book": parity.MarkBeyond} {
		t.Run(name, func(t *testing.T) {
			ms := months()
			var of []string
			for _, e := range ms[0].Ranked {
				if e.Mark == mark {
					of = append(of, e.Ticker)
				}
			}
			if len(of) < 6 {
				t.Fatalf("fixture: %d %s names, want more than the sample of 5", len(of), name)
			}
			// The first sampled name loses its column while a name outside
			// the sample keeps one: five columns remain, one is the wrong one.
			delete(ms[0].Closes, of[0])
			r := compare(ms)
			failsOnly(t, r, parity.CheckHistory)
			if c := checkNamed(r, parity.CheckHistory); !strings.Contains(c.First, of[0]) {
				t.Errorf("first failure %q must name the sampled name %s", c.First, of[0])
			}
		})
	}
	t.Run("a name outside the sample may have no column", func(t *testing.T) {
		ms := months()
		var capped []string
		for _, e := range ms[0].Ranked {
			if e.Mark == parity.MarkCapped {
				capped = append(capped, e.Ticker)
			}
		}
		delete(ms[0].Closes, capped[len(capped)-1])
		if r := compare(ms); !checkNamed(r, parity.CheckHistory).Pass() {
			t.Errorf("a sixth capped name is not sampled: %+v", checkNamed(r, parity.CheckHistory))
		}
	})
	t.Run("an order that does not list the short names fails the check", func(t *testing.T) {
		ms := months()
		ms[0].ShortOrder = ms[0].ShortOrder[:1]
		r := compare(ms)
		if c := checkNamed(r, parity.CheckHistory); c.Pass() || !strings.Contains(c.First, "short") {
			t.Errorf("history %+v must fail on the short names", c)
		}
	})
	t.Run("too short, in the export's order", func(t *testing.T) {
		ms := months()
		ms[0].Short["S3"] = 50
		ms[0].Members += 4
		ms[0].ShortOrder = []string{"S3", "S1", "S2"}
		for _, n := range []string{"S4", "S5", "S6"} {
			ms[0].Short[n] = 50
			ms[0].ShortOrder = append(ms[0].ShortOrder, n)
		}
		// S3 is listed first, so it is sampled and has no column; S6 is the
		// sixth listed and is not.
		for _, n := range []string{"S4", "S5", "S6"} {
			ms[0].Closes[n] = ms[0].Closes["S1"]
		}
		r := compare(ms)
		if c := checkNamed(r, parity.CheckHistory); c.Pass() || !strings.Contains(c.First, "S3") {
			t.Errorf("history %+v must fail naming the sampled S3", c)
		}
	})
}

func TestTheReportShowsEveryMonthsComparisonCounts(t *testing.T) {
	r := compare(months())
	if len(r.Months) != 2 {
		t.Fatalf("%d months counted, want 2", len(r.Months))
	}
	for _, m := range r.Months {
		if m.Run != "SYN" || m.Counts[parity.CheckRankedOrder] != 40 || m.Counts[parity.CheckHistory] != 42 {
			t.Errorf("month counts %+v", m)
		}
	}
	md := r.Markdown()
	for _, want := range []string{"| Run | Month | risk-off | ranked-order | cap-marks | weights | scores | volatility | history |",
		"| SYN | 2008-11 | 1 | 40 | 40 | 26 | 40 | 40 | 42 |", "| SYN | 2009-06 | 1 | 40 | 40 | 25 | 40 | 40 | 42 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
}

func TestAMonthThatComparedNothingShowsZeroCounts(t *testing.T) {
	ms := months()
	ms[1].Index = nil
	r := compare(ms)
	for _, m := range r.Months {
		if m.Month == "2009-06" && (m.Counts[parity.CheckHistory] != 0 || m.Counts[parity.CheckRankedOrder] != 0) {
			t.Errorf("a malformed month compared %+v", m.Counts)
		}
	}
	if md := r.Markdown(); !strings.Contains(md, "| SYN | 2009-06 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |") {
		t.Errorf("a skipped month must show its zero counts:\n%s", md)
	}
}
