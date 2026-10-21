package report_test

import (
	"fmt"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// skipped is a trade the order planner sized but could not order, for
// reason (strategy.order.skipped; ADR 0026, point 8).
func (s *stream) skipped(n int, clientID, side, reason string, shares float64) {
	p := event.OrderSkippedPayload{AccountID: "joint", Tag: fmt.Sprintf("targets:%d", n), ClientOrderID: clientID, InstrumentID: "SPY",
		Side: side, Phase: side, Reason: reason, Shares: shares, TargetWeight: 0.98, NetLiquidation: 4000, HeldQuantity: 39, ReferencePrice: 100}
	if reason == event.OrderSkippedScaledBelowOneShare {
		p.Scale = 0.5
	}
	s.add(event.OrderSkippedEventType, event.OrderSkippedSchemaVersion, "skip:"+clientID, day(n), p)
}

// TestTheSummaryListsTheDaysSkippedTrades: a skipped trade is recorded so
// no planned trade disappears silently (ADR 0026, point 8), so the daily
// summary lists each of the day's with its side, the shares the rule asked
// for and its reason. A skip of another day is not the day's.
func TestTheSummaryListsTheDaysSkippedTrades(t *testing.T) {
	s := newStream(t)
	s.skipped(0, "tw-0", event.OrderSideBuy, event.OrderSkippedBelowOneShare, 0.4)
	s.skipped(1, "tw-1", event.OrderSideBuy, event.OrderSkippedScaledBelowOneShare, 0.25)
	s.skipped(1, "tw-2", event.OrderSideSell, event.OrderSkippedHeldByWorkingSells, 3)
	got, err := report.Summarize(s.out, "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	want := []report.SkippedLine{
		{At: day(1), ClientOrderID: "tw-1", InstrumentID: "SPY", Side: event.OrderSideBuy, Reason: event.OrderSkippedScaledBelowOneShare, Shares: 0.25},
		{At: day(1), ClientOrderID: "tw-2", InstrumentID: "SPY", Side: event.OrderSideSell, Reason: event.OrderSkippedHeldByWorkingSells, Shares: 3},
	}
	if len(got.Skipped) != len(want) {
		t.Fatalf("skipped = %+v, want %+v", got.Skipped, want)
	}
	for i := range want {
		if got.Skipped[i] != want[i] {
			t.Errorf("skipped %d = %+v, want %+v", i, got.Skipped[i], want[i])
		}
	}
	text := report.RenderText(got)
	for _, line := range []string{"Trades skipped:", "tw-1 buy SPY 0.25 shares: scaled-below-one-share", "tw-2 sell SPY 3 shares: held-by-working-sells"} {
		if !contains(text, line) {
			t.Errorf("the summary text does not say %q:\n%s", line, text)
		}
	}
	none, err := report.Summarize(s.out, "2024-01-04", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Skipped) != 0 || contains(report.RenderText(none), "Trades skipped:") {
		t.Errorf("a day with no skipped trade lists %+v", none.Skipped)
	}
}

// TestAnUnreadableSkippedTradeRefusesTheSummary: a skipped trade is a
// decision the engine recorded, so one this build cannot read refuses the
// summary, as any other decision does (ADR 0015).
func TestAnUnreadableSkippedTradeRefusesTheSummary(t *testing.T) {
	s := newStream(t)
	s.skipped(0, "tw-0", event.OrderSideBuy, event.OrderSkippedBelowOneShare, 0.4)
	s.out[len(s.out)-1].Payload = []byte(`{`)
	if _, err := report.Summarize(s.out, "2024-01-02", "SPY"); err == nil {
		t.Fatal("Summarize accepted a corrupt skipped trade")
	}
}
