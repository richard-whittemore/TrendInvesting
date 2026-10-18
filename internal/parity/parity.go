// Package parity compares the Go target-weight modules with QuantConnect's
// own runs: it feeds the modules what a logged QuantConnect run used (its
// universe's scores, its adjusted closes, SPY's closes) and reports, check
// by check, whether they decide the same thing (ADR 0026 point 7, item 2;
// docs/live-trading/plan.md section 6, item 2).
//
// The pass/fail specification is the parity gate's: ranked order and cap
// marks exact; weights within WeightTolerance given QuantConnect's own
// inputs; scores and volatility, recomputed from the closes, within
// RelativeTolerance; Boost's RSI, means and state exact on every session.
// A difference ADR 0026 point 8 lists is reported with its size and does
// not fail; any other difference fails. A failed result is recorded, never
// rewritten (AGENTS.md rule 6).
//
// The package is pure: it reads decoded exports and returns a Report.
package parity

import (
	"fmt"
	"math"
	"strings"
)

// The parity gate's thresholds (docs/research/parity-2026-10.md,
// "Thresholds").
const (
	// WeightTolerance bounds |Go weight - QuantConnect weight|.
	WeightTolerance = 1e-12
	// RelativeTolerance bounds |Go - QuantConnect| / max(|Go|, |QuantConnect|)
	// for scores, volatilities and the trend mean.
	RelativeTolerance = 1e-9
)

// The checks a Report carries, one row each per run.
const (
	CheckMonths      = "months"
	CheckExport      = "export"
	CheckRiskOff     = "risk-off"
	CheckRankedOrder = "ranked-order"
	CheckCapMarks    = "cap-marks"
	CheckWeights     = "weights"
	CheckScores      = "scores"
	CheckVolatility  = "volatility"
	CheckHistory     = "history"
	CheckBoostRSI    = "boost-rsi"
	CheckBoostSMA    = "boost-sma"
	CheckBoostState  = "boost-state"
	CheckDifferences = "differences"
)

// Kind classifies a difference between the backtest and the Go module.
type Kind string

// The backtest behaviours live trading does not copy, as ADR 0026 point 8
// lists them, then the kinds that are not on that list.
const (
	// KindShortData: the backtest kept the last book when no name could be
	// scored (defensive_v2.py:319-320); live withholds.
	KindShortData Kind = "short-data"
	// KindTrendHistory: SPY with fewer than 200 closes, which the backtest
	// treated as risk-on (defensive_v2.py:279); live withholds.
	KindTrendHistory Kind = "trend-history"
	// KindGap: a lookback window that shifted across a missing close
	// (defensive_v2.py:286); live leaves the name out or withholds.
	KindGap Kind = "gap"
	// KindTie: equal scores in another order (defensive_v2.py:323).
	KindTie Kind = "tie"
	// KindZeroVolatility: a zero volatility the backtest weighted as 1
	// (defensive_v2.py:76); live leaves the name out.
	KindZeroVolatility Kind = "zero-volatility"
	// KindRSISeed: RSI(2)'s dependence on its warm-up seed
	// (template.py:54-55).
	KindRSISeed Kind = "rsi-seed"

	// KindNoSector: a name with no sector label, which the backtest left
	// uncapped and live leaves out (plan.md section 4, "Sectors"). Not on
	// ADR 0026 point 8's list.
	KindNoSector Kind = "no-sector"
)

// Listed reports whether ADR 0026 point 8 lists the kind.
func (k Kind) Listed() bool {
	switch k {
	case KindShortData, KindTrendHistory, KindGap, KindTie, KindZeroVolatility, KindRSISeed:
		return true
	}
	return false
}

// Check is one row of the report: what was compared, against which
// threshold, how many comparisons failed and the largest difference seen.
type Check struct {
	Run       string
	Name      string
	Threshold string
	Compared  int
	Failures  int
	MaxDiff   float64
	// First describes the first failure, for the record.
	First string
	// MayBeEmpty marks a check that passes with nothing to compare (the
	// differences check with no difference at all).
	MayBeEmpty bool
}

// Pass reports whether the check held. A check that compared nothing
// proves nothing and fails, unless MayBeEmpty.
func (c Check) Pass() bool {
	return c.Failures == 0 && (c.Compared > 0 || c.MayBeEmpty)
}

func (c *Check) compare(ok bool, diff float64, what string) {
	c.Compared++
	if diff > c.MaxDiff || math.IsNaN(diff) {
		c.MaxDiff = diff
	}
	if !ok {
		c.fail(what)
	}
}

func (c *Check) fail(what string) {
	c.Failures++
	if c.First == "" {
		c.First = what
	}
}

// Difference is one place where the Go module and the backtest differ by a
// behaviour the comparison can name.
type Difference struct {
	Run   string
	Where string
	Kind  Kind
	Size  string
}

// Listed reports whether ADR 0026 point 8 lists the difference.
func (d Difference) Listed() bool { return d.Kind.Listed() }

// Report is the comparison's result for one or more runs.
type Report struct {
	Checks      []Check
	Differences []Difference
}

// Pass reports whether every check held.
func (r Report) Pass() bool {
	for _, c := range r.Checks {
		if !c.Pass() {
			return false
		}
	}
	return true
}

// Merge joins reports in order.
func Merge(reports ...Report) Report {
	var out Report
	for _, r := range reports {
		out.Checks = append(out.Checks, r.Checks...)
		out.Differences = append(out.Differences, r.Differences...)
	}
	return out
}

// Markdown renders the report: the checks table, the overall result, the
// first failure of each failing check, and the differences table.
func (r Report) Markdown() string {
	var b strings.Builder
	b.WriteString("| Run | Check | Threshold | Compared | Failures | Max difference | Result |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	var failing []Check
	for _, c := range r.Checks {
		result := "PASS"
		if !c.Pass() {
			result = "FAIL"
			failing = append(failing, c)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %s | %s |\n",
			c.Run, c.Name, c.Threshold, c.Compared, c.Failures, formatDiff(c.MaxDiff), result)
	}
	overall := "PASS"
	if !r.Pass() {
		overall = "FAIL"
	}
	fmt.Fprintf(&b, "\n**Overall: %s**\n\n", overall)
	if len(failing) > 0 {
		b.WriteString("First failure per failing check:\n\n")
		for _, c := range failing {
			first := c.First
			if first == "" {
				first = "nothing was compared"
			}
			fmt.Fprintf(&b, "- %s %s: %s\n", c.Run, c.Name, first)
		}
		b.WriteString("\n")
	}
	if len(r.Differences) == 0 {
		b.WriteString("No differences.\n")
		return b.String()
	}
	b.WriteString("| Run | Where | Difference | On ADR 0026 point 8's list | Size |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, d := range r.Differences {
		listed := "no"
		if d.Listed() {
			listed = "yes"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", d.Run, d.Where, d.Kind, listed, d.Size)
	}
	return b.String()
}

func formatDiff(x float64) string {
	if x == 0 {
		return "0"
	}
	return fmt.Sprintf("%.3g", x)
}

// differencesCheck fails once per difference that is not on the list.
func differencesCheck(run string, ds []Difference) Check {
	c := Check{Run: run, Name: CheckDifferences, Threshold: "none off ADR 0026 point 8's list", MayBeEmpty: true}
	for _, d := range ds {
		c.Compared++
		if !d.Listed() {
			c.fail(fmt.Sprintf("%s: %s (%s)", d.Where, d.Kind, d.Size))
		}
	}
	return c
}

// relative is |a - b| / max(|a|, |b|), and 0 when both are 0.
func relative(a, b float64) float64 {
	scale := math.Max(math.Abs(a), math.Abs(b))
	if scale == 0 {
		return 0
	}
	return math.Abs(a-b) / scale
}
