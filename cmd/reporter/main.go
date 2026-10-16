// Command reporter reads one account's journal and delivers what a person
// needs to know: alerts for every refusal, Degraded or Halted state, withheld
// decision, missing input and lost heartbeat, each once per incident, and
// optionally the daily summary (docs/live-trading/plan.md section 3; ADR
// 0019, "Alerting"). It only reads the journal. The one file it writes is its
// own state file of delivered incident keys. Every rule lives in
// internal/report; this is composition.
//
// It runs once per invocation, from a timer. The heartbeat it checks is the
// journal file's modification time; a reporter on the engine's own host cannot
// be the external dead-man's switch ADR 0019 asks for, which is a separate
// service.
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
	channels := flags.String("channel", "stdout", "comma-separated delivery channels: stdout, ntfy")
	benchmark := flags.String("benchmark", "SPY", "benchmark instrument for the equity comparison")
	date := flags.String("date", "", "summary date, YYYY-MM-DD (default: the journal's latest event day)")
	summary := flags.Bool("summary", false, "also deliver the daily summary")
	timeout := flags.Duration("heartbeat-timeout", 36*time.Hour, "journal silence after which a heartbeat alert fires")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *journalPath == "" || *statePath == "" {
		return errors.New("reporter: -journal and -state are required")
	}
	senders, err := buildSenders(*channels, getenv, client, out)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*journalPath)
	if err != nil {
		return fmt.Errorf("reporter: read journal: %w", err)
	}
	info, err := os.Stat(*journalPath)
	if err != nil {
		return fmt.Errorf("reporter: stat journal: %w", err)
	}
	_, envelopes, err := report.Load(raw)
	if err != nil {
		return err
	}
	seen, err := loadState(*statePath)
	if err != nil {
		return err
	}

	alerts, err := report.Alerts(envelopes)
	if err != nil {
		return err
	}
	account, _, err := report.AccountOf(envelopes)
	if err != nil {
		return err
	}
	if a, lost := report.HeartbeatAlert(account, info.ModTime(), now(), *timeout); lost {
		alerts = append(alerts, a)
	}
	if *summary {
		a, err := summaryAlert(envelopes, *date, *benchmark)
		if err != nil {
			return err
		}
		alerts = append(alerts, a)
	}

	var failures []error
	for _, a := range report.Fresh(alerts, seen) {
		if err := deliver(senders, a); err != nil {
			failures = append(failures, err)
			continue
		}
		seen[a.Key] = true
	}
	if err := saveState(*statePath, seen); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// summaryAlert wraps the day's summary as a routine message keyed by account
// and date, so a rerun the same day does not resend it.
func summaryAlert(envelopes []event.Envelope, date, benchmark string) (report.Alert, error) {
	if date == "" {
		last, err := report.LastDate(envelopes)
		if err != nil {
			return report.Alert{}, err
		}
		date = last
	}
	s, err := report.Summarize(envelopes, date, benchmark)
	if err != nil {
		return report.Alert{}, err
	}
	return report.Alert{
		Key: "summary:" + s.AccountID + ":" + s.Date, Severity: report.Routine, AccountID: s.AccountID,
		Title: fmt.Sprintf("%s: daily summary %s", s.AccountID, s.Date), Body: report.RenderText(s),
	}, nil
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

// saveState replaces the state file atomically.
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
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("reporter: write state: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}
