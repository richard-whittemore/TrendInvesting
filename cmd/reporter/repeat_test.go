package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// The alert-delivery seam: what reaches each channel, run after run.

// writeEntries writes entries as an end-of-run journal, returning its path.
func writeRun(t *testing.T, dir string, entries []journal.Entry) string {
	t.Helper()
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "v", t0.Add(-time.Hour), t0), entries); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// twoChannels is a private ntfy server and PagerDuty, both answered by r.
func twoChannels() func(string) string {
	return env(map[string]string{
		"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": "https://ntfy.private.test",
		"TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue",
	})
}

func count(messages []string, sub string) int {
	n := 0
	for _, m := range messages {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

func alertKey(t *testing.T, entries []journal.Entry, kind report.Kind) string {
	t.Helper()
	var envelopes []event.Envelope
	for _, e := range entries {
		envelopes = append(envelopes, e.Envelope)
	}
	alerts, err := report.Alerts(envelopes)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		if a.Kind == kind {
			return a.Key
		}
	}
	t.Fatalf("no %s alert", kind)
	return ""
}

func TestAnUnacknowledgedHaltRepeatsOnBothChannelsUntilAcknowledged(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	client := &http.Client{Transport: r}
	dir := t.TempDir()
	entries := haltedEntries(t)
	path := writeRun(t, dir, entries)
	if err := os.Chtimes(path, t0, t0); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-repeat-interval", "30m"}
	runAt := func(after time.Duration) {
		t.Helper()
		if err := run(args, twoChannels(), func() time.Time { return t0.Add(after) }, &bytes.Buffer{}, client); err != nil {
			t.Fatal(err)
		}
	}
	runAt(time.Hour)
	runAt(time.Hour + 10*time.Minute)
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		if got := r.to(host); count(got, "engine halted") != 1 || count(got, "order refused") != 1 {
			t.Fatalf("%s: each alert once before the interval, got:\n%s", host, strings.Join(got, "\n---\n"))
		}
	}
	runAt(time.Hour + 31*time.Minute)
	for _, host := range []string{"ntfy.private.test", "events.pagerduty.test"} {
		if got := r.to(host); count(got, "engine halted") != 2 || count(got, "order refused") != 1 {
			t.Fatalf("%s: the halt repeats and the refusal does not:\n%s", host, strings.Join(got, "\n---\n"))
		}
	}

	ack := event.AlertAcknowledgedPayload{AccountID: "joint", AlertKey: alertKey(t, entries, report.KindHalted), AcknowledgedBy: "owner", AcknowledgedAt: t0.Add(time.Hour)}
	entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 6, event.AlertAcknowledgedEventType, event.AlertAcknowledgedSchemaVersion, "ack", t0, ack)})
	if err := os.Chtimes(writeRun(t, dir, entries), t0, t0); err != nil {
		t.Fatal(err)
	}
	runAt(3 * time.Hour)
	if got := r.to("ntfy.private.test"); count(got, "engine halted") != 2 {
		t.Fatalf("an acknowledged alert is not repeated:\n%s", strings.Join(got, "\n---\n"))
	}
}

// mismatchEntries is a Boost run whose morning reconciliation found SPY and
// the cash short, with the result and the Degraded state it caused.
func mismatchEntries(t *testing.T) []journal.Entry {
	entries := haltedEntries(t)[:3]
	obs := event.ReconciliationPayload{
		AccountID: "joint", ReconciliationID: "morning", Trigger: event.ReconciliationTriggerMorning, AsOf: t0,
		Unreadable: []string{}, Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 140}}, TotalCash: -4712.3, Currency: "USD",
	}
	result := event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "morning", InputID: "reconciliation", AnchorID: "snap",
		Classification: event.ReconciliationUnexplained, Action: event.ReconciliationActionDegrade,
		Components:         []event.ReconciliationComponent{{Component: event.ReconciliationComponentPosition, InstrumentID: "SPY", Book: 147, Expected: 147, Observed: 140, Residual: -7, Explanations: []string{}}},
		ScopeInstrumentIDs: []string{"SPY"}, Detail: "unexplained: SPY: book 147, expected 147, broker 140 (residual -7); net liquidation 8530.00",
	}
	state := event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonReconciliationUnexplained, Detail: "reconciliation morning", ScopeInstrumentIDs: []string{"SPY"}}
	r := envelope(t, 5, event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "result", obs.AsOf, result)
	s := envelope(t, 6, event.EngineStateEventType, event.EngineStateScopedVersion, "state", obs.AsOf, state)
	r.CausationID, s.CausationID = "reconciliation", "reconciliation"
	return append(entries,
		journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 4, event.ReconciliationEventType, event.ReconciliationSchemaVersion, "reconciliation", obs.AsOf, obs)},
		journal.Entry{Kind: journal.KindDecision, Envelope: r},
		journal.Entry{Kind: journal.KindDecision, Envelope: s},
	)
}

func TestAMismatchIsOneAlertAndThePublicRelayGetsNoFigures(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	dir := t.TempDir()
	path := writeRun(t, dir, mismatchEntries(t))
	if err := os.Chtimes(path, t0, t0); err != nil {
		t.Fatal(err)
	}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": "https://events.pagerduty.test/v2/enqueue"})
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty"}
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, &http.Client{Transport: r}); err != nil {
		t.Fatal(err)
	}
	public, paged := r.to("ntfy.sh"), r.to("events.pagerduty.test")
	if len(public) != 1 || len(paged) != 1 {
		t.Fatalf("one alert per channel for the mismatch and its state:\nntfy %v\npagerduty %v", public, paged)
	}
	if !strings.Contains(public[0], "SPY") || strings.Contains(public[0], "8530") || strings.Contains(public[0], "140") {
		t.Errorf("the public relay names what is in doubt and no figure:\n%s", public[0])
	}
	if !strings.Contains(paged[0], "8530") {
		t.Errorf("the private channel gets the figures:\n%s", paged[0])
	}
}

func TestTheRepeatIntervalMustBePositive(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-repeat-interval", "0s"}
	if err := run(args, fakeEnv, time.Now, &bytes.Buffer{}, fakeClient); err == nil || !strings.Contains(err.Error(), "-repeat-interval must be positive") {
		t.Fatalf("a zero interval would page on every run, got %v", err)
	}
}

// Each repeat must page again: PagerDuty folds a trigger whose dedup key
// matches an open incident into that incident, and an incident acknowledged
// on the phone takes no further triggers, so a repeat under the same key would
// page no one. The rule (ADR 0019, ADR 0026) is that only an acknowledgement
// recorded in the journal stops the repeats.
func TestEachPagerDutyRepeatOpensANewIncidentUntilTheJournalAcknowledges(t *testing.T) {
	r := &relay{sent: map[string][]string{}}
	client := &http.Client{Transport: r}
	dir := t.TempDir()
	entries := haltedEntries(t)
	path := writeRun(t, dir, entries)
	if err := os.Chtimes(path, t0, t0); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty", "-repeat-interval", "30m"}
	runAt := func(after time.Duration) {
		t.Helper()
		if err := run(args, twoChannels(), func() time.Time { return t0.Add(after) }, &bytes.Buffer{}, client); err != nil {
			t.Fatal(err)
		}
	}
	halted := alertKey(t, entries, report.KindHalted)
	pages := func() (keys map[string]bool, n int) {
		t.Helper()
		keys = map[string]bool{}
		for _, m := range r.to("events.pagerduty.test") {
			var e struct {
				DedupKey string `json:"dedup_key"`
				Payload  struct {
					CustomDetails map[string]string `json:"custom_details"`
				} `json:"payload"`
			}
			if err := json.Unmarshal([]byte(m[strings.Index(m, "\n")+1:]), &e); err != nil {
				t.Fatal(err)
			}
			if e.Payload.CustomDetails["alert_key"] != halted {
				continue
			}
			keys[e.DedupKey] = true
			n++
		}
		return keys, n
	}
	runAt(time.Hour)
	runAt(time.Hour + 31*time.Minute)
	runAt(2*time.Hour + 2*time.Minute)
	keys, n := pages()
	if n != 3 || len(keys) != 3 {
		t.Fatalf("the halt and two repeats must be three incidents, got %d triggers under %d dedup keys: %v", n, len(keys), keys)
	}
	if !keys[halted] {
		t.Errorf("the first trigger keeps the alert key as its dedup key, got %v", keys)
	}

	ack := event.AlertAcknowledgedPayload{AccountID: "joint", AlertKey: halted, AcknowledgedBy: "owner", AcknowledgedAt: t0.Add(time.Hour)}
	entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 6, event.AlertAcknowledgedEventType, event.AlertAcknowledgedSchemaVersion, "ack", t0, ack)})
	if err := os.Chtimes(writeRun(t, dir, entries), t0, t0); err != nil {
		t.Fatal(err)
	}
	runAt(4 * time.Hour)
	if _, after := pages(); after != 3 {
		t.Fatalf("an acknowledgement recorded in the journal stops the pages, got %d triggers", after)
	}
}
