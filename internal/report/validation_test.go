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
		"unknown configuration version": func(s *stream) {
			s.out[0].SchemaVersion = 9
		},
		"invalid configuration": func(s *stream) {
			s.rewrite(0, event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost, AccountID: "joint"})
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

func TestAccountOfReadsTheConfigurationOnlyAtAKnownVersionAndValidated(t *testing.T) {
	s := newStream(t)
	s.out[0].SchemaVersion = 9
	if _, _, err := report.AccountOf(s.out); err == nil || !contains(err.Error(), "schema version 9") {
		t.Fatalf("a configuration at an unknown version must be refused, err = %v", err)
	}
	s = newStream(t)
	s.rewrite(0, event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost})
	if _, _, err := report.AccountOf(s.out); err == nil {
		t.Fatal("a configuration that does not validate must be refused")
	}
}

func TestAVersion2BarIsValidatedAtItsOwnVersion(t *testing.T) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 4000, 3)
	s.bar(1, "SPY", 110)
	s.snapshot(1, 4400, 3)
	// Its version 1 fields are sound, but its adjusted base is after its own
	// session, which the engine refuses (event.CompletedBarAdjustedPayload).
	view := func(label string) event.PriceView {
		return event.PriceView{View: label, Open: 1, High: 1, Low: 1, Close: 1, Volume: 1}
	}
	s.add(event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "bar:bad", day(1), event.CompletedBarAdjustedPayload{
		CompletedBarPayload: event.CompletedBarPayload{InstrumentID: "SPY", PeriodEnd: day(1), SplitAdjusted: view(event.ViewSplitAdjusted), Raw: view(event.ViewRaw)},
		Adjusted:            view(event.ViewAdjusted),
		AdjustedBase:        "2030-01-01",
	})

	got, err := report.Summarize(s.out, "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedInputs != 1 || !near(got.Equity.BenchmarkReturn, 0.1) {
		t.Fatalf("an invalid version 2 bar must be counted and move no figure: rejected %d, equity %+v", got.RejectedInputs, got.Equity)
	}
}
