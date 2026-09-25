package fills_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/fills"
)

// stopLimitSharedCases is the table of ADR 0005's stop-limit buy, as amended
// 2026-09-24, that two implementations answer: ExecuteStopLimit here, and the
// LEAN adapter's own fill model (adapter/lean/orders.py,
// stop_limit_buy_fill_price), which reproduces the rule inside LEAN. The
// adapter's test suite reads the same file, so a change to either
// implementation that the other does not share fails one of the two suites.
const stopLimitSharedCases = "../../adapter/lean/tests/testdata/stop_limit_fill_cases.json"

type stopLimitSharedCase struct {
	Name      string   `json:"name"`
	Bar       string   `json:"bar"`
	Open      float64  `json:"open"`
	High      float64  `json:"high"`
	Low       float64  `json:"low"`
	Close     float64  `json:"close"`
	Level     float64  `json:"level"`
	PriceCap  float64  `json:"price_cap"`
	N         float64  `json:"n"`
	SlippageN float64  `json:"slippage_n"`
	Filled    bool     `json:"filled"`
	Price     *float64 `json:"price"`
}

// TestExecuteStopLimitAnswersTheSharedCases evaluates every shared case
// against ExecuteStopLimit: an order resting at the open (Reference = open),
// slipped by slippage_n x N (ADR 0013), fills or does not exactly as the
// table states, at its price to within the table's tolerance.
func TestExecuteStopLimitAnswersTheSharedCases(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.FromSlash(stopLimitSharedCases))
	if err != nil {
		t.Fatalf("reading the shared cases: %v", err)
	}
	var table struct {
		Tolerance float64               `json:"tolerance"`
		Cases     []stopLimitSharedCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("decoding the shared cases: %v", err)
	}
	if table.Tolerance <= 0 || table.Tolerance > 1e-9 {
		t.Fatalf("shared-case tolerance %v must be positive and at most 1e-9", table.Tolerance)
	}
	if len(table.Cases) == 0 {
		t.Fatal("the shared-case table is empty")
	}
	for _, c := range table.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.Filled != (c.Price != nil) {
				t.Fatalf("case states filled %v with price %v; a fill needs a price and a non-fill none", c.Filled, c.Price)
			}
			got, err := fills.ExecuteStopLimit(c.Level, c.PriceCap,
				fills.Range{Reference: c.Open, High: c.High, Low: c.Low}, c.SlippageN*c.N)
			if err != nil {
				t.Fatalf("ExecuteStopLimit() error = %v", err)
			}
			if got.Filled != c.Filled {
				t.Fatalf("ExecuteStopLimit() filled = %v, the shared case says %v", got.Filled, c.Filled)
			}
			if c.Filled && math.Abs(got.Price-*c.Price) > table.Tolerance {
				t.Fatalf("ExecuteStopLimit() price = %.12f, the shared case says %.12f", got.Price, *c.Price)
			}
		})
	}
}
