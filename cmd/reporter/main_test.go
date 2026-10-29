package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	cfg := event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR, Boost: event.BoostConfiguration{InstrumentID: "SPY", InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 10, TrendDays: 200, ExitSMA: 5, WarmUpStart: "2024-01-02"},
		Limits:    event.MarginLimits{MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 1, PriceBand: 0.05, ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30},
		Execution: event.ExecutionConfiguration{MaxSnapshotAge: 30}}
	snap := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: t0, Currency: "USD", NetLiquidation: 4000}
	refused := event.OrderRefusedPayload{AccountID: "joint", PlanID: "p", Tag: "targets:0", ClientOrderID: "tw-a", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 39, LimitPrice: 101, Reason: event.RefusedOrderSize, Detail: "figures",
		Figures: event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 0.5, ExposureAfter: 0.98, OrderFraction: 0.48, OrdersInDecision: 1}}
	state := event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "figures"}
	closed := event.SessionClosedPayload{PeriodEnd: t0, InstrumentIDs: []string{"SPY"}}
	entries := []journal.Entry{
		{Kind: journal.KindInput, Envelope: envelope(t, 1, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "cfg", t0.Add(-time.Hour), cfg)},
		{Kind: journal.KindInput, Envelope: envelope(t, 2, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "snap", t0, snap)},
		{Kind: journal.KindInput, Envelope: envelope(t, 3, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session", t0, closed)},
		{Kind: journal.KindDecision, Envelope: envelope(t, 4, event.OrderRefusedEventType, event.OrderRefusedSchemaVersion, "refuse", t0, refused)},
		{Kind: journal.KindDecision, Envelope: envelope(t, 5, event.EngineStateEventType, event.EngineStateScopedVersion, "state", t0, state)},
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
	r := &relay{sent: map[string][]string{}}
	fakeEnv := env(map[string]string{"TI_NTFY_TOPIC": "topic", "TI_NTFY_SERVER": "https://ntfy.test"})
	fakeClient := &http.Client{Transport: r}
	dir := t.TempDir()
	journalPath := writeJournal(t, dir)
	state := filepath.Join(dir, "state.json")
	args := []string{"-journal", journalPath, "-state", state, "-channel", "stdout,ntfy"}
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
	const notice = "reporter: heartbeat: not this instance\n"
	if strings.Count(first.String(), notice) != 1 {
		t.Errorf("first run lacks single informational notice:\n%s", first.String())
	}
	delivered := len(r.to("ntfy.test"))
	if delivered == 0 {
		t.Fatal("first run delivered no alerts")
	}
	var second bytes.Buffer
	if err := run(args, fakeEnv, now, &second, fakeClient); err != nil {
		t.Fatal(err)
	}
	if second.String() != notice {
		t.Errorf("second run must print only the informational notice:\n%s", second.String())
	}
	if got := len(r.to("ntfy.test")); got != delivered {
		t.Errorf("second run repeated deliveries: got %d, want %d", got, delivered)
	}
	if anyContains(r.to("ntfy.test"), "heartbeat: not this instance") {
		t.Error("informational heartbeat notice was delivered as an alert")
	}
}

func TestRunAlertsOnHeartbeatLossFromTheFileTime(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		modified, now time.Time
		lost          bool
	}{
		// Monday's session written, Tuesday's and Wednesday's closed with nothing.
		"two sessions closed": {time.Date(2026, time.October, 12, 18, 0, 0, 0, newYork), time.Date(2026, time.October, 14, 16, 30, 0, 0, newYork), true},
		// Friday's session written; nothing is due over the weekend, 63 hours later.
		"a weekend": {time.Date(2026, time.October, 16, 17, 30, 0, 0, newYork), time.Date(2026, time.October, 19, 9, 0, 0, 0, newYork), false},
	} {
		t.Run(name, func(t *testing.T) {
			fakeEnv, fakeClient := acceptingNtfy(t)
			dir := t.TempDir()
			journalPath := writeJournal(t, dir)
			if err := os.Chtimes(journalPath, c.modified, c.modified); err != nil {
				t.Fatal(err)
			}
			args := []string{"-journal", journalPath, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy"}
			var out bytes.Buffer
			if err := run(args, fakeEnv, func() time.Time { return c.now }, &out, fakeClient); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(out.String(), "no engine heartbeat"); got != c.lost {
				t.Fatalf("heartbeat alert %v, want %v:\n%s", got, c.lost, out.String())
			}
		})
	}
}

// The close is counted before the session's group is written, so one session
// of allowed silence would page on every trading day.
func TestTheHeartbeatSessionsMustBeAtLeastTwo(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	for _, n := range []string{"0", "1"} {
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-heartbeat-sessions", n}
		if err := run(args, fakeEnv, time.Now, &bytes.Buffer{}, fakeClient); err == nil || !strings.Contains(err.Error(), "-heartbeat-sessions must be at least 2") {
			t.Fatalf("-heartbeat-sessions %s: err = %v", n, err)
		}
	}
}

func TestRunSendsASummaryOncePerDay(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-summary"}
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
	var mu sync.Mutex
	var gotPath, gotPriority, gotTitle, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		mu.Lock()
		defer mu.Unlock()
		gotPath, gotPriority, gotTitle = r.URL.Path, r.Header.Get("Priority"), r.Header.Get("Title")
		gotBody = b.String()
	}))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "secret-topic-123", "TI_NTFY_SERVER": server.URL})
	var out bytes.Buffer
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &out, server.Client()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/secret-topic-123" || gotPriority != "5" || !strings.Contains(gotTitle, "joint") || !strings.Contains(gotBody, "Engine halted") {
		t.Fatalf("path %q priority %q title %q body %q", gotPath, gotPriority, gotTitle, gotBody)
	}
	if strings.Contains(out.String(), "secret-topic-123") {
		t.Fatalf("topic printed:\n%s", out.String())
	}
}

func TestNtfyFailureIsReportedWithoutTheTopicAndRetriedNextRun(t *testing.T) {
	var mu sync.Mutex
	fail := true
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "secret-topic-123", "TI_NTFY_SERVER": server.URL})
	now := func() time.Time { return t0.Add(time.Hour) }
	err := run(args, environment, now, &bytes.Buffer{}, server.Client())
	if err == nil || strings.Contains(err.Error(), "secret-topic-123") {
		t.Fatalf("error = %v", err)
	}
	mu.Lock()
	fail, hits = false, 0
	mu.Unlock()
	if err := run(args, environment, now, &bytes.Buffer{}, server.Client()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
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
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	good := writeJournal(t, dir)
	bad := filepath.Join(dir, "bad.journal")
	if err := os.WriteFile(bad, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "corrupt-state.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "s")
	// Each case names a real channel, so it fails at the input it names and
	// not at sender setup.
	cases := map[string]struct {
		args []string
		want string
	}{
		"no journal flag":  {[]string{"-state", "s", "-channel", "ntfy"}, "-journal and -state are required"},
		"no state flag":    {[]string{"-journal", good, "-channel", "ntfy"}, "-journal and -state are required"},
		"missing file":     {[]string{"-journal", filepath.Join(dir, "none"), "-state", state, "-channel", "ntfy"}, "read journal"},
		"unverifiable":     {[]string{"-journal", bad, "-state", state, "-channel", "ntfy"}, "report:"},
		"unknown channel":  {[]string{"-journal", good, "-state", state, "-channel", "pigeon"}, "unknown channel"},
		"bad flag":         {[]string{"-nope"}, "not defined"},
		"bad summary date": {[]string{"-journal", good, "-state", state, "-channel", "ntfy", "-summary", "-date", "x"}, "is not YYYY-MM-DD"},
		// A reset would resend every alert, so a corrupt state file is refused.
		"corrupt state": {[]string{"-journal", good, "-state", corrupt, "-channel", "ntfy"}, "state file is unreadable"},
	}
	for name, c := range cases {
		err := run(c.args, fakeEnv, time.Now, &bytes.Buffer{}, fakeClient)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want one containing %q", name, err, c.want)
		}
	}
}

func TestTheJournalFileIsNeverWritten(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	path := writeJournal(t, dir)
	before, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy", "-summary"}
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
	args := []string{"-journal", writeJournal(t, dir), "-state", state, "-channel", "stdout"}
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
	mu     sync.Mutex
	status int
	body   string
	got    []map[string]any
}

func (p *pagerDuty) handler(w http.ResponseWriter, r *http.Request) {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, m)
	w.WriteHeader(p.status)
	_, _ = w.Write([]byte(p.body))
}

// answer sets how the fake answers and forgets what it was sent.
func (p *pagerDuty) answer(status int, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status, p.body, p.got = status, body, nil
}

// received returns what the fake was sent.
func (p *pagerDuty) received() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.got...)
}

func TestPagerDutyTriggersWithTheAlertKeyAndReadsTheAcknowledgementBack(t *testing.T) {
	pd := &pagerDuty{status: http.StatusAccepted, body: `{"status":"success","message":"Event processed","dedup_key":"x"}`}
	server := httptest.NewServer(http.HandlerFunc(pd.handler))
	defer server.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty"}
	environment := env(map[string]string{"TI_PAGERDUTY_ROUTING_KEY": "routing-secret-9", "TI_PAGERDUTY_URL": server.URL})
	var out bytes.Buffer
	if err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &out, server.Client()); err != nil {
		t.Fatal(err)
	}
	got := pd.received()
	if len(got) == 0 {
		t.Fatal("nothing was sent")
	}
	first := got[0]
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
		args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "pagerduty"}
		environment := env(map[string]string{"TI_PAGERDUTY_ROUTING_KEY": "routing-secret-9", "TI_PAGERDUTY_URL": server.URL})
		err := run(args, environment, func() time.Time { return t0.Add(time.Hour) }, &bytes.Buffer{}, server.Client())
		server.Close()
		if err == nil || strings.Contains(err.Error(), "routing-secret-9") {
			t.Errorf("%s: error = %v", name, err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "s.json")); statErr == nil {
			raw, _ := os.ReadFile(filepath.Join(dir, "s.json"))
			if strings.Contains(string(raw), channelKey("pagerduty", "")) {
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
	var mu sync.Mutex
	var ntfyHits int
	ntfy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		ntfyHits++
	}))
	hits := func() int {
		mu.Lock()
		defer mu.Unlock()
		return ntfyHits
	}
	defer ntfy.Close()
	pd := &pagerDuty{status: http.StatusInternalServerError, body: `{}`}
	pager := httptest.NewServer(http.HandlerFunc(pd.handler))
	defer pager.Close()
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy,pagerduty"}
	environment := env(map[string]string{"TI_NTFY_TOPIC": "t", "TI_NTFY_SERVER": ntfy.URL, "TI_PAGERDUTY_ROUTING_KEY": "k", "TI_PAGERDUTY_URL": pager.URL})
	now := func() time.Time { return t0.Add(time.Hour) }
	if err := run(args, environment, now, &bytes.Buffer{}, http.DefaultClient); err == nil {
		t.Fatal("ntfy took the alerts but PagerDuty did not: that is not delivered")
	}
	sent := hits()
	pd.answer(http.StatusAccepted, `{"status":"success"}`)
	if err := run(args, environment, now, &bytes.Buffer{}, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if hits() != sent {
		t.Fatalf("ntfy was sent %d alerts again though it had taken them", hits()-sent)
	}
	if len(pd.received()) == 0 {
		t.Fatal("PagerDuty was not retried")
	}
	pd.answer(http.StatusAccepted, `{"status":"success"}`)
	if err := run(args, environment, now, &bytes.Buffer{}, http.DefaultClient); err != nil || len(pd.received()) != 0 {
		t.Fatalf("delivered on every channel, yet sent again: %v %d", err, len(pd.received()))
	}
}

func TestRunReadsAnEngineDurableLog(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	path := writeDurableLog(t, dir)
	args := []string{"-journal", path, "-state", filepath.Join(dir, "s.json"), "-channel", "stdout,ntfy"}
	var out bytes.Buffer
	if err := run(args, fakeEnv, func() time.Time { return t0.Add(time.Hour) }, &out, fakeClient); err != nil {
		t.Fatalf("a live engine writes only the durable log: %v", err)
	}
	if !strings.Contains(out.String(), "engine halted") {
		t.Fatalf("output:\n%s", out.String())
	}
}
