package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// counted is a readFile and an open that count, per log, the whole reads
// and the bytes the open reads took.
type counted struct {
	whole map[string]int
	bytes map[string]int
}

func newCounted() *counted { return &counted{whole: map[string]int{}, bytes: map[string]int{}} }

func (c *counted) readFile(path string) ([]byte, error) {
	c.whole[filepath.Base(path)]++
	return os.ReadFile(path)
}

func (c *counted) open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &countingFile{f: f, name: filepath.Base(path), c: c}, nil
}

func (c *counted) reset() { clear(c.whole); clear(c.bytes) }

type countingFile struct {
	f    *os.File
	name string
	c    *counted
}

func (f *countingFile) Read(p []byte) (int, error) {
	n, err := f.f.Read(p)
	f.c.bytes[f.name] += n
	return n, err
}

func (f *countingFile) Close() error { return f.f.Close() }

// TestDashboardFindsTheCurrentRunByTheChainNotTheModificationTime: an
// earlier log restored from a backup with a fresh modification time is the
// newest file, but the current run names it, so the page still shows the
// current run, and warns, naming both logs, that the restored log looks
// newer.
func TestDashboardFindsTheCurrentRunByTheChainNotTheModificationTime(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	h1 := writeRun(t, dir, "run1.log", approval("", ""), 0, base)
	h2 := writeRun(t, dir, "run2.log", approval("run1.log", h1), 2, base.Add(time.Hour))
	writeRun(t, dir, "run3.log", approval("run2.log", h2), 4, base.Add(2*time.Hour))
	restored := base.Add(3 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "run1.log"), restored, restored); err != nil {
		t.Fatal(err)
	}
	h := newHandlerFor(nil, []string{dir}, "SPY")
	got := summaryOf(t, h)
	if got.Summary.Date != at(5).Format(time.DateOnly) || len(got.Summary.Equity.Points) != 6 {
		t.Fatalf("the page shows %s with %d points, want run3.log's day and the whole history", got.Summary.Date, len(got.Summary.Equity.Points))
	}
	const warning = "run1.log looks newer than the current run run3.log; fix its modification time before the next restart"
	if got.Warning != warning {
		t.Fatalf("warning = %q, want %q", got.Warning, warning)
	}
	if rec := get(h, http.MethodGet, "/"); !strings.Contains(rec.Body.String(), warning) {
		t.Fatalf("the page does not warn:\n%s", rec.Body)
	}
	if err := os.Chtimes(filepath.Join(dir, "run1.log"), base, base); err != nil {
		t.Fatal(err)
	}
	if got := summaryOf(t, h); got.Warning != "" {
		t.Fatalf("a log with its time restored still warns: %q", got.Warning)
	}
}

// TestDashboardKeepsOnlyTheHistoryOfAnEarlierLog: the current log's whole
// recovery is never kept; of an earlier log the cache keeps only what the
// chain and the equity history read, however many other records it holds,
// and an entry whose file is gone is dropped.
func TestDashboardKeepsOnlyTheHistoryOfAnEarlierLog(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	var noise []journal.Entry
	for i := range 300 {
		noise = append(noise, input(t, 0, at(1), event.KillSwitchEventType, event.KillSwitchSchemaVersion, fmt.Sprintf("ks:%d", i), event.KillSwitchPayload{AccountID: "joint", AsOf: at(1), Operator: "owner"}))
	}
	h1 := writeRun(t, dir, "run1.log", approval("", ""), 0, base, noise...)
	writeRun(t, dir, "run2.log", approval("run1.log", h1), 2, base.Add(time.Hour))
	c := newCounted()
	logs := newLogCache(c.readFile, c.open)
	h := newHandlerCached(source{dirs: []string{dir}}, "SPY", logs)
	if got := summaryOf(t, h); len(got.Summary.Equity.Points) != 4 {
		t.Fatalf("points = %d, want 4", len(got.Summary.Equity.Points))
	}
	// Of run1.log: its configuration and approval, two bars and two
	// snapshots. Nothing of run2.log, the current log, is kept whole.
	if n := logs.retained(filepath.Join(dir, "run1.log")); n > 6 {
		t.Fatalf("the cache keeps %d of run1.log's records, want at most the six the history reads", n)
	}
	if n := logs.retained(filepath.Join(dir, "run2.log")); n > 2 {
		t.Fatalf("the cache keeps %d of the current log's records, want only its start", n)
	}
	if err := os.Remove(filepath.Join(dir, "run1.log")); err != nil {
		t.Fatal(err)
	}
	summaryOf(t, h)
	if logs.held(filepath.Join(dir, "run1.log")) {
		t.Fatal("the cache still holds a log that is gone")
	}
}

// TestDashboardOfAThousandRunsReadsEachEarlierLogOnce: a directory of a
// thousand chained runs: the first request reads each earlier log whole
// once, for its hash and its history, and the current run is found from the
// logs' starts; after the engine appends to the current log, a request reads
// that log whole once and no earlier log at all.
func TestDashboardOfAThousandRunsReadsEachEarlierLogOnce(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	previous, hash, before := "", "", ""
	var last string
	for i := range 1000 {
		last = fmt.Sprintf("run%04d.log", i)
		before = hash
		hash = writeRun(t, dir, last, approval(previous, hash), 0, base.Add(time.Duration(i)*time.Minute))
		previous = last
	}
	c := newCounted()
	h := newHandlerCached(source{dirs: []string{dir}}, "SPY", newLogCache(c.readFile, c.open))
	summaryOf(t, h)
	for name, n := range c.whole {
		if n != 1 {
			t.Fatalf("the first request read %s whole %d times", name, n)
		}
	}
	if len(c.whole) != 1000 {
		t.Fatalf("the first request read %d logs whole, want each of the thousand once", len(c.whole))
	}
	c.reset()
	writeRun(t, dir, last, approval("run0998.log", before), 0, base.Add(time.Hour*24), halted(t, 2))
	summaryOf(t, h)
	if len(c.whole) != 1 || c.whole[last] != 1 {
		t.Fatalf("after the current log changed, read whole %v; want %s once", c.whole, last)
	}
	for name := range c.bytes {
		if name != last {
			t.Fatalf("after the current log changed, %s was read again", name)
		}
	}
}

// writeStart writes a durable log holding only its configuration: a run
// still starting.
func writeStart(t *testing.T, dir, name string, modified time.Time) {
	t.Helper()
	var file memoryFile
	log, err := journal.CreateLog(&file, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]journal.Entry{configurationOf(t, name, 0)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// TestDashboardOfARunStillStartingSaysSo: the current run holding only its
// configuration is a start in progress: the equity card says the run is
// starting and has no history yet, and names no broken chain.
func TestDashboardOfARunStillStartingSaysSo(t *testing.T) {
	dir := t.TempDir()
	writeStart(t, dir, "run1.log", time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC))
	h := newHandlerFor(nil, []string{dir}, "SPY")
	got := summaryOf(t, h)
	if got.Summary.Equity.HistoryUnavailable != "" || got.Summary.Equity.HistoryStarting != "the run is starting; no history yet" {
		t.Fatalf("equity = %+v", got.Summary.Equity)
	}
	body := get(h, http.MethodGet, "/").Body.String()
	if !strings.Contains(body, "the run is starting; no history yet") || strings.Contains(body, "history unavailable") {
		t.Fatalf("page:\n%s", body)
	}
}

// TestDashboardOfAResumedRunReadsTheLogItCopiedOnce: a resumed run's log
// begins with the records of the log it copied, so the two tie for the
// current run and the engine's rule reads both whole; the copied log, which
// no longer changes, is read whole once, not on every request.
func TestDashboardOfAResumedRunReadsTheLogItCopiedOnce(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	writeRun(t, dir, "run1.log", approval("", ""), 0, base)
	// The resume: run1.log's records, then the resumed run's own.
	writeRun(t, scratch, "run1.log", approval("", ""), 0, base, halted(t, 2))
	resumed, err := os.ReadFile(filepath.Join(scratch, "run1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run2.log"), resumed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "run2.log"), base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c := newCounted()
	logs := newLogCache(c.readFile, c.open)
	h := newHandlerCached(source{dirs: []string{dir}}, "SPY", logs)
	if got := summaryOf(t, h); len(got.Alerts) == 0 {
		t.Fatalf("the page does not show the resumed run: %+v", got)
	}
	if c.whole["run1.log"] != 1 {
		t.Fatalf("the first request read run1.log whole %d times, want once", c.whole["run1.log"])
	}
	c.reset()
	summaryOf(t, h)
	if c.whole["run1.log"] != 0 {
		t.Fatalf("a later request read the copied log whole again: %v", c.whole)
	}
	if n := logs.retained(filepath.Join(dir, "run2.log")); n > startGroups {
		t.Fatalf("the cache keeps %d of the current log's records, want only its start", n)
	}
}

// TestDashboardForgetsTheCopiedLogOnceAFreshRunEndsTheTie: the log a resume
// copied is kept whole only while it ties for the chain's head; once a fresh
// run names the resumed run's log, the copied log is no longer read and is
// dropped from the whole logs the cache keeps.
func TestDashboardForgetsTheCopiedLogOnceAFreshRunEndsTheTie(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	writeRun(t, dir, "run1.log", approval("", ""), 0, base)
	hash := writeRun(t, scratch, "run1.log", approval("", ""), 0, base, halted(t, 2))
	resumed, err := os.ReadFile(filepath.Join(scratch, "run1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run2.log"), resumed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "run2.log"), base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	logs := newLogCache(os.ReadFile, func(p string) (io.ReadCloser, error) { return os.Open(p) })
	h := newHandlerCached(source{dirs: []string{dir}}, "SPY", logs)
	summaryOf(t, h)
	if len(logs.tied) != 1 {
		t.Fatalf("while the tie lasts the cache keeps %d whole logs, want the copied one", len(logs.tied))
	}
	writeRun(t, dir, "run3.log", approval("run2.log", hash), 2, base.Add(2*time.Hour))
	summaryOf(t, h)
	if len(logs.tied) != 0 {
		t.Fatalf("after a fresh run named the resumed log the cache still keeps %d whole logs", len(logs.tied))
	}
}
