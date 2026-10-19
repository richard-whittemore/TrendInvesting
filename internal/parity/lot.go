package parity

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// The D7-L checks (docs/research/parity-2026-10.md, "Remaining scope, plan,
// 2026-10-19").
const (
	CheckLotMonths  = "d7l-months"
	CheckLotExport  = "d7l-export"
	CheckLotPicks   = "d7l-picks"
	CheckLotSkipped = "d7l-skipped"
	CheckLotKept    = "d7l-kept"
	CheckLotWeights = "d7l-weights"
)

// The modes of research/edge-search/momentum/minimum_lot.py: D7-L as first
// written, and with a held pick kept (ADR 0026 point 2, amended
// 2026-10-18).
const (
	LotLiteral = "literal"
	LotHeld    = "held"
)

// LotEntry is one ranked name of a D7-L month, best first. An empty Sector
// means it had none.
type LotEntry struct {
	Ticker     string
	Score      float64
	Volatility float64
	Sector     string
}

// LotMonth is one logged rebalance of a QuantConnect D7-L run
// (research/edge-search/momentum/minimum_lot_parity_log.py): what D7-L
// valued the picks against (net liquidation, each ranked name's raw close,
// the long holdings) and what it decided: the targets, or Withheld with no
// targets, and the names it skipped and kept.
type LotMonth struct {
	Month    string
	Mode     string
	NLV      float64
	RiskOff  bool
	Ranked   []LotEntry
	Prices   map[string]float64
	Held     map[string]int64
	Weights  map[string]float64
	Withheld bool
	Skipped  []string
	Kept     []string
}

// CompareMinimumLot checks each logged D7-L month against
// momentum.SelectBookWithMinimumLot given the run's own inputs (its ranked
// names with their scores, volatilities and sectors, its raw closes, its
// net liquidation at full momentum scale, and in "held" mode its holdings):
// the picked names, a kept name among them, and whether the month is
// withheld, exactly; the names skipped and kept, exactly; the targets
// within WeightTolerance (docs/research/parity-2026-10.md, "What Go
// recomputes, and the thresholds"; ADR 0026 point 7, item 2). The skipped
// and kept checks compare every month, most of them of no name on either
// side, so the report also counts the months in which the rule fired.
//
// The live book ranks equal scores by ticker and leaves out a zero
// volatility, which the backtest did not (ADR 0026 point 8); each month
// holding either is listed as a difference of that kind. It is still
// compared: if it changes the book, the book checks fail.
func CompareMinimumLot(cfg momentum.Config, run string, planned []string, months []LotMonth) Report {
	checks := map[string]*Check{}
	var order []*Check
	for _, c := range []struct{ name, threshold string }{
		{CheckLotMonths, "every planned month exported once"},
		{CheckLotExport, "well-formed"},
		{CheckLotPicks, "exact, withheld included"},
		{CheckLotSkipped, "exact"},
		{CheckLotKept, "exact"},
		{CheckLotWeights, fmt.Sprintf("abs <= %g", WeightTolerance)},
	} {
		checks[c.name] = &Check{Run: run, Name: c.name, Threshold: c.threshold}
		order = append(order, checks[c.name])
	}
	plannedMonths(checks[CheckLotMonths], planned, monthsOf(months, func(m LotMonth) string { return m.Month }))
	var diffs []Difference
	for i := range months {
		diffs = append(diffs, lotMonth(cfg, run, checks, &months[i])...)
	}
	out := Report{Differences: diffs}
	for _, c := range order {
		out.Checks = append(out.Checks, *c)
	}
	out.Checks = append(out.Checks, differencesCheck(run, diffs))
	return out
}

func lotMonth(cfg momentum.Config, run string, checks map[string]*Check, m *LotMonth) []Difference {
	export := checks[CheckLotExport]
	if err := wellFormedLot(m); err != nil {
		export.compare(false, 0, fmt.Sprintf("%s: %v", m.Month, err))
		return nil
	}
	cands := make([]momentum.Candidate, len(m.Ranked))
	for i, e := range m.Ranked {
		cands[i] = momentum.Candidate{InstrumentID: e.Ticker, Sector: e.Sector, Score: e.Score, Volatility: e.Volatility}
	}
	lot := momentum.MinimumLot{NetLiquidation: m.NLV, Scale: 1, Prices: m.Prices}
	if m.Mode == LotHeld {
		lot.Held = m.Held
	}
	book, err := momentum.SelectBookWithMinimumLot(cfg, cands, m.RiskOff, lot)
	export.compare(err == nil, 0, fmt.Sprintf("%s: %v", m.Month, err))
	if err != nil {
		return nil
	}
	var picks, skipped, kept []string
	for _, r := range book.Ranked {
		switch r.Status {
		case momentum.StatusPicked:
			picks = append(picks, r.InstrumentID)
		case momentum.StatusHeldOneLotShort:
			picks = append(picks, r.InstrumentID)
			kept = append(kept, r.InstrumentID)
		case momentum.StatusUnaffordable:
			skipped = append(skipped, r.InstrumentID)
		}
	}
	goWithheld := len(picks) == 0 && len(skipped) > 0
	c := checks[CheckLotPicks]
	c.compare(goWithheld == m.Withheld, 0, fmt.Sprintf("%s: Go withheld %v, QuantConnect %v", m.Month, goWithheld, m.Withheld))
	if !goWithheld && !m.Withheld {
		qcPicks := map[string]float64{}
		for n, w := range m.Weights {
			if n != cfg.RiskOffInstrument {
				qcPicks[n] = w
			}
		}
		goPicks := set(picks)
		for _, n := range union(goPicks, qcPicks) {
			_, inGo := goPicks[n]
			_, inQC := qcPicks[n]
			c.compare(inGo && inQC, 0, fmt.Sprintf("%s %s: picked by Go %v, by QuantConnect %v", m.Month, n, inGo, inQC))
		}
		goW, w := targets(book), checks[CheckLotWeights]
		for _, n := range union(goW, m.Weights) {
			g, inGo := goW[n]
			q, inQC := m.Weights[n]
			d := math.Abs(g - q)
			w.compare(inGo && inQC && d <= WeightTolerance, d,
				fmt.Sprintf("%s %s: Go %v (set %v), QuantConnect %v (set %v)", m.Month, n, g, inGo, q, inQC))
		}
	}
	sameSet(checks[CheckLotSkipped], m.Month, skipped, m.Skipped)
	sameSet(checks[CheckLotKept], m.Month, kept, m.Kept)
	return lotDifferences(run, m)
}

// sameSet compares two lists of names as sets, once, and counts the month
// as one in which the rule fired when either side names a name.
func sameSet(c *Check, month string, goNames, qcNames []string) {
	c.CountsFired = true
	if len(goNames)+len(qcNames) > 0 {
		c.Fired++
	}
	g, q := append([]string(nil), goNames...), append([]string(nil), qcNames...)
	sort.Strings(g)
	sort.Strings(q)
	gs, qs := strings.Join(g, ","), strings.Join(q, ",")
	c.compare(gs == qs, 0, fmt.Sprintf("%s: Go [%s], QuantConnect [%s]", month, gs, qs))
}

func set(names []string) map[string]float64 {
	out := map[string]float64{}
	for _, n := range names {
		out[n] = 0
	}
	return out
}

// lotDifferences lists each tie of scores and each zero volatility among
// the month's ranked names (ADR 0026 point 8).
func lotDifferences(run string, m *LotMonth) []Difference {
	var out []Difference
	byScore := map[float64][]string{}
	for _, e := range m.Ranked {
		byScore[e.Score] = append(byScore[e.Score], e.Ticker)
		if e.Volatility == 0 {
			out = append(out, Difference{Run: run, Where: m.Month + " " + e.Ticker, Kind: KindZeroVolatility, Size: "volatility 0"})
		}
	}
	var tied []string
	for score, names := range byScore {
		if len(names) > 1 {
			tied = append(tied, fmt.Sprintf("%s at %v", strings.Join(names, " "), score))
		}
	}
	sort.Strings(tied)
	for _, t := range tied {
		out = append(out, Difference{Run: run, Where: m.Month, Kind: KindTie, Size: t})
	}
	return out
}

func wellFormedLot(m *LotMonth) error {
	switch {
	case m.Mode != LotLiteral && m.Mode != LotHeld:
		return fmt.Errorf("mode %q", m.Mode)
	case m.Withheld != (m.Weights == nil):
		return fmt.Errorf("withheld %v with %d targets", m.Withheld, len(m.Weights))
	}
	return nil
}

type lotPayload struct {
	Month   *string             `json:"month"`
	Mode    *string             `json:"mode"`
	NLV     *float64            `json:"nlv"`
	RiskOff *bool               `json:"risk_off"`
	Ranked  [][]json.RawMessage `json:"ranked"`
	Prices  map[string]*float64 `json:"prices"`
	Held    map[string]*int64   `json:"held"`
	Weights json.RawMessage     `json:"weights"`
	Skipped []string            `json:"skipped"`
	Kept    []string            `json:"kept"`
}

// DecodeLot decodes one LOTPARITY1 payload, refusing an unknown field, a
// required field missing or null (weights may be null: a withheld month,
// but not missing), a null price, holding or weight, and a ranked row that
// is not [ticker, score, volatility, sector or null].
func DecodeLot(raw []byte) (LotMonth, error) {
	var p lotPayload
	if err := strictDecode(raw, &p); err != nil {
		return LotMonth{}, err
	}
	if p.Month == nil || *p.Month == "" {
		return LotMonth{}, errors.New("parity: lot payload has no month")
	}
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"mode", p.Mode != nil}, {"nlv", p.NLV != nil}, {"risk_off", p.RiskOff != nil}, {"ranked", p.Ranked != nil},
		{"prices", p.Prices != nil}, {"held", p.Held != nil}, {"weights", p.Weights != nil},
		{"skipped", p.Skipped != nil}, {"kept", p.Kept != nil},
	} {
		if !f.present {
			return LotMonth{}, fmt.Errorf("parity: %s: %s missing or null", *p.Month, f.name)
		}
	}
	m := LotMonth{Month: *p.Month, Mode: *p.Mode, NLV: *p.NLV, RiskOff: *p.RiskOff, Skipped: p.Skipped, Kept: p.Kept}
	var err error
	if m.Prices, err = required(m.Month, "prices", p.Prices); err != nil {
		return LotMonth{}, err
	}
	if m.Held, err = required(m.Month, "held", p.Held); err != nil {
		return LotMonth{}, err
	}
	var weights map[string]*float64
	if err := json.Unmarshal(p.Weights, &weights); err != nil {
		return LotMonth{}, fmt.Errorf("parity: %s: weights: %w", m.Month, err)
	}
	if weights == nil {
		m.Withheld = true
	} else if m.Weights, err = required(m.Month, "weights", weights); err != nil {
		return LotMonth{}, err
	}
	for i, row := range p.Ranked {
		e, err := lotEntry(row)
		if err != nil {
			return LotMonth{}, fmt.Errorf("parity: %s ranked row %d: %w", m.Month, i+1, err)
		}
		m.Ranked = append(m.Ranked, e)
	}
	return m, nil
}

func lotEntry(row []json.RawMessage) (LotEntry, error) {
	var e LotEntry
	if len(row) != 4 {
		return e, fmt.Errorf("%d fields, want 4", len(row))
	}
	var ticker *string
	var score, vol, sector *float64
	for i, dst := range []any{&ticker, &score, &vol, &sector} {
		if err := json.Unmarshal(row[i], dst); err != nil {
			return e, err
		}
	}
	switch {
	case ticker == nil || *ticker == "":
		return e, errors.New("no ticker")
	case score == nil || vol == nil:
		return e, fmt.Errorf("%s: a null score or volatility", *ticker)
	}
	e.Ticker, e.Score, e.Volatility = *ticker, *score, *vol
	if sector != nil {
		if *sector != math.Trunc(*sector) || math.Abs(*sector) > 1e9 {
			return e, fmt.Errorf("sector %v is not a whole code", *sector)
		}
		e.Sector = strconv.FormatInt(int64(*sector), 10)
	}
	return e, nil
}
