package report_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// twoRuns is a first run closing on sessions 0 to 2 and the run after it,
// started fresh, closing on sessions 3 and 4: synthetic prices.
func twoRuns(t *testing.T) (first, second []event.Envelope) {
	f := newStream(t)
	for n, net := range []float64{4000, 4040, 4100} {
		f.bar(n, "SPY", 100+float64(n))
		f.snapshot(n, net, 39)
	}
	f.engineState(2, event.EngineStateHalted, "test")
	s := newStream(t)
	for i, net := range []float64{4200, 4300} {
		n := i + 3
		s.bar(n, "SPY", 100+float64(n))
		s.snapshot(n, net, 39)
	}
	return f.out, s.out
}

// TestSummarizeAcrossRunsPairsPointsFromTheFirstRun: the equity comparison
// spans both logs, its first paired point is the first run's first close,
// and the day's figures still compare the last two points.
func TestSummarizeAcrossRunsPairsPointsFromTheFirstRun(t *testing.T) {
	first, second := twoRuns(t)
	got, err := report.SummarizeWithHistory(report.History{Earlier: first}, second, day(4).Format("2006-01-02"), "SPY")
	if err != nil {
		t.Fatal(err)
	}
	e := got.Equity
	if !e.Available || len(e.Points) != 5 {
		t.Fatalf("equity = %+v, want five paired points across both runs", e)
	}
	if e.StartDate != day(0).Format("2006-01-02") || e.EndDate != day(4).Format("2006-01-02") {
		t.Fatalf("equity runs %s to %s, want the first run's first close to the second run's last", e.StartDate, e.EndDate)
	}
	if !near(e.AccountReturn, 4300.0/4000-1) || !near(e.BenchmarkReturn, 104.0/100-1) {
		t.Fatalf("returns = %v, %v", e.AccountReturn, e.BenchmarkReturn)
	}
	if !e.DayAvailable || e.DayStartDate != day(3).Format("2006-01-02") || !near(e.DayAccountReturn, 4300.0/4200-1) {
		t.Fatalf("day figures = %+v", e)
	}
}

// TestSummarizeAcrossRunsKeepsTheCurrentRunsOwnMeaning: the engine state, the
// holdings and the rejected-input count are the current run's, not the
// history's: a fresh run starts Normal even if the one before ended Halted.
func TestSummarizeAcrossRunsKeepsTheCurrentRunsOwnMeaning(t *testing.T) {
	first, second := twoRuns(t)
	got, err := report.SummarizeWithHistory(report.History{Earlier: first}, second, day(4).Format("2006-01-02"), "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.EngineState != report.StateNormal {
		t.Fatalf("engine state = %s, want the current run's", got.EngineState)
	}
	if got.NetLiquidation != 4300 || got.RejectedInputs != 0 {
		t.Fatalf("summary = %+v", got)
	}
	alone, _ := report.Summarize(second, day(4).Format("2006-01-02"), "SPY")
	if alone.Equity.StartDate != day(3).Format("2006-01-02") {
		t.Fatalf("without the history the comparison starts at the current run's first close: %+v", alone.Equity)
	}
}

// TestEarlierRunsFeedOnlyWhatTheCurrentRunWouldRead: an earlier record after
// the summary's date, an equity-only snapshot, another account's snapshot
// and one not later than the last taken move no point.
func TestEarlierRunsFeedOnlyWhatTheCurrentRunWouldRead(t *testing.T) {
	first, second := twoRuns(t)
	noise := newStreamFor(t, "other")
	noise.snapshot(1, 9999, 1)
	noise.bar(9, "SPY", 500)
	noise.snapshot(9, 9999, 1)
	own := newStream(t)
	own.snapshot(0, 1234, 1) // not later than the last taken
	own.add(event.AccountSnapshotEventType, event.AccountSnapshotSchemaVersion, "equity-only", day(1), event.AccountSnapshotPayload{})
	history := append(append([]event.Envelope{}, first...), noise.out[1:]...)
	history = append(history, own.out[1:]...)
	got, err := report.SummarizeWithHistory(report.History{Earlier: history}, second, day(4).Format("2006-01-02"), "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Equity.Points) != 5 || !near(got.Equity.AccountReturn, 4300.0/4000-1) {
		t.Fatalf("equity = %+v", got.Equity)
	}
}

// TestABrokenChainNamesBothLogsInTheEquityComparison: the comparison runs
// over the intact runs and says where the chain broke, naming both logs;
// the text summary gives that one line in place of the comparison, and the
// day's figures as before.
func TestABrokenChainNamesBothLogsInTheEquityComparison(t *testing.T) {
	_, second := twoRuns(t)
	broken := &restart.ChainError{Run: "run2.log", Previous: "run1.log", Reason: "that log cannot be read", Intact: []restart.Link{{Name: "run2.log"}}}
	got, err := report.SummarizeWithHistory(report.History{Broken: broken}, second, day(4).Format("2006-01-02"), "SPY")
	if err != nil {
		t.Fatal(err)
	}
	e := got.Equity
	if e.HistoryUnavailable != "history unavailable: the chain of runs is broken between run2.log and run1.log" || e.HistoryStartsAt != "run2.log" {
		t.Fatalf("equity = %+v", e)
	}
	if !e.Available || e.StartDate != day(3).Format("2006-01-02") || got.NetLiquidation != 4300 {
		t.Fatalf("the intact run's figures are gone: %+v", got)
	}
	text := report.RenderText(got)
	if !strings.Contains(text, "Equity vs SPY: history unavailable: the chain of runs is broken between run2.log and run1.log\n") ||
		strings.Contains(text, "Equity vs SPY, ") || !strings.Contains(text, "Day, "+day(3).Format("2006-01-02")) {
		t.Fatalf("text:\n%s", text)
	}
	if line := report.HistoryUnavailable(&restart.ChainError{Run: "run2.log"}); line != "history unavailable: the chain of runs is broken at run2.log" {
		t.Fatalf("a break at one log reads %q", line)
	}
}

// TestHistoryUnavailableAlertIsOneIncidentPerBreak: the owner learns of a
// broken chain once, by the reporter's once-only delivery, with the remedy.
func TestHistoryUnavailableAlertIsOneIncidentPerBreak(t *testing.T) {
	broken := &restart.ChainError{Run: "run2.log", Previous: "run1.log", Reason: "that log cannot be read"}
	a := report.HistoryUnavailableAlert("joint", broken, day(1))
	again := report.HistoryUnavailableAlert("joint", broken, day(2))
	if a.Key != again.Key || a.Repeat || a.Kind != report.KindHistoryUnavailable || a.Severity != report.Routine {
		t.Fatalf("alert = %+v, again = %+v", a, again)
	}
	for _, w := range []string{"run2.log", "run1.log", "backup", "quarantine", "incident alerts"} {
		if !strings.Contains(a.Title+" "+a.Body, w) {
			t.Errorf("alert lacks %q: %s / %s", w, a.Title, a.Body)
		}
	}
}
