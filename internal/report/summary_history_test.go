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
	// The summary gives the comparison over the intact runs the page shows,
	// with the break named above it.
	text := report.RenderText(got)
	broke := strings.Index(text, "Equity vs SPY: history unavailable: the chain of runs is broken between run2.log and run1.log\n")
	over := strings.Index(text, "Equity vs SPY, from run2.log, the first intact run, "+day(3).Format("2006-01-02")+" to "+day(4).Format("2006-01-02")+": account ")
	if broke < 0 || over < broke || !strings.Contains(text, "Day, "+day(3).Format("2006-01-02")) {
		t.Fatalf("text:\n%s", text)
	}
	if line := report.HistoryUnavailable(&restart.ChainError{Run: "run2.log"}); line != "history unavailable: the chain of runs is broken at run2.log" {
		t.Fatalf("a break at one log reads %q", line)
	}
}

// TestHistoryUnavailableAlertIsOneIncidentPerBreak: the owner learns of a
// broken chain once, by the reporter's once-only delivery, with the remedy;
// the same link broken for another reason, a restored log whose hash does
// not match, is another incident.
func TestHistoryUnavailableAlertIsOneIncidentPerBreak(t *testing.T) {
	broken := &restart.ChainError{Run: "run2.log", Previous: "run1.log", Cause: restart.CauseUnreadable, Reason: "that log cannot be read"}
	a := report.HistoryUnavailableAlert("joint", broken, day(1))
	again := report.HistoryUnavailableAlert("joint", broken, day(2))
	if a.Key != again.Key || a.Repeat || a.Kind != report.KindHistoryUnavailable || a.Severity != report.Routine {
		t.Fatalf("alert = %+v, again = %+v", a, again)
	}
	mismatch := *broken
	mismatch.Cause = restart.CauseHashMismatch
	if other := report.HistoryUnavailableAlert("joint", &mismatch, day(2)); other.Key == a.Key {
		t.Fatalf("a break of another cause at the same link shares the key %q", a.Key)
	}
	if !strings.HasPrefix(a.Key, report.HistoryUnavailableKeyPrefix("joint")) {
		t.Fatalf("key %q does not begin with the account's prefix", a.Key)
	}
	for _, w := range []string{"run2.log", "run1.log", "backup", "quarantine", "incident alerts", "daily summary", "modification time"} {
		if !strings.Contains(a.Title+" "+a.Body, w) {
			t.Errorf("alert lacks %q: %s / %s", w, a.Title, a.Body)
		}
	}
}

// TestTheModeBoundaryIsNamedInTheComparison: a history that starts at the
// first run of the current mode says so on the page and in the summary.
func TestTheModeBoundaryIsNamedInTheComparison(t *testing.T) {
	_, second := twoRuns(t)
	h := report.History{Since: &report.ModeStart{Log: "run2.log", Mode: event.TradingModePaper, Date: day(3).Format("2006-01-02")}}
	got, err := report.SummarizeWithHistory(h, second, day(4).Format("2006-01-02"), "SPY")
	if err != nil {
		t.Fatal(err)
	}
	want := "history since " + day(3).Format("2006-01-02") + ", the first paper run"
	if got.Equity.HistorySince != want {
		t.Fatalf("since = %q, want %q", got.Equity.HistorySince, want)
	}
	if text := report.RenderText(got); !strings.Contains(text, "Equity vs SPY: "+want+"\n") {
		t.Fatalf("text:\n%s", text)
	}
}

// TestARestoredLogNewerThanTheCurrentRunIsOneRoutineAlert: a log restored
// without its modification time is named, with the current run, once.
func TestARestoredLogNewerThanTheCurrentRunIsOneRoutineAlert(t *testing.T) {
	a := report.RestoredLogNewerAlert("joint", "run1.log", "run3.log", day(1))
	again := report.RestoredLogNewerAlert("joint", "run1.log", "run3.log", day(2))
	if a.Key != again.Key || a.Repeat || a.Kind != report.KindRestoredLogNewer || a.Severity != report.Routine {
		t.Fatalf("alert = %+v, again = %+v", a, again)
	}
	line := "run1.log looks newer than the current run run3.log; fix its modification time before the next restart"
	if report.RestoredLogNewer("run1.log", "run3.log") != line || !strings.Contains(a.Body, line) || !strings.Contains(a.Title, "joint") {
		t.Fatalf("alert = %+v", a)
	}
}
