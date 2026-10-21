package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func writeEntries(t *testing.T, path string, entries []journal.Entry) {
	t.Helper()
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "v", t0.Add(-time.Hour), t0), entries); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A journal that does not load, or a decision in it that does not decode,
// must not silence the one path that reaches a person: the failure is itself
// an urgent alert, the heartbeat is still checked, and then the error is
// returned (ADR 0019, "Alerting").
func TestAJournalTheReporterCannotReadStillAlertsAndChecksTheHeartbeat(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	modified := time.Date(2026, time.October, 12, 18, 0, 0, 0, newYork)
	now := func() time.Time { return time.Date(2026, time.October, 14, 16, 30, 0, 0, newYork) }
	for name, c := range map[string]struct {
		write   func(t *testing.T, path string)
		extra   []string
		account string
		want    string
	}{
		"garbage, named by the account flag": {
			write: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			extra: []string{"-account", "ira"}, account: "ira", want: "report:",
		},
		"garbage, named by the file": {
			write: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			account: "joint.journal", want: "report:",
		},
		"a decision at an unknown schema version": {
			write: func(t *testing.T, path string) {
				entries := haltedEntries(t)
				entries[3].Envelope.SchemaVersion = 99
				writeEntries(t, path, entries)
			},
			account: "joint", want: "schema version 99",
		},
		"an acknowledgement at an unknown schema version": {
			write: func(t *testing.T, path string) {
				ack := event.AlertAcknowledgedPayload{AccountID: "joint", AlertKey: "k", AcknowledgedBy: "owner", AcknowledgedAt: t0}
				entries := append(haltedEntries(t), journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 6, event.AlertAcknowledgedEventType, 99, "ack", t0, ack)})
				writeEntries(t, path, entries)
			},
			account: "joint", want: "schema version 99",
		},
		"an order plan at an unknown schema version": {
			write: func(t *testing.T, path string) {
				plan := event.OrderPlannedPayload{AccountID: "joint", ClientOrderID: "sale-1", InstrumentID: "SPY"}
				entries := append(haltedEntries(t), journal.Entry{Kind: journal.KindDecision, Envelope: envelope(t, 6, event.OrderPlannedEventType, 99, "plan-sale-1", t0, plan)})
				writeEntries(t, path, entries)
			},
			account: "joint", want: `"plan-sale-1" is at schema version 99`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "joint.journal")
			c.write(t, path)
			if err := os.Chtimes(path, modified, modified); err != nil {
				t.Fatal(err)
			}
			r := &relay{sent: map[string][]string{}}
			args := append([]string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty", "-summary"}, c.extra...)
			environment := env(map[string]string{"TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue"})
			err := run(args, environment, now, &bytes.Buffer{}, &http.Client{Transport: r})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the failure must still be returned, got %v", err)
			}
			sent := r.to("events.pagerduty.test")
			all := strings.Join(sent, "\n")
			for _, want := range []string{"The heartbeat is still checked", c.account + ": the reporter cannot read the journal", c.account + ": no engine heartbeat", `"severity":"critical"`} {
				if !strings.Contains(all, want) {
					t.Errorf("%q was not delivered:\n%s", want, all)
				}
			}
			if err := run(args, environment, now, &bytes.Buffer{}, &http.Client{Transport: r}); err == nil {
				t.Fatal("the failure must be returned on every run")
			}
			if again := r.to("events.pagerduty.test"); len(again) != len(sent) {
				t.Fatalf("one failure must be sent once, not every run: %d then %d", len(sent), len(again))
			}
		})
	}
}

// A journal path that does not exist is a journal the reporter cannot read:
// the refusal is delivered once, there is no modification time so the
// heartbeat is skipped, and the error is returned.
func TestAMissingJournalPathIsAlertedOnceWithoutAHeartbeatCheck(t *testing.T) {
	dir := t.TempDir()
	r := &relay{sent: map[string][]string{}}
	args := []string{"-journal", filepath.Join(dir, "missing.journal"), "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty", "-summary"}
	environment := env(map[string]string{"TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue"})
	now := func() time.Time { return t0.Add(time.Hour) }
	client := &http.Client{Transport: r}
	if err := run(args, environment, now, &bytes.Buffer{}, client); err == nil || !strings.Contains(err.Error(), "read journal") {
		t.Fatalf("the failure must be returned, got %v", err)
	}
	all := strings.Join(r.to("events.pagerduty.test"), "\n")
	if !strings.Contains(all, "missing.journal: the reporter cannot read the journal") || strings.Contains(all, "no engine heartbeat") || strings.Contains(all, "The heartbeat is still checked") || !strings.Contains(all, "The heartbeat is NOT checked: the journal file has no modification time") {
		t.Fatalf("want the refusal alert and no heartbeat alert:\n%s", all)
	}
	sent := len(r.to("events.pagerduty.test"))
	if err := run(args, environment, now, &bytes.Buffer{}, client); err == nil {
		t.Fatal("the failure must be returned on every run")
	}
	if again := len(r.to("events.pagerduty.test")); again != sent || sent != 1 {
		t.Fatalf("the refusal must be delivered once: %d then %d", sent, again)
	}
}

// A heartbeat check the calendar cannot answer is never a silent failure on a
// timer run: it is an urgent alert, once per distinct failure, and the error
// is still returned.
func TestAHeartbeatTheCalendarCannotAnswerIsAnUrgentAlertOnce(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ modified, now time.Time }{
		"the journal was last changed before the calendar begins": {time.Date(2020, time.March, 2, 12, 0, 0, 0, newYork), time.Date(2026, time.October, 14, 16, 30, 0, 0, newYork)},
		"now is after the calendar ends":                          {time.Date(2026, time.October, 12, 18, 0, 0, 0, newYork), time.Date(2035, time.January, 5, 16, 30, 0, 0, newYork)},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeJournal(t, dir)
			if err := os.Chtimes(path, c.modified, c.modified); err != nil {
				t.Fatal(err)
			}
			r := &relay{sent: map[string][]string{}}
			args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty"}
			environment := env(map[string]string{"TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue"})
			client := &http.Client{Transport: r}
			if err := run(args, environment, func() time.Time { return c.now }, &bytes.Buffer{}, client); err == nil {
				t.Fatal("the failure must be returned")
			}
			all := strings.Join(r.to("events.pagerduty.test"), "\n")
			if !strings.Contains(all, "The reporter cannot check the heartbeat:") || !strings.Contains(all, `"severity":"critical"`) {
				t.Fatalf("an urgent alert was not delivered:\n%s", all)
			}
			sent := len(r.to("events.pagerduty.test"))
			if err := run(args, environment, func() time.Time { return c.now }, &bytes.Buffer{}, client); err == nil {
				t.Fatal("the failure must be returned on every run")
			}
			if again := len(r.to("events.pagerduty.test")); again != sent {
				t.Fatalf("one failure must be sent once: %d then %d", sent, again)
			}
		})
	}
}

// An unreadable journal whose heartbeat check also fails must say so: the
// person paged has to know liveness is unmonitored, not be told it is checked.
func TestAnUnreadableJournalWhoseHeartbeatCannotBeCheckedSaysLivenessIsUnmonitored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, time.March, 2, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	r := &relay{sent: map[string][]string{}}
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty"}
	environment := env(map[string]string{"TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue"})
	now := func() time.Time { return time.Date(2026, time.October, 14, 16, 30, 0, 0, time.UTC) }
	if err := run(args, environment, now, &bytes.Buffer{}, &http.Client{Transport: r}); err == nil {
		t.Fatal("the failure must be returned")
	}
	all := strings.Join(r.to("events.pagerduty.test"), "\n")
	if !strings.Contains(all, "The heartbeat is NOT checked:") || strings.Contains(all, "The heartbeat is still checked") {
		t.Fatalf("the refusal must say the heartbeat is not checked:\n%s", all)
	}
}

// decidedDay is a day whose session has closed, with its decision.
func decidedDay(t *testing.T) []journal.Entry {
	t.Helper()
	entries := haltedEntries(t)[:2] // the configuration and the snapshot
	decided := event.TargetsDecidedPayload{AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, DecisionTime: t0, SessionPeriodEnd: t0,
		Outcome: event.TargetsOutcomeDecided, Targets: []event.TargetWeight{{InstrumentID: "SPY", Weight: 0.98}},
		Boost: event.BoostFigures{InstrumentID: "SPY", Close: 100}}
	return append(entries,
		journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 3, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session", t0,
			event.SessionClosedPayload{PeriodEnd: t0, InstrumentIDs: []string{"SPY"}})},
		journal.Entry{Kind: journal.KindDecision, Envelope: envelope(t, 4, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets", t0, decided)})
}

func TestTheSummaryWaitsForTheDaysSessionToCloseAndThenCarriesItsDecision(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "joint.journal")
	writeEntries(t, path, haltedEntries(t)[:2])
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-summary"}
	now := func() time.Time { return t0.Add(time.Hour) }
	var early bytes.Buffer
	if err := run(args, fakeEnv, now, &early, fakeClient); err != nil {
		t.Fatalf("a day whose session has not closed is not an error, only not yet due: %v", err)
	}
	if strings.Contains(early.String(), "daily summary") {
		t.Fatalf("a summary was sent before the day's session closed:\n%s", early.String())
	}
	writeEntries(t, path, decidedDay(t))
	var late bytes.Buffer
	if err := run(args, fakeEnv, now, &late, fakeClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(late.String(), "daily summary, 2024-01-02") || !strings.Contains(late.String(), "SPY 98%") {
		t.Fatalf("the complete summary, with the day's decision, must be delivered:\n%s", late.String())
	}
}

func TestASummaryForADateWhoseSessionHasNotClosedIsRefused(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-summary", "-date", "2024-01-03"}
	err := run(args, fakeEnv, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, fakeClient)
	if err == nil || !strings.Contains(err.Error(), "no session closed on 2024-01-03") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPagerDutyDedupKeyNeverExceedsTheEventsAPILimit(t *testing.T) {
	pd := &pagerDuty{status: http.StatusAccepted, body: `{"status":"success"}`}
	server := httptest.NewServer(http.HandlerFunc(pd.handler))
	defer server.Close()
	sender := pagerDutySender{url: server.URL, routingKey: "k", client: server.Client()}
	long := "engine-state:degraded:broker-discrepancy:SPY|" + strings.Repeat("tw-0123456789abcdef,", 20) + "|@joint"
	other := long[:len(long)-len("@joint")] + "@ira"
	for _, key := range []string{"refusal:short", long, other} {
		if err := sender.send(report.Alert{Key: key, AccountID: "joint", Severity: report.Urgent, Title: "t", Body: "b"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	got := pd.received()
	keys := make([]string, len(got))
	for i, m := range got {
		keys[i], _ = m["dedup_key"].(string)
		if len(keys[i]) > 255 {
			t.Errorf("dedup_key is %d characters; PagerDuty refuses over 255", len(keys[i]))
		}
	}
	if keys[0] != "refusal:short" {
		t.Errorf("a key within the limit must be sent as it is, got %q", keys[0])
	}
	if !strings.HasPrefix(keys[1], "engine-state:degraded:broker-discrepancy:") || keys[1] == keys[2] {
		t.Errorf("a long key keeps a readable prefix and stays distinct: %q, %q", keys[1], keys[2])
	}
	details, _ := got[1]["payload"].(map[string]any)["custom_details"].(map[string]any)
	if details["alert_key"] != long {
		t.Errorf("the full key must travel in custom_details, got %v", details)
	}
}
