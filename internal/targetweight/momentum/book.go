package momentum

import (
	"fmt"
	"sort"
)

// Candidate is a scored universe member offered to SelectBook.
type Candidate struct {
	InstrumentID string
	Sector       string
	Score        float64
	Volatility   float64
}

// RankedName is a Candidate with what SelectBook did with it.
type RankedName struct {
	Candidate
	// ScoreRank is its place among all candidates by score, 1 the highest.
	ScoreRank int
	Status    Status
	// Weight is its target weight when Status is StatusPicked, else 0.
	Weight float64
	// BacktestWouldPick reports whether defensive_book, which weights a
	// zero volatility as 1, would have picked it. It differs from Status
	// only for names live trading treats differently, and makes each such
	// difference visible in the decision (ADR 0026 point 8).
	BacktestWouldPick bool
	// OneShare and TargetValue are set when Status is StatusUnaffordable:
	// its previous raw close, and the target value it exceeded on the pass
	// that skipped it.
	OneShare    float64
	TargetValue float64
}

// MinimumLot is what D7-L's substitution values each pick against (ADR
// 0026, point 2, "D7-L"): net liquidation V and the raw closes of the
// previous session, the valuation basis the planner sizes orders from (ADR
// 0026, point 2, "Valuation basis"), and Scale, the momentum sleeve's share
// of the account, 1 - b (plan.md section 2, "Bond sleeve and glide path").
// A pick's target value is its weight x Scale x V.
type MinimumLot struct {
	NetLiquidation float64
	Scale          float64
	Prices         map[string]float64
}

func (l MinimumLot) validate(candidates []Candidate) error {
	switch {
	case !finite(l.NetLiquidation) || l.NetLiquidation <= 0:
		return fmt.Errorf("momentum: minimum lot net liquidation %v must be finite and positive", l.NetLiquidation)
	case !finite(l.Scale) || l.Scale <= 0 || l.Scale > 1:
		return fmt.Errorf("momentum: minimum lot scale %v outside (0, 1]", l.Scale)
	}
	for _, c := range candidates {
		if p, ok := l.Prices[c.InstrumentID]; !ok || !finite(p) || p <= 0 {
			return fmt.Errorf("momentum: minimum lot has no finite, positive price for %s", c.InstrumentID)
		}
	}
	return nil
}

// Target is one instrument's target weight, a fraction of the account.
type Target struct {
	InstrumentID string
	Weight       float64
}

// Book is SelectBook's result: every candidate in score order, and the
// targets in ascending instrument ID with the risk-off instrument last.
type Book struct {
	Ranked  []RankedName
	Targets []Target
}

// SelectBook ranks candidates by score, highest first, picks the first
// TopN with at most SectorCap per sector label, a name with no sector label
// uncapped and counted toward no sector's cap, as defensive_book left it
// (defensive_v2.py:69-74; ADR 0026 point 8, the owner's decision of
// 2026-10-18), and weights the picks by inverse volatility to Invested;
// when riskOff, RiskOffFraction of that goes to RiskOffInstrument instead
// (defensive_book, defensive_v2.py:49-89, with F_COMBO's sector cap,
// inv_vol and risk_off_fraction; plan.md section 2).
//
// Two live rules differ from the backtest, each recorded per name through
// BacktestWouldPick (ADR 0026 point 8):
//   - equal scores rank by ascending instrument ID; the backtest kept the
//     order its universe arrived in (sorted, defensive_v2.py:323);
//   - a name with zero volatility is left out (StatusZeroVolatility);
//     defensive_book weighted it as 1 against others' 1/vol of about 50
//     (defensive_v2.py:76).
//
// With no pick the whole of Invested goes to RiskOffInstrument, as
// defensive_book does (84-85); Decide withholds such a decision instead
// (no-eligible-names, or all-unaffordable when D7-L skipped a name).
//
// The inverse volatilities are summed in pick order, as Python summed its
// dict's values, so the weights can match the backtest's to the last bit.
func SelectBook(cfg Config, candidates []Candidate, riskOff bool) (Book, error) {
	return selectBook(cfg, candidates, riskOff, nil)
}

// SelectBookWithMinimumLot is SelectBook under D7-L, whole shares (ADR 0026,
// point 2, "D7-L"; plan.md section 5, "whole shares"; the owner's choice for
// the IRAs, 2026-10-16): before the book is final, a pick whose one share at
// its previous raw close exceeds its target value is recorded
// StatusUnaffordable and skipped, and the next eligible ranked name, within
// the sector cap, takes its place. A skipped name holds no place in the book
// or its sector.
//
// Only a pick with a positive target value is compared. A risk-off book with
// RiskOffFraction 1 gives every pick a zero target, which buys none of it:
// that book is SelectBook's, the whole of Invested in RiskOffInstrument, and
// Decide makes it, so the planner sells every stock held, as the risk-off
// rule asks. When every pick with a positive target is skipped, at any
// RiskOffFraction, the book has no pick; Decide withholds it as
// all-unaffordable and the last book is kept.
//
// A target value depends on the weights, which depend on the picks, so the
// substitution is a fixed point, reached by repeating the selection: each
// pass selects from every name not yet skipped, weights the picks, and skips
// every pick then above its target. A name once skipped stays skipped, so
// each pass that changes the book skips at least one more name, and the
// passes end, after at most one per candidate, when no pick's one share
// exceeds its target. Every pick of the final book is affordable at its final
// weight. The backtest had no such rule, so BacktestWouldPick ignores it and
// the parity rerun measures it.
func SelectBookWithMinimumLot(cfg Config, candidates []Candidate, riskOff bool, lot MinimumLot) (Book, error) {
	if err := lot.validate(candidates); err != nil {
		return Book{}, err
	}
	return selectBook(cfg, candidates, riskOff, &lot)
}

func selectBook(cfg Config, candidates []Candidate, riskOff bool, lot *MinimumLot) (Book, error) {
	ranked, err := rankable(cfg, candidates)
	if err != nil {
		return Book{}, err
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].InstrumentID < ranked[j].InstrumentID
	})

	skipped := map[string]bool{}
	for {
		targets, picks := pickAndWeigh(cfg, ranked, riskOff, skipped)
		more := false
		for _, i := range picks {
			r := &ranked[i]
			if lot == nil {
				break
			}
			value := float64(float64(r.Weight*lot.Scale) * lot.NetLiquidation)
			if price := lot.Prices[r.InstrumentID]; r.Weight > 0 && price > value {
				skipped[r.InstrumentID], more = true, true
				r.OneShare, r.TargetValue = price, value
			}
		}
		if !more {
			return Book{Ranked: ranked, Targets: targets}, nil
		}
	}
}

// pickAndWeigh runs one selection pass over ranked, in score order: it sets
// each name's rank, status and weight, leaving the names in skipped
// StatusUnaffordable, and returns the targets and the picks' indices.
func pickAndWeigh(cfg Config, ranked []RankedName, riskOff bool, skipped map[string]bool) (targets []Target, picks []int) {
	live, backtest := capper{cfg: cfg}, capper{cfg: cfg}
	for i := range ranked {
		r := &ranked[i]
		r.ScoreRank = i + 1
		r.Weight = 0
		r.BacktestWouldPick = backtest.take(r.Sector)
		switch {
		case skipped[r.InstrumentID]:
			r.Status = StatusUnaffordable
		case r.Volatility == 0:
			r.Status = StatusZeroVolatility
		case live.full():
			r.Status = StatusBeyondTopN
		case !live.take(r.Sector):
			r.Status = StatusSectorCapped
		default:
			r.Status = StatusPicked
			picks = append(picks, i)
		}
	}

	equity := cfg.Invested
	part := equity
	if riskOff {
		part = float64(equity * (1 - cfg.RiskOffFraction))
	}
	toBond := equity - part
	if len(picks) == 0 {
		toBond = equity
	} else {
		total := 0.0
		for _, i := range picks {
			total += 1 / ranked[i].Volatility
		}
		for _, i := range picks {
			ranked[i].Weight = part * (1 / ranked[i].Volatility) / total
			targets = append(targets, Target{InstrumentID: ranked[i].InstrumentID, Weight: ranked[i].Weight})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].InstrumentID < targets[j].InstrumentID })
	if toBond > 0 {
		targets = append(targets, Target{InstrumentID: cfg.RiskOffInstrument, Weight: toBond})
	}
	return targets, picks
}

// capper counts picks per sector label for one pass of the pick loop.
type capper struct {
	cfg    Config
	picks  int
	counts map[string]int
}

func (c *capper) full() bool { return c.picks >= c.cfg.TopN }

// take picks a name of sector if the book has room and the sector is under
// its cap, and reports whether it did. An empty sector is never capped and
// counts toward no sector's cap, as defensive_book's sec is None (69-74).
func (c *capper) take(sector string) bool {
	if c.full() {
		return false
	}
	if sector != "" && c.counts[sector] >= c.cfg.SectorCap {
		return false
	}
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.picks++
	if sector != "" {
		c.counts[sector]++
	}
	return true
}

// rankable validates cfg and the candidates, refusing a repeated
// instrument, and returns them as unranked names in the order given.
func rankable(cfg Config, candidates []Candidate) ([]RankedName, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ranked := make([]RankedName, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for i, c := range candidates {
		if err := c.validate(cfg); err != nil {
			return nil, err
		}
		if seen[c.InstrumentID] {
			return nil, fmt.Errorf("momentum: candidate %s repeated", c.InstrumentID)
		}
		seen[c.InstrumentID] = true
		ranked[i] = RankedName{Candidate: c}
	}
	return ranked, nil
}

func (c Candidate) validate(cfg Config) error {
	switch {
	case c.InstrumentID == "":
		return fmt.Errorf("momentum: candidate with empty instrument ID")
	case c.InstrumentID == cfg.RiskOffInstrument:
		return fmt.Errorf("momentum: candidate %s is the risk-off instrument", c.InstrumentID)
	case !finite(c.Score):
		return fmt.Errorf("momentum: candidate %s score %v", c.InstrumentID, c.Score)
	case !finite(c.Volatility) || c.Volatility < 0:
		return fmt.Errorf("momentum: candidate %s volatility %v", c.InstrumentID, c.Volatility)
	}
	return nil
}
