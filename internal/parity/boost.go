package parity

import (
	"errors"
	"fmt"
	"math"
	"time"

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

// BoostPlan is the session range of a Boost run, recorded before the run:
// its first and last session and how many there are. A complete export
// covers exactly it.
type BoostPlan struct {
	First    string
	Last     string
	Sessions int
}

// BacktestBoost returns the parameters BX_SPY ran with: SPY at 98%, 1.5x
// while RSI(2) is under 10 above the 200-day mean, until a close above the
// 5-day mean (research/edge-search/boost/template.py:33-37, 96).
func BacktestBoost() boost.Parameters {
	return boost.Parameters{InvestedFraction: 0.98, BoostSize: 1.5, RSIEntry: 10, RSIPeriod: 2, TrendDays: 200, ExitSMA: 5}
}

// CompareBoost rebuilds Boost's indicators from the logged closes, from the
// first, and runs the rule from the first session after warm-up, unlatched,
// on its own readings. It checks every session's RSI(2), both means and
// their readiness, and the state and target, all exactly (ADR 0026 point
// 7, item 2: "Boost's state must match day by day").
//
// The export must start and end on the plan's first and last session and
// hold exactly its session count, and the plan must make sense
// (BoostPlan.Validate); otherwise the export check fails and no session is
// compared.
//
// The state is carried on Go's own readings, never reset to QuantConnect's,
// so a difference on one session shows on every session it affects.
func CompareBoost(p boost.Parameters, run string, plan BoostPlan, sessions []BoostSession) Report {
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
	if err := wellFormedBoost(plan, sessions); err != nil {
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

// wellFormedBoost refuses an export that is out of order or malformed, or
// that does not cover the planned range: the plan was recorded before the
// run, so an export that stops short of it, starts after it or holds
// another number of sessions is not the run that was planned.
func wellFormedBoost(plan BoostPlan, sessions []BoostSession) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if len(sessions) > 0 {
		switch first, last := sessions[0].Date, sessions[len(sessions)-1].Date; {
		case first != plan.First:
			return fmt.Errorf("export starts at %s, planned first session %s", first, plan.First)
		case last != plan.Last:
			return fmt.Errorf("export ends at %s, planned last session %s", last, plan.Last)
		case len(sessions) != plan.Sessions:
			return fmt.Errorf("export holds %d sessions, planned %d", len(sessions), plan.Sessions)
		}
	}
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

// boostPlanFile is the plan file's JSON: all three fields are required.
type boostPlanFile struct {
	First    *string `json:"first"`
	Last     *string `json:"last"`
	Sessions *int    `json:"sessions"`
}

// DecodeBoostPlan reads a plan file, {"first":"YYYY-MM-DD","last":
// "YYYY-MM-DD","sessions":N}, refusing an unknown or missing field, trailing
// data and a plan that Validate refuses. The file is committed before the
// run, so the plan cannot be fitted to the export.
func DecodeBoostPlan(raw []byte) (BoostPlan, error) {
	var f boostPlanFile
	if err := strictDecode(raw, &f); err != nil {
		return BoostPlan{}, err
	}
	if f.First == nil || f.Last == nil || f.Sessions == nil {
		return BoostPlan{}, errors.New("parity: plan needs first, last and sessions")
	}
	p := BoostPlan{First: *f.First, Last: *f.Last, Sessions: *f.Sessions}
	return p, p.Validate()
}

// Validate refuses a plan that cannot describe a run: a first or last that
// is not a date, a first that is not before the last, no sessions, or more
// sessions than there are weekdays from the first to the last, the most any
// exchange calendar holds.
func (p BoostPlan) Validate() error {
	first, err := time.Parse(time.DateOnly, p.First)
	if err != nil {
		return fmt.Errorf("planned first session %q: %w", p.First, err)
	}
	last, err := time.Parse(time.DateOnly, p.Last)
	if err != nil {
		return fmt.Errorf("planned last session %q: %w", p.Last, err)
	}
	if !first.Before(last) {
		return fmt.Errorf("planned first session %s is not before the last, %s", p.First, p.Last)
	}
	if p.Sessions <= 0 {
		return fmt.Errorf("planned %d sessions", p.Sessions)
	}
	if most := weekdays(first, last); p.Sessions > most {
		return fmt.Errorf("planned %d sessions, but only %d weekdays from %s to %s", p.Sessions, most, p.First, p.Last)
	}
	return nil
}

// weekdays counts the Monday-to-Friday days from first to last, both
// included.
func weekdays(first, last time.Time) int {
	days := int(last.Sub(first).Hours()/24) + 1
	n := days / 7 * 5
	for d := first.AddDate(0, 0, days/7*7); !d.After(last); d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			n++
		}
	}
	return n
}
