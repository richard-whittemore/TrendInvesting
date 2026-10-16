package momentum_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight/momentum"
)

func TestScoreNeeds253ClosesNot252(t *testing.T) {
	cfg := momentum.FCombo()
	cal := calendar(300)

	short, err := momentum.Score(cfg, cal, series(cal, alternating(252)))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "status at 252", short.Status, momentum.StatusShortHistory)
	mustEqual(t, "closes at 252", short.Closes, 252)

	full, err := momentum.Score(cfg, cal, series(cal, alternating(253)))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "status at 253", full.Status, momentum.StatusScored)
	mustEqual(t, "closes at 253", full.Closes, 253)
	// px[-22] / px[-253] - 1: index 231 (200) over index 0 (100).
	mustEqual(t, "score", full.Score, 1.0)
	// Returns +1 and -0.5 in turn: population std exactly 0.75.
	mustEqual(t, "volatility", full.Volatility, 0.75)
}

func TestScoreLeavesOutANameWithAGapInItsWindow(t *testing.T) {
	cfg := momentum.FCombo()
	cal := calendar(300)
	for _, missing := range []int{299, 260, 47} { // the last close, mid-window, the window's first close
		closes := series(cal, alternating(300))
		delete(closes, cal[missing])
		got, err := momentum.Score(cfg, cal, closes)
		if err != nil {
			t.Fatal(err)
		}
		mustEqual(t, "status", got.Status, momentum.StatusGap)
		mustEqual(t, "closes", got.Closes, 299)
	}
	// A gap before the window does not matter.
	closes := series(cal, alternating(300))
	delete(closes, cal[46])
	got, err := momentum.Score(cfg, cal, closes)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "status, gap before window", got.Status, momentum.StatusScored)
}

func TestScoreMarksZeroVolatility(t *testing.T) {
	cal := calendar(253)
	got, err := momentum.Score(momentum.FCombo(), cal, series(cal, constant(253, 42)))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "status", got.Status, momentum.StatusZeroVolatility)
	mustEqual(t, "volatility", got.Volatility, 0.0)
}

func TestScoreRefusesMalformedCloses(t *testing.T) {
	cal := calendar(10)
	cases := map[string]map[string]float64{
		"zero close":     {cal[3]: 0},
		"NaN close":      {cal[3]: math.NaN()},
		"off calendar":   {"2001-01-06": 10}, // a Saturday inside the calendar
		"after the last": {"2030-01-02": 10},
		"not a date":     {"soon": 10},
	}
	for name, closes := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := momentum.Score(momentum.FCombo(), cal, closes); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	backwards := []string{cal[1], cal[0]}
	if _, err := momentum.Score(momentum.FCombo(), backwards, nil); err == nil {
		t.Fatal("Score, unordered calendar: want an error")
	}
	if _, _, err := momentum.EvaluateTrend(momentum.FCombo(), backwards, nil); err == nil {
		t.Fatal("EvaluateTrend, unordered calendar: want an error")
	}
}

func TestTrendRiskOffOnlyStrictlyBelowTheMeanIncludingTheLastClose(t *testing.T) {
	cfg := momentum.FCombo()
	cal := calendar(250)

	// 199 closes of 100 and a last close of 100: the mean is 100, not below.
	at, reason, err := momentum.EvaluateTrend(cfg, cal, series(cal, constant(250, 100)))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "reason at mean", reason, momentum.WithheldReason(""))
	mustEqual(t, "risk-off at mean", at.RiskOff, false)
	mustEqual(t, "mean", at.Mean, 100.0)

	// A last close of 98 against 199 of 100: the mean, that close included,
	// is 99.99, and 98 is below it.
	prices := constant(250, 100)
	prices[249] = 98
	below, _, err := momentum.EvaluateTrend(cfg, cal, series(cal, prices))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "risk-off below", below.RiskOff, true)
	near(t, "mean below", below.Mean, 99.99, 1e-9)
	mustEqual(t, "close", below.Close, 98.0)
	mustEqual(t, "closes", below.Closes, 250)

	// Only the last 200 count: an old crash outside the window is ignored.
	prices = constant(250, 100)
	prices[10] = 1
	old, _, err := momentum.EvaluateTrend(cfg, cal, series(cal, prices))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "mean ignores old closes", old.Mean, 100.0)
}

func TestTrendWithheldOnShortOrGappedSPY(t *testing.T) {
	cfg := momentum.FCombo()
	cal := calendar(250)

	_, reason, err := momentum.EvaluateTrend(cfg, cal, series(cal, constant(199, 100)))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "199 closes", reason, momentum.ReasonInsufficientTrendHistory)

	_, reason, err = momentum.EvaluateTrend(cfg, cal, series(cal, constant(200, 100)))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "200 closes", reason, momentum.WithheldReason(""))

	gapped := series(cal, constant(250, 100))
	delete(gapped, cal[249])
	_, reason, err = momentum.EvaluateTrend(cfg, cal, gapped)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "missing last close", reason, momentum.ReasonMissingBars)
}
