package report

import (
	"math"
	"strings"
	"testing"
)

// A benchmark close that is not positive cannot be a link of the growth
// index: it is left out, as the pairing filter leaves it out of the points,
// and the sentence says so.
func TestANonPositiveBenchmarkCloseIsLeftOutOfTheIndex(t *testing.T) {
	bars := map[string]benchBar{
		"2023-12-29": {split: 90}, // before the comparison: not in it
		"2024-01-02": {split: 100},
		"2024-01-03": {split: 0},
		"2024-01-04": {split: 110},
	}
	points := []equityPoint{{date: "2024-01-02", net: 1000, benchmark: 100}, {date: "2024-01-04", net: 1100, benchmark: 110}}
	got := equityOf(points, bars, CashLedger{}, "SPY", "2024-01-04")
	if !(math.Abs(got.BenchmarkReturn-0.1) < 1e-9) || !(math.Abs(got.ShadowValue-1100) < 1e-9) {
		t.Fatalf("SPY %v shadow %v: the session without a positive close must be skipped", got.BenchmarkReturn, got.ShadowValue)
	}
	if !strings.Contains(got.Assumption, "SPY close on 2024-01-03 is not positive: that session is left out") {
		t.Fatalf("assumption %q", got.Assumption)
	}
}
