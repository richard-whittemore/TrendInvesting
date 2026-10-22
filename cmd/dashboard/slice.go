package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The journal slice is the triage job's one way into the server: a GET route
// serving journal-query's own reports over each account's current run, as
// text (docs/manual/alerts-and-dashboard.md, "Alerts analysed by Claude").
// It is read-only as the whole dashboard is (readOnly; the package holds no
// write path), and it needs no credential: the tailnet decides who reaches it
// (docs/runbooks/lightsail-deployment.md: a tailnet grant admits only the
// owner's devices and the Mac mini). It is no wider than the page: every
// withheld column is left out and every figure removed, through the
// reporter's own redactor (docs/live-trading/plan.md section 7), so no
// figure leaves through it.

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

// sliceRedactor removes this host's names and every pattern and figure the
// reporter removes; the dashboard holds no secret to add to it.
func sliceRedactor() report.Redactor {
	var names []string
	if h, err := os.Hostname(); err == nil && h != "" {
		short, _, _ := strings.Cut(h, ".")
		names = append(names, h, short)
	}
	return report.NewRedactor(names)
}

// alertKeyHash is the hash of an alert's key as the reporter writes it in
// the issue's hidden key line: SHA-256, in lowercase hex.
var alertKeyHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// sliceAlert reads the route's optional alert: the hash of the alert's key,
// so the span line of the log whose own alerts include it says so, and the
// triage job takes coverage from the alert's own account and run only.
func sliceAlert(r *http.Request) (string, error) {
	v := r.URL.Query().Get("alert")
	if v != "" && !alertKeyHash.MatchString(v) {
		return "", fmt.Errorf("alert must be the 64 hexadecimal digits of the alert key's hash")
	}
	return v, nil
}

// raises reports whether the log's own alerts (report.Alerts, the reporter's
// rules over its records) include the one whose key hashes to want.
func raises(loaded journalquery.Journal, want string) bool {
	if want == "" {
		return false
	}
	envelopes := make([]event.Envelope, 0, len(loaded.Records))
	for _, rec := range loaded.Records {
		envelopes = append(envelopes, rec.Envelope)
	}
	alerts, err := report.Alerts(envelopes)
	if err != nil {
		return false
	}
	for _, a := range alerts {
		sum := sha256.Sum256([]byte(a.Key))
		if hex.EncodeToString(sum[:]) == want {
			return true
		}
	}
	return false
}

// sliceWindow reads the route's optional window: from and to, RFC 3339
// instants, either may be absent. The rows kept are those of the UTC days
// the window touches, journal-query's own filter.
func sliceWindow(r *http.Request) (journalquery.Filter, error) {
	day := func(name string) (string, time.Time, error) {
		v := r.URL.Query().Get(name)
		if v == "" {
			return "", time.Time{}, nil
		}
		at, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("%s must be an RFC 3339 time", name)
		}
		return at.UTC().Format(time.DateOnly), at, nil
	}
	from, fromAt, err := day("from")
	if err != nil {
		return journalquery.Filter{}, err
	}
	to, toAt, err := day("to")
	if err != nil {
		return journalquery.Filter{}, err
	}
	if !fromAt.IsZero() && !toAt.IsZero() && toAt.Before(fromAt) {
		return journalquery.Filter{}, fmt.Errorf("the window runs backwards")
	}
	f, err := journalquery.NewFilter("", "", from, to)
	if err != nil {
		return journalquery.Filter{}, err
	}
	f.AllowTrailingFills = true
	return f, nil
}

// serveSlice answers the slice: for each fixed journal and each engine
// directory's current run, the span its records cover (and, asked for an
// alert, whether this log raised it), then each report's last rows in the
// window, headed by the log's file name (never its path), with every
// account id, token and host taken out of it (report.Redactor.Identifiers;
// its digits stay, so a run's file name still reads). A source that cannot be read is said so in one line, as the page
// says it, and the others are still served.
func serveSlice(sources source, logs *logCache) http.HandlerFunc {
	redactor := sliceRedactor()
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := sliceWindow(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		alert, err := sliceAlert(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var b strings.Builder
		for _, path := range sources.journals {
			sliceOf(&b, filepath.Base(path), path, logs, filter, alert, redactor)
		}
		for _, dir := range sources.dirs {
			path, _, err := currentLogIn(dir, logs)
			if err != nil {
				fmt.Fprintf(&b, "## %s: %s\n\n", redactor.Identifiers(filepath.Base(dir)), redactor.Text(reason(err)))
				continue
			}
			sliceOf(&b, filepath.Base(path), path, logs, filter, alert, redactor)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	}
}

// sliceOf writes one log's span and reports. Reconcile returns its table
// with the disagreement it found, which is the reason to show it, so a
// report's error is printed under whatever table came back.
func sliceOf(b *strings.Builder, name, path string, logs *logCache, filter journalquery.Filter, alert string, r report.Redactor) {
	shown := r.Identifiers(name)
	raw, err := logs.readFile(path)
	if err != nil {
		fmt.Fprintf(b, "## %s: journal unavailable\n\n", shown)
		return
	}
	loaded, err := journalquery.Load(name, raw)
	if err != nil {
		fmt.Fprintf(b, "## %s: %s\n\n", shown, r.Text(reason(err)))
		return
	}
	if n := len(loaded.Records); n > 0 {
		raised := ""
		if raises(loaded, alert) {
			raised = ", and raised the alert"
		}
		fmt.Fprintf(b, "## %s: covers %s to %s%s\n\n", shown,
			loaded.Records[0].Envelope.RecordedAt.UTC().Format(time.RFC3339), loaded.Records[n-1].Envelope.RecordedAt.UTC().Format(time.RFC3339), raised)
	}
	journals := []journalquery.Journal{loaded}
	for _, q := range sliceReports {
		table, qerr := q.run(journals, filter)
		fmt.Fprintf(b, "## %s: journal-query %s\n\n", shown, q.name)
		var text bytes.Buffer
		if len(table.Columns) > 0 {
			_ = redactedTable(table, r).WriteText(&text) // a bytes.Buffer never fails a write
		}
		b.WriteString(text.String())
		if qerr != nil {
			fmt.Fprintf(b, "%s\n", r.Text(qerr.Error()))
		}
		b.WriteString("\n")
	}
}

// redactedTable is t without its withheld columns (report.Withheld), cut to
// its last rows, with every cell through the redactor.
func redactedTable(t journalquery.Table, r report.Redactor) journalquery.Table {
	var keep []int
	var out journalquery.Table
	for i, c := range t.Columns {
		if !report.Withheld(c) {
			keep = append(keep, i)
			out.Columns = append(out.Columns, c)
		}
	}
	for _, row := range tail(t.Rows) {
		cells := make([]string, 0, len(keep))
		for _, i := range keep {
			cells = append(cells, r.Text(row[i]))
		}
		out.Rows = append(out.Rows, cells)
	}
	return out
}
