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

// TestTheJournalSliceCarriesNoFigure: the slice is no wider than the
// dashboard's page (docs/live-trading/plan.md section 7: what leaves the
// server for a reader the owner does not control carries no figure): every
// withheld column is left out and every figure left is removed, through the
// reporter's own redactor, so a figure never reaches the triage job, Claude
// or a push. The rows' words stay.
func TestTheJournalSliceCarriesNoFigure(t *testing.T) {
	h := newHandlerFor([]string{writeJournal(t, t.TempDir())}, nil, nil, "SPY")
	body := get(h, http.MethodGet, "/api/journal-slice").Body.String()
	for _, leak := range []string{"101.00", " 39 ", "quantity", "price", "commission", "account  instrument", "joint  SPY"} {
		if strings.Contains(body, leak) {
			t.Errorf("slice holds %q:\n%s", leak, body)
		}
	}
	for _, kept := range []string{"refused", "order-size", "SPY", "2024-01-02"} {
		if !strings.Contains(body, kept) {
			t.Errorf("slice lost %q:\n%s", kept, body)
		}
	}
}

// TestTheJournalSliceTakesAWindowAndSaysWhatEachRunCovers: the triage job
// asks for the rows of a time window around the alert, and each log's
// section first says the span its records cover, so the job can tell
// whether the slice covers the alert's run and time at all.
func TestTheJournalSliceTakesAWindowAndSaysWhatEachRunCovers(t *testing.T) {
	h := newHandlerFor([]string{writeJournal(t, t.TempDir())}, nil, nil, "SPY")
	body := get(h, http.MethodGet, "/api/journal-slice?from=2024-01-05T00:00:00Z&to=2024-01-06T00:00:00Z").Body.String()
	if !strings.Contains(body, "## joint.journal: covers 2024-01-02T21:00:00Z to 2024-01-02T21:00:00Z") {
		t.Errorf("slice does not say what the run covers:\n%s", body)
	}
	if strings.Contains(body, "order-size") {
		t.Errorf("slice holds a row outside the window:\n%s", body)
	}
	inside := get(h, http.MethodGet, "/api/journal-slice?from=2024-01-01T00:00:00Z&to=2024-01-03T00:00:00Z").Body.String()
	if !strings.Contains(inside, "order-size") {
		t.Errorf("slice lacks the row inside the window:\n%s", inside)
	}
	for _, bad := range []string{"?from=yesterday", "?to=2024-01-01", "?from=2024-01-03T00:00:00Z&to=2024-01-01T00:00:00Z"} {
		if rec := get(h, http.MethodGet, "/api/journal-slice"+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", bad, rec.Code)
		}
	}
}
