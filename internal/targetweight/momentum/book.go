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
	// BacktestWouldPick reports whether defensive_book, which keeps an
	// unlabelled name uncapped and weights a zero volatility as 1, would
	// have picked it. It differs from Status only for names live trading
	// treats differently, and makes each such difference visible in the
	// decision (ADR 0026 point 8; plan.md section 4, "Sectors": alerted if
	// it would otherwise have been among the top 25).
	BacktestWouldPick bool
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
// TopN with at most SectorCap per sector label, and weights the picks by
// inverse volatility to Invested; when riskOff, RiskOffFraction of that
// goes to RiskOffInstrument instead (defensive_book,
// defensive_v2.py:49-89, with F_COMBO's sector cap, inv_vol and
// risk_off_fraction; plan.md section 2).
//
// Three live rules differ from the backtest, each recorded per name
// through BacktestWouldPick (ADR 0026 point 8):
//   - equal scores rank by ascending instrument ID; the backtest kept the
//     order its universe arrived in (sorted, defensive_v2.py:323);
//   - a name with no sector label is left out (StatusNoSector), never
//     uncapped as defensive_book left it (plan.md section 4, "Sectors");
//   - a name with zero volatility is left out (StatusZeroVolatility);
//     defensive_book weighted it as 1 against others' 1/vol of about 50
//     (defensive_v2.py:76).
//
// With no pick the whole of Invested goes to RiskOffInstrument, as
// defensive_book does (84-85); Decide withholds such a decision instead.
//
// The inverse volatilities are summed in pick order, as Python summed its
// dict's values, so the weights can match the backtest's to the last bit.
func SelectBook(cfg Config, candidates []Candidate, riskOff bool) (Book, error) {
	if err := cfg.Validate(); err != nil {
		return Book{}, err
	}
	ranked := make([]RankedName, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for i, c := range candidates {
		if err := c.validate(cfg); err != nil {
			return Book{}, err
		}
		if seen[c.InstrumentID] {
			return Book{}, fmt.Errorf("momentum: candidate %s repeated", c.InstrumentID)
		}
		seen[c.InstrumentID] = true
		ranked[i] = RankedName{Candidate: c}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].InstrumentID < ranked[j].InstrumentID
	})

	live, backtest := capper{cfg: cfg}, capper{cfg: cfg}
	var picks []int
	for i := range ranked {
		r := &ranked[i]
		r.ScoreRank = i + 1
		r.BacktestWouldPick = backtest.take(r.Sector)
		switch {
		case r.Sector == "":
			r.Status = StatusNoSector
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
	var targets []Target
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
	return Book{Ranked: ranked, Targets: targets}, nil
}

// capper counts picks per sector label for one pass of the pick loop.
type capper struct {
	cfg    Config
	picks  int
	counts map[string]int
}

func (c *capper) full() bool { return c.picks >= c.cfg.TopN }

// take picks a name of sector if the book has room and the sector is under
// its cap, and reports whether it did. An empty sector is never capped, as
// defensive_book's sec is None (69-74); the live pass never offers one.
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
