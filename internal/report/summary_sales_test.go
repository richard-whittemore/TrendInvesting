package report_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// TestTheSummaryListsTheDaysWithheldSalesIncludingLapses: a lapsed sale back
// to 98% goes in the daily summary, not an urgent alert (ADR 0019, amendment
// of 2026-10-17, reading 1, "Lapse"; docs/manual/states-and-kill-switch.md),
// so the summary lists every sale withheld that day with its reason. A
// withheld sale of another day is not the day's.
func TestTheSummaryListsTheDaysWithheldSalesIncludingLapses(t *testing.T) {
	s := newStream(t)
	s.saleWithheld(0, "sale-0", event.SaleWithheldStaleBrokerSnapshot)
	s.saleWithheld(1, "sale-1", event.SaleWithheldStaleBrokerSnapshot)
	s.saleWithheld(1, "sale-1", event.SaleWithheldLapsed)
	got, err := report.Summarize(s.out, "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SalesWithheld) != 2 || got.SalesWithheld[1].Reason != event.SaleWithheldLapsed || got.SalesWithheld[1].ClientOrderID != "sale-1" {
		t.Fatalf("sales withheld = %+v, want the day's two, the lapse last", got.SalesWithheld)
	}
	text := report.RenderText(got)
	if !contains(text, "Sales back to 98% withheld:") || !contains(text, "sale-1 SPY: lapsed") {
		t.Errorf("the summary text does not list the lapse:\n%s", text)
	}
	none, err := report.Summarize(s.out, "2024-01-04", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if len(none.SalesWithheld) != 0 || contains(report.RenderText(none), "Sales back to 98% withheld:") {
		t.Errorf("a day with no withheld sale lists %+v", none.SalesWithheld)
	}
}

// TestAnUnreadableWithheldSaleRefusesTheSummary: a withheld sale is a
// decision the engine recorded, so one this build cannot read refuses the
// summary, as any other decision does.
func TestAnUnreadableWithheldSaleRefusesTheSummary(t *testing.T) {
	s := newStream(t)
	s.saleWithheld(0, "sale-0", event.SaleWithheldLapsed)
	s.out[len(s.out)-1].Payload = []byte(`{`)
	if _, err := report.Summarize(s.out, "2024-01-02", "SPY"); err == nil {
		t.Fatal("Summarize accepted a corrupt withheld sale")
	}
}

// TestTheSummaryListsTargetWeightFills: the Boost and momentum engines
// record fills at execution.fill version 5 (ADR 0026, Consequences), so the
// summary lists them as fills and does not count them as inputs not read;
// a Turtle fill (version 4) is still read.
func TestTheSummaryListsTargetWeightFills(t *testing.T) {
	s := newStream(t)
	s.twFill(0, "fill:a", "tw-a", 5)
	s.fill(0)
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedInputs != 0 {
		t.Errorf("rejected inputs = %d, want 0", got.RejectedInputs)
	}
	if len(got.Fills) != 2 || got.Fills[0].InstrumentID != "SPY" || got.Fills[0].Quantity != 5 || got.Fills[0].Price != 96 ||
		got.Fills[0].Side != event.OrderSideSell || got.Fills[0].ClientOrderID != "tw-a" {
		t.Fatalf("fills = %+v", got.Fills)
	}
	if text := report.RenderText(got); !contains(text, "sell SPY 5 @ 96.00") {
		t.Errorf("the summary text does not show the fill:\n%s", text)
	}
}

// TestAnUnreadableFillOfEitherVersionIsCounted: a fill is an input, so one
// at either version that does not read is skipped and counted, never shown.
func TestAnUnreadableFillOfEitherVersionIsCounted(t *testing.T) {
	s := newStream(t)
	s.twFill(0, "fill:a", "tw-a", 5)
	s.out[len(s.out)-1].Payload = []byte(`{`)
	s.fill(0)
	s.out[len(s.out)-1].Payload = []byte(`{`)
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedInputs != 2 || len(got.Fills) != 0 {
		t.Fatalf("rejected %d, fills %+v, want 2 and none", got.RejectedInputs, got.Fills)
	}
}
