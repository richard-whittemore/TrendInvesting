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
		add(CheckRiskOff, fmt.Sprintf("flag and close exact; mean rel <= %g", RelativeTolerance)),
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

// trend compares the risk-off inputs. Fewer than TrendCloses closes, which
// the backtest treated as risk-on, and a missing close in the window, which
// it dropped, are differences ADR 0026 point 8 lists.
func (r *momentumRun) trend(m *MomentumMonth) {
	tr, reason, err := momentum.EvaluateTrend(r.cfg, m.Index, m.closes(r.cfg.TrendInstrument))
	if err != nil {
		r.checks[CheckExport].fail(fmt.Sprintf("%s: %s: %v", m.Month, r.cfg.TrendInstrument, err))
		return
	}
	r.trendResult(m, tr, reason)
}

// trendResult compares EvaluateTrend's result with QuantConnect's. A
// listed difference is attributed only when QuantConnect's own data shows
// it: fewer than TrendCloses closes counted, or a close missing from the
// window; otherwise Go's reason is a failure.
func (r *momentumRun) trendResult(m *MomentumMonth, tr momentum.Trend, reason momentum.WithheldReason) {
	c := r.checks[CheckRiskOff]
	where := m.Month + " " + r.cfg.TrendInstrument
	switch {
	case reason == momentum.ReasonInsufficientTrendHistory && m.SPYCloses < r.cfg.TrendCloses:
		c.Attributed++
		r.diff(where, KindTrendHistory,
			fmt.Sprintf("%d closes; QuantConnect counted %d and decided risk_off=%v, Go withholds", tr.Closes, m.SPYCloses, m.RiskOff))
	case reason == momentum.ReasonMissingBars && missingIn(m.Closes[r.cfg.TrendInstrument], r.cfg.TrendCloses):
		c.Attributed++
		r.diff(where, KindGap,
			fmt.Sprintf("a close missing from the %d-close window; QuantConnect decided risk_off=%v, Go withholds", r.cfg.TrendCloses, m.RiskOff))
	case reason != "":
		c.compare(false, 0, fmt.Sprintf("%s: Go withholds (%s) with %d closes; QuantConnect counted %d, none missing from the window",
			where, reason, tr.Closes, m.SPYCloses))
	default:
		rel := relative(tr.Mean, m.SPYMean)
		c.compare(tr.RiskOff == m.RiskOff && tr.Close == m.SPYClose && rel <= RelativeTolerance, rel,
			fmt.Sprintf("%s: Go risk_off=%v close %v mean %v, QuantConnect risk_off=%v close %v mean %v",
				m.Month, tr.RiskOff, tr.Close, tr.Mean, m.RiskOff, m.SPYClose, m.SPYMean))
	}
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

// histories recomputes each sampled name's score and volatility, or why it
// has none, from its closes. It returns an error for closes Score refuses.
func (r *momentumRun) histories(m *MomentumMonth) error {
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
	ranked := map[string]RankedEntry{}
	for _, e := range m.Ranked {
		ranked[e.Ticker] = e
	}
	for _, x := range results {
		r.history(m, ranked, x.name, x.h)
	}
	return nil
}

// history compares one sampled name's Go history with what QuantConnect
// did with it.
func (r *momentumRun) history(m *MomentumMonth, ranked map[string]RankedEntry, name string, h momentum.History) {
	hist, scores, vols := r.checks[CheckHistory], r.checks[CheckScores], r.checks[CheckVolatility]
	where := m.Month + " " + name
	e, scored := ranked[name]
	n, short := m.Short[name]
	switch {
	case scored && h.Status == momentum.StatusGap && missingIn(m.Closes[name], r.cfg.ScoreLookback+1):
		hist.Attributed++
		r.diff(where, KindGap, fmt.Sprintf("QuantConnect scored %v, volatility %v across a missing close; Go leaves it out", e.Score, e.Volatility))
	case scored && (h.Status == momentum.StatusScored || h.Status == momentum.StatusZeroVolatility):
		hist.compare(true, 0, "")
		rs, rv := relative(h.Score, e.Score), relative(h.Volatility, e.Volatility)
		scores.compare(rs <= RelativeTolerance, rs, fmt.Sprintf("%s: Go %v, QuantConnect %v", where, h.Score, e.Score))
		vols.compare(rv <= RelativeTolerance, rv, fmt.Sprintf("%s: Go %v, QuantConnect %v", where, h.Volatility, e.Volatility))
	case short:
		hist.compare(h.Status == momentum.StatusShortHistory && h.Closes == n, 0,
			fmt.Sprintf("%s: QuantConnect left it out with %d closes; Go %s with %d", where, n, h.Status, h.Closes))
	default:
		hist.compare(false, 0, fmt.Sprintf("%s: Go %s with %d closes; QuantConnect scored=%v", where, h.Status, h.Closes, scored))
	}
}

// cause is a backtest behaviour that changes the book from some rank on:
// a tie reordered, a zero volatility or a missing sector. inBook is
// whether QuantConnect's book held it (marked P or C): only then can it
// change a later mark or the picked set.
type cause struct {
	kind   Kind
	where  string
	rank   int
	inBook bool
}

func inBook(k Mark) bool { return k == MarkPicked || k == MarkCapped }

// book compares the ranked order, the cap marks and the weights. A listed
// cause, or a no-sector name, is that cause's difference, not a failure;
// it excuses only what can follow from it: a cap mark at or after the
// first cause QuantConnect's book held, and the weights of a month whose
// picked set differs because of such a cause. Every other item is
// compared (parity.go: any other difference fails).
func (r *momentumRun) book(m *MomentumMonth, book momentum.Book) {
	order, marks, weights := r.checks[CheckRankedOrder], r.checks[CheckCapMarks], r.checks[CheckWeights]
	if len(book.Ranked) != len(m.Ranked) {
		order.compare(false, 0, fmt.Sprintf("%s: Go ranked %d names, QuantConnect %d", m.Month, len(book.Ranked), len(m.Ranked)))
		return
	}
	byTicker := map[string]RankedEntry{}
	for _, e := range m.Ranked {
		byTicker[e.Ticker] = e
	}
	var causes []cause
	for i, g := range book.Ranked {
		q := m.Ranked[i]
		where := fmt.Sprintf("%s rank %d", m.Month, i+1)
		switch {
		case g.InstrumentID == q.Ticker:
			order.compare(true, 0, "")
		case g.Score == q.Score:
			order.Attributed++
			r.diff(where, KindTie, fmt.Sprintf("Go %s, QuantConnect %s, both scored %v", g.InstrumentID, q.Ticker, q.Score))
			causes = append(causes, cause{KindTie, where, i, inBook(q.Mark) || inBook(byTicker[g.InstrumentID].Mark)})
		default:
			order.compare(false, 0, fmt.Sprintf("%s: Go %s (%v), QuantConnect %s (%v)", where, g.InstrumentID, g.Score, q.Ticker, q.Score))
		}
	}
	// A cause of Go's own status is recorded only when QuantConnect's data
	// shows it: a zero volatility, or no sector.
	own := map[int]Kind{}
	for j, g := range book.Ranked {
		q := byTicker[g.InstrumentID]
		where := fmt.Sprintf("%s %s", m.Month, g.InstrumentID)
		switch {
		case g.Status == momentum.StatusZeroVolatility && q.Volatility == 0:
			own[j] = KindZeroVolatility
		case g.Status == momentum.StatusNoSector && q.Sector == "":
			own[j] = KindNoSector
		default:
			continue
		}
		causes = append(causes, cause{own[j], where, j, inBook(q.Mark)})
	}
	var from *cause
	for i := range causes {
		if c := &causes[i]; c.inBook && (from == nil || c.rank < from.rank) {
			from = c
		}
	}
	for j, g := range book.Ranked {
		q := byTicker[g.InstrumentID]
		where := fmt.Sprintf("%s %s", m.Month, g.InstrumentID)
		if k, ok := own[j]; ok {
			marks.Attributed++
			r.diff(where, k, fmt.Sprintf("QuantConnect marked %q; Go leaves it out (%s)", q.Mark, g.Status))
			continue
		}
		mark, err := liveMark(g.Status)
		switch {
		case g.Status == momentum.StatusZeroVolatility || g.Status == momentum.StatusNoSector:
			marks.compare(false, 0, fmt.Sprintf("%s: Go leaves it out (%s), but QuantConnect has volatility %v, sector %q",
				where, g.Status, q.Volatility, q.Sector))
		case err != nil:
			r.checks[CheckExport].fail(fmt.Sprintf("%s: %v", where, err))
		case mark == q.Mark:
			marks.compare(true, 0, "")
		case from != nil && j >= from.rank:
			marks.Attributed++
			r.diff(where, from.kind, fmt.Sprintf("Go %q, QuantConnect %q, following %s", mark, q.Mark, from.where))
		default:
			marks.compare(false, 0, fmt.Sprintf("%s: Go %q (%s), QuantConnect %q", where, mark, g.Status, q.Mark))
		}
	}
	r.weights(m, book, from, weights)
}

// weights compares every target weight, unless the picked sets differ and
// a cause QuantConnect's book held explains it: then the month's weights
// follow from that cause and are attributed to it.
func (r *momentumRun) weights(m *MomentumMonth, book momentum.Book, from *cause, c *Check) {
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
	setsDiffer, maxDiff := false, 0.0
	for _, n := range sorted {
		_, inGo := goW[n]
		_, inQC := m.Weights[n]
		setsDiffer = setsDiffer || inGo != inQC
		maxDiff = math.Max(maxDiff, math.Abs(goW[n]-m.Weights[n]))
	}
	if setsDiffer && from != nil {
		c.Attributed += len(sorted)
		r.diff(m.Month+" weights", from.kind, fmt.Sprintf("picked sets differ; largest weight difference %.3g, following %s", maxDiff, from.where))
		return
	}
	for _, n := range sorted {
		g, inGo := goW[n]
		q, inQC := m.Weights[n]
		d := math.Abs(g - q)
		c.compare(inGo && inQC && d <= WeightTolerance, d,
			fmt.Sprintf("%s %s: Go %v (set %v), QuantConnect %v (set %v)", m.Month, n, g, inGo, q, inQC))
	}
}

// liveMark maps a live status the backtest's marks can express to that
// mark. Any other status (an unaffordable skip, a status added later) is
// an error: the comparison refuses what it does not understand.
func liveMark(s momentum.Status) (Mark, error) {
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
