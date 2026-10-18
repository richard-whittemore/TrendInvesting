package parity

import (
	"errors"
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
// It never puts a mismatch down to a cause. The ranked order, cap marks
// and weights come from momentum.BacktestBook, the scores, volatilities
// and close counts of the sampled names from momentum.BacktestScore and
// the risk-off inputs from momentum.BacktestTrend: the module's own
// recomputation under the backtest's handling of every case ADR 0026
// point 8 lists and of a name with no sector. Each is compared with
// QuantConnect's export exactly, within the spec's thresholds only, every
// month. The book is given QuantConnect's own scores, volatilities,
// sectors and risk-off flag, in QuantConnect's order.
//
// Apart from that, the live rule (momentum.SelectBook, Score and
// EvaluateTrend) is run on the same inputs and every departure from the
// backtest-compatible result is listed as a difference, classified by
// cause (departures); the differences check fails on any cause ADR 0026
// point 8 does not list.
func CompareMomentum(cfg momentum.Config, run string, planned []string, months []MomentumMonth) Report {
	r, order := newMomentumRun(cfg, run)
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

// newMomentumRun returns a comparison with every check but differences,
// and those checks in report order.
func newMomentumRun(cfg momentum.Config, run string) (*momentumRun, []*Check) {
	r := &momentumRun{cfg: cfg, run: run, checks: map[string]*Check{}}
	add := func(name, threshold string) *Check {
		c := &Check{Run: run, Name: name, Threshold: threshold}
		r.checks[name] = c
		return c
	}
	order := []*Check{
		add(CheckMonths, "every planned month exported, no other"),
		add(CheckExport, "well-formed"),
		add(CheckRiskOff, fmt.Sprintf("flag, close and close count exact; mean rel <= %g", RelativeTolerance)),
		add(CheckRankedOrder, "exact"),
		add(CheckCapMarks, "exact"),
		add(CheckWeights, fmt.Sprintf("abs <= %g", WeightTolerance)),
		add(CheckScores, fmt.Sprintf("rel <= %g", RelativeTolerance)),
		add(CheckVolatility, fmt.Sprintf("rel <= %g", RelativeTolerance)),
		add(CheckHistory, "exact"),
	}
	return r, order
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

// month compares one month. A malformed export fails the export check
// before anything is compared. One whose histories or books cannot be
// computed fails it after the history comparisons and the differences found
// there are recorded, and the trend, book and departures are not compared.
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
	gaps, err := r.histories(m)
	var b books
	if err == nil {
		b, err = r.books(m, cands, gaps)
	}
	export.compare(err == nil, 0, fmt.Sprintf("%s: %v", m.Month, err))
	if err != nil {
		return
	}
	r.trend(m)
	r.book(m, b.backtest)
	r.departures(m, b)
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
	for i := 1; i < len(m.Index); i++ {
		if m.Index[i] <= m.Index[i-1] {
			return fmt.Errorf("index row %s does not follow %s", m.Index[i], m.Index[i-1])
		}
	}
	if m.Scored != len(m.Ranked) {
		return fmt.Errorf("scored %d, ranked %d", m.Scored, len(m.Ranked))
	}
	if m.Members != len(m.Ranked)+len(m.Short) {
		return fmt.Errorf("%d members, %d ranked and %d too short", m.Members, len(m.Ranked), len(m.Short))
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

// missingIn reports whether any of the last n rows of col has no close.
func missingIn(col []*float64, n int) bool {
	for _, p := range col[max(0, len(col)-n):] {
		if p == nil {
			return true
		}
	}
	return false
}

// trend compares BacktestTrend's risk-off inputs with QuantConnect's, and
// lists EvaluateTrend's departure from them.
func (r *momentumRun) trend(m *MomentumMonth) {
	closes := m.closes(r.cfg.TrendInstrument)
	bt, berr := momentum.BacktestTrend(r.cfg, m.Index, closes)
	live, reason, lerr := momentum.EvaluateTrend(r.cfg, m.Index, closes)
	if err := errors.Join(berr, lerr); err != nil {
		r.checks[CheckExport].fail(fmt.Sprintf("%s: %s: %v", m.Month, r.cfg.TrendInstrument, err))
		return
	}
	rel := relative(bt.Mean, m.SPYMean)
	r.checks[CheckRiskOff].compare(
		bt.RiskOff == m.RiskOff && bt.Close == m.SPYClose && bt.Closes == m.SPYCloses && rel <= RelativeTolerance, rel,
		fmt.Sprintf("%s: Go risk_off=%v close %v mean %v from %d closes, QuantConnect risk_off=%v close %v mean %v from %d",
			m.Month, bt.RiskOff, bt.Close, bt.Mean, bt.Closes, m.RiskOff, m.SPYClose, m.SPYMean, m.SPYCloses))
	r.trendDeparture(m, bt, live, reason)
}

// trendDeparture lists the live risk-off test's departure from the
// backtest's: withholding with fewer than TrendCloses closes, or with a
// close missing from the window, each as Go's own data shows it. Any
// other departure is unexplained.
func (r *momentumRun) trendDeparture(m *MomentumMonth, bt, live momentum.Trend, reason momentum.WithheldReason) {
	where := m.Month + " " + r.cfg.TrendInstrument
	switch {
	case reason == "" && live == bt:
	case reason == momentum.ReasonInsufficientTrendHistory && bt.Closes < r.cfg.TrendCloses:
		r.diff(where, KindTrendHistory,
			fmt.Sprintf("%d closes; the backtest decided risk_off=%v, live withholds", bt.Closes, bt.RiskOff))
	case reason == momentum.ReasonMissingBars && missingIn(m.Closes[r.cfg.TrendInstrument], r.cfg.TrendCloses):
		r.diff(where, KindGap,
			fmt.Sprintf("a close missing from the %d-close window; the backtest decided risk_off=%v, live withholds", r.cfg.TrendCloses, bt.RiskOff))
	default:
		r.diff(where, KindUnexplained,
			fmt.Sprintf("live risk_off=%v close %v mean %v withheld %q; backtest-compatible risk_off=%v close %v mean %v",
				live.RiskOff, live.Close, live.Mean, reason, bt.RiskOff, bt.Close, bt.Mean))
	}
}

// histories recomputes each sampled name's score and volatility, or why it
// has none, from its closes, under the backtest's handling and under the
// live rule's. It returns the names the live rule leaves out for a gap,
// each with whether its closes show one, and an error for closes either
// refuses.
func (r *momentumRun) histories(m *MomentumMonth) (map[string]bool, error) {
	names := make([]string, 0, len(m.Closes))
	for name := range m.Closes {
		if name != r.cfg.TrendInstrument && name != r.cfg.RiskOffInstrument {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	type result struct {
		name     string
		bt, live momentum.History
	}
	results := make([]result, 0, len(names))
	for _, name := range names {
		closes := m.closes(name)
		bt, berr := momentum.BacktestScore(r.cfg, m.Index, closes)
		live, lerr := momentum.Score(r.cfg, m.Index, closes)
		if err := errors.Join(berr, lerr); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		results = append(results, result{name, bt, live})
	}
	ranked := map[string]RankedEntry{}
	for _, e := range m.Ranked {
		ranked[e.Ticker] = e
	}
	gaps := map[string]bool{}
	for _, x := range results {
		r.history(m, ranked, x.name, x.bt)
		if shown := r.scoreDeparture(m, x.name, x.bt, x.live); x.live.Status == momentum.StatusGap {
			gaps[x.name] = shown
		}
	}
	return gaps, nil
}

// history compares one sampled name's backtest-compatible history with
// what QuantConnect did with it.
func (r *momentumRun) history(m *MomentumMonth, ranked map[string]RankedEntry, name string, h momentum.History) {
	hist, scores, vols := r.checks[CheckHistory], r.checks[CheckScores], r.checks[CheckVolatility]
	where := m.Month + " " + name
	e, scored := ranked[name]
	n, short := m.Short[name]
	switch {
	case scored:
		ok := h.Status == momentum.StatusScored || h.Status == momentum.StatusZeroVolatility
		hist.compare(ok, 0, fmt.Sprintf("%s: QuantConnect scored it; Go %s with %d closes", where, h.Status, h.Closes))
		if ok {
			rs, rv := relative(h.Score, e.Score), relative(h.Volatility, e.Volatility)
			scores.compare(rs <= RelativeTolerance, rs, fmt.Sprintf("%s: Go %v, QuantConnect %v", where, h.Score, e.Score))
			vols.compare(rv <= RelativeTolerance, rv, fmt.Sprintf("%s: Go %v, QuantConnect %v", where, h.Volatility, e.Volatility))
		}
	case short:
		hist.compare(h.Status == momentum.StatusShortHistory && h.Closes == n, 0,
			fmt.Sprintf("%s: QuantConnect left it out with %d closes; Go %s with %d", where, n, h.Status, h.Closes))
	default:
		hist.compare(false, 0, fmt.Sprintf("%s: Go %s with %d closes; QuantConnect neither scored it nor left it out", where, h.Status, h.Closes))
	}
}

// scoreDeparture lists the live Score's departure from the backtest's: a
// name left out for a missing close in its window, when its closes show
// one. Any other departure is unexplained. It reports whether a gap was
// shown.
func (r *momentumRun) scoreDeparture(m *MomentumMonth, name string, bt, live momentum.History) bool {
	where := m.Month + " " + name
	switch {
	case live == bt:
		return false
	case live.Status == momentum.StatusGap && bt.Status != momentum.StatusShortHistory &&
		missingIn(m.Closes[name], r.cfg.ScoreLookback+1):
		r.diff(where, KindGap, fmt.Sprintf("the backtest scored %v, volatility %v across a missing close; live leaves it out", bt.Score, bt.Volatility))
		return true
	default:
		r.diff(where, KindUnexplained, fmt.Sprintf("live %s score %v volatility %v from %d closes; backtest-compatible %s score %v volatility %v from %d",
			live.Status, live.Score, live.Volatility, live.Closes, bt.Status, bt.Score, bt.Volatility, bt.Closes))
		return false
	}
}

// stage is the backtest-compatible book with one more live rule applied
// than the stage before it.
type stage struct {
	kind Kind
	book momentum.Book
}

// books holds a month's backtest-compatible book, the stages that apply
// the live rules to it one at a time, and the live book.
type books struct {
	backtest momentum.Book
	stages   []stage
	live     momentum.Book
}

// books computes them from QuantConnect's candidates, in its order, and
// the names the live rule leaves out for a gap (gaps: whether the closes
// show it). The stages apply, in turn: the live tie order (ascending ID),
// leaving out each name whose closes show a gap, each zero volatility and
// each name with no sector. The live book is SelectBook given every
// candidate but the gaps the closes show. A gap they do not show is kept
// in every book, so it is never explained away. Every book takes QuantConnect's
// risk-off flag.
func (r *momentumRun) books(m *MomentumMonth, cands []momentum.Candidate, gaps map[string]bool) (books, error) {
	var b books
	var errs [6]error
	b.backtest, errs[0] = momentum.BacktestBook(r.cfg, cands, m.RiskOff)
	live := append([]momentum.Candidate(nil), cands...)
	sort.SliceStable(live, func(i, j int) bool {
		if live[i].Score != live[j].Score {
			return live[i].Score > live[j].Score
		}
		return live[i].InstrumentID < live[j].InstrumentID
	})
	keep := live
	for i, s := range []struct {
		kind Kind
		out  func(momentum.Candidate) bool
	}{
		{KindTie, func(momentum.Candidate) bool { return false }},
		{KindGap, func(c momentum.Candidate) bool { return gaps[c.InstrumentID] }},
		{KindZeroVolatility, func(c momentum.Candidate) bool { return c.Volatility == 0 }},
		{KindNoSector, func(c momentum.Candidate) bool { return c.Sector == "" }},
	} {
		var next []momentum.Candidate
		for _, c := range keep {
			if !s.out(c) {
				next = append(next, c)
			}
		}
		keep = next
		st := stage{kind: s.kind}
		st.book, errs[i+1] = momentum.BacktestBook(r.cfg, keep, m.RiskOff)
		b.stages = append(b.stages, st)
	}
	var liveCands []momentum.Candidate
	for _, c := range cands {
		if !gaps[c.InstrumentID] {
			liveCands = append(liveCands, c)
		}
	}
	b.live, errs[5] = momentum.SelectBook(r.cfg, liveCands, m.RiskOff)
	return b, errors.Join(errs[:]...)
}

// book compares the backtest-compatible ranked order, cap marks and
// weights with QuantConnect's, every name and every target, exactly.
func (r *momentumRun) book(m *MomentumMonth, book momentum.Book) {
	order, marks := r.checks[CheckRankedOrder], r.checks[CheckCapMarks]
	if len(book.Ranked) != len(m.Ranked) {
		order.compare(false, 0, fmt.Sprintf("%s: Go ranked %d names, QuantConnect %d", m.Month, len(book.Ranked), len(m.Ranked)))
		return
	}
	byTicker := map[string]RankedEntry{}
	for _, e := range m.Ranked {
		byTicker[e.Ticker] = e
	}
	for i, g := range book.Ranked {
		q := m.Ranked[i]
		order.compare(g.InstrumentID == q.Ticker, 0,
			fmt.Sprintf("%s rank %d: Go %s (%v), QuantConnect %s (%v)", m.Month, i+1, g.InstrumentID, g.Score, q.Ticker, q.Score))
	}
	for _, g := range book.Ranked {
		where := fmt.Sprintf("%s %s", m.Month, g.InstrumentID)
		mark, err := backtestMark(g.Status)
		if err != nil {
			r.checks[CheckExport].fail(fmt.Sprintf("%s: %v", where, err))
			continue
		}
		q := byTicker[g.InstrumentID]
		marks.compare(mark == q.Mark, 0, fmt.Sprintf("%s: Go %q, QuantConnect %q", where, mark, q.Mark))
	}
	goW := targets(book)
	c := r.checks[CheckWeights]
	for _, n := range union(goW, m.Weights) {
		g, inGo := goW[n]
		q, inQC := m.Weights[n]
		d := math.Abs(g - q)
		c.compare(inGo && inQC && d <= WeightTolerance, d,
			fmt.Sprintf("%s %s: Go %v (set %v), QuantConnect %v (set %v)", m.Month, n, g, inGo, q, inQC))
	}
}

func targets(b momentum.Book) map[string]float64 {
	out := map[string]float64{}
	for _, t := range b.Targets {
		out[t.InstrumentID] = t.Weight
	}
	return out
}

// union returns the names of a and b, sorted.
func union(a, b map[string]float64) []string {
	names := map[string]bool{}
	for n := range a {
		names[n] = true
	}
	for n := range b {
		names[n] = true
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// departures lists, stage by stage, every change one live rule makes to
// the backtest-compatible book, classified by that rule; then every way
// the live book differs from the last stage, which no listed rule
// explains.
func (r *momentumRun) departures(m *MomentumMonth, b books) {
	prev := b.backtest
	for _, s := range b.stages {
		r.stageDepartures(m, prev, s.book, s.kind)
		prev = s.book
	}
	r.unexplained(m, prev, b.stages[1].book, b.live)
}

// stageDepartures lists what changes between two books: the order of the
// names both rank, a name left out, a status and the targets.
func (r *momentumRun) stageDepartures(m *MomentumMonth, prev, next momentum.Book, k Kind) {
	if len(prev.Ranked) == len(next.Ranked) {
		for i, p := range prev.Ranked {
			if n := next.Ranked[i]; n.InstrumentID != p.InstrumentID {
				r.diff(fmt.Sprintf("%s rank %d", m.Month, i+1), k,
					fmt.Sprintf("live order %s (%v), backtest order %s (%v)", n.InstrumentID, n.Score, p.InstrumentID, p.Score))
			}
		}
	}
	after := map[string]momentum.Status{}
	for _, n := range next.Ranked {
		after[n.InstrumentID] = n.Status
	}
	for _, p := range prev.Ranked {
		where := fmt.Sprintf("%s %s", m.Month, p.InstrumentID)
		switch s, ok := after[p.InstrumentID]; {
		case !ok:
			r.diff(where, k, fmt.Sprintf("backtest %s; live leaves it out", p.Status))
		case s != p.Status:
			r.diff(where, k, fmt.Sprintf("backtest %s, live %s", p.Status, s))
		}
	}
	pw, nw := targets(prev), targets(next)
	changed, largest := 0, 0.0
	for _, n := range union(pw, nw) {
		p, inP := pw[n]
		q, inN := nw[n]
		if inP != inN || p != q {
			changed++
			largest = math.Max(largest, math.Abs(p-q))
		}
	}
	if changed > 0 {
		r.diff(m.Month+" weights", k, fmt.Sprintf("%d targets differ, largest by %.3g", changed, largest))
	}
}

// unexplained lists every way the live book differs from the last stage:
// a name the live rule leaves out for no sector or a zero volatility its
// data does not show, a name ranked, given a status or weighted
// otherwise, a target, or a BacktestWouldPick that is not whether the
// backtest's handling of the live candidates in live order (withGaps)
// picks it.
func (r *momentumRun) unexplained(m *MomentumMonth, last, withGaps, live momentum.Book) {
	var rest []momentum.RankedName
	for _, n := range live.Ranked {
		ownRule := (n.Status == momentum.StatusNoSector && n.Sector == "") ||
			(n.Status == momentum.StatusZeroVolatility && n.Volatility == 0 && n.Sector != "")
		if !ownRule {
			rest = append(rest, n)
		}
	}
	for i := 0; i < max(len(rest), len(last.Ranked)); i++ {
		var g, w momentum.RankedName
		if i < len(rest) {
			g = rest[i]
		}
		if i < len(last.Ranked) {
			w = last.Ranked[i]
		}
		if g.InstrumentID != w.InstrumentID || g.Status != w.Status || g.Weight != w.Weight {
			r.diff(fmt.Sprintf("%s rank %d of the book", m.Month, i+1), KindUnexplained,
				fmt.Sprintf("live %q %s weight %v; with the listed rules applied %q %s weight %v",
					g.InstrumentID, g.Status, g.Weight, w.InstrumentID, w.Status, w.Weight))
		}
	}
	lw, sw := targets(live), targets(last)
	for _, n := range union(lw, sw) {
		g, inL := lw[n]
		w, inS := sw[n]
		if inL != inS || g != w {
			r.diff(fmt.Sprintf("%s target %s", m.Month, n), KindUnexplained,
				fmt.Sprintf("live %v (set %v); with the listed rules applied %v (set %v)", g, inL, w, inS))
		}
	}
	picks := map[string]bool{}
	for _, n := range withGaps.Ranked {
		picks[n.InstrumentID] = n.Status == momentum.StatusPicked
	}
	for _, n := range live.Ranked {
		if want, ok := picks[n.InstrumentID]; !ok || n.BacktestWouldPick != want {
			r.diff(fmt.Sprintf("%s %s", m.Month, n.InstrumentID), KindUnexplained,
				fmt.Sprintf("live BacktestWouldPick=%v; the backtest's handling picks it: %v", n.BacktestWouldPick, want))
		}
	}
}

// backtestMark maps a status BacktestBook gives to the backtest's mark.
// Any other status is an error: the comparison refuses what it does not
// understand.
func backtestMark(s momentum.Status) (Mark, error) {
	switch s {
	case momentum.StatusPicked:
		return MarkPicked, nil
	case momentum.StatusSectorCapped:
		return MarkCapped, nil
	case momentum.StatusBeyondTopN:
		return MarkBeyond, nil
	}
	return "", fmt.Errorf("status %q has no backtest mark", s)
}
