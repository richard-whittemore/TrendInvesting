package parity_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/parity"
)

func compareBoost(s []parity.BoostSession) parity.Report {
	return parity.CompareBoost(boostParams(), "BSYN", s)
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

// Boost's RSI and means must match exactly on every session (ADR 0026
// point 7, item 2; docs/research/parity-2026-10.md, "Thresholds").

func TestAnRSIOneBitOffFails(t *testing.T) {
	s := syntheticBoost(400, 210)
	v := math.Nextafter(*s[300].RSI, math.Inf(1))
	s[300].RSI = &v
	if c := boostFailsOnly(t, s, parity.CheckBoostRSI); c.Failures != 1 || c.MaxDiff == 0 {
		t.Errorf("one session off by one bit: %+v", c)
	}
}

func TestAnRSIReadyEarlierThanGoFails(t *testing.T) {
	s := syntheticBoost(400, 210)
	v := 50.0
	s[1].RSI = &v // RSI(2) needs three closes
	boostFailsOnly(t, s, parity.CheckBoostRSI)
}

func TestATrendMeanOneBitOffFails(t *testing.T) {
	s := syntheticBoost(400, 210)
	v := math.Nextafter(*s[250].TrendMean, 0)
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

func TestAnEmptyBoostExportFails(t *testing.T) {
	if compareBoost(nil).Pass() {
		t.Error("an export with no session compares nothing and must fail")
	}
}

func TestBoostParametersWithNoIndicatorLengthFail(t *testing.T) {
	p := boostParams()
	p.RSIPeriod = 0
	if r := parity.CompareBoost(p, "BSYN", syntheticBoost(400, 210)); r.Pass() || checkNamed(r, parity.CheckExport).Pass() {
		t.Error("parameters the indicators refuse must fail the export check")
	}
}

func TestTheBacktestBoostParametersAreTemplatePy(t *testing.T) {
	// research/edge-search/boost/template.py:33-37 and 96 (98% invested).
	if got := parity.BacktestBoost(); got != boostParams() {
		t.Errorf("BacktestBoost() = %+v", got)
	}
}
