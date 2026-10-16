package report_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func buildDay(t *testing.T) []event.Envelope {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 4000, 0)
	s.decided(0, 0.98)
	s.planned(0, "tw-a", 39)
	s.released(0, "tw-a", 39)
	s.bar(1, "SPY", 110)
	s.snapshot(1, 4300, 39)
	s.decided(1, 0.98)
	s.planned(1, "tw-b", 10)
	s.refused(1, "tw-b", event.RefusedOrderSize)
	return s.out
}

func TestSummaryCarriesTheDaysHoldingsTargetsOrdersAndRefusals(t *testing.T) {
	got, err := report.Summarize(buildDay(t), "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "joint" || got.StrategyID != event.TargetWeightStrategyBoost || got.Date != "2024-01-03" {
		t.Fatalf("identity = %+v", got)
	}
	if !got.HaveSnapshot || got.NetLiquidation != 4300 || len(got.Holdings) != 1 || got.Holdings[0].Quantity != 39 {
		t.Fatalf("holdings = %+v", got)
	}
	if len(got.Decisions) != 1 || got.Decisions[0].Outcome != event.TargetsOutcomeDecided || got.Decisions[0].Targets[0].Weight != 0.98 {
		t.Fatalf("decisions = %+v", got.Decisions)
	}
	if len(got.Orders) != 1 || got.Orders[0].ClientOrderID != "tw-b" || got.Orders[0].Status != report.OrderRefused || got.Orders[0].Reason != event.RefusedOrderSize {
		t.Fatalf("orders = %+v", got.Orders)
	}
	if got.Refusals != 1 {
		t.Fatalf("refusals = %d", got.Refusals)
	}
	if got.EngineState != report.StateNormal {
		t.Fatalf("engine state = %q", got.EngineState)
	}
}

func TestSummaryOfAnEarlierDayExcludesLaterEventsAndShowsReleasedOrders(t *testing.T) {
	got, err := report.Summarize(buildDay(t), "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.NetLiquidation != 4000 || len(got.Holdings) != 0 {
		t.Fatalf("holdings = %+v", got)
	}
	if len(got.Orders) != 1 || got.Orders[0].Status != report.OrderReleased || got.Orders[0].Quantity != 39 {
		t.Fatalf("orders = %+v", got.Orders)
	}
}

func TestSummaryEquityAgainstTheBenchmark(t *testing.T) {
	got, err := report.Summarize(buildDay(t), "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	e := got.Equity
	if !e.Available {
		t.Fatal("equity should be available with two paired points")
	}
	// Account 4000 -> 4300 is +7.5%; SPY 100 -> 110 is +10%.
	if !near(e.AccountReturn, 0.075) || !near(e.BenchmarkReturn, 0.10) || !near(e.Excess, -0.025) {
		t.Fatalf("since start = %+v", e)
	}
	if !near(e.DayAccountReturn, 0.075) || !near(e.DayBenchmarkReturn, 0.10) {
		t.Fatalf("day = %+v", e)
	}
	if e.StartDate != "2024-01-02" {
		t.Fatalf("start date = %q", e.StartDate)
	}
}

func TestSummaryEquityUnavailableWithoutPairedPoints(t *testing.T) {
	s := newStream(t)
	s.snapshot(0, 4000, 0)
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.Equity.Available {
		t.Fatalf("equity = %+v", got.Equity)
	}
}

func TestSummaryEngineStateFollowsTheLatestRecord(t *testing.T) {
	s := newStream(t)
	s.snapshot(0, 4000, 0)
	s.engineState(0, event.EngineStateDegraded, event.EngineStateReasonRiskLimitBreached, "SPY")
	s.engineState(1, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.EngineState != report.StateDegraded {
		t.Fatalf("state on day 0 = %q", got.EngineState)
	}
	got, err = report.Summarize(s.out, "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.EngineState != report.StateHalted {
		t.Fatalf("state on day 1 = %q", got.EngineState)
	}
}

func TestSummaryFailsOnAMalformedPayloadAndAMissingConfiguration(t *testing.T) {
	s := newStream(t)
	s.out[0].Payload = []byte(`{`)
	if _, err := report.Summarize(s.out, "2024-01-02", "SPY"); err == nil {
		t.Fatal("expected a decode error")
	}
	if _, err := report.Summarize(nil, "2024-01-02", "SPY"); err == nil {
		t.Fatal("expected an error for a journal with no configuration")
	}
	if _, err := report.Summarize(newStream(t).out, "not a date", "SPY"); err == nil {
		t.Fatal("expected an error for a bad date")
	}
}

func TestLastDateIsTheLatestEventDay(t *testing.T) {
	got, err := report.LastDate(buildDay(t))
	if err != nil || got != "2024-01-03" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := report.LastDate(nil); err == nil {
		t.Fatal("expected an error for an empty journal")
	}
}

func TestRenderTextNamesEverySection(t *testing.T) {
	got, err := report.Summarize(buildDay(t), "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	text := report.RenderText(got)
	for _, want := range []string{"joint", "2024-01-03", "SPY 39", "refused", "order-size", "vs SPY", "normal"} {
		if !contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}
