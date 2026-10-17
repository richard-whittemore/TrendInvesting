// Command reporter reads one account's journal and delivers what a person
// needs to know: alerts for every refusal, Degraded or Halted state, withheld
// decision, missing input, lost heartbeat and journal it cannot read, each
// once per incident, and optionally the daily summary
// (docs/live-trading/plan.md section 3; ADR 0019, "Alerting"). It only reads
// the journal. The one file it writes is its own state file of delivered
// incident keys. Every rule lives in internal/report; this is composition.
//
// It runs once per invocation, from a timer. The heartbeat it checks is the
// journal file's modification time, with silence counted in NYSE sessions; a
// reporter on the engine's own host cannot be the external dead-man's switch
// ADR 0019 asks for, which is a separate service.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, time.Now, os.Stdout, http.DefaultClient); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// delivered is the reporter's state file: the incident keys already sent.
type delivered struct {
	Keys []string `json:"keys"`
}

func run(args []string, getenv func(string) string, now func() time.Time, out io.Writer, client *http.Client) error {
	flags := flag.NewFlagSet("reporter", flag.ContinueOnError)
	flags.SetOutput(out)
	journalPath := flags.String("journal", "", "path to the account's journal (read only)")
	statePath := flags.String("state", "", "path to the reporter's own state file of delivered alerts")
	channels := flags.String("channel", "stdout", "comma-separated channels: stdout (terminal echo, never a delivery), ntfy, pagerduty")
	benchmark := flags.String("benchmark", "SPY", "benchmark instrument for the equity comparison")
	date := flags.String("date", "", "summary date, YYYY-MM-DD, sent once the log records that day's session closed (default: the latest closed session)")
	summary := flags.Bool("summary", false, "also deliver the daily summary")
	sessions := flags.Int("heartbeat-sessions", 2, "NYSE sessions that may close with nothing written to the journal before a heartbeat alert fires (at least 2); weekends and holidays close none")
	accountName := flags.String("account", "", "account name for alerts when the journal cannot name its own (default: the journal file's name)")
	sendTimeout := flags.Duration("send-timeout", 30*time.Second, "longest one send to one channel may take before it counts as failed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *journalPath == "" || *statePath == "" {
		return errors.New("reporter: -journal and -state are required")
	}
	if *sessions < 2 {
		return errors.New("reporter: -heartbeat-sessions must be at least 2: a session's close is counted before the engine writes that session's records, so 1 would page on every trading day")
	}
	if *sendTimeout <= 0 {
		return errors.New("reporter: -send-timeout must be positive: a send with no bound can hold the run forever, and with it every later alert")
	}
	// Every send is bounded, so a stalled channel fails as that channel's
	// failure, is retried next run, and never holds back the other alerts or
	// the state file.
	bounded := *client
	bounded.Timeout = *sendTimeout
	senders, err := buildSenders(*channels, getenv, &bounded, out)
	if err != nil {
		return err
	}
	seen, err := loadState(*statePath)
	if err != nil {
		return err
	}
	label := *accountName
	if label == "" {
		label = filepath.Base(*journalPath)
	}
	info, statErr := os.Stat(*journalPath)
	envelopes, account, alerts, refused := readJournal(*journalPath)
	if account != "" {
		label = account
	}
	var failures []error
	if refused != nil {
		// A journal that cannot be read must not silence the one path that
		// reaches a person (ADR 0019, "Alerting"): the failure is itself an
		// alert, the heartbeat is still checked, and the error is returned.
		failures = append(failures, refused)
		alerts = []report.Alert{report.JournalRefusedAlert(label, refused, now())}
	}
	if statErr == nil {
		a, lost, err := report.HeartbeatAlert(label, info.ModTime(), now(), *sessions)
		if err != nil {
			// A check the calendar cannot answer is as silent as the lost
			// heartbeat it would have caught, so it reaches a person too.
			failures = append(failures, err)
			alerts = append(alerts, report.HeartbeatUncheckedAlert(label, err, now()))
		} else if lost {
			alerts = append(alerts, a)
		}
	}
	// A summary that cannot be built is reported, but never holds back the
	// incident alerts: they are what ADR 0019 requires to reach a person.
	if *summary && refused == nil {
		a, due, err := summaryAlert(envelopes, *date, *benchmark)
		switch {
		case err != nil:
			failures = append(failures, fmt.Errorf("reporter: the daily summary was not sent: %w", err))
		case due:
			alerts = append(alerts, a)
		}
	}

	for _, a := range report.Fresh(alerts, nil) {
		if err := deliver(senders, a, seen); err != nil {
			failures = append(failures, err)
		}
	}
	if err := saveState(*statePath, seen); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// readJournal reads, verifies and decodes the journal, returning its
// envelopes, its account and its alerts, or the reason it was refused.
func readJournal(path string) ([]event.Envelope, string, []report.Alert, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", nil, fmt.Errorf("reporter: read journal: %w", err)
	}
	_, envelopes, err := report.Load(raw)
	if err != nil {
		return nil, "", nil, err
	}
	account, _, err := report.AccountOf(envelopes)
	if err != nil {
		return nil, "", nil, err
	}
	alerts, err := report.Alerts(envelopes)
	if err != nil {
		return nil, account, nil, err
	}
	return envelopes, account, alerts, nil
}

// summaryAlert wraps a day's summary as a routine message keyed by account
// and date, so a rerun the same day does not resend it. A day is summarised
// only once the log records its session closed, since the day's decision and
// orders are written with that close: a summary sent earlier would be
// incomplete and, being keyed by the day, never replaced. With no date it is
// the latest closed session's day, and it is not yet due when none has
// closed. A stated date whose session has not closed is an error.
func summaryAlert(envelopes []event.Envelope, date, benchmark string) (report.Alert, bool, error) {
	if date == "" {
		last, ok := report.ClosedSessionDate(envelopes)
		if !ok {
			return report.Alert{}, false, nil
		}
		date = last
	}
	s, err := report.Summarize(envelopes, date, benchmark)
	if err != nil {
		return report.Alert{}, false, err
	}
	if !report.SessionClosedOn(envelopes, date) {
		return report.Alert{}, false, fmt.Errorf("the log records no session closed on %s, so that day is not complete; it is sent once it is", date)
	}
	return report.Alert{
		Key: "summary:" + s.AccountID + ":" + s.Date, Kind: kindSummary, Severity: report.Routine, AccountID: s.AccountID,
		Title: fmt.Sprintf("%s: daily summary %s", s.AccountID, s.Date), Body: report.RenderText(s),
	}, true, nil
}

func loadState(path string) (map[string]bool, error) {
	seen := map[string]bool{}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return seen, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reporter: read state: %w", err)
	}
	var d delivered
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("reporter: the state file is unreadable and is not reset, since a reset resends every alert: %w", err)
	}
	for _, k := range d.Keys {
		seen[k] = true
	}
	return seen, nil
}

// syncFile flushes a file to disk; a test replaces it to see a failure.
var syncFile = func(f *os.File) error { return f.Sync() }

// saveState replaces the state file atomically and durably: the new content
// is on disk before the rename, and the rename before it returns. Without
// that, a power cut can leave an empty state file, which loadState refuses,
// and every later run then fails.
func saveState(path string, seen map[string]bool) error {
	d := delivered{Keys: make([]string, 0, len(seen))}
	for k := range seen {
		d.Keys = append(d.Keys, k)
	}
	sort.Strings(d.Keys)
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".reporter-state-*")
	if err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("reporter: write state: %w", err)
	}
	if err := syncFile(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("reporter: write state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := syncFile(dir); err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	return nil
}
