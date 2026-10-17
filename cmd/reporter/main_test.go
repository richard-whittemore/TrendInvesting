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

// haltedEntries is a synthetic Boost run with one refused order and a Halted
// engine.
func haltedEntries(t *testing.T) []journal.Entry {
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
	return entries
}

// writeJournal writes the run as an end-of-run journal, returning its path.
func writeJournal(t *testing.T, dir string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := journal.Write(&buf, journal.NewHeader("h", "v", t0.Add(-time.Hour), t0), haltedEntries(t)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "joint.journal")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// acceptingNtfy is a delivery channel that takes everything: the environment
// that configures it and the client that reaches it.
func acceptingNtfy(t *testing.T) (func(string) string, *http.Client) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	return env(map[string]string{"TI_NTFY_TOPIC": "topic", "TI_NTFY_SERVER": server.URL}), server.Client()
}

type syncBuffer struct{ bytes.Buffer }

func (*syncBuffer) Sync() error { return nil }

// writeDurableLog writes the same run as the durable log a live engine
// streams: the configuration and the snapshot are inputs, each opening a group
// that the decisions after it join.
func writeDurableLog(t *testing.T, dir string) string {
	t.Helper()
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	var group []journal.Entry
	flush := func() {
		if len(group) == 0 {
			return
		}
		if err := log.Append(group); err != nil {
			t.Fatal(err)
		}
		group = nil
	}
	for _, e := range haltedEntries(t) {
		if e.Kind == journal.KindInput {
			flush()
		}
		group = append(group, e)
	}
	flush()
	path := filepath.Join(dir, "joint.log")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRunDeliversEachAlertOnceAcrossRuns(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	state := filepath.Join(dir, "state.json")
	args := []string{"-journal", journalPath, "-state", state, "-channel", "stdout,ntfy", "-heartbeat-timeout", "1000h"}
	now := func() time.Time { return t0.Add(time.Hour) }

	var first bytes.Buffer
	if err := run(args, fakeEnv, now, &first, fakeClient); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[urgent]", "order refused", "engine halted"} {
		if !strings.Contains(first.String(), want) {
			t.Errorf("first run lacks %q:\n%s", want, first.String())
		}
	}
	var second bytes.Buffer
	if err := run(args, fakeEnv, now, &second, fakeClient); err != nil {
		t.Fatal(err)
	}
	if second.Len() != 0 {
		t.Fatalf("second run repeated alerts:\n%s", second.String())
	}
}

func TestRunAlertsOnHeartbeatLossFromTheFileTime(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	modified := time.Date(2024, time.January, 3, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(journalPath, modified, modified); err != nil {
		t.Fatal(err)
	}
	args := []string{"-journal", journalPath, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-heartbeat-timeout", "2h"}
	var out bytes.Buffer
	if err := run(args, fakeEnv, func() time.Time { return modified.Add(3 * time.Hour) }, &out, fakeClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no engine heartbeat") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRunSendsASummaryOncePerDay(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-summary", "-heartbeat-timeout", "1000h"}
	now := func() time.Time { return t0.Add(time.Hour) }
	var first, second bytes.Buffer
	if err := run(args, fakeEnv, now, &first, fakeClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "daily summary, 2024-01-02") || !strings.Contains(first.String(), "[routine]") {
		t.Fatalf("output:\n%s", first.String())
	}
	if err := run(args, fakeEnv, now, &second, fakeClient); err != nil {
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
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	path := writeJournal(t, dir)
	before, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-summary", "-heartbeat-timeout", "1000h"}
	if err := run(args, fakeEnv, func() time.Time { return t0 }, &bytes.Buffer{}, fakeClient); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	again, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !info.ModTime().Equal(again.ModTime()) {
		t.Fatal("the reporter changed the journal")
	}
}

func TestStdoutAloneIsNotADeliveryChannel(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "s.json")
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "stdout", "-heartbeat-timeout", "1000h"}
	var out bytes.Buffer
	err := run(args, env(nil), func() time.Time { return t0.Add(time.Hour) }, &out, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "delivery channel") {
		t.Fatalf("a run that can reach no one must say so, got %v", err)
	}
	if _, statErr := os.Stat(state); statErr == nil {
		t.Fatal("nothing was delivered, so nothing may be recorded as delivered")
	}
}

// pagerDuty is a fake Events API: it records what it was sent and answers as
// the real one does.
type pagerDuty struct {
	status int
	body   string
	got    []map[string]any
}

func (p *pagerDuty) handler(w http.ResponseWriter, r *http.Request) {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	p.got = append(p.got, m)
	w.WriteHeader(p.status)
	_, _ = w.Write([]byte(p.body))
}

func TestPagerDutyTriggersWithTheAlertKeyAndReadsTheAcknowledgementBack(t *testing.T) {
	pd := &pagerDuty{status: http.StatusAccepted, body: `{"status":"success","message":"Event processed","dedup_key":"x"}`}
	server := httptest.NewServer(http.HandlerFunc(pd.handler))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty", "-heartbeat-timeout", "1000h"}
	environment := env(map[string]string{"TI_PAGERDUTY_ROUTING_KEY": "routing-secret-9", "TI_PAGERDUTY_URL": server.URL})
	var out bytes.Buffer
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &out, server.Client()); err != nil {
		t.Fatal(err)
	}
	if len(pd.got) == 0 {
		t.Fatal("nothing was sent")
	}
	first := pd.got[0]
	payload, _ := first["payload"].(map[string]any)
	if first["routing_key"] != "routing-secret-9" || first["event_action"] != "trigger" || first["dedup_key"] == "" || payload["severity"] == "" || payload["summary"] == "" {
		t.Fatalf("request = %v", first)
	}
	if strings.Contains(out.String(), "routing-secret-9") {
		t.Fatalf("routing key printed:\n%s", out.String())
	}
}

func TestPagerDutyAnswerThatIsNotASuccessIsNotADelivery(t *testing.T) {
	for name, pd := range map[string]*pagerDuty{
		"http error":          {status: http.StatusBadRequest, body: `{"status":"invalid event"}`},
		"accepted not stated": {status: http.StatusAccepted, body: `{"status":"throttled"}`},
		"unreadable answer":   {status: http.StatusAccepted, body: `not json`},
	} {
		server := httptest.NewServer(http.HandlerFunc(pd.handler))
		dir := t.TempDir()
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty", "-heartbeat-timeout", "1000h"}
		environment := env(map[string]string{"TI_PAGERDUTY_ROUTING_KEY": "routing-secret-9", "TI_PAGERDUTY_URL": server.URL})
		err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client())
		server.Close()
		if err == nil || strings.Contains(err.Error(), "routing-secret-9") {
			t.Errorf("%s: error = %v", name, err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "s.json")); statErr == nil {
			raw, _ := os.ReadFile(filepath.Join(dir, "s.json"))
			if strings.Contains(string(raw), "pagerduty:") {
				t.Errorf("%s: recorded as delivered: %s", name, raw)
			}
		}
	}
}

func TestPagerDutyWithoutARoutingKeyIsAConfigurationError(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty"}
	if err := run(args, env(nil), time.Now, &bytes.Buffer{}, http.DefaultClient); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAFailureOnOneChannelIsNeverADeliveryAndOnlyThatChannelIsRetried(t *testing.T) {
	var ntfyHits int
	ntfy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ntfyHits++ }))
	defer ntfy.Close()
	pd := &pagerDuty{status: http.StatusInternalServerError, body: `{}`}
	pager := httptest.NewServer(http.HandlerFunc(pd.handler))
	defer pager.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty", "-heartbeat-timeout", "1000h"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": ntfy.URL, "TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": pager.URL})
	now := func() time.Time { return t0.Add(time.Hour) }
	if err := run(args, environment, now, &bytes.Buffer{}, http.DefaultClient); err == nil {
		t.Fatal("ntfy took the alerts but PagerDuty did not: that is not delivered")
	}
	sent := ntfyHits
	pd.status, pd.body, pd.got = http.StatusAccepted, `{"status":"success"}`, nil
	if err := run(args, environment, now, &bytes.Buffer{}, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if ntfyHits != sent {
		t.Fatalf("ntfy was sent %d alerts again though it had taken them", ntfyHits-sent)
	}
	if len(pd.got) == 0 {
		t.Fatal("PagerDuty was not retried")
	}
	pd.got = nil
	if err := run(args, environment, now, &bytes.Buffer{}, http.DefaultClient); err != nil || len(pd.got) != 0 {
		t.Fatalf("delivered on every channel, yet sent again: %v %d", err, len(pd.got))
	}
}

func TestRunReadsAnEngineDurableLog(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	path := writeDurableLog(t, dir)
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-heartbeat-timeout", "1000h"}
	var out bytes.Buffer
	if err := run(args, fakeEnv, func() time.Time { return t0.Add(time.Hour) }, &out, fakeClient); err != nil {
		t.Fatalf("a live engine writes only the durable log: %v", err)
	}
	if !strings.Contains(out.String(), "engine halted") {
		t.Fatalf("output:\n%s", out.String())
	}
}
