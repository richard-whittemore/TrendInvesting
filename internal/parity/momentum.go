package parity

import (
	"fmt"
	"math"
	"sort"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// Mark is what the backtest's book did with a ranked name (the golden
// logs' marks, research/edge-search/momentum/golden_log.py).
type Mark string

// The closed set of Mark values.
const (
	MarkPicked Mark = "P"
	MarkCapped Mark = "C"
	MarkBeyond Mark = "-"
)

// RankedEntry is one scored universe member as QuantConnect ranked it.
// An empty Sector means it had none.
type RankedEntry struct {
	Ticker     string
	Score      float64
	Volatility float64
	Sector     string
	Mark       Mark
}

// MomentumMonth is one logged rebalance of a QuantConnect momentum run: the
// risk-off inputs, every scored member in QuantConnect's order with its
// mark, the target weights, the members too short to score with their
// close counts, and the adjusted closes of a sample of names and of SPY,
// aligned to Index (nil where a name has no close on that row).
type MomentumMonth struct {
	Month     string
	RiskOff   bool
	SPYClose  float64
	SPYMean   float64
	SPYCloses int
	Members   int
	Scored    int
	Index     []string
	Ranked    []RankedEntry
	Weights   map[string]float64
	Short     map[string]int
	Closes    map[string][]*float64
}

type momentumRun struct {
	cfg    momentum.Config
	run    string
	checks map[string]*Check
	diffs  []Difference
}

func (r *momentumRun) diff(where string, k Kind, size string) {
	r.diffs = append(r.diffs, Difference{Run: r.run, Where: where, Kind: k, Size: size})
}

// CompareMomentum compares each logged month with the Go momentum module
// fed the same inputs, and checks that the export holds exactly the
// planned months, which were chosen before any comparison
// (docs/research/parity-2026-10.md, "Months").
//
// The ranked order, cap marks and weights come from momentum.SelectBook
// given QuantConnect's own scores, volatilities, sectors and risk-off
// flag; the scores, volatilities and close counts of the sampled names
// from momentum.Score, and the risk-off inputs from momentum.EvaluateTrend,
// given QuantConnect's adjusted closes.
func CompareMomentum(cfg momentum.Config, run string, planned []string, months []MomentumMonth) Report {
	r := &momentumRun{cfg: cfg, run: run, checks: map[string]*Check{}}
	add := func(name, threshold string) *Check {
		c := &Check{Run: run, Name: name, Threshold: threshold}
		r.checks[name] = c
		return c
	}
	order := []*Check{
		add(CheckMonths, "every planned month exported, no other"),
		add(CheckExport, "well-formed"),
		add(CheckRiskOff, fmt.Sprintf("flag and close exact; mean rel <= %g", RelativeTolerance)),
		add(CheckRankedOrder, "exact"),
		add(CheckCapMarks, "exact"),
		add(CheckWeights, fmt.Sprintf("abs <= %g", WeightTolerance)),
		add(CheckScores, fmt.Sprintf("rel <= %g", RelativeTolerance)),
		add(CheckVolatility, fmt.Sprintf("rel <= %g", RelativeTolerance)),
		add(CheckHistory, "exact"),
	}
	r.monthsCheck(planned, months)
	sorted := append([]MomentumMonth(nil), months...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Month < sorted[j].Month })
	for i := range sorted {
		r.month(&sorted[i])
	}
	out := Report{Differences: r.diffs}
	for _, c := range order {
		out.Checks = append(out.Checks, *c)
	}
	out.Checks = append(out.Checks, differencesCheck(run, r.diffs))
	return out
}

func (r *momentumRun) monthsCheck(planned []string, months []MomentumMonth) {
	c := r.checks[CheckMonths]
	got := map[string]int{}
	for _, m := range months {
		got[m.Month]++
	}
	want := map[string]bool{}
	for _, p := range planned {
		want[p] = true
		c.compare(got[p] == 1, 0, fmt.Sprintf("planned month %s exported %d times", p, got[p]))
	}
	extra := make([]string, 0, len(got))
	for m := range got {
		if !want[m] {
			extra = append(extra, m)
		}
	}
	sort.Strings(extra)
	for _, m := range extra {
		c.compare(false, 0, fmt.Sprintf("month %s exported but not planned", m))
	}
}

// month compares one month. A malformed export fails the export check and
// the month is compared no further.
func (r *momentumRun) month(m *MomentumMonth) {
	export := r.checks[CheckExport]
	if err := r.wellFormed(m); err != nil {
		export.compare(false, 0, fmt.Sprintf("%s: %v", m.Month, err))
		return
	}
	cands := make([]momentum.Candidate, len(m.Ranked))
	for i, e := range m.Ranked {
		cands[i] = momentum.Candidate{InstrumentID: e.Ticker, Sector: e.Sector, Score: e.Score, Volatility: e.Volatility}
	}
	book, err := momentum.SelectBook(r.cfg, cands, m.RiskOff)
	if err == nil {
		err = r.histories(m)
	}
	export.compare(err == nil, 0, fmt.Sprintf("%s: %v", m.Month, err))
	if err != nil {
		return
	}
	r.trend(m)
	r.book(m, book)
}

func (r *momentumRun) wellFormed(m *MomentumMonth) error {
	if len(m.Index) == 0 {
		return fmt.Errorf("no close index")
	}
	if _, ok := m.Closes[r.cfg.TrendInstrument]; !ok {
		return fmt.Errorf("no %s closes", r.cfg.TrendInstrument)
	}
	for name, col := range m.Closes {
		if len(col) != len(m.Index) {
			return fmt.Errorf("%s has %d closes for %d index rows", name, len(col), len(m.Index))
		}
	}
	if m.Scored != len(m.Ranked) {
		return fmt.Errorf("scored %d, ranked %d", m.Scored, len(m.Ranked))
	}
	seen := map[string]bool{}
	for _, e := range m.Ranked {
		if seen[e.Ticker] {
			return fmt.Errorf("%s ranked twice", e.Ticker)
		}
		seen[e.Ticker] = true
		_, weighted := m.Weights[e.Ticker]
		if (e.Mark == MarkPicked) != weighted {
			return fmt.Errorf("%s marked %q but weighted=%v", e.Ticker, e.Mark, weighted)
		}
	}
	return nil
}

func (m *MomentumMonth) closes(name string) map[string]float64 {
	out := map[string]float64{}
	for i, p := range m.Closes[name] {
		if p != nil {
			out[m.Index[i]] = *p
		}
	}
	return out
}

// trend compares the risk-off inputs. Fewer than TrendCloses closes, which
// the backtest treated as risk-on, and a missing close in the window, which
// it dropped, are differences ADR 0026 point 8 lists.
func (r *momentumRun) trend(m *MomentumMonth) {
	c := r.checks[CheckRiskOff]
	tr, reason, err := momentum.EvaluateTrend(r.cfg, m.Index, m.closes(r.cfg.TrendInstrument))
	switch {
	case err != nil:
		r.checks[CheckExport].fail(fmt.Sprintf("%s: %s: %v", m.Month, r.cfg.TrendInstrument, err))
	case reason == momentum.ReasonInsufficientTrendHistory:
		r.diff(m.Month+" "+r.cfg.TrendInstrument, KindTrendHistory,
			fmt.Sprintf("%d closes; QuantConnect counted %d and decided risk_off=%v, Go withholds", tr.Closes, m.SPYCloses, m.RiskOff))
	case reason == momentum.ReasonMissingBars:
		r.diff(m.Month+" "+r.cfg.TrendInstrument, KindGap,
			fmt.Sprintf("a close missing from the %d-close window; QuantConnect decided risk_off=%v, Go withholds", r.cfg.TrendCloses, m.RiskOff))
	default:
		rel := relative(tr.Mean, m.SPYMean)
		c.compare(tr.RiskOff == m.RiskOff && tr.Close == m.SPYClose && rel <= RelativeTolerance, rel,
			fmt.Sprintf("%s: Go risk_off=%v close %v mean %v, QuantConnect risk_off=%v close %v mean %v",
				m.Month, tr.RiskOff, tr.Close, tr.Mean, m.RiskOff, m.SPYClose, m.SPYMean))
	}
}

// histories recomputes each sampled name's score and volatility, or why it
// has none, from its closes. It returns an error for closes Score refuses.
func (r *momentumRun) histories(m *MomentumMonth) error {
	ranked := map[string]RankedEntry{}
	for _, e := range m.Ranked {
		ranked[e.Ticker] = e
	}
	names := make([]string, 0, len(m.Closes))
	for name := range m.Closes {
		if name != r.cfg.TrendInstrument && name != r.cfg.RiskOffInstrument {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	type result struct {
		name string
		h    momentum.History
	}
	results := make([]result, 0, len(names))
	for _, name := range names {
		h, err := momentum.Score(r.cfg, m.Index, m.closes(name))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		results = append(results, result{name, h})
	}
	hist, scores, vols := r.checks[CheckHistory], r.checks[CheckScores], r.checks[CheckVolatility]
	for _, x := range results {
		where := m.Month + " " + x.name
		e, scored := ranked[x.name]
		n, short := m.Short[x.name]
		switch {
		case scored && x.h.Status == momentum.StatusGap:
			hist.compare(true, 0, "")
			r.diff(where, KindGap, fmt.Sprintf("QuantConnect scored %v, volatility %v across a missing close; Go leaves it out", e.Score, e.Volatility))
		case scored && (x.h.Status == momentum.StatusScored || x.h.Status == momentum.StatusZeroVolatility):
			hist.compare(true, 0, "")
			rs, rv := relative(x.h.Score, e.Score), relative(x.h.Volatility, e.Volatility)
			scores.compare(rs <= RelativeTolerance, rs, fmt.Sprintf("%s: Go %v, QuantConnect %v", where, x.h.Score, e.Score))
			vols.compare(rv <= RelativeTolerance, rv, fmt.Sprintf("%s: Go %v, QuantConnect %v", where, x.h.Volatility, e.Volatility))
		case short:
			hist.compare(x.h.Status == momentum.StatusShortHistory && x.h.Closes == n, 0,
				fmt.Sprintf("%s: QuantConnect left it out with %d closes; Go %s with %d", where, n, x.h.Status, x.h.Closes))
		default:
			hist.compare(false, 0, fmt.Sprintf("%s: Go %s with %d closes; QuantConnect scored=%v", where, x.h.Status, x.h.Closes, scored))
		}
	}
	return nil
}

// cause is a backtest behaviour that changes the book from some rank on:
// a tie reordered, a zero volatility or a missing sector.
type cause struct {
	kind  Kind
	where string
}

// book compares the ranked order, the cap marks and the weights. A
// mismatch from a name with no sector or a zero volatility, or from a tie
// reordered, onwards is that name's difference, not a check failure:
// every later mark and every weight follow from it.
func (r *momentumRun) book(m *MomentumMonth, book momentum.Book) {
	order, marks, weights := r.checks[CheckRankedOrder], r.checks[CheckCapMarks], r.checks[CheckWeights]
	var first *cause
	byTicker := map[string]RankedEntry{}
	for _, e := range m.Ranked {
		byTicker[e.Ticker] = e
	}
	for i, g := range book.Ranked {
		q := m.Ranked[i]
		where := fmt.Sprintf("%s rank %d", m.Month, i+1)
		switch {
		case g.InstrumentID == q.Ticker:
			order.compare(true, 0, "")
		case g.Score == q.Score:
			order.Compared++
			r.diff(where, KindTie, fmt.Sprintf("Go %s, QuantConnect %s, both scored %v", g.InstrumentID, q.Ticker, q.Score))
			if first == nil {
				first = &cause{KindTie, where}
			}
		default:
			order.compare(false, 0, fmt.Sprintf("%s: Go %s (%v), QuantConnect %s (%v)", where, g.InstrumentID, g.Score, q.Ticker, q.Score))
		}
	}
	for _, g := range book.Ranked {
		q := byTicker[g.InstrumentID]
		where := fmt.Sprintf("%s %s", m.Month, g.InstrumentID)
		mark, own := liveMark(g.Status)
		switch {
		case own != "":
			marks.Compared++
			r.diff(where, own, fmt.Sprintf("QuantConnect marked %q; Go leaves it out (%s)", q.Mark, g.Status))
			if first == nil {
				first = &cause{own, where}
			}
		case mark == q.Mark:
			marks.compare(true, 0, "")
		case first != nil:
			marks.Compared++
			r.diff(where, first.kind, fmt.Sprintf("Go %q, QuantConnect %q, following %s", mark, q.Mark, first.where))
		default:
			marks.compare(false, 0, fmt.Sprintf("%s: Go %q, QuantConnect %q", where, mark, q.Mark))
		}
	}
	r.weights(m, book, first, weights)
}

func (r *momentumRun) weights(m *MomentumMonth, book momentum.Book, first *cause, c *Check) {
	goW := map[string]float64{}
	for _, t := range book.Targets {
		goW[t.InstrumentID] = t.Weight
	}
	names := map[string]bool{}
	for n := range goW {
		names[n] = true
	}
	for n := range m.Weights {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	type entry struct {
		name   string
		g, q   float64
		inGo   bool
		inQC   bool
		within bool
	}
	entries := make([]entry, 0, len(sorted))
	mismatch, maxDiff := false, 0.0
	for _, n := range sorted {
		g, inGo := goW[n]
		q, inQC := m.Weights[n]
		d := math.Abs(g - q)
		e := entry{n, g, q, inGo, inQC, inGo && inQC && d <= WeightTolerance}
		mismatch = mismatch || !e.within
		maxDiff = math.Max(maxDiff, d)
		entries = append(entries, e)
	}
	// Only a book that differs is put down to the cause; a book the cause
	// left alone is compared weight by weight.
	if mismatch && first != nil {
		c.Compared += len(entries)
		r.diff(m.Month+" weights", first.kind, fmt.Sprintf("largest weight difference %.3g, following %s", maxDiff, first.where))
		return
	}
	for _, e := range entries {
		c.compare(e.within, math.Abs(e.g-e.q),
			fmt.Sprintf("%s %s: Go %v (set %v), QuantConnect %v (set %v)", m.Month, e.name, e.g, e.inGo, e.q, e.inQC))
	}
}

// liveMark maps a live status to the backtest's mark, or names the
// difference kind of a status the backtest had no equivalent for.
func liveMark(s momentum.Status) (Mark, Kind) {
	switch s {
	case momentum.StatusPicked:
		return MarkPicked, ""
	case momentum.StatusSectorCapped:
		return MarkCapped, ""
	case momentum.StatusZeroVolatility:
		return "", KindZeroVolatility
	case momentum.StatusNoSector:
		return "", KindNoSector
	}
	return MarkBeyond, ""
}
