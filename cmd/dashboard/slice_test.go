package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTheJournalSliceServesTheJournalQueryReportsOfEachCurrentRun: the
// triage job's read-only access path (docs/manual/alerts-and-dashboard.md,
// "Alerts analysed by Claude") is one GET route serving journal-query's
// own reports over each account's current run, as text, and nothing else.
func TestTheJournalSliceServesTheJournalQueryReportsOfEachCurrentRun(t *testing.T) {
	h := newHandlerFor([]string{writeJournal(t, t.TempDir())}, nil, nil, "SPY")
	rec := get(h, http.MethodGet, "/api/journal-slice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("content type %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"## joint.journal: journal-query refusals",
		"## joint.journal: journal-query orders",
		"## joint.journal: journal-query reconcile",
		"## joint.journal: journal-query operator",
		"## joint.journal: journal-query fills",
		"order-size",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("slice lacks %q:\n%s", want, body)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("slice may be cached: %q", rec.Header().Get("Cache-Control"))
	}
}

// TestTheJournalSliceFollowsTheEngineDirectoryAndCutsEachReport: an engine
// directory's slice is its current run's, and each report is cut to its
// last sliceRows rows, so one answer stays bounded however long the run.
func TestTheJournalSliceFollowsTheEngineDirectoryAndCutsEachReport(t *testing.T) {
	dir := t.TempDir()
	writeRun(t, dir, "run1.log", approval("", ""), 0, time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC))
	empty := filepath.Join(t.TempDir(), "trad-ira")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	body := get(newHandlerFor(nil, []string{dir, empty}, nil, "SPY"), http.MethodGet, "/api/journal-slice").Body.String()
	if !strings.Contains(body, "## run1.log: journal-query reconcile") {
		t.Errorf("slice does not name the current run:\n%s", body)
	}
	if !strings.Contains(body, "## trad-ira: no run yet: the engine has not started a run") {
		t.Errorf("slice does not say the empty directory has no run:\n%s", body)
	}
	if strings.Contains(body, dir) {
		t.Errorf("slice names the server's paths:\n%s", body)
	}
}

func TestTailKeepsTheLastRows(t *testing.T) {
	rows := make([][]string, sliceRows+5)
	for i := range rows {
		rows[i] = []string{string(rune('a' + i%26))}
	}
	got := tail(rows)
	if len(got) != sliceRows || got[0][0] != rows[5][0] {
		t.Errorf("tail kept %d rows from %v", len(got), got[0])
	}
}
