package parity_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
)

// planOf is the session range recorded before a run: what a complete
// export covers.
func planOf(s []parity.BoostSession) parity.BoostPlan {
	return parity.BoostPlan{First: s[0].Date, Last: s[len(s)-1].Date, Sessions: len(s)}
}

func compareBoost(s []parity.BoostSession) parity.Report {
	return parity.CompareBoost(boostParams(), "BSYN", planOf(syntheticBoost(400, 210)), s)
}

func TestMatchingBoostExportPassesEveryCheck(t *testing.T) {
	s := syntheticBoost(400, 210)
	latched := 0
	for _, x := range s {
		if x.Latched {
			latched++
		}
	}
	if latched == 0 {
		t.Fatal("fixture: no session is boosted, so the state check would prove nothing")
	}
	r := compareBoost(s)
	if !r.Pass() {
		t.Fatalf("a matching export must pass: %+v", r.Checks)
	}
	if c := checkNamed(r, parity.CheckBoostState); c.Compared != 190 {
		t.Errorf("state compared %d sessions, want the 190 after warm-up", c.Compared)
	}
	if c := checkNamed(r, parity.CheckBoostRSI); c.Compared != 400 {
		t.Errorf("RSI compared %d sessions, want every one of 400, warm-up included", c.Compared)
	}
	if c := checkNamed(r, parity.CheckBoostSMA); c.Compared != 800 {
		t.Errorf("SMA compared %d readings, want both means on 400 sessions", c.Compared)
	}
}

func boostFailsOnly(t *testing.T, s []parity.BoostSession, name string) parity.Check {
	t.Helper()
	r := compareBoost(s)
	if r.Pass() {
		t.Fatalf("an export altered for %s must fail", name)
	}
	for _, c := range r.Checks {
		if c.Pass() == (c.Name == name) {
			t.Errorf("%s: pass=%v (%+v)", c.Name, c.Pass(), c)
		}
	}
	return checkNamed(r, name)
}

// Boost's RSI and means must match within RelativeTolerance on every
// session, readiness exactly: the owner's decision of 2026-10-18, amending
// ADR 0026 point 7, item 2, which had required them bit for bit
// (docs/research/parity-2026-10.md, "Thresholds").

func TestAnRSIOneBitOffPasses(t *testing.T) {
	s := syntheticBoost(400, 210)
	v := math.Nextafter(*s[300].RSI, math.Inf(1))
	s[300].RSI = &v
	r := compareBoost(s)
	if c := checkNamed(r, parity.CheckBoostRSI); !c.Pass() || c.MaxDiff == 0 || c.MaxDiff > parity.RelativeTolerance {
		t.Errorf("one session off by one bit is within the tolerance and reported: %+v", c)
	}
	if !r.Pass() {
		t.Errorf("checks %+v", r.Checks)
	}
}

func TestAnRSIOffByMoreThanTheToleranceFails(t *testing.T) {
	s := syntheticBoost(400, 210)
	v := *s[300].RSI * (1 + 2*parity.RelativeTolerance)
	s[300].RSI = &v
	if c := boostFailsOnly(t, s, parity.CheckBoostRSI); c.Failures != 1 || c.MaxDiff <= parity.RelativeTolerance {
		t.Errorf("one session off by twice the tolerance: %+v", c)
	}
}

func TestBoostIndicatorThresholdsAreRelative(t *testing.T) {
	r := compareBoost(syntheticBoost(400, 210))
	want := fmt.Sprintf("rel <= %g, readiness exact", parity.RelativeTolerance)
	for _, name := range []string{parity.CheckBoostRSI, parity.CheckBoostSMA} {
		if c := checkNamed(r, name); c.Threshold != want {
			t.Errorf("%s threshold %q, want %q", name, c.Threshold, want)
		}
	}
}

func TestAMeanOneBitOffPasses(t *testing.T) {
	s := syntheticBoost(400, 210)
	v := math.Nextafter(*s[250].TrendMean, 0)
	s[250].TrendMean = &v
	w := math.Nextafter(*s[250].ExitMean, math.Inf(1))
	s[250].ExitMean = &w
	if c := checkNamed(compareBoost(s), parity.CheckBoostSMA); !c.Pass() || c.MaxDiff == 0 {
		t.Errorf("means off by one bit are within the tolerance: %+v", c)
	}
}

func TestAnRSIReadyEarlierThanGoFails(t *testing.T) {
	s := syntheticBoost(400, 210)
	v := 50.0
	s[1].RSI = &v // RSI(2) needs three closes
	boostFailsOnly(t, s, parity.CheckBoostRSI)
}

func TestATrendMeanOffByMoreThanTheToleranceFails(t *testing.T) {
	s := syntheticBoost(400, 210)
	v := *s[250].TrendMean * (1 - 2*parity.RelativeTolerance)
	s[250].TrendMean = &v
	boostFailsOnly(t, s, parity.CheckBoostSMA)
}

func TestAnExitMeanMissingFails(t *testing.T) {
	s := syntheticBoost(400, 210)
	s[250].ExitMean = nil
	boostFailsOnly(t, s, parity.CheckBoostSMA)
}

func TestAFlippedStateFails(t *testing.T) {
	s := syntheticBoost(400, 210)
	for i := 210; i < len(s); i++ {
		if s[i].Latched {
			s[i].Latched = false
			s[i].Target = 0.98
			break
		}
	}
	boostFailsOnly(t, s, parity.CheckBoostState)
}

func TestATargetNotTheStatesFails(t *testing.T) {
	s := syntheticBoost(400, 210)
	s[260].Target += 1e-9
	boostFailsOnly(t, s, parity.CheckBoostState)
}

func TestSessionsOutOfOrderOrAWarmUpAfterADecisionFail(t *testing.T) {
	for name, alter := range map[string]func([]parity.BoostSession){
		"sessions out of order": func(s []parity.BoostSession) { s[5].Date, s[6].Date = s[6].Date, s[5].Date },
		"a warm-up after a decision": func(s []parity.BoostSession) {
			s[300].Warmup = true
		},
		"a non-positive close": func(s []parity.BoostSession) { s[3].Close = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			s := syntheticBoost(400, 210)
			alter(s)
			r := compareBoost(s)
			if r.Pass() || checkNamed(r, parity.CheckExport).Pass() {
				t.Errorf("%s must fail the export check", name)
			}
		})
	}
}

// TestAnRSIOutsideItsRangeFailsTheExport: RSI lies in [0, 100], so an
// exported RSI outside it, or not finite, is a malformed export, refused
// before the tolerance applies: a value just above 100 is within 1e-9 of a
// Go RSI of 100 and would otherwise pass.
func TestAnRSIOutsideItsRangeFailsTheExport(t *testing.T) {
	for _, bad := range []float64{100.5, 100 * (1 + 1e-10), -0.5, math.NaN(), math.Inf(1)} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			s := syntheticBoost(400, 210)
			v := bad
			s[300].RSI = &v
			if c := checkNamed(compareBoost(s), parity.CheckExport); c.Pass() || !strings.Contains(c.First, "RSI") {
				t.Errorf("an RSI of %v must fail the export check, naming the RSI: %+v", bad, c)
			}
		})
	}
}

func TestAnEmptyBoostExportFails(t *testing.T) {
	if compareBoost(nil).Pass() {
		t.Error("an export with no session compares nothing and must fail")
	}
}

func TestBoostParametersWithNoIndicatorLengthFail(t *testing.T) {
	p := boostParams()
	p.RSIPeriod = 0
	if r := parity.CompareBoost(p, "BSYN", planOf(syntheticBoost(400, 210)), syntheticBoost(400, 210)); r.Pass() || checkNamed(r, parity.CheckExport).Pass() {
		t.Error("parameters the indicators refuse must fail the export check")
	}
}

func TestTheBacktestBoostParametersAreTemplatePy(t *testing.T) {
	// research/edge-search/boost/template.py:33-37 and 96 (98% invested).
	if got := parity.BacktestBoost(); got != boostParams() {
		t.Errorf("BacktestBoost() = %+v", got)
	}
}

// An export that stops short of the planned range, or starts late, or holds
// another number of sessions, is not the run that was planned: every check
// on what it does hold would pass (the plan is recorded before the run).

func TestABoostExportMissingItsFinalChunkFails(t *testing.T) {
	full := syntheticBoost(400, 210)
	plan := planOf(full)
	r := parity.CompareBoost(boostParams(), "BSYN", plan, full[:300])
	if r.Pass() {
		t.Fatal("an export that ends before the planned last session must fail")
	}
	c := checkNamed(r, parity.CheckExport)
	if c.Pass() || !strings.Contains(c.First, plan.Last) {
		t.Errorf("export check %+v must fail naming the planned last session %s", c, plan.Last)
	}
}

func TestABoostExportThatStartsLateOrCountsOtherwiseFails(t *testing.T) {
	full := syntheticBoost(400, 210)
	for name, c := range map[string]struct {
		plan parity.BoostPlan
		got  []parity.BoostSession
	}{
		"a missing first chunk": {planOf(full), full[100:]},
		"another session count": {parity.BoostPlan{First: full[0].Date, Last: full[399].Date, Sessions: 399}, full},
		"a plan with no range":  {parity.BoostPlan{}, full},
		"a plan with no count":  {parity.BoostPlan{First: full[0].Date, Last: full[399].Date}, full},
	} {
		t.Run(name, func(t *testing.T) {
			if r := parity.CompareBoost(boostParams(), "BSYN", c.plan, c.got); r.Pass() || checkNamed(r, parity.CheckExport).Pass() {
				t.Errorf("%s must fail the export check", name)
			}
		})
	}
}

// The plan is read from a file recorded before the run, and refused when it
// cannot describe a run: a typed flag could say anything.

func TestAPlanFileDecodesAndAMakesSense(t *testing.T) {
	p, err := parity.DecodeBoostPlan([]byte(`{"first":"1998-01-02","last":"2026-06-30","sessions":7166}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := (parity.BoostPlan{First: "1998-01-02", Last: "2026-06-30", Sessions: 7166}); p != want {
		t.Errorf("plan %+v, want %+v", p, want)
	}
}

func TestAPlanThatCannotDescribeARunIsRefused(t *testing.T) {
	for name, raw := range map[string]string{
		"first after last":            `{"first":"2026-06-30","last":"1998-01-02","sessions":10}`,
		"first equal to last":         `{"first":"1998-01-02","last":"1998-01-02","sessions":1}`,
		"no sessions":                 `{"first":"1998-01-02","last":"1998-01-09","sessions":0}`,
		"negative sessions":           `{"first":"1998-01-02","last":"1998-01-09","sessions":-3}`,
		"more sessions than weekdays": `{"first":"1998-01-02","last":"1998-01-09","sessions":7}`,
		"a first that is not a date":  `{"first":"1998-02-30","last":"1998-03-09","sessions":3}`,
		"a last that is not a date":   `{"first":"1998-02-02","last":"1998-03-32","sessions":3}`,
		"a missing first":             `{"last":"1998-01-09","sessions":3}`,
		"a missing count":             `{"first":"1998-01-02","last":"1998-01-09"}`,
		"an unknown field":            `{"first":"1998-01-02","last":"1998-01-09","sessions":3,"note":"x"}`,
		"trailing data":               `{"first":"1998-01-02","last":"1998-01-09","sessions":3} {}`,
		"not JSON":                    `{`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parity.DecodeBoostPlan([]byte(raw)); err == nil {
				t.Errorf("%s must be refused", name)
			}
		})
	}
}

func TestAPlanWithNoSenseFailsTheExportCheckWhateverTheExportHolds(t *testing.T) {
	full := syntheticBoost(400, 210)
	for name, plan := range map[string]parity.BoostPlan{
		"first after last": {First: full[399].Date, Last: full[0].Date, Sessions: 400},
		"too many":         {First: full[0].Date, Last: full[399].Date, Sessions: 10000},
	} {
		t.Run(name, func(t *testing.T) {
			if r := parity.CompareBoost(boostParams(), "BSYN", plan, full); checkNamed(r, parity.CheckExport).Pass() {
				t.Errorf("%s must fail the export check", name)
			}
		})
	}
}
