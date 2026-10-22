package main

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
)

// The journal slice is the triage job's one way into the server: a GET route
// serving journal-query's own reports over each account's current run, as
// text (docs/manual/alerts-and-dashboard.md, "Alerts analysed by Claude").
// It is read-only as the whole dashboard is (readOnly; the package holds no
// write path), and it needs no credential: the tailnet decides who reaches it.

// sliceRows is how many of each report's last rows the journal slice keeps.
const sliceRows = 50

// sliceReports are the journal-query reports the slice serves, in order.
var sliceReports = []struct {
	name string
	run  func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error)
}{
	{"refusals", journalquery.Refusals},
	{"orders", journalquery.Orders},
	{"reconcile", journalquery.Reconcile},
	{"operator", journalquery.Operator},
	{"fills", journalquery.Fills},
}

// tail is the last sliceRows of rows.
func tail(rows [][]string) [][]string {
	return rows[max(0, len(rows)-sliceRows):]
}

// serveSlice answers the slice: for each fixed journal and each engine
// directory's current run, each report's last rows, headed by the log's file
// name (never its path). A source that cannot be read is said so in one line,
// as the page says it, and the others are still served.
func serveSlice(sources source, logs *logCache) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		for _, path := range sources.journals {
			sliceOf(&b, filepath.Base(path), path, logs)
		}
		for _, dir := range sources.dirs {
			path, _, err := currentLogIn(dir, logs)
			if err != nil {
				fmt.Fprintf(&b, "## %s: %s\n\n", filepath.Base(dir), reason(err))
				continue
			}
			sliceOf(&b, filepath.Base(path), path, logs)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	}
}

// sliceOf writes one log's reports. Reconcile returns its table with the
// disagreement it found, which is the reason to show it, so a report's error
// is printed under whatever table came back.
func sliceOf(b *strings.Builder, name, path string, logs *logCache) {
	raw, err := logs.readFile(path)
	if err != nil {
		fmt.Fprintf(b, "## %s: journal unavailable\n\n", name)
		return
	}
	loaded, err := journalquery.Load(name, raw)
	if err != nil {
		fmt.Fprintf(b, "## %s: %s\n\n", name, reason(err))
		return
	}
	journals := []journalquery.Journal{loaded}
	for _, r := range sliceReports {
		table, qerr := r.run(journals, journalquery.Filter{AllowTrailingFills: true})
		fmt.Fprintf(b, "## %s: journal-query %s\n\n", name, r.name)
		table.Rows = tail(table.Rows)
		var text bytes.Buffer
		if len(table.Columns) > 0 {
			_ = table.WriteText(&text) // a bytes.Buffer never fails a write
		}
		b.WriteString(text.String())
		if qerr != nil {
			fmt.Fprintf(b, "%s\n", qerr)
		}
		b.WriteString("\n")
	}
}
