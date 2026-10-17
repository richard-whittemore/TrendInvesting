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
	for _, want := range []string{"<td>tw-a</td><td>buy 39 SPY @ 101.00</td><td>released (sent)</td>", "<td>tw-b</td><td>buy 1 SPY @ 101.00</td><td>released (not sent)</td>", "<td>refused</td><td>order-size</td>"} {
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
