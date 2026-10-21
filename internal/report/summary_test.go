package report_test

import (
	"math"
	"strings"
	"testing"
	"time"

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
	if !e.DayAvailable || !near(e.DayAccountReturn, 0.075) || !near(e.DayBenchmarkReturn, 0.10) {
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

// snapshotAs records a positions snapshot for any account and time, as the
// log keeps one the engine refused.
func (s *stream) snapshotAs(id, account string, asOf, at time.Time, netLiquidation float64) {
	s.add(event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, id, at,
		event.AccountPositionsSnapshotPayload{AccountID: account, AsOf: asOf, Currency: "USD", NetLiquidation: netLiquidation,
			Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 99}}})
}

func TestSummarySkipsSnapshotsTheEngineRefuses(t *testing.T) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 4000, 0)
	s.bar(1, "SPY", 110)
	s.snapshot(1, 4300, 39)
	// Another account's snapshot, and one that does not follow the last.
	s.snapshotAs("snapshot:other", "ira", day(1).Add(time.Hour), day(1).Add(time.Hour), 9000)
	s.snapshotAs("snapshot:stale", "joint", day(0), day(1).Add(2*time.Hour), 1000)
	got, err := report.Summarize(s.out, "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.NetLiquidation != 4300 || len(got.Holdings) != 1 || got.Holdings[0].Quantity != 39 || !got.HoldingsAsOf.Equal(day(1)) {
		t.Fatalf("holdings = %+v", got)
	}
	if !near(got.Equity.AccountReturn, 0.075) {
		t.Fatalf("equity moved by a refused snapshot: %+v", got.Equity)
	}
	if got.RejectedInputs != 2 {
		t.Fatalf("rejected inputs = %d, want 2", got.RejectedInputs)
	}
}

func TestSummaryDayReturnIsUnavailableWhenTheReportDateHasNoPairedPoint(t *testing.T) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 4000, 0)
	s.bar(1, "SPY", 110)
	s.snapshot(1, 4300, 39)
	s.withheld(2, event.WithheldMissingBars)
	got, err := report.Summarize(s.out, "2024-01-04", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	e := got.Equity
	if !e.Available || e.EndDate != "2024-01-03" || e.DayAvailable {
		t.Fatalf("equity = %+v", e)
	}
	text := report.RenderText(got)
	if !strings.Contains(text, "2024-01-02 to 2024-01-03") || !strings.Contains(text, "Day: not available") {
		t.Fatalf("text does not date the comparison:\n%s", text)
	}
}

func TestSummaryDayReturnNamesTheDatesItCompares(t *testing.T) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 4000, 0)
	s.bar(2, "SPY", 110) // no paired point on day 1
	s.snapshot(2, 4300, 39)
	got, err := report.Summarize(s.out, "2024-01-04", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	e := got.Equity
	if !e.DayAvailable || e.DayStartDate != "2024-01-02" || e.EndDate != "2024-01-04" {
		t.Fatalf("equity = %+v", e)
	}
	if text := report.RenderText(got); !strings.Contains(text, "Day, 2024-01-02 to 2024-01-04:") {
		t.Fatalf("text does not name the compared dates:\n%s", text)
	}
}
