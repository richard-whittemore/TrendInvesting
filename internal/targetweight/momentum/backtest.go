package momentum

import "sort"

// The backtest-compatible functions decide as defensive_v2.py did in each
// case where the live rule departs from it (ADR 0026 point 8; plan.md
// section 4, "Sectors"). No live decision uses them: they exist so the
// parity comparison can recompute QuantConnect's book exactly and list
// each live departure from it apart (ADR 0026 point 7, item 2).

// backtestBondFloor is the smallest risk-off share defensive_book wrote as
// a target (defensive_v2.py:87, to_bond > 1e-12).
const backtestBondFloor = 1e-12

// BacktestBook is defensive_book as F_COMBO ran it (defensive_v2.py:49-89,
// ranked by defensive_v2.py:323): the candidates ranked by score, highest
// first, equal scores kept in the order given, as Python's stable sort
// kept its universe's order; the first TopN picked with at most SectorCap
// per sector, a name with no sector uncapped and counted toward no
// sector's cap; the picks weighted by inverse volatility, a zero
// volatility weighted as 1 (defensive_v2.py:76), summed in pick order;
// when riskOff, RiskOffFraction of Invested goes to RiskOffInstrument, and
// with no pick or no momentum share all of it. Every name is
// StatusPicked, StatusSectorCapped or StatusBeyondTopN, and
// BacktestWouldPick is whether it was picked.
func BacktestBook(cfg Config, candidates []Candidate, riskOff bool) (Book, error) {
	ranked, err := rankable(cfg, candidates)
	if err != nil {
		return Book{}, err
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
	book := capper{cfg: cfg}
	var picks []int
	for i := range ranked {
		r := &ranked[i]
		r.ScoreRank = i + 1
		switch {
		case book.take(r.Sector):
			r.Status = StatusPicked
			picks = append(picks, i)
		case book.full():
			r.Status = StatusBeyondTopN
		default:
			r.Status = StatusSectorCapped
		}
		r.BacktestWouldPick = r.Status == StatusPicked
	}

	equity := cfg.Invested
	part := equity
	if riskOff {
		part = float64(equity * (1 - cfg.RiskOffFraction))
	}
	toBond := equity - part
	var targets []Target
	if len(picks) > 0 && part > 0 {
		raw := func(v float64) float64 {
			if v == 0 {
				return 1
			}
			return 1 / v
		}
		total := 0.0
		for _, i := range picks {
			total += raw(ranked[i].Volatility)
		}
		for _, i := range picks {
			ranked[i].Weight = part * raw(ranked[i].Volatility) / total
			targets = append(targets, Target{InstrumentID: ranked[i].InstrumentID, Weight: ranked[i].Weight})
		}
	} else {
		toBond += part
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].InstrumentID < targets[j].InstrumentID })
	if toBond > backtestBondFloor {
		targets = append(targets, Target{InstrumentID: cfg.RiskOffInstrument, Weight: toBond})
	}
	return Book{Ranked: ranked, Targets: targets}, nil
}

// BacktestScore is Score as defensive_v2.py computed it (286-291): it
// drops the sessions with no close and reads the last ScoreLookback+1
// closes the name has, so its window shifts across a missing close where
// Score's gives StatusGap. Fewer closes is StatusShortHistory.
func BacktestScore(cfg Config, sessions []string, closes map[string]float64) (History, error) {
	return score(cfg, sessions, closes, lastClosesHad)
}

// BacktestTrend is EvaluateTrend as defensive_v2.py decided it (277-280,
// with parity_log.py's export of the mean): it drops the sessions with no
// close; with fewer than TrendCloses closes it is risk-on, its Mean that
// of every close it has; otherwise its Mean is that of the last
// TrendCloses closes it has. With no close at all the Trend is zero.
func BacktestTrend(cfg Config, sessions []string, closes map[string]float64) (Trend, error) {
	if err := cfg.Validate(); err != nil {
		return Trend{}, err
	}
	if err := checkInputs(sessions, closes); err != nil {
		return Trend{}, err
	}
	tr := Trend{Closes: len(closes)}
	if tr.Closes == 0 {
		return tr, nil
	}
	w, _ := lastClosesHad(sessions, closes, min(tr.Closes, cfg.TrendCloses))
	tr.Close, tr.Mean = closeAndMean(w)
	tr.RiskOff = tr.Closes >= cfg.TrendCloses && tr.Close < tr.Mean
	return tr, nil
}

// lastClosesHad returns the last n closes there are, oldest first, across
// any session with none. Callers check first that there are at least n.
func lastClosesHad(sessions []string, closes map[string]float64, n int) ([]float64, bool) {
	out := make([]float64, 0, n)
	for i := len(sessions) - 1; i >= 0 && len(out) < n; i-- {
		if p, ok := closes[sessions[i]]; ok {
			out = append(out, p)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, true
}
