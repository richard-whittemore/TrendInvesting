package parity

import (
	"fmt"
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

// The live rule's departures from the backtest-compatible book are
// classified by cause; a live result no listed rule produces is
// unexplained, is not on ADR 0026 point 8's list, and fails the
// differences check.

func onlyUnexplained(t *testing.T, r *momentumRun, what string) {
	t.Helper()
	if len(r.diffs) == 0 {
		t.Fatalf("%s must be listed", what)
	}
	for _, d := range r.diffs {
		if d.Kind != KindUnexplained || d.Listed() {
			t.Errorf("%s: difference %+v, want unexplained", what, d)
		}
	}
	if differencesCheck(r.run, r.diffs).Pass() {
		t.Errorf("%s must fail the differences check", what)
	}
}

// withPicks sets BacktestWouldPick on each picked name, as SelectBook does
// when no live rule changed the picks.
func withPicks(b momentum.Book) momentum.Book {
	for i := range b.Ranked {
		b.Ranked[i].BacktestWouldPick = b.Ranked[i].Status == momentum.StatusPicked
	}
	return b
}

func TestALiveBookTheListedRulesExplainIsNoDeparture(t *testing.T) {
	r, m := internalRun(), twoNames()
	book := withPicks(goBook(m, momentum.StatusPicked, momentum.StatusPicked))
	r.unexplained(m, book, book, book)
	if len(r.diffs) != 0 {
		t.Errorf("differences %+v", r.diffs)
	}
}

func TestALiveZeroVolatilityTheDataDoesNotShowIsUnexplained(t *testing.T) {
	for _, sector := range []string{"102", ""} {
		t.Run("sector "+sector, func(t *testing.T) {
			r, m := internalRun(), twoNames()
			m.Ranked[1].Sector = sector
			want := withPicks(goBook(m, momentum.StatusPicked, momentum.StatusPicked))
			live := withPicks(goBook(m, momentum.StatusPicked, momentum.StatusZeroVolatility))
			r.unexplained(m, want, want, live)
			onlyUnexplained(t, r, fmt.Sprintf("a name of sector %q with volatility left out as zero-volatility", sector))
		})
	}
}

// TestAnUnlabelledZeroVolatilityNameIsLeftOutByTheZeroVolatilityRule: with
// no sector rule of its own, a name with no sector and zero volatility is
// the zero-volatility rule's, whatever its label.
func TestAnUnlabelledZeroVolatilityNameIsLeftOutByTheZeroVolatilityRule(t *testing.T) {
	r, m := internalRun(), twoNames()
	m.Ranked[1].Sector, m.Ranked[1].Volatility = "", 0
	m.Weights = map[string]float64{"AAA": 0.98}
	last := withPicks(goBook(m, momentum.StatusPicked))
	live := goBook(m, momentum.StatusPicked, momentum.StatusZeroVolatility)
	// The backtest picks it, weighting its zero volatility as 1.
	live.Ranked[0].BacktestWouldPick, live.Ranked[1].BacktestWouldPick = true, true
	for i := range live.Ranked {
		live.Ranked[i].Weight = m.Weights[live.Ranked[i].InstrumentID]
	}
	for i := range last.Ranked {
		last.Ranked[i].Weight = m.Weights[last.Ranked[i].InstrumentID]
	}
	withGaps := withPicks(goBook(m, momentum.StatusPicked, momentum.StatusPicked))
	r.unexplained(m, last, withGaps, live)
	if len(r.diffs) != 0 {
		t.Errorf("differences %+v, want none", r.diffs)
	}
}

func TestALiveRankingStatusOrWeightNoListedRuleProducesIsUnexplained(t *testing.T) {
	for name, alter := range map[string]func(*momentum.Book){
		"a capped pick":  func(b *momentum.Book) { b.Ranked[1].Status = momentum.StatusSectorCapped },
		"another weight": func(b *momentum.Book) { b.Ranked[0].Weight = 0.1; b.Targets[0].Weight = 0.1 },
		"a missing name": func(b *momentum.Book) { b.Ranked = b.Ranked[:1] },
		"an extra target": func(b *momentum.Book) {
			b.Targets = append(b.Targets, momentum.Target{InstrumentID: "IEF", Weight: 0.01})
		},
		"a would-pick off": func(b *momentum.Book) { b.Ranked[0].BacktestWouldPick = false },
	} {
		t.Run(name, func(t *testing.T) {
			r, m := internalRun(), twoNames()
			want := withPicks(goBook(m, momentum.StatusPicked, momentum.StatusPicked))
			for i, n := range want.Ranked {
				want.Ranked[i].Weight = m.Weights[n.InstrumentID]
			}
			live := momentum.Book{
				Ranked:  append([]momentum.RankedName(nil), want.Ranked...),
				Targets: append([]momentum.Target(nil), want.Targets...),
			}
			alter(&live)
			r.unexplained(m, want, want, live)
			onlyUnexplained(t, r, name)
		})
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

func TestALiveWithheldTrendTheDataDoesNotExplainIsUnexplained(t *testing.T) {
	for name, reason := range map[string]momentum.WithheldReason{
		"a missing close": momentum.ReasonMissingBars,
		"short history":   momentum.ReasonInsufficientTrendHistory,
		"another result":  "",
	} {
		t.Run(name, func(t *testing.T) {
			r, m := internalRun(), trendMonth()
			bt := momentum.Trend{Closes: 210, Close: 309, Mean: 250}
			live := bt
			if reason == "" {
				live.RiskOff = true
			}
			r.trendDeparture(m, bt, live, reason)
			onlyUnexplained(t, r, name)
		})
	}
}

func TestALiveGapTheClosesDoNotShowIsUnexplained(t *testing.T) {
	r, m := internalRun(), trendMonth()
	m.Closes["AAA"] = m.Closes["SPY"]
	bt := momentum.History{Status: momentum.StatusScored, Closes: 210, Score: 0.1, Volatility: 0.01}
	if r.scoreDeparture(m, "AAA", bt, momentum.History{Status: momentum.StatusGap, Closes: 210}) {
		t.Error("no close is missing, so no gap is shown")
	}
	onlyUnexplained(t, r, "a gap with every close present")
}

func TestAGapTheClosesDoNotShowIsKeptInEveryBook(t *testing.T) {
	r, m := internalRun(), twoNames()
	cands := []momentum.Candidate{
		{InstrumentID: "AAA", Sector: "101", Score: 0.5, Volatility: 0.01},
		{InstrumentID: "BBB", Sector: "102", Score: 0.4, Volatility: 0.02},
	}
	b, err := r.books(m, cands, map[string]bool{"AAA": false})
	if err != nil {
		t.Fatal(err)
	}
	names := func(k momentum.Book) int { return len(k.Ranked) }
	if got, want := names(b.live), names(b.stages[1].book); got != 2 || want != 2 {
		t.Errorf("a gap not shown by the closes must stay in the gap stage (%d) and the live book (%d)", want, got)
	}
}
