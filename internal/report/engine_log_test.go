package report_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// engineLog runs the real target-weight handler over a few sessions through a
// durable Recorder and returns the log it streamed: the file a live engine
// leaves for the reporter and the dashboard. Each bar is at the schema version
// the handler reads; its adjusted view is a decoy, so a summary that took the
// benchmark's close from it would report the wrong return.
func engineLog(t *testing.T, closes []float64) []byte {
	t.Helper()
	return engineLogHolding(t, closes, nil)
}

// engineLogHolding is engineLog with every snapshot reporting positions as
// held, so the gate journals the orders a holding leads to, a sell within it
// among them, at the version it writes them at.
func engineLogHolding(t *testing.T, closes []float64, positions []event.AccountPosition) []byte {
	t.Helper()
	return engineLogWith(t, closes, positions, false)
}

// engineLogWith is engineLogHolding with the kill switch engaged before the
// first session when killed, so the gate refuses every order it would release.
func engineLogWith(t *testing.T, closes []float64, positions []event.AccountPosition, killed bool) []byte {
	t.Helper()
	const strategyVersion = "boost/1.0.0+test"
	cfg := event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR,
		Boost: event.BoostConfiguration{InstrumentID: "SPY", InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 30,
			TrendDays: 10, ExitSMA: 3, WarmUpStart: "2024-01-02"},
		Limits:    event.MarginLimits{MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 1, PriceBand: 0.05, ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30},
		Execution: event.ExecutionConfiguration{MaxSnapshotAge: 30},
	}
	handler, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: day(0)})
	if err != nil {
		t.Fatal(err)
	}
	hash := event.TargetWeightConfigurationHash(cfg)
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader(hash, strategyVersion))
	if err != nil {
		t.Fatal(err)
	}
	rec := journal.NewDurableRecorder(handler, log, journal.DefaultMaxRecords)
	var seq uint64
	apply := func(eventType string, version uint32, id string, at time.Time, payload any) {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		seq++
		in := event.Envelope{
			ID: id, Type: eventType, EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: version,
			EventTime: at, RecordedAt: at, Sequence: seq, Source: "fixture",
			StrategyVersion: strategyVersion, ConfigurationHash: hash, PayloadHash: event.HashPayload(raw), Payload: raw,
		}
		if _, err := rec.Apply(context.Background(), in); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
	}
	apply(event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", day(0).Add(-time.Hour), cfg)
	if killed {
		apply(event.KillSwitchEventType, event.KillSwitchSchemaVersion, "kill-switch", day(0).Add(-time.Minute),
			event.KillSwitchPayload{AccountID: "joint", AsOf: day(0).Add(-time.Minute), Engaged: true, Operator: "owner", Detail: "test"})
	}
	for n, c := range closes {
		at := day(n)
		view := func(label string, p float64) event.PriceView {
			return event.PriceView{View: label, Open: p, High: p, Low: p, Close: p, Volume: 1e6}
		}
		apply(event.CompletedBarEventType, event.CompletedBarAdjustedVersion, fmt.Sprintf("bar:%d", n), at, event.CompletedBarAdjustedPayload{
			CompletedBarPayload: event.CompletedBarPayload{InstrumentID: "SPY", PeriodEnd: at, SplitAdjusted: view(event.ViewSplitAdjusted, c), Raw: view(event.ViewRaw, c)},
			// Synthetic dividends: the adjusted close gains a tenth a session.
			Adjusted:     view(event.ViewAdjusted, c*float64(10+n)/10),
			AdjustedBase: "2024-01-02",
		})
		apply(event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, fmt.Sprintf("snapshot:%d", n), at,
			event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: at, Currency: "USD", NetLiquidation: 4000 + float64(100*n), Positions: positions})
		apply(event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, fmt.Sprintf("mark:%d", n), at,
			event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: c, Source: event.AdapterIBKR})
		apply(event.SessionClosedEventType, event.SessionClosedSchemaVersion, fmt.Sprintf("session:%d", n), at,
			event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}})
	}
	return buf.Bytes()
}

func TestSummaryOfAnEngineDurableLogReadsItsBarsAndComparesEquity(t *testing.T) {
	_, envelopes, err := report.Load(engineLog(t, []float64{100, 102, 105}))
	if err != nil {
		t.Fatal(err)
	}
	date, ok := report.ClosedSessionDate(envelopes)
	if !ok {
		t.Fatal("the log records no closed session")
	}
	got, err := report.Summarize(envelopes, date, "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedInputs != 0 {
		t.Fatalf("the engine's own bars must be read: %d inputs not read", got.RejectedInputs)
	}
	if !got.Equity.Available || !got.Equity.DayAvailable {
		t.Fatalf("the equity comparison must be available from the engine's log: %+v", got.Equity)
	}
	if !near(got.Equity.BenchmarkReturn, 105.0*1.2/100-1) || !near(got.Equity.DayBenchmarkReturn, 105.0*1.2/(102*1.1)-1) {
		t.Fatalf("the benchmark return must come from the adjusted view, dividends reinvested: %+v", got.Equity)
	}
	if p := got.Equity.Points[2]; p.Benchmark != 105 {
		t.Fatalf("the quoted close stays the split-adjusted price: %+v", p)
	}
	if !near(got.Equity.AccountReturn, 4200.0/4000-1) {
		t.Fatalf("account return = %v", got.Equity.AccountReturn)
	}
}

// TestTheReporterReadsTheReleasesTheGateWrites: the gate journals a sell
// within the holding it was sized from at risk.order.released version 3
// (its broker figures), so the reporter reads every release version the
// event package holds, as journalquery does (ADR 0015). A log the real
// engine wrote yields its order lifecycles, its alerts and its summary, not a
// refused log that silences every alert.
func TestTheReporterReadsTheReleasesTheGateWrites(t *testing.T) {
	raw := engineLogHolding(t, []float64{100}, []event.AccountPosition{{InstrumentID: "SPY", Quantity: 60}})
	_, entries, err := report.LoadEntries(raw)
	if err != nil {
		t.Fatal(err)
	}
	var sell string
	for _, e := range entries {
		if e.Envelope.Type != event.OrderReleasedEventType {
			continue
		}
		var p event.OrderReleasedPayload
		if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Side == event.OrderSideSell && e.Envelope.SchemaVersion == event.OrderReleasedSafetyVersion {
			sell = p.ClientOrderID
		}
	}
	if sell == "" {
		t.Fatal("the engine released no sell at version 3: the fixture no longer shows what the gate writes")
	}
	orders, _, err := report.OrderLifecycles(entries, event.TargetWeightStrategyBoost)
	if err != nil {
		t.Fatalf("order lifecycles of the engine's own log: %v", err)
	}
	if got := stateOf(t, orders, sell); got.State != report.LifecycleCommitted {
		t.Errorf("the sell released in the log's last group is %q, want committed", got.State)
	}
	_, envelopes, err := report.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := report.Alerts(envelopes); err != nil {
		t.Errorf("alerts of the engine's own log: %v", err)
	}
	date, ok := report.ClosedSessionDate(envelopes)
	if !ok {
		t.Fatal("the log records no closed session")
	}
	summary, err := report.Summarize(envelopes, date, "SPY")
	if err != nil {
		t.Fatalf("summary of the engine's own log: %v", err)
	}
	released := false
	for _, o := range summary.Orders {
		released = released || (o.ClientOrderID == sell && o.Status == report.OrderReleased)
	}
	if !released {
		t.Errorf("the summary's orders %+v do not show the sell %s released", summary.Orders, sell)
	}
}

// TestTheReporterReadsTheRefusalsTheGateWrites: the gate journals a kill-switch,
// broker-holding or stale-broker-snapshot refusal at risk.order.refused
// version 3, so the reporter reads every refusal version the event package
// holds (ADR 0015). A log the real gate wrote with such a refusal still
// raises its alert and yields its summary, not a refused log that silences
// every alert.
func TestTheReporterReadsTheRefusalsTheGateWrites(t *testing.T) {
	raw := engineLogWith(t, []float64{100}, nil, true)
	_, envelopes, err := report.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	refusals := 0
	for _, e := range envelopes {
		if e.Type == event.OrderRefusedEventType && e.SchemaVersion == event.OrderRefusedSafetyVersion {
			refusals++
		}
	}
	if refusals == 0 {
		t.Fatal("the gate wrote no version 3 refusal: the fixture no longer shows what the gate writes")
	}
	alerts, err := report.Alerts(envelopes)
	if err != nil {
		t.Fatalf("alerts of the gate's own log: %v", err)
	}
	if len(alerts) != refusals {
		t.Errorf("%d alerts for %d version 3 refusals: %+v", len(alerts), refusals, alerts)
	}
	date, ok := report.ClosedSessionDate(envelopes)
	if !ok {
		t.Fatal("the log records no closed session")
	}
	summary, err := report.Summarize(envelopes, date, "SPY")
	if err != nil {
		t.Fatalf("summary of the gate's own log: %v", err)
	}
	refused := 0
	for _, o := range summary.Orders {
		if o.Status == report.OrderRefused {
			refused++
		}
	}
	if refused != refusals {
		t.Errorf("the summary shows %d refused orders, want %d: %+v", refused, refusals, summary.Orders)
	}
}

// TestEquityCarriesEveryPairedPointInDateOrder: the comparison's chart is
// drawn from the paired points themselves, so the summary carries each date
// where both the account's net liquidation and the benchmark's close are
// recorded, with both values, oldest first.
func TestEquityCarriesEveryPairedPointInDateOrder(t *testing.T) {
	_, envelopes, err := report.Load(engineLog(t, []float64{100, 102, 105}))
	if err != nil {
		t.Fatal(err)
	}
	date, _ := report.ClosedSessionDate(envelopes)
	got, err := report.Summarize(envelopes, date, "SPY")
	if err != nil {
		t.Fatal(err)
	}
	points := got.Equity.Points
	if len(points) != 3 {
		t.Fatalf("paired points = %d, want 3: %+v", len(points), points)
	}
	for i, wantNet := range []float64{4000, 4100, 4200} {
		if points[i].Net != wantNet || points[i].Benchmark != []float64{100, 102, 105}[i] {
			t.Errorf("point %d = %+v", i, points[i])
		}
		if i > 0 && points[i].Date <= points[i-1].Date {
			t.Errorf("points are not oldest first: %+v", points)
		}
	}
	if points[0].Date != got.Equity.StartDate || points[2].Date != got.Equity.EndDate {
		t.Errorf("the series must run from StartDate to EndDate: %+v", got.Equity)
	}
}
