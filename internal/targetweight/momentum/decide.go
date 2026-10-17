package momentum

import (
	"fmt"
	"strings"
)

// Outcome is whether a decision set targets (ADR 0026 point 1, "Output").
type Outcome string

// The closed set of Outcome values.
const (
	OutcomeDecided  Outcome = "decided"
	OutcomeWithheld Outcome = "withheld"
)

// WithheldReason says why a decision set no targets. Empty when decided.
type WithheldReason string

// The closed set of WithheldReason values (ADR 0026 points 1 and 5).
const (
	// ReasonMissingBars: the trend instrument has no close on a session of
	// its window, the last included.
	ReasonMissingBars WithheldReason = "missing-bars"
	// ReasonInsufficientTrendHistory: the trend instrument has fewer than
	// TrendCloses closes. The backtest treated this as risk-on
	// (defensive_v2.py:279).
	ReasonInsufficientTrendHistory WithheldReason = "insufficient-trend-history"
	// ReasonMissingUniverse: no universe snapshot for the close session.
	ReasonMissingUniverse WithheldReason = "missing-universe"
	// ReasonNoEligibleNames: no universe member could be picked, and none
	// was skipped as unaffordable under D7-L. The backtest kept the last
	// book (defensive_v2.py:319-320).
	ReasonNoEligibleNames WithheldReason = "no-eligible-names"
	// ReasonAllUnaffordable: no universe member could be picked and D7-L
	// skipped at least one, every pick with a positive target being dear by
	// the share. The last book is kept: a bad net liquidation or bad raw
	// closes would look the same, and selling every stock on them is the
	// wrong failure (ADR 0026, point 2, "D7-L", and point 5).
	ReasonAllUnaffordable WithheldReason = "all-unaffordable"
)

// Snapshot is one session's universe rows (ADR 0026 point 1,
// market.universe.snapshot).
type Snapshot struct {
	Session string
	Rows    []Row
}

// Input is everything one decision reads.
type Input struct {
	// Sessions is the trading calendar, ascending, as YYYY-MM-DD; its last
	// session is the close session, the last before the decision.
	Sessions []string
	// Universe is ranked on the close session (plan.md section 4, D8
	// "rank": the last session before the rebalance).
	Universe Snapshot
	// Closes maps instrument -> session -> split- and dividend-adjusted
	// close, for the trend instrument and every universe member
	// (ADR 0026 point 1, "Weights use split- and dividend-adjusted
	// closes").
	Closes map[string]map[string]float64
	// MinimumLot, when set, applies D7-L's substitution
	// (SelectBookWithMinimumLot); nil selects the book as the backtest did.
	MinimumLot *MinimumLot
}

// NameRecord is the decision's record of one universe member.
type NameRecord struct {
	InstrumentID string
	UniverseRank int
	DollarVolume float64
	Sector       string
	SectorSource string
	Status       Status
	// Closes counts its adjusted closes up to the close session.
	Closes int
	// Score and Volatility are set when it had a full window.
	Score      float64
	Volatility float64
	// ScoreRank is its place by score among the names with a full window,
	// 0 for the others.
	ScoreRank         int
	Weight            float64
	BacktestWouldPick bool
	// OneShare and TargetValue are set for an unaffordable name (D7-L).
	OneShare    float64
	TargetValue float64
}

// Decision is one momentum decision with the figures behind it, so the
// record alone can check it (ADR 0026 point 1, "Output").
type Decision struct {
	Outcome Outcome
	Reason  WithheldReason
	// Detail explains an all-unaffordable withhold, naming the valuation,
	// and a decided book whose every pick has weight zero, which sells every
	// stock held. Empty otherwise.
	Detail       string
	CloseSession string
	Trend        Trend
	Universe     UniverseCounts
	// Names holds every universe member in universe rank order.
	Names []NameRecord
	// Targets is set only when decided: ascending instrument ID, with the
	// risk-off instrument last.
	Targets []Target
}

// Decide makes one month's momentum decision: risk-off from the trend
// instrument (EvaluateTrend), the universe (RankUniverse), each member's
// score and volatility (Score) and the book (SelectBook) (plan.md
// section 2, "Momentum, combination").
//
// A decision it cannot make from complete data is withheld with its
// reason, never made from the last book or a guess (ADR 0026 point 5). A
// malformed input or configuration is an error.
func Decide(cfg Config, in Input) (Decision, error) {
	if err := cfg.Validate(); err != nil {
		return Decision{}, err
	}
	if err := checkCalendar(in.Sessions); err != nil {
		return Decision{}, err
	}
	d := Decision{CloseSession: in.Sessions[len(in.Sessions)-1]}

	trend, reason, err := EvaluateTrend(cfg, in.Sessions, in.Closes[cfg.TrendInstrument])
	if err != nil {
		return Decision{}, fmt.Errorf("momentum: %s: %w", cfg.TrendInstrument, err)
	}
	d.Trend = trend
	if reason != "" {
		return withhold(d, reason), nil
	}

	members, counts, err := RankUniverse(cfg, in.Universe.Rows)
	if err != nil {
		return Decision{}, err
	}
	d.Universe = counts
	if in.Universe.Session != d.CloseSession || len(members) == 0 {
		return withhold(d, ReasonMissingUniverse), nil
	}

	d.Names = make([]NameRecord, len(members))
	index := make(map[string]int, len(members))
	var candidates []Candidate
	for i, m := range members {
		h, err := Score(cfg, in.Sessions, in.Closes[m.InstrumentID])
		if err != nil {
			return Decision{}, fmt.Errorf("momentum: %s: %w", m.InstrumentID, err)
		}
		d.Names[i] = NameRecord{
			InstrumentID: m.InstrumentID,
			UniverseRank: m.Rank,
			DollarVolume: m.DollarVolume,
			Sector:       m.Sector,
			SectorSource: m.SectorSource,
			Status:       h.Status,
			Closes:       h.Closes,
		}
		index[m.InstrumentID] = i
		if h.Status == StatusScored || h.Status == StatusZeroVolatility {
			candidates = append(candidates, Candidate{
				InstrumentID: m.InstrumentID,
				Sector:       m.Sector,
				Score:        h.Score,
				Volatility:   h.Volatility,
			})
		}
	}

	var book Book
	if in.MinimumLot != nil {
		book, err = SelectBookWithMinimumLot(cfg, candidates, trend.RiskOff, *in.MinimumLot)
	} else {
		book, err = SelectBook(cfg, candidates, trend.RiskOff)
	}
	if err != nil {
		return Decision{}, err
	}
	picked, unaffordable, allZero := false, false, true
	for _, r := range book.Ranked {
		n := &d.Names[index[r.InstrumentID]]
		n.Status = r.Status
		n.Score = r.Score
		n.Volatility = r.Volatility
		n.ScoreRank = r.ScoreRank
		n.Weight = r.Weight
		n.BacktestWouldPick = r.BacktestWouldPick
		n.OneShare, n.TargetValue = r.OneShare, r.TargetValue
		if r.Status == StatusPicked {
			picked = true
			allZero = allZero && r.Weight == 0
		}
		unaffordable = unaffordable || r.Status == StatusUnaffordable
	}
	// A book with no pick is withheld and the last book kept (ADR 0026,
	// point 5). When D7-L skipped a name the reason is all-unaffordable: a
	// systematic valuation error, a bad net liquidation or bad raw closes,
	// looks the same as an account too small for every pick, so the owner
	// is shown the valuation rather than every stock being sold on it
	// (ADR 0026, point 2, "D7-L").
	if !picked {
		if !unaffordable {
			return withhold(d, ReasonNoEligibleNames), nil
		}
		d = withhold(d, ReasonAllUnaffordable)
		d.Detail = allUnaffordableDetail(d.Names, in.MinimumLot)
		return d, nil
	}
	d.Outcome = OutcomeDecided
	d.Targets = book.Targets
	// Every pick's weight is zero only by design, in a risk-off month at
	// RiskOffFraction 1: the whole of Invested is in RiskOffInstrument and
	// the planner sells every stock held, as the risk-off rule asks.
	if allZero {
		d.Detail = fmt.Sprintf("every pick's target weight is zero (risk-off at risk-off fraction %g): the whole invested fraction goes to %s and every stock held is sold",
			cfg.RiskOffFraction, cfg.RiskOffInstrument)
	}
	return d, nil
}

// allUnaffordableDetail counts the candidates D7-L valued and found
// unaffordable, and the candidates left out for another reason. With no pick
// every candidate is unaffordable, no sector or zero volatility, since a
// sector cap or the top-N limit needs a pick first, so only those two reasons
// can leave a name out here.
func allUnaffordableDetail(names []NameRecord, lot *MinimumLot) string {
	var valued, noSector, zeroVol int
	for _, n := range names {
		switch n.Status {
		case StatusUnaffordable:
			valued++
		case StatusNoSector:
			noSector++
		case StatusZeroVolatility:
			zeroVol++
		}
	}
	left := "none left out"
	if noSector+zeroVol > 0 {
		var reasons []string
		if noSector > 0 {
			reasons = append(reasons, fmt.Sprintf("%d no sector", noSector))
		}
		if zeroVol > 0 {
			reasons = append(reasons, fmt.Sprintf("%d zero volatility", zeroVol))
		}
		left = fmt.Sprintf("%d left out (%s)", noSector+zeroVol, strings.Join(reasons, ", "))
	}
	candidates := "candidates"
	if valued == 1 {
		candidates = "candidate"
	}
	return fmt.Sprintf("%d %s valued, every one unaffordable (one share at its previous raw close exceeds its target value: "+
		"weight x momentum scale %g x net liquidation %.2f); %s; the book is kept: check the net liquidation and the raw closes",
		valued, candidates, lot.Scale, lot.NetLiquidation, left)
}

func withhold(d Decision, reason WithheldReason) Decision {
	d.Outcome = OutcomeWithheld
	d.Reason = reason
	return d
}
