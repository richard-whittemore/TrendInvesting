package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// writeChainedLog writes haltedEntries as a durable log with a run approval
// recorded straight after the configuration, and returns its final hash.
func writeChainedLog(t *testing.T, path string, approval event.OperatorRunApprovedPayload) string {
	t.Helper()
	entries := haltedEntries(t)
	approved := journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 2, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approved:"+filepath.Base(path), t0, approval)}
	entries = append(entries[:1], append([]journal.Entry{approved}, entries[1:]...)...)
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	var group []journal.Entry
	flush := func() {
		if len(group) > 0 {
			if err := log.Append(group); err != nil {
				t.Fatal(err)
			}
			group = nil
		}
	}
	for _, e := range entries {
		if e.Kind == journal.KindInput {
			flush()
		}
		group = append(group, e)
	}
	flush()
	if err := os.WriteFile(path, bytes.Clone(buf.Bytes()), 0o600); err != nil {
		t.Fatal(err)
	}
	return log.FinalRecordHash()
}

func approvalNaming(previous, hash string) event.OperatorRunApprovedPayload {
	latched := false
	p := event.OperatorRunApprovedPayload{AccountID: "joint", ApprovedBy: "owner", ApprovedAt: t0.Add(-time.Hour), Boost: event.BoostStateInForce{Latched: &latched}}
	if previous == "" {
		p.StartingBook = &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 1000, Currency: "USD"}
		return p
	}
	p.PreviousLog, p.PreviousLogFinalHash = previous, hash
	return p
}

// TestReadJournalFollowsTheChainOfRunsForTheSummary: the reporter's daily
// summary reads the runs before the current one, so a restart does not
// restart the equity comparison.
func TestReadJournalFollowsTheChainOfRunsForTheSummary(t *testing.T) {
	dir := t.TempDir()
	first := writeChainedLog(t, filepath.Join(dir, "run1.log"), approvalNaming("", ""))
	current := filepath.Join(dir, "run2.log")
	writeChainedLog(t, current, approvalNaming("run1.log", first))
	history, envelopes, _, _, err := readJournal(current, "boost", t0, time.Minute, map[string]bool{})
	if err != nil || history.Broken != nil || len(envelopes) == 0 || len(history.Earlier) == 0 {
		t.Fatalf("earlier = %d, broken = %v, envelopes = %d, err = %v", len(history.Earlier), history.Broken, len(envelopes), err)
	}
}

// TestABrokenChainCostsNoAlertAndIsReportedOnce: the earlier runs feed only
// the equity comparison, so a chain broken between two earlier logs never
// holds back the current log's incident alerts or its summary (ADR 0019,
// "Alerting"). The summary's equity comparison is one line naming both
// logs, and the break itself is one alert, delivered once.
func TestABrokenChainCostsNoAlertAndIsReportedOnce(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	writeChainedLog(t, filepath.Join(dir, "run1.log"), approvalNaming("", ""))
	current := filepath.Join(dir, "run2.log")
	writeChainedLog(t, current, approvalNaming("run1.log", "sha256:"+strings.Repeat("0", 64)))
	args := []string{"-journal", current, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-summary"}
	now := func() time.Time { return t0.Add(time.Hour) }

	var first, second bytes.Buffer
	if err := run(args, fakeEnv, now, &first, fakeClient); err != nil {
		t.Fatalf("first run: %v\n%s", err, first.String())
	}
	out := first.String()
	if strings.Contains(out, "cannot read the journal") {
		t.Fatalf("the broken chain refused the journal:\n%s", out)
	}
	for _, want := range []string{"engine halted", "order refused", "daily summary, 2024-01-02",
		"Equity vs SPY: history unavailable: the chain of runs is broken between run2.log and run1.log\n",
		"joint: the history of earlier runs is unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("first run lacks %q:\n%s", want, out)
		}
	}
	if err := run(args, fakeEnv, now, &second, fakeClient); err != nil {
		t.Fatalf("second run: %v\n%s", err, second.String())
	}
	if strings.Contains(second.String(), "history of earlier runs") {
		t.Fatalf("the break was reported twice:\n%s", second.String())
	}
}

// TestARepairedChainBrokenAgainAlertsAgain: the break's key is forgotten
// when a later firing finds the chain whole, so the same link broken again
// is a new incident, and a break of another cause at the same link is one
// too.
func TestARepairedChainBrokenAgainAlertsAgain(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "run1.log")
	hash := writeChainedLog(t, first, approvalNaming("", ""))
	backup, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(dir, "run2.log")
	writeChainedLog(t, current, approvalNaming("run1.log", hash))
	args := []string{"-journal", current, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy"}
	now := func() time.Time { return t0.Add(time.Hour) }
	fire := func() string {
		t.Helper()
		var out bytes.Buffer
		if err := run(args, fakeEnv, now, &out, fakeClient); err != nil {
			t.Fatalf("run: %v\n%s", err, out.String())
		}
		return out.String()
	}
	const title = "joint: the history of earlier runs is unavailable"
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if out := fire(); !strings.Contains(out, title) {
		t.Fatalf("the break was not reported:\n%s", out)
	}
	if err := os.WriteFile(first, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := fire(); strings.Contains(out, title) {
		t.Fatalf("a whole chain was reported broken:\n%s", out)
	}
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if out := fire(); !strings.Contains(out, title) {
		t.Fatalf("the same link broken again after a repair was not reported:\n%s", out)
	}
	// The wrong backup restored: the same link, another cause.
	wrong := approvalNaming("", "")
	wrong.ApprovedBy = "someone else"
	writeChainedLog(t, first, wrong)
	if out := fire(); !strings.Contains(out, title) || !strings.Contains(out, "final record hash") {
		t.Fatalf("a break of another cause at the same link was not reported:\n%s", out)
	}
}

// TestARestoredLogNewerThanTheCurrentRunIsReportedOnce: a log the current
// run names, restored with a fresh modification time, is one routine alert
// naming both logs.
func TestARestoredLogNewerThanTheCurrentRunIsReportedOnce(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "run1.log")
	hash := writeChainedLog(t, first, approvalNaming("", ""))
	current := filepath.Join(dir, "run2.log")
	writeChainedLog(t, current, approvalNaming("run1.log", hash))
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(current, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(first, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", current, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy"}
	now := func() time.Time { return t0.Add(time.Hour) }
	var out, again bytes.Buffer
	if err := run(args, fakeEnv, now, &out, fakeClient); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "joint: a restored log looks newer than the current run") ||
		!strings.Contains(out.String(), "run1.log looks newer than the current run run2.log; fix its modification time before the next restart") {
		t.Fatalf("the restored log was not reported:\n%s", out.String())
	}
	if err := run(args, fakeEnv, now, &again, fakeClient); err != nil {
		t.Fatalf("second run: %v\n%s", err, again.String())
	}
	if strings.Contains(again.String(), "looks newer") {
		t.Fatalf("reported twice:\n%s", again.String())
	}
}

// TestARunStillStartingCostsNoHistoryAlert: the current log holding only its
// configuration is a start in progress: the summary says the run is
// starting, and no history alert is sent, since nothing is to be restored.
func TestARunStillStartingCostsNoHistoryAlert(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	writeChainedLog(t, filepath.Join(dir, "run1.log"), approvalNaming("", ""))
	current := filepath.Join(dir, "run2.log")
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(haltedEntries(t)[:1]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, bytes.Clone(buf.Bytes()), 0o600); err != nil {
		t.Fatal(err)
	}
	history, _, _, alerts, err := readJournal(current, "boost", t0, time.Minute, map[string]bool{})
	if err != nil || history.Broken != nil || !history.Starting {
		t.Fatalf("history = %+v, err = %v; want a start in progress", history, err)
	}
	for _, a := range alerts {
		if a.Kind == report.KindHistoryUnavailable {
			t.Fatalf("a run still starting alerted: %+v", a)
		}
	}
	var out bytes.Buffer
	args := []string{"-journal", current, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy"}
	if err := run(args, fakeEnv, func() time.Time { return t0.Add(time.Hour) }, &out, fakeClient); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "history of earlier runs") {
		t.Fatalf("a run still starting alerted:\n%s", out.String())
	}
}

// TestARestoredLogNewerAgainAfterAFixAlertsAgain: the restored-log alert is
// forgotten once a firing finds no log newer than the current run, so the
// same log restored wrongly again is a new incident.
func TestARestoredLogNewerAgainAfterAFixAlertsAgain(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "run1.log")
	hash := writeChainedLog(t, first, approvalNaming("", ""))
	current := filepath.Join(dir, "run2.log")
	writeChainedLog(t, current, approvalNaming("run1.log", hash))
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(current, base, base); err != nil {
		t.Fatal(err)
	}
	stamp := func(at time.Time) {
		t.Helper()
		if err := os.Chtimes(first, at, at); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"-journal", current, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy"}
	fire := func() string {
		t.Helper()
		var out bytes.Buffer
		if err := run(args, fakeEnv, func() time.Time { return t0.Add(time.Hour) }, &out, fakeClient); err != nil {
			t.Fatalf("run: %v\n%s", err, out.String())
		}
		return out.String()
	}
	const title = "joint: a restored log looks newer than the current run"
	stamp(base.Add(time.Hour))
	if out := fire(); !strings.Contains(out, title) {
		t.Fatalf("the restored log was not reported:\n%s", out)
	}
	stamp(base.Add(-time.Hour))
	if out := fire(); strings.Contains(out, title) {
		t.Fatalf("a log with its time fixed was reported:\n%s", out)
	}
	stamp(base.Add(2 * time.Hour))
	if out := fire(); !strings.Contains(out, title) {
		t.Fatalf("the same log restored wrongly again after a fix was not reported:\n%s", out)
	}
}

// TestAFiringThatCannotExamineTheFolderKeepsTheRestoredLogAlertDelivered: a
// firing in which a log's start cannot be read says nothing of the restored
// log, so it must not clear the delivered alert; the next firing, with the
// log still newer, does not send it a second time.
func TestAFiringThatCannotExamineTheFolderKeepsTheRestoredLogAlertDelivered(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "run1.log")
	hash := writeChainedLog(t, first, approvalNaming("", ""))
	current := filepath.Join(dir, "run2.log")
	writeChainedLog(t, current, approvalNaming("run1.log", hash))
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(current, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(first, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", current, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy"}
	fire := func() string {
		t.Helper()
		var out bytes.Buffer
		if err := run(args, fakeEnv, func() time.Time { return t0.Add(time.Hour) }, &out, fakeClient); err != nil {
			t.Fatalf("run: %v\n%s", err, out.String())
		}
		return out.String()
	}
	const title = "joint: a restored log looks newer than the current run"
	if out := fire(); !strings.Contains(out, title) {
		t.Fatalf("the restored log was not reported:\n%s", out)
	}
	if err := os.Chmod(first, 0); err != nil {
		t.Fatal(err)
	}
	fire()
	if err := os.Chmod(first, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := fire(); strings.Contains(out, title) {
		t.Fatalf("a firing that could not read the folder made the alert go out again:\n%s", out)
	}
}
