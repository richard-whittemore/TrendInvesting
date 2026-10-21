package report

import (
	"fmt"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// RenderText formats a summary as plain text for a message or a terminal.
func RenderText(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) daily summary, %s\n", s.AccountID, s.StrategyID, s.Date)
	fmt.Fprintf(&b, "Engine state: %s\n", s.EngineState)
	if s.HaveSnapshot {
		fmt.Fprintf(&b, "Net liquidation %.2f as of %s\n", s.NetLiquidation, s.HoldingsAsOf.UTC().Format("2006-01-02 15:04Z"))
	} else {
		b.WriteString("No account snapshot recorded.\n")
	}
	if s.CashChecked {
		fmt.Fprintf(&b, "Cash drift since the previous close %.2f; unexplained cash since the cash was last explained (a reconciliation the engine explained, else the starting book, always Boost's as its book carries no cash; the day's drift when the run's chain holds neither) %.2f\n", s.CashDrift, s.UnexplainedCash)
	} else if s.CashUnchecked {
		b.WriteString("Cash drift: not checked (the latest reading after the close was stamped at its read time)\n")
	}
	b.WriteString("Holdings:")
	if len(s.Holdings) == 0 {
		b.WriteString(" none")
	}
	for _, h := range s.Holdings {
		fmt.Fprintf(&b, " %s %d", h.InstrumentID, h.Quantity)
	}
	b.WriteString("\nTargets:\n")
	if len(s.Decisions) == 0 {
		b.WriteString("  no decision today\n")
	}
	for _, d := range s.Decisions {
		if d.Outcome == event.TargetsOutcomeWithheld {
			fmt.Fprintf(&b, "  withheld (%s)\n", d.WithheldReason)
			continue
		}
		for _, t := range d.Targets {
			fmt.Fprintf(&b, "  %s %.0f%%\n", t.InstrumentID, t.Weight*100)
		}
	}
	b.WriteString("Orders:\n")
	if len(s.Orders) == 0 {
		b.WriteString("  none\n")
	}
	for _, o := range s.Orders {
		fmt.Fprintf(&b, "  %s %s %d %s limit %.2f: %s", o.ClientOrderID, o.Side, o.Quantity, o.InstrumentID, o.LimitPrice, o.StatusText())
		if o.Reason != "" {
			fmt.Fprintf(&b, " (%s)", o.Reason)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Refusals: %d\nFills:", s.Refusals)
	if len(s.Fills) == 0 {
		b.WriteString(" none")
	}
	for _, f := range s.Fills {
		if f.Side != "" {
			fmt.Fprintf(&b, " %s", f.Side)
		}
		fmt.Fprintf(&b, " %s %d @ %.2f", f.InstrumentID, f.Quantity, f.Price)
	}
	b.WriteString("\n")
	if len(s.SalesWithheld) > 0 {
		b.WriteString("Sales back to 98% withheld:\n")
	}
	for _, w := range s.SalesWithheld {
		fmt.Fprintf(&b, "  %s %s: %s (%s). %s\n", w.ClientOrderID, w.InstrumentID, w.Reason, w.Trigger, w.Detail)
	}
	if len(s.Skipped) > 0 {
		b.WriteString("Trades skipped:\n")
	}
	for _, k := range s.Skipped {
		fmt.Fprintf(&b, "  %s %s %s %s shares: %s\n", k.ClientOrderID, k.Side, k.InstrumentID, event.FormatSkippedShares(k.Shares), k.Reason)
	}
	switch {
	case s.Equity.HistoryUnavailable != "":
		e := s.Equity
		fmt.Fprintf(&b, "Equity vs %s: %s\n", s.Benchmark, e.HistoryUnavailable)
		if e.DayAvailable {
			fmt.Fprintf(&b, "Day, %s to %s: account %+.2f%%, %s %+.2f%%\n", e.DayStartDate, e.EndDate, e.DayAccountReturn*100, s.Benchmark, e.DayBenchmarkReturn*100)
		}
	case s.Equity.Available:
		e := s.Equity
		fmt.Fprintf(&b, "Equity vs %s, %s to %s: account %+.2f%%, %s %+.2f%%, difference %+.2f points\n",
			s.Benchmark, e.StartDate, e.EndDate, e.AccountReturn*100, s.Benchmark, e.BenchmarkReturn*100, e.Excess*100)
		if e.DayAvailable {
			fmt.Fprintf(&b, "Day, %s to %s: account %+.2f%%, %s %+.2f%%\n", e.DayStartDate, e.EndDate, e.DayAccountReturn*100, s.Benchmark, e.DayBenchmarkReturn*100)
		} else {
			fmt.Fprintf(&b, "Day: not available (no paired snapshot and %s close on %s)\n", s.Benchmark, s.Date)
		}
	default:
		fmt.Fprintf(&b, "Equity vs %s: not enough paired snapshots and bars yet.\n", s.Benchmark)
	}
	if s.RejectedInputs > 0 {
		fmt.Fprintf(&b, "Inputs not read: %d (an unknown schema version or values that fail validation; they move no figure)\n", s.RejectedInputs)
	}
	return b.String()
}
