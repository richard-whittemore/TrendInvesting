// Package parity compares the Go target-weight modules with QuantConnect's
// own runs: it feeds the modules what a logged QuantConnect run used (its
// universe's scores, its adjusted closes, SPY's closes) and reports, check
// by check, whether they decide the same thing (ADR 0026 point 7, item 2;
// docs/live-trading/plan.md section 6, item 2).
//
// The pass/fail specification is the parity gate's: ranked order and cap
// marks exact; weights within WeightTolerance given QuantConnect's own
// inputs; scores and volatility, recomputed from the closes, and Boost's
// RSI and means, within RelativeTolerance, readiness exact; Boost's state
// exact on every session (ADR 0026 point 7, item 2, as amended on
// 2026-10-18).
//
// The comparison never puts a mismatch down to a cause. For momentum it
// compares the Go module's recomputation under the backtest's own handling
// of each case ADR 0026 point 8 lists, and of a name with no sector (ranked
// and capped as QuantConnect did, ties kept in QuantConnect's order, a zero
// volatility weighted as 1, a short or gapped history scored as
// QuantConnect scored it), with QuantConnect's export exactly: ranked
// order, cap marks, picked set and weights, every month, with no tolerance
// beyond the specification's. Separately, the differences check lists
// every departure of the live rule from that backtest-compatible result,
// classified by cause, and fails on any cause ADR 0026 point 8 does not
// list. A failed result is recorded, never rewritten (AGENTS.md rule 6).
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
	// for scores, volatilities, the trend mean, and Boost's RSI and means.
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
// lists them, then the kind that is not on that list. A name with no
// sector label is none of them: since the owner's decision of 2026-10-18
// the live rule ranks and picks it as the backtest did.
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

	// KindPriceWait: buys the backtest held back for a stock's first price,
	// which live trading, sizing from the previous close, does not.
	KindPriceWait Kind = "price-wait"
	// KindUnpriced: a name with no reference price, traded outside the
	// planner.
	KindUnpriced Kind = "unpriced"
	// KindDelisted: a stock delisted or acquired at the rebalance, which
	// the backtest could not trade (ADR 0026 point 8, amended 2026-10-18),
	// detected as the export records it: the order refused by QuantConnect
	// as non-tradable, or no price within five sessions.
	KindDelisted Kind = "delisted"

	// KindLiquidated: a holding LEAN sold itself at the stock's delisting,
	// outside the plan. ADR 0026 point 8 does not list it.
	KindLiquidated Kind = "liquidated"

	// KindUnexplained: a live departure from the backtest-compatible result
	// that no rule above produces. Never listed.
	KindUnexplained Kind = "unexplained"
)

// Listed reports whether ADR 0026 point 8 lists the kind.
func (k Kind) Listed() bool {
	switch k {
	case KindShortData, KindTrendHistory, KindGap, KindTie, KindZeroVolatility, KindRSISeed, KindDelisted:
		return true
	}
	return false
}

// Check is one row of the report: what was compared, against which
// threshold, how many comparisons there were, how many failed and the
// largest difference seen.
type Check struct {
	Run       string
	Name      string
	Threshold string
	Compared  int
	Failures  int
	MaxDiff   float64
	// First describes the first failure, for the record.
	First string
	// Fired counts the comparisons in which the rule fired on either side,
	// for a check whose comparisons are mostly of nothing (CountsFired).
	Fired       int
	CountsFired bool
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

// Difference is one place where the live rule departs from the
// backtest-compatible result, and the cause.
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
	Months      []MonthCounts
}

// MonthCounts is how many comparisons each check made in one month of a
// momentum run, so that a month that compared less than the others cannot
// hide behind their counts.
type MonthCounts struct {
	Run    string
	Month  string
	Counts map[string]int
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
		out.Months = append(out.Months, r.Months...)
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
	var fired []string
	for _, c := range r.Checks {
		if c.CountsFired {
			fired = append(fired, fmt.Sprintf("- %s %s: fired in %d of the %d months compared\n", c.Run, c.Name, c.Fired, c.Compared))
		}
	}
	if len(fired) > 0 {
		b.WriteString("Months in which the rule fired, on either side:\n\n" + strings.Join(fired, "") + "\n")
	}
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
	r.writeMonths(&b)
	if len(r.Differences) == 0 {
		b.WriteString("No differences.\n")
		return b.String()
	}
	b.WriteString("| Run | Where | Live departure | On ADR 0026 point 8's list | Size |\n")
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

// writeMonths renders the comparisons each month made, so that a month
// that compared less cannot hide behind the others' counts.
func (r Report) writeMonths(b *strings.Builder) {
	if len(r.Months) == 0 {
		return
	}
	b.WriteString("Comparisons per month:\n\n| Run | Month | " + strings.Join(monthChecks, " | ") + " |\n")
	b.WriteString("| --- | --- |" + strings.Repeat(" --- |", len(monthChecks)) + "\n")
	for _, m := range r.Months {
		fmt.Fprintf(b, "| %s | %s |", m.Run, m.Month)
		for _, name := range monthChecks {
			fmt.Fprintf(b, " %d |", m.Counts[name])
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
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
