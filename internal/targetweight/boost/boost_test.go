package boost_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/boost"
)

func params() boost.Parameters {
	return boost.Parameters{InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 10, TrendDays: 200, ExitSMA: 5}
}

// ready returns readings with every indicator ready.
func ready(price, rsi, trend, exit float64) boost.Readings {
	return boost.Readings{Close: price, RSI: rsi, RSIReady: true, TrendMean: trend, TrendMeanReady: true, ExitMean: exit, ExitMeanReady: true}
}

func TestEntryNeedsRSIStrictlyBelowTheThreshold(t *testing.T) {
	p := params()
	if p.Next(false, ready(110, 10, 100, 112)) {
		t.Error("RSI exactly 10 must not enter: the rule is RSI < 10 (template.py:91)")
	}
	if !p.Next(false, ready(110, 9.999999, 100, 112)) {
		t.Error("RSI just below 10 with the close above the 200-day mean must enter")
	}
}

func TestEntryIsBlockedAtOrUnderTheTrendMean(t *testing.T) {
	p := params()
	if p.Next(false, ready(99, 3, 100, 101)) {
		t.Error("a close under the 200-day mean must block entry")
	}
	if p.Next(false, ready(100, 3, 100, 101)) {
		t.Error("a close exactly at the 200-day mean must block entry: the rule is close > mean (template.py:90)")
	}
}

func TestEntryNeedsReadyIndicators(t *testing.T) {
	p := params()
	r := ready(110, 3, 100, 112)
	r.TrendMeanReady = false
	if p.Next(false, r) {
		t.Error("a trend mean not yet ready must block entry (template.py:90)")
	}
	r = ready(110, 3, 100, 112)
	r.RSIReady = false
	if p.Next(false, r) {
		t.Error("an RSI not yet ready must block entry (template.py:91)")
	}
}

func TestExitNeedsACloseStrictlyAboveTheExitMean(t *testing.T) {
	p := params()
	if !p.Next(true, ready(105, 50, 100, 105)) {
		t.Error("a close exactly at the 5-day mean must not exit: the rule is close > mean (template.py:93)")
	}
	if p.Next(true, ready(105.000001, 50, 100, 105)) {
		t.Error("a close above the 5-day mean must exit")
	}
}

func TestLatchedBoostSurvivesACloseBelowTheTrendMean(t *testing.T) {
	if !params().Next(true, ready(90, 40, 100, 95)) {
		t.Error("the boost is latched: only the exit mean ends it, never the trend mean (template.py:89-94)")
	}
}

func TestTargets(t *testing.T) {
	p := params()
	if got := p.Target(false); got != 0.98 {
		t.Errorf("Target(false) = %v, want 0.98", got)
	}
	if got := p.Target(true); math.Abs(got-1.47) > 1e-12 {
		t.Errorf("Target(true) = %v, want 1.47", got)
	}
}

func TestFirstDecisionFromCashIsNinetyEightPercentWithNoBoost(t *testing.T) {
	p := params()
	ind, err := boost.NewIndicators(p)
	if err != nil {
		t.Fatal(err)
	}
	r := ind.Add(100)
	if r.RSIReady || r.TrendMeanReady || r.ExitMeanReady {
		t.Fatalf("one close readies nothing: %+v", r)
	}
	latched := p.Next(false, r)
	if latched || p.Target(latched) != 0.98 {
		t.Errorf("first decision: latched %v target %v, want false 0.98", latched, p.Target(latched))
	}
}

func TestRSIIsWilderSmoothed(t *testing.T) {
	ind, err := boost.NewIndicators(params())
	if err != nil {
		t.Fatal(err)
	}
	// Changes +1, -0.5, +1.5. Gain and loss averages: seeded by the mean of
	// the first two changes (0.5, 0.25), then halved toward each new change
	// (1.0, 0.125).
	ind.Add(10)
	r := ind.Add(11)
	if r.RSIReady {
		t.Fatal("RSI(2) needs three closes")
	}
	r = ind.Add(10.5)
	if !r.RSIReady || math.Abs(r.RSI-200.0/3) > 1e-12 {
		t.Fatalf("RSI after three closes = %v (ready %v), want 66.67", r.RSI, r.RSIReady)
	}
	r = ind.Add(12)
	if math.Abs(r.RSI-800.0/9) > 1e-12 {
		t.Fatalf("RSI after four closes = %v, want 88.89", r.RSI)
	}
}

func TestRSIWithNoLossIsOneHundred(t *testing.T) {
	ind, _ := boost.NewIndicators(params())
	for _, c := range []float64{10, 11, 12, 12} {
		ind.Add(c)
	}
	if r := ind.Add(13); r.RSI != 100 {
		t.Errorf("RSI with no losses = %v, want 100", r.RSI)
	}
}

func TestMeansIncludeTheCloseAndWaitForTheirLength(t *testing.T) {
	p := params()
	p.TrendDays = 3
	p.ExitSMA = 2
	ind, _ := boost.NewIndicators(p)
	ind.Add(10)
	r := ind.Add(20)
	if !r.ExitMeanReady || r.ExitMean != 15 || r.TrendMeanReady {
		t.Fatalf("after two closes: %+v", r)
	}
	r = ind.Add(30)
	if !r.TrendMeanReady || r.TrendMean != 20 || r.ExitMean != 25 {
		t.Fatalf("after three closes: %+v", r)
	}
	r = ind.Add(40)
	if r.TrendMean != 30 || r.ExitMean != 35 || r.Close != 40 {
		t.Fatalf("after four closes: %+v", r)
	}
}

func TestNewIndicatorsRefusesBadLengths(t *testing.T) {
	for _, mutate := range []func(*boost.Parameters){
		func(p *boost.Parameters) { p.RSIPeriod = 0 },
		func(p *boost.Parameters) { p.TrendDays = 0 },
		func(p *boost.Parameters) { p.ExitSMA = -1 },
	} {
		p := params()
		mutate(&p)
		if _, err := boost.NewIndicators(p); err == nil {
			t.Errorf("NewIndicators accepted %+v", p)
		}
	}
}
