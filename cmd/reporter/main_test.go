package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

var t0 = time.Date(2024, time.January, 2, 21, 0, 0, 0, time.UTC)

func envelope(t *testing.T, seq uint64, eventType string, version uint32, id string, at time.Time, payload any) event.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return event.Envelope{ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
		EventTime: at, RecordedAt: at, Sequence: seq, Source: "test", StrategyVersion: "v", ConfigurationHash: "h",
		PayloadHash: event.HashPayload(raw), Payload: raw}
}

// writeJournal writes a synthetic Boost journal with one refused order and a
// Halted engine, returning its path.
func writeJournal(t *testing.T, dir string) string {
	t.Helper()
	cfg := event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR}
	snap := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: t0, Currency: "USD", NetLiquidation: 4000}
	refused := event.OrderRefusedPayload{AccountID: "joint", PlanID: "p", Tag: "targets:0", ClientOrderID: "tw-a", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 39, LimitPrice: 101, Reason: event.RefusedOrderSize, Detail: "figures"}
	state := event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "figures"}
	entries := []journal.Entry{
		{Kind: journal.KindInput, Envelope: envelope(t, 1, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "cfg", t0.Add(-time.Hour), cfg)},
		{Kind: journal.KindInput, Envelope: envelope(t, 2, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "snap", t0, snap)},
		{Kind: journal.KindDecision, Envelope: envelope(t, 3, event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, "refuse", t0, refused)},
		{Kind: journal.KindDecision, Envelope: envelope(t, 4, event.EngineStateEventType, event.EngineStateScopedVersion, "state", t0, state)},
	}
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

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRunDeliversEachAlertOnceAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	state := filepath.Join(dir, "state.json")
	args := []string{"-journal", journalPath, "-state", state, "-channel", "stdout", "-heartbeat-timeout", "1000h"}
	now := func() time.Time { return t0.Add(time.Hour) }

	var first bytes.Buffer
	if err := run(args, env(nil), now, &first, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[urgent]", "order refused", "engine halted"} {
		if !strings.Contains(first.String(), want) {
			t.Errorf("first run lacks %q:\n%s", want, first.String())
		}
	}
	var second bytes.Buffer
	if err := run(args, env(nil), now, &second, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if second.Len() != 0 {
		t.Fatalf("second run repeated alerts:\n%s", second.String())
	}
}

func TestRunAlertsOnHeartbeatLossFromTheFileTime(t *testing.T) {
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	modified := time.Date(2024, time.January, 3, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(journalPath, modified, modified); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", journalPath, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout", "-heartbeat-timeout", "2h"}
	var out bytes.Buffer
	if err := run(args, env(nil), func() time.Time { return modified.Add(3 * time.Hour) }, &out, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no engine heartbeat") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRunSendsASummaryOncePerDay(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "stdout", "-summary", "-heartbeat-timeout", "1000h"}
	now := func() time.Time { return t0.Add(time.Hour) }
	var first, second bytes.Buffer
	if err := run(args, env(nil), now, &first, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "daily summary, 2024-01-02") || !strings.Contains(first.String(), "[routine]") {
		t.Fatalf("output:\n%s", first.String())
	}
	if err := run(args, env(nil), now, &second, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.String(), "daily summary") {
		t.Fatalf("summary repeated:\n%s", second.String())
	}
}

func TestNtfyPostsToTheTopicWithPriorityAndNeverEchoesIt(t *testing.T) {
	var gotPath, gotPriority, gotTitle, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotPriority, gotTitle = r.URL.Path, r.Header.Get("Priority"), r.Header.Get("Title")
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		gotBody = b.String()
	}))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-heartbeat-timeout", "1000h"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "secret-topic-123", "TI_NTFY_SERVER": server.URL})
	var out bytes.Buffer
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &out, server.Client()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/secret-topic-123" || gotPriority != "5" || !strings.Contains(gotTitle, "joint") || !strings.Contains(gotBody, "Engine halted") {
		t.Fatalf("path %q priority %q title %q body %q", gotPath, gotPriority, gotTitle, gotBody)
	}
	if strings.Contains(out.String(), "secret-topic-123") {
		t.Fatalf("topic printed:\n%s", out.String())
	}
}

func TestNtfyFailureIsReportedWithoutTheTopicAndRetriedNextRun(t *testing.T) {
	fail := true
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-heartbeat-timeout", "1000h"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "secret-topic-123", "TI_NTFY_SERVER": server.URL})
	now := func() time.Time { return t0.Add(time.Hour) }
	err := run(args, environment, now, &bytes.Buffer{}, server.Client())
	if err == nil || strings.Contains(err.Error(), "secret-topic-123") {
		t.Fatalf("error = %v", err)
	}
	fail = false
	hits = 0
	if err := run(args, environment, now, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("a failed delivery must be retried: %d posts", hits)
	}
}

func TestNtfyWithoutATopicIsAConfigurationError(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy"}
	if err := run(args, env(nil), time.Now, &bytes.Buffer{}, http.DefaultClient); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRunRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	good := writeJournal(t, dir)
	bad := filepath.Join(dir, "bad.journal")
	if err := os.WriteFile(bad, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"no journal flag":  {"-state", "s"},
		"no state flag":    {"-journal", good},
		"missing file":     {"-journal", filepath.Join(dir, "none"), "-state", filepath.Join(dir, "s")},
		"unverifiable":     {"-journal", bad, "-state", filepath.Join(dir, "s")},
		"unknown channel":  {"-journal", good, "-state", filepath.Join(dir, "s"), "-channel", "pigeon"},
		"bad flag":         {"-nope"},
		"bad summary date": {"-journal", good, "-state", filepath.Join(dir, "s"), "-summary", "-date", "x"},
	}
	for name, args := range cases {
		if err := run(args, env(nil), time.Now, &bytes.Buffer{}, http.DefaultClient); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	corrupt := filepath.Join(dir, "corrupt-state.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-journal", good, "-state", corrupt, "-channel", "stdout"}, env(nil), time.Now, &bytes.Buffer{}, http.DefaultClient); err == nil {
		t.Fatal("a corrupt state file must not be silently reset: it would resend every alert")
	}
}

func TestTheJournalFileIsNeverWritten(t *testing.T) {
	dir := t.TempDir()
	path := writeJournal(t, dir)
	before, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout", "-summary", "-heartbeat-timeout", "1000h"}
	if err := run(args, env(nil), func() time.Time { return t0 }, &bytes.Buffer{}, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	again, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !info.ModTime().Equal(again.ModTime()) {
		t.Fatal("the reporter changed the journal")
	}
}
