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
	const strategyVersion = "boost/1.0.0+test"
	cfg := event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR,
		Boost: event.BoostConfiguration{InstrumentID: "SPY", InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 30,
			TrendDays: 10, ExitSMA: 3, WarmUpStart: "2024-01-02"},
		Limits: event.MarginLimits{MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 1, PriceBand: 0.05},
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
	for n, c := range closes {
		at := day(n)
		view := func(label string, p float64) event.PriceView {
			return event.PriceView{View: label, Open: p, High: p, Low: p, Close: p, Volume: 1e6}
		}
		apply(event.CompletedBarEventType, event.CompletedBarAdjustedVersion, fmt.Sprintf("bar:%d", n), at, event.CompletedBarAdjustedPayload{
			CompletedBarPayload: event.CompletedBarPayload{InstrumentID: "SPY", PeriodEnd: at, SplitAdjusted: view(event.ViewSplitAdjusted, c), Raw: view(event.ViewRaw, c)},
			Adjusted:            view(event.ViewAdjusted, 7),
			AdjustedBase:        "2024-01-02",
		})
		apply(event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, fmt.Sprintf("snapshot:%d", n), at,
			event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: at, Currency: "USD", NetLiquidation: 4000 + float64(100*n)})
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
	if !near(got.Equity.BenchmarkReturn, 105.0/100-1) || !near(got.Equity.DayBenchmarkReturn, 105.0/102-1) {
		t.Fatalf("the benchmark return must come from the split-adjusted close, not the adjusted view: %+v", got.Equity)
	}
	if !near(got.Equity.AccountReturn, 4200.0/4000-1) {
		t.Fatalf("account return = %v", got.Equity.AccountReturn)
	}
}
