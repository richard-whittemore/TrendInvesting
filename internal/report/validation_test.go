package report_test

import (
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// rewrite replaces envelope i's payload, keeping its hash consistent, as a
// record that verified but carries values the engine would reject.
func (s *stream) rewrite(i int, payload any) {
	s.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		s.t.Fatal(err)
	}
	s.out[i].Payload, s.out[i].PayloadHash = raw, event.HashPayload(raw)
}

func TestARejectedInputNeverMovesAFigureAndIsCounted(t *testing.T) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 4000, 3)
	s.bar(1, "SPY", 110)
	s.snapshot(1, 4400, 3)
	// The engine keeps an input it refused in the journal: a bar with its
	// raw view mislabelled, and a snapshot with no net liquidation.
	bad := event.CompletedBarPayload{InstrumentID: "SPY", PeriodEnd: day(1),
		SplitAdjusted: event.PriceView{View: event.ViewSplitAdjusted, Open: 1, High: 1, Low: 1, Close: 1, Volume: 1},
		Raw:           event.PriceView{View: "wrong", Open: 1, High: 1, Low: 1, Close: 1, Volume: 1}}
	s.add(event.CompletedBarEventType, event.CompletedBarSchemaVersion, "bar:bad", day(1), bad)
	s.add(event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "snapshot:bad", day(1),
		event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: day(1), Currency: "USD", NetLiquidation: -1})
	// And one at a schema version this build does not read.
	s.add(event.CompletedBarEventType, 9, "bar:future", day(1), bad)

	got, err := report.Summarize(s.out, "2024-01-03", "SPY")
	if err != nil {
		t.Fatalf("a rejected input must not stop the report: %v", err)
	}
	if got.NetLiquidation != 4400 || !got.Equity.Available || got.Equity.BenchmarkReturn < 0.099 || got.Equity.BenchmarkReturn > 0.101 {
		t.Fatalf("a rejected input moved a figure: net %v equity %+v", got.NetLiquidation, got.Equity)
	}
	if got.RejectedInputs != 3 {
		t.Fatalf("RejectedInputs = %d, want 3", got.RejectedInputs)
	}
	if !contains(report.RenderText(got), "Inputs not read: 3") {
		t.Fatalf("the summary must say what it left out:\n%s", report.RenderText(got))
	}
}

func TestADecisionThatDoesNotValidateRefusesBothReports(t *testing.T) {
	for name, mutate := range map[string]func(s *stream){
		"invalid engine state": func(s *stream) {
			s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
			s.rewrite(1, event.EngineStatePayload{State: "bogus", Reason: event.EngineStateReasonRiskLimitBreached, Detail: "d"})
		},
		"unknown engine state version": func(s *stream) {
			s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
			s.out[1].SchemaVersion = 9
		},
		"invalid refusal": func(s *stream) {
			s.refused(0, "tw-a", event.RefusedOrderSize)
			s.rewrite(1, event.OrderRefusedPayload{AccountID: "joint"})
		},
		"invalid decision": func(s *stream) {
			s.decided(0, 0.98)
			s.rewrite(1, event.TargetsDecidedPayload{AccountID: "joint", Outcome: "bogus"})
		},
	} {
		s := newStream(t)
		mutate(s)
		if _, err := report.Alerts(s.out); err == nil {
			t.Errorf("%s: Alerts accepted it", name)
		}
		if _, err := report.Summarize(s.out, "2024-01-02", "SPY"); err == nil {
			t.Errorf("%s: Summarize accepted it", name)
		}
	}
}
