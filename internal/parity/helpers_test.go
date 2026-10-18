package parity_test

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/boost"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

// Synthetic exports only: every close here is generated, none is market
// data (AGENTS.md rule 6; committed fixtures stay synthetic).

// sessions returns n weekday dates ending on the last weekday before the
// first of month (YYYY-MM), oldest first.
func sessions(month string, n int) []string {
	first, err := time.Parse("2006-01", month)
	if err != nil {
		panic(err)
	}
	var out []string
	for day := first.AddDate(0, 0, -1); len(out) < n; day = day.AddDate(0, 0, -1) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, day.Format(time.DateOnly))
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// path returns n positive synthetic closes: steady growth by growth per
// session times a small repeating wobble, so returns vary but stay exact
// functions of i and seed.
func path(n int, growth float64, seed int) []float64 {
	out := make([]float64, n)
	p := 100.0
	for i := range out {
		wobble := 1 + float64(0.004*float64((i*7+seed*3)%5-2))
		p *= growth
		out[i] = float64(p * wobble)
	}
	return out
}

func ptrs(xs []float64) []*float64 {
	out := make([]*float64, len(xs))
	for i := range xs {
		v := xs[i]
		out[i] = &v
	}
	return out
}

// backtestScore and backtestVol restate defensive_v2.py:289-291 plainly:
// px.iloc[-22] / px.iloc[-253] - 1, and np.std of the last 252 returns.
func backtestScore(px []float64) float64 { return px[len(px)-22]/px[len(px)-253] - 1 }

func backtestVol(px []float64) float64 {
	w := px[len(px)-253:]
	rets := make([]float64, 252)
	sum := 0.0
	for i := range rets {
		rets[i] = w[i+1]/w[i] - 1
		sum += rets[i]
	}
	mean := sum / 252
	sq := 0.0
	for _, r := range rets {
		d := r - mean
		sq += float64(d * d)
	}
	return math.Sqrt(sq / 252)
}

type name struct {
	ticker string
	sector *int
	closes []float64
}

// syntheticMonth builds a matching export: what defensive_v2.py would log
// for 40 synthetic names in eleven sectors (the first sector's names score
// highest, so the cap of 3 binds), two names too short to score, and a
// trend instrument rising (risk-on) or falling at the end (risk-off).
func syntheticMonth(month string, riskOff bool) parity.MomentumMonth {
	const rows = 260
	index := sessions(month, rows)
	var names []name
	for i := 0; i < 40; i++ {
		s := 101 + i%11
		if i < 8 {
			s = 101
		}
		names = append(names, name{ticker: "N" + strconv.Itoa(100+i), sector: &s,
			closes: path(rows, 1+float64(0.0001*float64(40-i)), i)})
	}
	spy := path(rows, 1.0005, 99)
	if riskOff {
		for i := rows - 30; i < rows; i++ {
			spy[i] = float64(spy[rows-31] * (1 - float64(0.01*float64(i-rows+31))))
		}
	}
	m := parity.MomentumMonth{
		Month: month, Index: index, Members: 42, Weights: map[string]float64{},
		Short:  map[string]int{"S1": 100, "S2": 30},
		Closes: map[string][]*float64{"SPY": ptrs(spy)},
	}
	sum := 0.0
	for _, p := range spy[rows-200:] {
		sum += p
	}
	m.SPYClose, m.SPYMean, m.SPYCloses = spy[rows-1], sum/200, rows
	m.RiskOff = m.SPYClose < m.SPYMean
	for short, n := range m.Short {
		col := make([]*float64, rows)
		copy(col[rows-n:], ptrs(path(n, 1.001, n)))
		m.Closes[short] = col
	}
	for _, nm := range names {
		var sector string
		if nm.sector != nil {
			sector = strconv.Itoa(*nm.sector)
		}
		m.Ranked = append(m.Ranked, parity.RankedEntry{Ticker: nm.ticker, Score: backtestScore(nm.closes),
			Volatility: backtestVol(nm.closes), Sector: sector})
		m.Closes[nm.ticker] = ptrs(nm.closes)
	}
	sort.SliceStable(m.Ranked, func(i, j int) bool { return m.Ranked[i].Score > m.Ranked[j].Score })
	m.Scored = len(m.Ranked)
	markAndWeigh(&m)
	return m
}

// rebook re-sorts the logged ranking by score, as the backtest's stable
// sort does, and logs the book again.
func rebook(m *parity.MomentumMonth) {
	sort.SliceStable(m.Ranked, func(i, j int) bool { return m.Ranked[i].Score > m.Ranked[j].Score })
	markAndWeigh(m)
}

// markAndWeigh restates defensive_book (defensive_v2.py:49-89) for
// F_COMBO: the first 25 by score with at most 3 per sector, weighted by
// inverse volatility, half to IEF when risk-off.
func markAndWeigh(m *parity.MomentumMonth) {
	cfg := momentum.FCombo()
	counts, picks := map[string]int{}, []int{}
	for i := range m.Ranked {
		r := &m.Ranked[i]
		switch {
		case len(picks) == cfg.TopN:
			r.Mark = parity.MarkBeyond
		case r.Sector != "" && counts[r.Sector] >= cfg.SectorCap:
			r.Mark = parity.MarkCapped
		default:
			r.Mark = parity.MarkPicked
			picks = append(picks, i)
			counts[r.Sector]++
		}
	}
	part := cfg.Invested
	if m.RiskOff {
		part = float64(cfg.Invested * (1 - cfg.RiskOffFraction))
	}
	// raw is 1/vol, or 1 for a zero volatility (defensive_v2.py:76).
	raw := func(i int) float64 {
		if v := m.Ranked[i].Volatility; v != 0 {
			return 1 / v
		}
		return 1
	}
	total := 0.0
	for _, i := range picks {
		total += raw(i)
	}
	m.Weights = map[string]float64{}
	for _, i := range picks {
		m.Weights[m.Ranked[i].Ticker] = part * raw(i) / total
	}
	if bond := cfg.Invested - part; bond > 1e-12 {
		m.Weights[cfg.RiskOffInstrument] = bond
	}
}

func boostParams() boost.Parameters {
	return boost.Parameters{InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 10, TrendDays: 200, ExitSMA: 5}
}

// syntheticBoost builds a matching Boost export of n sessions, the first
// warmup of them warm-up: an uptrend with a sharp two-day dip every 23
// sessions, which drives RSI(2) under 10 above the 200-day mean.
func syntheticBoost(n, warmup int) []parity.BoostSession {
	p := boostParams()
	ind, err := boost.NewIndicators(p)
	if err != nil {
		panic(err)
	}
	day := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	price, latched := 100.0, false
	var out []parity.BoostSession
	for i := 0; i < n; i++ {
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, 1)
		}
		price = float64(price * 1.002)
		closePx := price
		if k := i % 23; k == 20 || k == 21 {
			closePx = float64(price * (1 - float64(0.02*float64(k-19))))
		}
		r := ind.Add(closePx)
		s := parity.BoostSession{Date: day.Format(time.DateOnly), Close: closePx, Warmup: i < warmup}
		if r.RSIReady {
			s.RSI = &r.RSI
		}
		if r.TrendMeanReady {
			s.TrendMean = &r.TrendMean
		}
		if r.ExitMeanReady {
			s.ExitMean = &r.ExitMean
		}
		if !s.Warmup {
			latched = p.Next(latched, r)
			s.Latched, s.Target = latched, p.Target(latched)
		}
		out = append(out, s)
		day = day.AddDate(0, 0, 1)
	}
	return out
}

func checkNamed(r parity.Report, name string) parity.Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	panic("no check " + name)
}
