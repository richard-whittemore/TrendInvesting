package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// momentumConfigPath is a momentum configuration for a dry-run IRA with
// short lookbacks (a 6-session score skipping 1, 4 returns of volatility, a
// 5-close trend, a top 2 of a universe of 5), so one synthetic month
// decides. It names the placeholder account "trad-ira", as the shipped IRA
// configuration does; it is a test configuration, never the backtested one.
const momentumConfigPath = "testdata/momentum-dry-run.json"

// momentumMonthPath is the synthetic month the engine replays: the
// adapter's inputs, without the run's identity and sequence, which the feed
// stamps (momentumFeed). Invented prices only, never market data.
const momentumMonthPath = "testdata/momentum-month.jsonl"

// momentumAsOf is the first synthetic session's close, so every session is
// decided; 2025-01-31 is the month's last, the one decision time.
const momentumAsOf = "2025-01-02T21:00:00Z"

// fixtureAccount is the account the month's payloads name, replaced by the
// configuration's account as they are fed (momentumFeed).
const fixtureAccount = "trad-ira"

// fixtureInput is one line of the month: an input as the adapter sends it,
// before the run's identity and sequence are stamped on it.
type fixtureInput struct {
	Type          string          `json:"type"`
	SchemaVersion uint32          `json:"schema_version"`
	ID            string          `json:"id"`
	EventTime     time.Time       `json:"event_time"`
	Payload       json.RawMessage `json:"payload"`
}

// monthName is one invented instrument: its listing type, sector, volume
// and growth per session.
type monthName struct {
	id, listing, sector string
	volume, base, g     float64
}

func monthNames() []monthName {
	return []monthName{
		{"AAA", "CS", "Tech", 3e6, 50, 1.02},
		{"BBB", "CS", "Tech", 2e6, 40, 1.018},
		{"CCC", "CS", "Health", 1.5e6, 30, 1.015},
		{"DDD", "CS", "Energy", 9e6, 20, 0.99},
		{"EEE", "CS", "Utilities", 1e6, 60, 1.005},
		{"IEF", "ETF", "", 1e7, 95, 1.0005},
		{"SHY", "ETF", "", 1e7, 80, 1.0002},
		{"SPY", "ETF", "", 1e8, 500, 1.002},
	}
}

// monthSessions is every weekday from 2025-01-02 to 2025-02-03: the month's
// sessions and the first of the next, which makes 2025-01-31 its last.
func monthSessions() []string {
	var out []string
	for day := time.Date(2025, time.January, 2, 0, 0, 0, 0, time.UTC); !day.After(time.Date(2025, time.February, 3, 0, 0, 0, 0, time.UTC)); day = day.AddDate(0, 0, 1) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, day.Format(time.DateOnly))
		}
	}
	return out
}

// momentumMonth builds the synthetic month: the calendar, then per session
// each instrument's bar, at the month's last session the universe
// snapshot (from the source and sector source the IRA configuration names,
// so the shipped trad-ira.config reads it too), the account snapshot (10,000 in settled cash, nothing held: a
// dry-run sends nothing, so nothing ever fills), each instrument's mark and
// the session's close.
func momentumMonth(t *testing.T) []fixtureInput {
	t.Helper()
	sessions := monthSessions()
	start, err := time.Parse(time.RFC3339, momentumAsOf)
	if err != nil {
		t.Fatal(err)
	}
	start = start.Add(-12 * time.Hour)
	var out []fixtureInput
	add := func(eventType string, version uint32, id string, at time.Time, payload any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fixtureInput{Type: eventType, SchemaVersion: version, ID: id, EventTime: at, Payload: raw})
	}
	add(event.SessionCalendarEventType, event.SessionCalendarSchemaVersion, "calendar", start, event.SessionCalendarPayload{Source: "fixture", Sessions: sessions})
	for i, session := range sessions[:len(sessions)-1] {
		day, _ := time.Parse(time.DateOnly, session)
		end := day.Add(21 * time.Hour)
		prices := map[string]float64{}
		var ids []string
		for _, n := range monthNames() {
			p := math.Round(float64(float64(n.base*math.Pow(n.g, float64(i)))*float64(1+float64(0.002*float64((i*7)%5))))*100) / 100
			prices[n.id] = p
			ids = append(ids, n.id)
			view := func(label string) event.PriceView {
				return event.PriceView{View: label, Open: p, High: p, Low: p, Close: p, Volume: n.volume}
			}
			add(event.CompletedBarEventType, event.CompletedBarAdjustedVersion, fmt.Sprintf("bar:%s:%s", n.id, session), end, event.CompletedBarAdjustedPayload{
				CompletedBarPayload: event.CompletedBarPayload{InstrumentID: n.id, PeriodEnd: end, SplitAdjusted: view(event.ViewSplitAdjusted), Raw: view(event.ViewRaw)},
				Adjusted:            view(event.ViewAdjusted), AdjustedBase: "2025-01-02",
			})
		}
		if sessions[i+1][:7] != session[:7] {
			snap := event.UniverseSnapshotPayload{Session: session, Source: "massive"}
			for _, n := range monthNames() {
				snap.Rows = append(snap.Rows, event.UniverseRow{InstrumentID: n.id, Type: n.listing, Close: prices[n.id], Volume: n.volume, Sector: n.sector, SectorSource: "ishares-iwv-gics"})
			}
			for k := range snap.Rows {
				if snap.Rows[k].Sector == "" {
					snap.Rows[k].SectorSource = ""
				}
			}
			add(event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "universe:"+session, end, snap)
		}
		add(event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, "snapshot:"+session, end, event.AccountCashSnapshotPayload{
			AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: fixtureAccount, AsOf: end, NetLiquidation: 10000, Currency: "USD"},
			SettledCash:                     10000, TotalCash: 10000,
		})
		for _, id := range ids {
			add(event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, fmt.Sprintf("mark:%s:%s", id, session), end,
				event.MarkRecordedPayload{InstrumentID: id, AsOf: end, Price: prices[id], Source: event.AdapterIBKR})
		}
		add(event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session:"+session, end, event.SessionClosedPayload{PeriodEnd: end, InstrumentIDs: ids})
	}
	return out
}

// encodeMonth is the month as the fixture file holds it, one input a line.
func encodeMonth(t *testing.T, month []fixtureInput) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, in := range month {
		line, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// TestTheMomentumMonthFixtureIsTheGeneratedOne: the committed month is the
// one momentumMonth builds, so it is synthetic and reproducible;
// TI_WRITE_MOMENTUM_MONTH=1 writes it.
func TestTheMomentumMonthFixtureIsTheGeneratedOne(t *testing.T) {
	want := encodeMonth(t, momentumMonth(t))
	if os.Getenv("TI_WRITE_MOMENTUM_MONTH") == "1" {
		if err := os.WriteFile(momentumMonthPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(momentumMonthPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not the month momentumMonth builds; rewrite it with TI_WRITE_MOMENTUM_MONTH=1", momentumMonthPath)
	}
}

// readMonth reads the fixture file.
func readMonth(t *testing.T, path string) []fixtureInput {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var out []fixtureInput
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var in fixtureInput
		if err := json.Unmarshal(scanner.Bytes(), &in); err != nil {
			t.Fatal(err)
		}
		out = append(out, in)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// momentumFeed plays the adapter for a momentum engine: it sends each input
// of the month, the configuration's account in place of the fixture's,
// stamped with the run's identity and the next sequence, and keeps every
// decision the engine returns.
type momentumFeed struct {
	t         *testing.T
	client    *transport.Client
	version   string
	hash      string
	account   string
	sequence  uint64
	decisions []event.Envelope
}

func newMomentumFeed(t *testing.T, socketPath string, cfg event.TargetWeightConfigurationPayload, build string, next uint64) *momentumFeed {
	t.Helper()
	client, err := transport.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial the engine: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &momentumFeed{
		t: t, client: client, account: cfg.AccountID, sequence: next,
		version: event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, build),
		hash:    event.TargetWeightConfigurationHash(cfg),
	}
}

func (f *momentumFeed) send(in fixtureInput) {
	f.t.Helper()
	raw := bytes.ReplaceAll(in.Payload, []byte(`"account_id":"`+fixtureAccount+`"`), []byte(`"account_id":"`+f.account+`"`))
	env := event.Envelope{
		ID: in.ID, Type: in.Type, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: in.SchemaVersion,
		EventTime: in.EventTime, RecordedAt: in.EventTime, Sequence: f.sequence, Source: "test-adapter",
		StrategyVersion: f.version, ConfigurationHash: f.hash, PayloadHash: event.HashPayload(raw), Payload: raw,
	}
	reply, err := f.client.Decide(context.Background(), env)
	if err != nil {
		f.t.Fatalf("decide %s: %v", in.ID, err)
	}
	f.sequence = reply.Sequence + 1
	var bundle decisionsPayload
	if err := json.Unmarshal(reply.Payload, &bundle); err != nil {
		f.t.Fatalf("decode the decisions for %s: %v", in.ID, err)
	}
	f.decisions = append(f.decisions, bundle.Decisions...)
}

// momentumOptions is a dry-run momentum invocation writing into dir.
func momentumOptions(t *testing.T, dir string) options {
	t.Helper()
	return options{
		mode: modeDryRun, socketPath: filepath.Join(shortSocketDir(t), "engine.sock"), configPath: momentumConfigPath,
		asOf: momentumAsOf, outPath: filepath.Join(dir, "run.journal"), logPath: filepath.Join(dir, "run.log"), build: "test-build",
	}
}

// TestAMomentumConfigurationIsComposed: the engine composes a momentum
// configuration as the target-weight handler for an IRA (ADR 0026, points
// 1 to 3 and 6), with its own hash, account and mode, never as a Turtle
// configuration.
func TestAMomentumConfigurationIsComposed(t *testing.T) {
	asOf := time.Date(2025, time.January, 2, 21, 0, 0, 0, time.UTC)
	c, err := compose(momentumConfigPath, "test-build", asOf)
	if err != nil {
		t.Fatalf("compose(momentum) = %v", err)
	}
	cfg := boostConfiguration(t, momentumConfigPath)
	if c.mode != modeDryRun || c.accountID != fixtureAccount || c.strategyID != event.TargetWeightStrategyMomentum ||
		c.configurationHash != event.TargetWeightConfigurationHash(cfg) || c.configEnvelope.Type != event.TargetWeightConfigurationEventType {
		t.Fatalf("composed %+v, want the momentum target-weight handler in dry-run for %s", c, fixtureAccount)
	}
	if _, ok := c.handler.(*targetweight.Handler); !ok {
		t.Fatalf("the handler is %T, want the target-weight handler", c.handler)
	}
}

// TestADryRunEngineDecidesASyntheticIRAMonthAndStopsCleanly: the one engine,
// given the momentum configuration, replays the synthetic month from the
// fixture: the month's last session decides the targets, every order is
// released with send false and ended by the engine's own synthetic report
// (ADR 0026, point 3, "Mode"), and on SIGTERM the run stops cleanly, its
// journal written. The durable log replays to the same decisions, byte for
// byte (ADR 0027; ADR 0026, point 7, item 6).
func TestADryRunEngineDecidesASyntheticIRAMonthAndStopsCleanly(t *testing.T) {
	dir := t.TempDir()
	opts := momentumOptions(t, dir)
	stop := startEngine(t, opts)
	cfg := boostConfiguration(t, momentumConfigPath)
	feed := newMomentumFeed(t, opts.socketPath, cfg, "test-build", 2)
	for _, in := range readMonth(t, momentumMonthPath) {
		feed.send(in)
	}
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the engine: %v", err)
	}
	if _, err := os.Stat(opts.outPath); err != nil {
		t.Fatalf("the stopped run wrote no journal: %v", err)
	}

	var decided []event.TargetsDecidedPayload
	released, synthetic := 0, 0
	for _, d := range feed.decisions {
		switch d.Type {
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			if err := json.Unmarshal(d.Payload, &p); err != nil {
				t.Fatal(err)
			}
			decided = append(decided, p)
		case event.OrderReleasedEventType:
			var p event.OrderReleasedPayload
			if err := json.Unmarshal(d.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Send || p.Mode != modeDryRun || p.AccountID != fixtureAccount {
				t.Errorf("%s released with send %v in mode %q for %s, want send false in dry-run for %s", d.ID, p.Send, p.Mode, p.AccountID, fixtureAccount)
			}
			released++
		case event.OrderLifecycleEventType:
			synthetic++
		}
	}
	if len(decided) != 1 || decided[0].Outcome != event.TargetsOutcomeDecided || decided[0].Momentum.CloseSession != "2025-01-31" || len(decided[0].Targets) == 0 {
		t.Fatalf("decisions %+v, want one decided month at 2025-01-31", decided)
	}
	if released == 0 || synthetic != released {
		t.Fatalf("%d orders released and %d synthetic reports, want the month's buys each ended by its synthetic report", released, synthetic)
	}

	recovery := recoverFile(t, opts.logPath)
	var inputs, decisions []event.Envelope
	for _, e := range recovery.Entries {
		if e.Kind == journal.KindInput {
			inputs = append(inputs, e.Envelope)
		} else {
			decisions = append(decisions, e.Envelope)
		}
	}
	asOf, _ := time.Parse(time.RFC3339, momentumAsOf)
	handler, err := targetweight.New(event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, "test-build"), cfg, targetweight.Options{DecideFrom: asOf})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := replay.New(handler)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatalf("replay the durable log: %v", err)
	}
	if d := replay.Equivalent(decisions, replayed); d != nil {
		t.Fatalf("the durable log's replay diverged: %+v", d)
	}
}

// TestAMomentumRunInLiveModeIsRefused: the mode binding and the sending
// gate apply to an IRA as to Boost: a dry-run configuration started in
// another mode, and any live run, are refused before anything is created
// (ADR 0026, point 6; AGENTS.md rule 5).
func TestAMomentumRunInLiveModeIsRefused(t *testing.T) {
	for _, mode := range []string{modePaper, modeLive} {
		dir := t.TempDir()
		opts := momentumOptions(t, dir)
		opts.mode = mode
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := run(ctx, opts, &strings.Builder{})
		cancel()
		if err == nil || !strings.Contains(err.Error(), `does not match the configuration's mode "dry-run"`) {
			t.Errorf("-mode %s with the dry-run IRA configuration: %v", mode, err)
		}
		if _, statErr := os.Stat(opts.logPath); !os.IsNotExist(statErr) {
			t.Errorf("-mode %s: the refused run left a durable log behind", mode)
		}
	}
	dir := t.TempDir()
	cfg := boostConfiguration(t, momentumConfigPath)
	cfg.Mode, cfg.AccountID = modeLive, "U7654321"
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "live.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := momentumOptions(t, dir)
	opts.mode, opts.configPath = modeLive, path
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err = run(ctx, opts, &strings.Builder{})
	cancel()
	if err == nil || !strings.Contains(err.Error(), "limited-live gate") {
		t.Fatalf("-mode live with a live IRA configuration: %v", err)
	}
}

// TestTheShippedIRAConfigurationRecordsTheMonthWithheld: the server's
// demonstration replays the same month through the shipped trad-ira
// configuration, its birth named as on the server. Its rule wants 200 SPY
// closes and the 500 ranked members' histories, which a month does not
// hold, so the engine reads every input, records the month's decision
// withheld with its reason (ADR 0026, points 1 and 5: no decision time
// passes without one), sends nothing, and stops cleanly.
func TestTheShippedIRAConfigurationRecordsTheMonthWithheld(t *testing.T) {
	dir := t.TempDir()
	cfg := boostConfiguration(t, "../../deploy/etc/engines/trad-ira.config")
	cfg.Glide.Birth = "1975-06"
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trad-ira.config")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := momentumOptions(t, dir)
	opts.configPath = path
	stop := startEngine(t, opts)
	feed := newMomentumFeed(t, opts.socketPath, cfg, "test-build", 2)
	for _, in := range readMonth(t, momentumMonthPath) {
		feed.send(in)
	}
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the engine: %v", err)
	}
	var decided []event.TargetsDecidedPayload
	for _, d := range feed.decisions {
		switch d.Type {
		case event.TargetsDecidedEventType:
			var p event.TargetsDecidedPayload
			if err := json.Unmarshal(d.Payload, &p); err != nil {
				t.Fatal(err)
			}
			decided = append(decided, p)
		case event.OrderReleasedEventType, event.OrderPlannedEventType:
			t.Errorf("the withheld month planned or released %s", d.ID)
		}
	}
	if len(decided) != 1 || decided[0].Outcome != event.TargetsOutcomeWithheld || decided[0].WithheldReason == "" || !decided[0].SessionPeriodEnd.Equal(time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("decisions %+v, want the month of 2025-01-31 withheld with its reason", decided)
	}
	t.Logf("withheld: %s (%s)", decided[0].WithheldReason, decided[0].Detail)
}
