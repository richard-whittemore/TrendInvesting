package parity

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// These tests hand the comparison a Go result it could not get from
// momentum.SelectBook, Score or EvaluateTrend given a well-formed export:
// a Go module that misreports must fail the comparison, not pass it as a
// listed difference. Synthetic values only.

func internalRun() *momentumRun {
	r, _ := newMomentumRun(momentum.FCombo(), "INT")
	return r
}

// twoNames is a month whose book picks both of its two ranked names.
func twoNames() *MomentumMonth {
	return &MomentumMonth{
		Month: "2009-06",
		Ranked: []RankedEntry{
			{Ticker: "AAA", Score: 0.5, Volatility: 0.01, Sector: "101", Mark: MarkPicked},
			{Ticker: "BBB", Score: 0.4, Volatility: 0.02, Sector: "102", Mark: MarkPicked},
		},
		Weights: map[string]float64{"AAA": 0.5, "BBB": 0.48},
	}
}

func goBook(m *MomentumMonth, statuses ...momentum.Status) momentum.Book {
	var b momentum.Book
	for i, s := range statuses {
		e := m.Ranked[i]
		b.Ranked = append(b.Ranked, momentum.RankedName{
			Candidate: momentum.Candidate{InstrumentID: e.Ticker, Sector: e.Sector, Score: e.Score, Volatility: e.Volatility},
			Status:    s,
		})
	}
	for name, w := range m.Weights {
		b.Targets = append(b.Targets, momentum.Target{InstrumentID: name, Weight: w})
	}
	return b
}

func TestAGoRankingOfAnotherLengthFailsRankedOrder(t *testing.T) {
	for name, book := range map[string]func(*MomentumMonth) momentum.Book{
		"shorter": func(m *MomentumMonth) momentum.Book { return goBook(m, momentum.StatusPicked) },
		"longer": func(m *MomentumMonth) momentum.Book {
			b := goBook(m, momentum.StatusPicked, momentum.StatusPicked)
			b.Ranked = append(b.Ranked, momentum.RankedName{Candidate: momentum.Candidate{InstrumentID: "CCC"}})
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, m := internalRun(), twoNames()
			r.book(m, book(m))
			if c := r.checks[CheckRankedOrder]; c.Pass() || c.Failures != 1 {
				t.Errorf("a %s Go ranking must fail the ranked order: %+v", name, c)
			}
		})
	}
}

func TestAStatusTheComparisonDoesNotKnowFailsTheExport(t *testing.T) {
	r, m := internalRun(), twoNames()
	r.book(m, goBook(m, momentum.StatusPicked, momentum.StatusUnaffordable))
	if c := r.checks[CheckExport]; c.Failures != 1 {
		t.Errorf("an unknown status must fail the export check: %+v", c)
	}
}

func TestAGoZeroVolatilityQuantConnectDidNotHaveFailsCapMarks(t *testing.T) {
	r, m := internalRun(), twoNames()
	r.book(m, goBook(m, momentum.StatusPicked, momentum.StatusZeroVolatility))
	if c := r.checks[CheckCapMarks]; c.Failures != 1 || len(r.diffs) != 0 {
		t.Errorf("Go's zero volatility on a nonzero one must fail, not be listed: %+v %+v", c, r.diffs)
	}
}

func TestAGoNoSectorQuantConnectDidNotHaveFailsCapMarks(t *testing.T) {
	r, m := internalRun(), twoNames()
	r.book(m, goBook(m, momentum.StatusPicked, momentum.StatusNoSector))
	if c := r.checks[CheckCapMarks]; c.Failures != 1 || len(r.diffs) != 0 {
		t.Errorf("Go's no-sector on a labelled name must fail, not be a difference: %+v %+v", c, r.diffs)
	}
}

// trendMonth has SPY closes on every row of a 210-row index.
func trendMonth() *MomentumMonth {
	m := &MomentumMonth{Month: "2009-06", SPYCloses: 210, Closes: map[string][]*float64{}}
	for i := 0; i < 210; i++ {
		p := 100.0 + float64(i)
		m.Index = append(m.Index, "d"+string(rune('a'+i/26))+string(rune('a'+i%26)))
		m.Closes["SPY"] = append(m.Closes["SPY"], &p)
	}
	return m
}

func TestAGoMissingTrendCloseQuantConnectHadFailsRiskOff(t *testing.T) {
	r, m := internalRun(), trendMonth()
	r.trendResult(m, momentum.Trend{Closes: 210}, momentum.ReasonMissingBars)
	if c := r.checks[CheckRiskOff]; c.Failures != 1 || len(r.diffs) != 0 {
		t.Errorf("no close is missing from SPY's window, so the gap must fail: %+v %+v", c, r.diffs)
	}
}

func TestAGoGapQuantConnectDidNotHaveFailsHistory(t *testing.T) {
	r, m := internalRun(), trendMonth()
	m.Closes["AAA"] = m.Closes["SPY"]
	e := RankedEntry{Ticker: "AAA", Score: 0.1, Volatility: 0.01, Sector: "101", Mark: MarkBeyond}
	r.history(m, map[string]RankedEntry{"AAA": e}, "AAA", momentum.History{Status: momentum.StatusGap, Closes: 210})
	if c := r.checks[CheckHistory]; c.Failures != 1 || len(r.diffs) != 0 {
		t.Errorf("no close is missing from its window, so the gap must fail: %+v %+v", c, r.diffs)
	}
}
