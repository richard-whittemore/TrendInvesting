package parity

import (
	"fmt"
	"math"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/boost"
)

// BoostSession is one session of a logged QuantConnect Boost run: the
// adjusted close its indicators were updated with, the indicators after it
// (nil while not ready), and, after warm-up, the latched state and target.
// Every bar the indicators saw is logged, warm-up included, so the Go
// indicators can be rebuilt from the same first close.
type BoostSession struct {
	Date      string
	Close     float64
	RSI       *float64
	TrendMean *float64
	ExitMean  *float64
	Warmup    bool
	Latched   bool
	Target    float64
}

// BacktestBoost returns the parameters BX_SPY ran with: SPY at 98%, 1.5x
// while RSI(2) is under 10 above the 200-day mean, until a close above the
// 5-day mean (research/edge-search/boost/template.py:33-37, 96).
func BacktestBoost() boost.Parameters {
	return boost.Parameters{InvestedFraction: 0.98, BoostSize: 1.5, RSIEntry: 10, RSIPeriod: 2, TrendDays: 200, ExitSMA: 5}
}

// CompareBoost rebuilds Boost's indicators from the logged closes, from the
// first, and runs the rule from the first session after warm-up, unlatched,
// on its own readings; it checks every session's RSI(2), both means and
// their readiness, and the state and target, all exactly (ADR 0026 point
// 7, item 2: "Boost's state must match day by day").
//
// The state is carried on Go's own readings, never reset to QuantConnect's,
// so a difference on one session shows on every session it affects.
func CompareBoost(p boost.Parameters, run string, sessions []BoostSession) Report {
	export := Check{Run: run, Name: CheckExport, Threshold: "well-formed"}
	rsi := Check{Run: run, Name: CheckBoostRSI, Threshold: "exact, readiness included"}
	sma := Check{Run: run, Name: CheckBoostSMA, Threshold: "exact, readiness included"}
	state := Check{Run: run, Name: CheckBoostState, Threshold: "latched state and target exact"}
	done := func() Report {
		return Report{Checks: []Check{export, rsi, sma, state, differencesCheck(run, nil)}}
	}
	ind, err := boost.NewIndicators(p)
	if err != nil {
		export.compare(false, 0, err.Error())
		return done()
	}
	if err := wellFormedBoost(sessions); err != nil {
		export.compare(false, 0, err.Error())
		return done()
	}
	if len(sessions) > 0 {
		export.compare(true, 0, "")
	}
	latched := false
	for _, s := range sessions {
		r := ind.Add(s.Close)
		reading(&rsi, s.Date+" RSI", r.RSI, r.RSIReady, s.RSI)
		reading(&sma, s.Date+" trend mean", r.TrendMean, r.TrendMeanReady, s.TrendMean)
		reading(&sma, s.Date+" exit mean", r.ExitMean, r.ExitMeanReady, s.ExitMean)
		if s.Warmup {
			continue
		}
		latched = p.Next(latched, r)
		target := p.Target(latched)
		d := math.Abs(target - s.Target)
		state.compare(latched == s.Latched && target == s.Target, d,
			fmt.Sprintf("%s: Go latched=%v target %v, QuantConnect latched=%v target %v", s.Date, latched, target, s.Latched, s.Target))
	}
	return done()
}

// reading compares one indicator: ready on the same sessions, and then
// bit for bit.
func reading(c *Check, where string, got float64, ready bool, want *float64) {
	switch {
	case want == nil:
		c.compare(!ready, 0, fmt.Sprintf("%s: Go ready with %v, QuantConnect not ready", where, got))
	case !ready:
		c.compare(false, 0, fmt.Sprintf("%s: Go not ready, QuantConnect %v", where, *want))
	default:
		d := math.Abs(got - *want)
		c.compare(got == *want, d, fmt.Sprintf("%s: Go %v, QuantConnect %v", where, got, *want))
	}
}

func wellFormedBoost(sessions []BoostSession) error {
	decided := false
	for i, s := range sessions {
		if i > 0 && s.Date <= sessions[i-1].Date {
			return fmt.Errorf("session %s does not follow %s", s.Date, sessions[i-1].Date)
		}
		if !(s.Close > 0) || math.IsInf(s.Close, 0) {
			return fmt.Errorf("%s: close %v", s.Date, s.Close)
		}
		if s.Warmup && decided {
			return fmt.Errorf("%s: a warm-up session after a decision", s.Date)
		}
		decided = decided || !s.Warmup
	}
	return nil
}
