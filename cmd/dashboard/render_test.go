package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func TestDashboardSaysWhetherAReleasedOrderWasSent(t *testing.T) {
	var buf bytes.Buffer
	s := report.Summary{AccountID: "joint", Date: "2024-01-02", Benchmark: "SPY", Orders: []report.OrderLine{
		{ClientOrderID: "tw-a", Side: event.OrderSideBuy, Quantity: 39, InstrumentID: "SPY", LimitPrice: 101, Status: report.OrderReleased, Send: true},
		{ClientOrderID: "tw-b", Side: event.OrderSideBuy, Quantity: 1, InstrumentID: "SPY", LimitPrice: 101, Status: report.OrderReleased},
		{ClientOrderID: "tw-c", Side: event.OrderSideBuy, Quantity: 2, InstrumentID: "SPY", LimitPrice: 101, Status: report.OrderRefused, Reason: "order-size"},
	}}
	if err := page.Execute(&buf, []account{{Summary: s}}); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{"<td>tw-a</td><td>buy 39 SPY @ 101.00</td><td>released (send requested)</td>", "<td>tw-b</td><td>buy 1 SPY @ 101.00</td><td>released (not sent)</td>", "<td>refused</td><td>order-size</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
}

func TestDashboardShowsEveryTimeInUTC(t *testing.T) {
	eastern := time.FixedZone("EST", -5*3600)
	var buf bytes.Buffer
	s := report.Summary{AccountID: "joint", Date: "2024-01-02", Benchmark: "SPY", HaveSnapshot: true, HoldingsAsOf: time.Date(2024, time.January, 2, 16, 0, 0, 0, eastern)}
	alerts := []alertView{{Kind: report.KindRefusal, Severity: report.Urgent, At: time.Date(2024, time.January, 2, 16, 5, 0, 0, eastern), Title: "t"}}
	if err := page.Execute(&buf, []account{{Summary: s, Alerts: alerts}}); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if !strings.Contains(body, "as of 2024-01-02 21:00Z") || !strings.Contains(body, "<td>2024-01-02 21:05Z</td>") || strings.Contains(body, "16:0") {
		t.Fatalf("a time with an offset must be shown in UTC, as its Z says:\n%s", body)
	}
}

// TestDashboardShowsTheDaysWithheldSalesAndFillSides: the dashboard shows
// the summary the reporter sends, so a lapsed sale back to 98% and a
// target-weight fill's side appear on the page as in the daily summary.
func TestDashboardShowsTheDaysWithheldSalesAndFillSides(t *testing.T) {
	var buf bytes.Buffer
	s := report.Summary{AccountID: "joint", Date: "2024-01-02", Benchmark: "SPY",
		Fills:         []report.FillLine{{InstrumentID: "SPY", Side: event.OrderSideSell, ClientOrderID: "sale-1", Quantity: 5, Price: 96}},
		SalesWithheld: []report.SaleWithheldLine{{ClientOrderID: "sale-1", InstrumentID: "SPY", Trigger: event.SaleTriggerExit, Reason: event.SaleWithheldLapsed, Detail: "the exit is no longer refused"}}}
	if err := page.Execute(&buf, []account{{Summary: s}}); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{"<td>sell</td><td>SPY</td><td>5 @ 96.00</td><td>sale-1</td>", "<h3>Sales back to 98% withheld</h3>", "<td>sale-1</td><td>SPY</td><td>lapsed</td><td>exit</td><td>the exit is no longer refused</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
}

// TestDashboardShowsTheDaysSkippedTrades: a skipped trade is recorded so no
// planned trade disappears silently (ADR 0026, point 8), and the dashboard
// shows the summary the reporter sends, so each of the day's skips appears
// with its side, the shares the rule asked for and its reason.
func TestDashboardShowsTheDaysSkippedTrades(t *testing.T) {
	var buf bytes.Buffer
	s := report.Summary{AccountID: "joint", Date: "2024-01-02", Benchmark: "SPY",
		Skipped: []report.SkippedLine{{ClientOrderID: "tw-a", InstrumentID: "SPY", Side: event.OrderSideBuy, Reason: event.OrderSkippedBelowOneShare, Shares: 0.004}}}
	if err := page.Execute(&buf, []account{{Summary: s}}); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{"<h3>Trades skipped</h3>", "<td>tw-a</td><td>buy</td><td>SPY</td><td>0.004 shares</td><td>below-one-share</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	buf.Reset()
	if err := page.Execute(&buf, []account{{Summary: report.Summary{AccountID: "joint"}}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Trades skipped") {
		t.Error("a day with no skipped trade shows the section")
	}
}
