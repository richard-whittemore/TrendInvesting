// Package momentum is the IRAs' momentum combination rule: from one
// session's universe snapshot and the adjusted closes up to that session,
// it decides the target weight of each instrument, and records why
// (ADR 0026 point 1; docs/live-trading/plan.md section 2, "Momentum,
// combination").
//
// The rule is research/edge-search/momentum/defensive_v2.py as the F_COMBO
// variant configures it (research/edge-search/build_variants.py:152-153).
// Where live trading departs from that file, the departure is ADR 0026's
// (points 5 and 8, plan.md section 4) and each function says so.
//
// The package is pure: plain inputs, plain outputs, no events, no clock.
// It shares nothing with internal/strategy (AGENTS.md rule 4).
package momentum

import (
	"errors"
	"fmt"
	"math"
)

// Config holds the rule's parameters (ADR 0026 point 6, "Strategy").
type Config struct {
	// ListingType is the listing type a universe row must carry: "CS",
	// common stock (plan.md section 4, D8 "As built").
	ListingType string
	// UniverseSize is how many rows, by dollar volume, join the universe
	// (defensive_v2.py:117,176).
	UniverseSize int
	// MinPrice is the unadjusted close a row needs, inclusive
	// (defensive_v2.py:174, c.Price >= 5.0).
	MinPrice float64
	// ScoreLookback is how many sessions back the score's base close is:
	// the score needs ScoreLookback+1 closes (defensive_v2.py:287-289).
	ScoreLookback int
	// ScoreSkip is how many sessions back the score's end close is
	// (px.iloc[-22] is 21 sessions back, defensive_v2.py:289).
	ScoreSkip int
	// VolatilityReturns is how many daily returns the volatility measures,
	// ending at the last close (defensive_v2.py:290).
	VolatilityReturns int
	// TopN is how many names are picked (F_COMBO TOP_N 25).
	TopN int
	// SectorCap is the most picks one sector label may hold (F_COMBO
	// SECTOR_CAP 3).
	SectorCap int
	// Invested is the book's total weight (defensive_book's invested=0.98).
	Invested float64
	// TrendInstrument is the instrument whose closes decide risk-off (SPY).
	TrendInstrument string
	// TrendCloses is how many closes the trend mean covers, the last close
	// included (TREND_DAYS 200, defensive_v2.py:279-280).
	TrendCloses int
	// RiskOffInstrument receives the risk-off share (TREND_ASSET IEF).
	RiskOffInstrument string
	// RiskOffFraction is the share of the book moved to RiskOffInstrument
	// when risk-off (F_COMBO RISK_OFF_FRACTION 0.5).
	RiskOffFraction float64
}

// FCombo returns the F_COMBO configuration: mom_trend, TOP_N 25,
// SECTOR_CAP 3, INV_VOL, RISK_OFF_FRACTION 0.5 over defensive_v2.py's
// other defaults (build_variants.py:152-153; plan.md section 2).
func FCombo() Config {
	return Config{
		ListingType:       "CS",
		UniverseSize:      500,
		MinPrice:          5,
		ScoreLookback:     252,
		ScoreSkip:         21,
		VolatilityReturns: 252,
		TopN:              25,
		SectorCap:         3,
		Invested:          0.98,
		TrendInstrument:   "SPY",
		TrendCloses:       200,
		RiskOffInstrument: "IEF",
		RiskOffFraction:   0.5,
	}
}

// Validate reports the first parameter that makes the rule meaningless.
func (c Config) Validate() error {
	switch {
	case c.ListingType == "":
		return errors.New("momentum: empty listing type")
	case c.UniverseSize < 1:
		return fmt.Errorf("momentum: universe size %d < 1", c.UniverseSize)
	case !finite(c.MinPrice) || c.MinPrice < 0:
		return fmt.Errorf("momentum: price floor %v", c.MinPrice)
	case c.ScoreLookback < 1:
		return fmt.Errorf("momentum: score lookback %d < 1", c.ScoreLookback)
	case c.ScoreSkip < 0 || c.ScoreSkip >= c.ScoreLookback:
		return fmt.Errorf("momentum: score skip %d outside [0, %d)", c.ScoreSkip, c.ScoreLookback)
	case c.VolatilityReturns < 2 || c.VolatilityReturns > c.ScoreLookback:
		return fmt.Errorf("momentum: volatility returns %d outside [2, %d]", c.VolatilityReturns, c.ScoreLookback)
	case c.TopN < 1:
		return fmt.Errorf("momentum: top %d < 1", c.TopN)
	case c.SectorCap < 1:
		return fmt.Errorf("momentum: sector cap %d < 1", c.SectorCap)
	case !finite(c.Invested) || c.Invested <= 0 || c.Invested > 1:
		return fmt.Errorf("momentum: invested %v outside (0, 1]", c.Invested)
	case c.TrendInstrument == "" || c.RiskOffInstrument == "":
		return errors.New("momentum: empty trend or risk-off instrument")
	case c.TrendInstrument == c.RiskOffInstrument:
		return errors.New("momentum: trend and risk-off instrument are the same")
	case c.TrendCloses < 1:
		return fmt.Errorf("momentum: trend closes %d < 1", c.TrendCloses)
	case !finite(c.RiskOffFraction) || c.RiskOffFraction < 0 || c.RiskOffFraction > 1:
		return fmt.Errorf("momentum: risk-off fraction %v outside [0, 1]", c.RiskOffFraction)
	}
	return nil
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
