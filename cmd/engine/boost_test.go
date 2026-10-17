package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// boostConfigPath is a Boost configuration for a dry-run joint account
// (plan.md section 2: SPY, 98% invested, BOOST_SIZE 1.5, RSI(2) below 10,
// SMA 200, exit SMA 5). MaxOrderFraction is 1, so the first decision from
// cash buys 98% of net liquidation in one order.
const boostConfigPath = "testdata/boost-dry-run.json"

// boostAsOf is the close of the first synthetic session, so every session
// is decided.
const boostAsOf = "2024-01-02T21:00:00Z"

func boostConfiguration(t *testing.T, path string) event.TargetWeightConfigurationPayload {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return cfg
}

// boostOptions is a dry-run Boost invocation writing into dir.
func boostOptions(t *testing.T, dir string) options {
	t.Helper()
	return options{
		mode: modeDryRun, socketPath: filepath.Join(shortSocketDir(t), "engine.sock"), configPath: boostConfigPath,
		asOf: boostAsOf, outPath: filepath.Join(dir, "run.journal"), logPath: filepath.Join(dir, "run.log"), build: "test-build",
	}
}

// boostFeed plays the adapter for a dry-run Boost engine: per session, the
// SPY bar, the account snapshot, the mark and the session close, continuing
// the engine's one input sequence. The account is simulated: an order the
// engine releases fills at the next session's close.
type boostFeed struct {
	t        *testing.T
	client   *transport.Client
	version  string
	hash     string
	account  string
	sequence uint64
	cash     float64
	held     int64
	pending  []event.OrderReleasedPayload
	released []event.OrderReleasedPayload
}

func newBoostFeed(t *testing.T, socketPath string, cfg event.TargetWeightConfigurationPayload, build string, next uint64) *boostFeed {
	t.Helper()
	client, err := transport.Dial(socketPath)
	if err != nil {
		t.Fatalf("dial the engine: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &boostFeed{
		t: t, client: client, account: cfg.AccountID, sequence: next, cash: 4000,
		version: event.ComposeStrategyVersion(cfg.StrategyID, targetweight.RulesVersion, build),
		hash:    event.TargetWeightConfigurationHash(cfg),
	}
}

// boostClose is an invented series: each 20-session cycle rises 0.4% a
// session for 17 sessions, falls 2% twice, and recovers 3%.
func boostClose(i int) float64 {
	price := 400.0
	for j := 0; j < i; j++ {
		factor := 1.004
		switch j % 20 {
		case 17, 18:
			factor = 0.98
		case 19:
			factor = 1.03
		}
		price *= factor
	}
	return price
}

// boostSession returns the close of the i-th weekday session from
// 2024-01-02, 21:00 UTC.
func boostSession(i int) time.Time {
	day := time.Date(2024, time.January, 2, 21, 0, 0, 0, time.UTC)
	for n := 0; ; day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		if n == i {
			return day
		}
		n++
	}
}

func (f *boostFeed) send(eventType string, version uint32, id string, at time.Time, payload any) {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatalf("encode %s: %v", id, err)
	}
	in := event.Envelope{
		ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
		EventTime: at, RecordedAt: at, Sequence: f.sequence, Source: "test-adapter",
		StrategyVersion: f.version, ConfigurationHash: f.hash, PayloadHash: event.HashPayload(raw), Payload: raw,
	}
	f.sequence++
	reply, err := f.client.Decide(context.Background(), in)
	if err != nil {
		f.t.Fatalf("decide %s: %v", id, err)
	}
	var bundle decisionsPayload
	if err := json.Unmarshal(reply.Payload, &bundle); err != nil {
		f.t.Fatalf("decode the decisions for %s: %v", id, err)
	}
	for _, d := range bundle.Decisions {
		if d.Type != event.OrderReleasedEventType {
			continue
		}
		var p event.OrderReleasedPayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			f.t.Fatalf("decode %s: %v", d.ID, err)
		}
		f.pending = append(f.pending, p)
		f.released = append(f.released, p)
	}
}

// sessions feeds sessions [from, to).
func (f *boostFeed) sessions(from, to int) {
	f.t.Helper()
	for i := from; i < to; i++ {
		at, price := boostSession(i), boostClose(i)
		for _, o := range f.pending {
			q := o.Quantity
			if o.Side == event.OrderSideSell {
				q = -q
			}
			f.held += q
			f.cash -= float64(float64(q) * price)
		}
		f.pending = nil
		view := func(label string) event.PriceView {
			return event.PriceView{View: label, Open: price, High: price, Low: price, Close: price, Volume: 1e6}
		}
		snapshot := event.AccountPositionsSnapshotPayload{AccountID: f.account, AsOf: at, Currency: "USD", NetLiquidation: f.cash + float64(float64(f.held)*price)}
		if f.held != 0 {
			snapshot.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: f.held}}
		}
		f.send(event.CompletedBarEventType, event.CompletedBarSchemaVersion, fmt.Sprintf("bar:%d", i), at,
			event.CompletedBarPayload{InstrumentID: "SPY", PeriodEnd: at, SplitAdjusted: view(event.ViewSplitAdjusted), Raw: view(event.ViewRaw)})
		f.send(event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, fmt.Sprintf("snapshot:%d", i), at, snapshot)
		f.send(event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, fmt.Sprintf("mark:%d", i), at,
			event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: price, Source: event.AdapterIBKR})
		f.send(event.SessionClosedEventType, event.SessionClosedSchemaVersion, fmt.Sprintf("session:%d", i), at,
			event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}})
	}
}

// TestADryRunEngineRunsBoostAndSendsNothing: the one engine, given a Boost
// configuration, decides Boost through the risk gate into the durable log,
// and every released order is recorded with send false (ADR 0026, point 3,
// "Mode"). The log opens with the configuration, which carries the mode.
func TestADryRunEngineRunsBoostAndSendsNothing(t *testing.T) {
	dir := t.TempDir()
	opts := boostOptions(t, dir)
	stop := startEngine(t, opts)
	feed := newBoostFeed(t, opts.socketPath, boostConfiguration(t, boostConfigPath), "test-build", 2)
	feed.sessions(0, 30)
	_ = feed.client.Close()
	if err := stop(); err != nil {
		t.Fatalf("stop the engine: %v", err)
	}

	recovery := recoverFile(t, opts.logPath)
	first := recovery.Entries[0].Envelope
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(first.Payload, &cfg); err != nil || first.Type != event.TargetWeightConfigurationEventType || cfg.Mode != "dry-run" {
		t.Fatalf("the durable log opens with %s mode %q (%v), want the target-weight configuration in dry-run", first.Type, cfg.Mode, err)
	}
	var released []event.OrderReleasedPayload
	for _, e := range recovery.Entries {
		if e.Kind != journal.KindDecision || e.Envelope.Type != event.OrderReleasedEventType {
			continue
		}
		var p event.OrderReleasedPayload
		if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", e.Envelope.ID, err)
		}
		if p.Send || p.Mode != "dry-run" {
			t.Errorf("%s is recorded with send %v in mode %q, want send false in dry-run", e.Envelope.ID, p.Send, p.Mode)
		}
		released = append(released, p)
	}
	// 98% of $4,000 at $400 is 9.8 shares; whole shares round toward zero.
	if len(released) == 0 || released[0].Side != event.OrderSideBuy || released[0].Quantity != 9 {
		t.Fatalf("released %+v, want the first to buy 9 SPY", released)
	}
	if len(released) != len(feed.released) {
		t.Errorf("the log holds %d released orders, the adapter received %d", len(released), len(feed.released))
	}
}

// TestTheDeclaredModeMustBeTheConfiguredMode: the mode is hashed with the
// configuration (ADR 0026, point 6), so -mode must name it; a Turtle
// configuration declares none and runs only as a backtest.
func TestTheDeclaredModeMustBeTheConfiguredMode(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		mode, config, want string
	}{
		"a dry-run configuration started as paper": {modePaper, boostConfigPath, `does not match the configuration's mode "dry-run"`},
		"a dry-run configuration started as live":  {modeLive, boostConfigPath, `does not match the configuration's mode "dry-run"`},
		"a dry-run configuration as a backtest":    {modeBacktest, boostConfigPath, `does not match the configuration's mode "dry-run"`},
		"a Turtle configuration in dry-run":        {modeDryRun, testConfigPath, "runs only in -mode backtest"},
	} {
		opts := boostOptions(t, filepath.Join(dir, name))
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		opts.mode, opts.configPath = tc.mode, tc.config
		// Bounded, so a run that wrongly starts serving fails the test
		// rather than hanging it.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := run(ctx, opts, io.Discard)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: run() = %v, want a refusal saying %q", name, err, tc.want)
		}
		if _, statErr := os.Stat(opts.logPath); !os.IsNotExist(statErr) {
			t.Errorf("%s: the refused run left a durable log behind", name)
		}
	}
}

// writeBoostConfiguration writes the dry-run fixture with mode and account
// changed, and returns its path.
func writeBoostConfiguration(t *testing.T, dir, mode, account string) string {
	t.Helper()
	cfg := boostConfiguration(t, boostConfigPath)
	cfg.Mode, cfg.AccountID = mode, account
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, mode+"-"+account+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheAccountMustMatchTheMode: an IBKR paper account's ID begins "DU"
// and a live account's does not, so a paper configuration naming a live
// account, or a live one naming a paper account, is refused before
// anything is created.
func TestTheAccountMustMatchTheMode(t *testing.T) {
	for _, tc := range []struct{ mode, account, want string }{
		{modePaper, "U1234567", `paper account ID begins "DU"`},
		{modeLive, "DU1234567", `"DU" is a paper account`},
	} {
		dir := t.TempDir()
		opts := boostOptions(t, dir)
		opts.mode, opts.configPath = tc.mode, writeBoostConfiguration(t, dir, tc.mode, tc.account)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := run(ctx, opts, io.Discard)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("-mode %s with account %s: run() = %v, want a refusal saying %q", tc.mode, tc.account, err, tc.want)
		}
		if _, statErr := os.Stat(opts.logPath); !os.IsNotExist(statErr) {
			t.Errorf("-mode %s with account %s: the refused run left a durable log behind", tc.mode, tc.account)
		}
	}

	// A paper configuration naming a paper account starts.
	dir := t.TempDir()
	opts := boostOptions(t, dir)
	opts.mode, opts.configPath = modePaper, writeBoostConfiguration(t, dir, modePaper, "DU1234567")
	if err := startEngine(t, opts)(); err != nil {
		t.Fatalf("a paper account in -mode paper: %v", err)
	}
}

// TestAMomentumConfigurationIsRefusedAsNotComposedHere: cmd/engine composes
// Boost as its one target-weight strategy (ADR 0026, point 6); a momentum
// configuration is refused by name rather than read as a Turtle
// configuration, so the refusal tells the operator what is not composed.
func TestAMomentumConfigurationIsRefusedAsNotComposedHere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "momentum.json")
	if err := os.WriteFile(path, []byte(`{"strategy_id":"momentum"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := compose(path, "test-build", time.Date(2024, time.January, 2, 21, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), `strategy_id "momentum"`) || strings.Contains(err.Error(), "Turtle") {
		t.Fatalf("compose(momentum) = %v, want a refusal naming strategy_id \"momentum\" as not composed here", err)
	}
}
