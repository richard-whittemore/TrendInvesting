// Command reporter reads one account's journal and delivers what a person
// needs to know: alerts for every refusal, Degraded or Halted state,
// reconciliation that did not match, withheld decision, missing input, order
// not acknowledged in time, venue report the engine did not read, lost
// heartbeat and journal it cannot read, each once per incident, and
// optionally the daily summary (docs/live-trading/plan.md section 3; ADR
// 0019, "Alerting"). A Degraded, Halted or reconciliation alert is sent
// again every -repeat-interval until the journal records the owner's
// acknowledgement of it. Each delivery channel is validated on its own: one
// whose secret is empty or malformed is set aside, named in the run's error,
// in an urgent alert on the other channels and in the daily summary, and the
// others still deliver. It only reads the journal. It writes its own state
// file of delivered incident keys and when each channel last took each and,
// with -clock-check, the host's clock state and its lock, shared by every
// reporter on the host. Every rule lives in internal/report; this is
// composition.
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
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The defaults of -repeat-interval and -ack-within, which the reporter's
// timer is set against (deploy/systemd/ti-reporter@.timer).
const (
	defaultRepeatInterval = 30 * time.Minute
	defaultAckWithin      = 10 * time.Minute
)

func main() {
	if err := run(os.Args[1:], os.Getenv, time.Now, os.Stdout, http.DefaultClient); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// delivered is the reporter's state file: the incident keys already sent,
// per channel, when each channel last took each, which times a repeat, and
// how many repeats each channel has taken, which names each repeat.
type delivered struct {
	Keys    []string             `json:"keys"`
	Sent    map[string]time.Time `json:"sent,omitempty"`
	Repeats map[string]int       `json:"repeats,omitempty"`
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
	repeat := flags.Duration("repeat-interval", defaultRepeatInterval, "how often a Degraded, Halted or reconciliation alert is sent again until the journal records its acknowledgement (the owner's setting, ADR 0019)")
	secretsFile := flags.String("secrets-file", "", "KEY=VALUE file of channel secrets, read for any not set in the environment (under systemd: %d/secrets, the credential file)")
	ackWithin := flags.Duration("ack-within", defaultAckWithin, "longest an order to be sent may go without the venue acknowledging it, from its commit, before an alert fires")
	clockCheck := flags.String("clock-check", "", "command that checks the server's clock (deploy/libexec/clock-check.sh): run after the incident alerts at every firing, a failure two checks in a row pages once for the host, a passing check resolves it, and the daily summary shows the last check")
	clockState := flags.String("clock-state", "", "the host's clock state, shared by every reporter instance on the host (default: host-clock.json beside -state)")
	err := flags.Parse(args)
	if err != nil {
		return err
	}
	if *journalPath == "" || *statePath == "" {
		return errors.New("reporter: -journal and -state are required")
	}
	if *sessions < 2 {
		return errors.New("reporter: -heartbeat-sessions must be at least 2: a session's close is counted before the engine writes that session's records, so 1 would page on every trading day")
	}
	if *ackWithin <= 0 {
		return errors.New("reporter: -ack-within must be positive: an order is acknowledged some time after its commit, so a bound of zero would page on every order")
	}
	if *sendTimeout <= 0 {
		return errors.New("reporter: -send-timeout must be positive: a send with no bound can hold the run forever, and with it every later alert")
	}
	if *repeat <= 0 {
		return errors.New("reporter: -repeat-interval must be positive: an alert repeated on every run would page without pause")
	}
	// Every send is bounded, so a stalled channel fails as that channel's
	// failure, is retried next run, and never holds back the other alerts or
	// the state file.
	bounded := *client
	bounded.Timeout = *sendTimeout
	// A fault in the secrets file or in one channel's secrets sets aside only
	// what it touches: every other channel still delivers, and the run fails
	// naming each fault (ADR 0019, "Alerting").
	var failures []error
	if *secretsFile != "" {
		var secretsErr error
		if getenv, secretsErr = withSecretsFile(getenv, *secretsFile); secretsErr != nil {
			failures = append(failures, secretsErr)
		}
	}
	senders, setAside, err := buildSenders(*channels, getenv, &bounded, out)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, f := range setAside {
		failures = append(failures, f.err)
	}
	seen, sent, repeats, err := loadState(*statePath)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	label := *accountName
	if label == "" {
		label = filepath.Base(*journalPath)
	}
	info, statErr := os.Stat(*journalPath)
	earlier, envelopes, account, alerts, refused := readJournal(*journalPath, now(), *ackWithin, seen)
	if account != "" {
		label = account
	}
	// The heartbeat is checked first so a refused journal's alert can say
	// whether liveness is monitored.
	var heartbeat []report.Alert
	unchecked := ""
	if statErr != nil {
		unchecked = "the journal file has no modification time to check"
	} else {
		a, lost, err := report.HeartbeatAlert(label, info.ModTime(), now(), *sessions)
		if err != nil {
			// A check the calendar cannot answer is as silent as the lost
			// heartbeat it would have caught, so it reaches a person too.
			unchecked = "the calendar cannot answer: " + err.Error()
			failures = append(failures, err)
			heartbeat = append(heartbeat, report.HeartbeatUncheckedAlert(label, err, now()))
		} else if lost {
			heartbeat = append(heartbeat, a)
		}
	}
	if refused != nil {
		// A journal that cannot be read must not silence the one path that
		// reaches a person (ADR 0019, "Alerting"): the failure is itself an
		// alert, the heartbeat is checked where it can be, and the error is
		// returned.
		failures = append(failures, refused)
		alerts = []report.Alert{report.JournalRefusedAlert(label, refused, unchecked, now())}
	}
	alerts = append(alerts, heartbeat...)
	for _, f := range setAside {
		alerts = append(alerts, report.ChannelRefusedAlert(label, f.channel, f.err, now()))
	}

	// Each delivery is saved as soon as a channel takes it, so a run stopped
	// before it ends (systemd stops one at its TimeoutStartSec) makes the
	// next run send again at most the one alert on the one channel that had
	// taken it but was not yet saved: delivery is at-least-once, PagerDuty
	// folds the repeat by its key and ntfy posts it twice (ADR 0019,
	// "Alerting"). The state is saved once more at the end, so the
	// file exists after a run that delivered nothing.
	at := now()
	record := func() error { return saveState(*statePath, seen, sent, repeats) }
	for _, a := range report.Fresh(alerts, nil) {
		if err := deliver(senders, a, seen, sent, repeats, at, *repeat, record); err != nil {
			failures = append(failures, err)
		}
	}
	// The clock check comes after every incident alert is sent, so a slow or
	// hung check never holds one back (ADR 0019, "Alerting"; its bounds are in
	// clock.go). The clock is the host's: its state is shared by every
	// reporter instance on the host.
	var clock *report.ClockCheck
	if *clockCheck != "" {
		path := *clockState
		if path == "" {
			path = filepath.Join(filepath.Dir(*statePath), "host-clock.json")
		}
		c, err := clockStep(path, *clockCheck, hostName(), senders, setAside, now, *repeat)
		clock = c
		if err != nil {
			failures = append(failures, err)
		}
	}
	// A summary that cannot be built is reported, but never holds back the
	// incident alerts: they are what ADR 0019 requires to reach a person.
	if *summary && refused == nil {
		a, due, err := summaryAlert(earlier, envelopes, *date, *benchmark, clock)
		switch {
		case err != nil:
			failures = append(failures, fmt.Errorf("reporter: the daily summary was not sent: %w", err))
		case due:
			a.Body += notDelivering(setAside)
			if err := deliver(senders, a, seen, sent, repeats, at, *repeat, record); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := saveState(*statePath, seen, sent, repeats); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// readJournal reads, verifies and decodes the journal, returning its
// envelopes, its account and its alerts, including every venue report the
// engine did not read and every order to be sent not acknowledged within
// ackWithin of its commit by now, or the reason it was refused.
func readJournal(path string, now time.Time, ackWithin time.Duration, seen map[string]bool) (earlier, envelopes []event.Envelope, account string, alerts []report.Alert, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("reporter: read journal: %w", err)
	}
	_, entries, err := report.LoadEntries(raw)
	if err != nil {
		return nil, nil, "", nil, err
	}
	envelopes = make([]event.Envelope, len(entries))
	for i, e := range entries {
		envelopes[i] = e.Envelope
	}
	earlier, err = report.EarlierRuns(path, os.ReadFile)
	if err != nil {
		return nil, nil, "", nil, err
	}
	account, strategy, err := report.AccountOf(envelopes)
	if err != nil {
		return nil, nil, "", nil, err
	}
	alerts, err = report.Alerts(envelopes)
	if err != nil {
		return nil, nil, account, nil, err
	}
	orders, unread, err := report.OrderLifecycles(entries, strategy)
	if err != nil {
		return nil, nil, account, nil, err
	}
	alerts = append(alerts, report.UnreadReportAlerts(account, unread)...)
	alerts = append(alerts, report.UnacknowledgedAlerts(orders, now, ackWithin, func(key string) bool { return deliveredAnywhere(seen, key) })...)
	return earlier, envelopes, account, alerts, nil
}

// deliveredAnywhere reports whether any channel took the alert keyed key, so
// an owner who was told of an incident is told it cleared.
func deliveredAnywhere(seen map[string]bool, key string) bool {
	for k := range seen {
		if strings.HasSuffix(k, channelKey("", key)) {
			return true
		}
	}
	return false
}

// summaryAlert wraps a day's summary as a routine message keyed by account
// and date, so a rerun the same day does not resend it. A day is summarised
// only once the log records its session closed, since the day's decision and
// orders are written with that close: a summary sent earlier would be
// incomplete and, being keyed by the day, never replaced. With no date it is
// the latest closed session's day, and it is not yet due when none has
// closed. A stated date whose session has not closed is an error. A clock
// check run in this firing adds its time and result as the summary's last line.
func summaryAlert(earlier, envelopes []event.Envelope, date, benchmark string, clock *report.ClockCheck) (report.Alert, bool, error) {
	if date == "" {
		last, ok := report.ClosedSessionDate(envelopes)
		if !ok {
			return report.Alert{}, false, nil
		}
		date = last
	}
	s, err := report.SummarizeWithHistory(earlier, envelopes, date, benchmark)
	if err != nil {
		return report.Alert{}, false, err
	}
	if !report.SessionClosedOn(envelopes, date) {
		return report.Alert{}, false, fmt.Errorf("the log records no session closed on %s, so that day is not complete; it is sent once it is", date)
	}
	body := report.RenderText(s)
	if clock != nil {
		body += report.ClockSummaryLine(*clock) + "\n"
	}
	return report.Alert{
		Key: "summary:" + s.AccountID + ":" + s.Date, Kind: kindSummary, Severity: report.Routine, AccountID: s.AccountID,
		Title: fmt.Sprintf("%s: daily summary %s", s.AccountID, s.Date), Body: body,
	}, true, nil
}

// notDelivering is the daily summary's line naming each channel set aside
// this run, and why; empty when every channel named delivers.
func notDelivering(setAside []channelFault) string {
	if len(setAside) == 0 {
		return ""
	}
	names := make([]string, len(setAside))
	for i, f := range setAside {
		names[i] = fmt.Sprintf("%s (%s)", f.channel, f.err)
	}
	return "Channels not delivering: " + strings.Join(names, "; ") + "\n"
}

// loadState reads the delivered channel keys, when each was last sent and
// how many repeats each has taken. A key with no send time (a state file
// written before repeats) is due for its repeat at once, if it repeats; a key
// with no count has taken no repeat.
func loadState(path string) (seen map[string]bool, sent map[string]time.Time, repeats map[string]int, err error) {
	seen, sent, repeats = map[string]bool{}, map[string]time.Time{}, map[string]int{}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return seen, sent, repeats, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reporter: read state: %w", err)
	}
	var d delivered
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, nil, nil, fmt.Errorf("reporter: the state file is unreadable and is not reset, since a reset resends every alert: %w", err)
	}
	for _, k := range d.Keys {
		seen[k] = true
	}
	for k, at := range d.Sent {
		sent[k] = at
	}
	for k, n := range d.Repeats {
		repeats[k] = n
	}
	return seen, sent, repeats, nil
}

// syncFile flushes a file to disk; a test replaces it to see a failure.
var syncFile = func(f *os.File) error { return f.Sync() }

// saveState replaces the state file atomically and durably: the new content
// is on disk before the rename, and the rename before it returns. Without
// that, a power cut can leave an empty state file, which loadState refuses,
// and every later run then fails.
func saveState(path string, seen map[string]bool, sent map[string]time.Time, repeats map[string]int) error {
	raw, err := json.Marshal(deliveredOf(seen, sent, repeats))
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

// deliveredOf is the state file's form of the delivered keys, their send
// times and repeat counts.
func deliveredOf(seen map[string]bool, sent map[string]time.Time, repeats map[string]int) delivered {
	d := delivered{Keys: make([]string, 0, len(seen)), Sent: sent, Repeats: repeats}
	for k := range seen {
		d.Keys = append(d.Keys, k)
	}
	sort.Strings(d.Keys)
	return d
}

// writeAtomic replaces the file at path with raw, atomically and durably.
func writeAtomic(path string, raw []byte) error {
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
