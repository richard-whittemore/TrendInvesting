package report_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func TestFractionalSummaryOrderText(t *testing.T) {
	for _, kind := range []string{"planned", "released", "refused", "planned-released", "planned-refused"} {
		t.Run(kind, func(t *testing.T) {
			s := newStream(t)
			p := event.OrderPlannedPayload{AccountID: "joint", Tag: "targets", ClientOrderID: "fractional", InstrumentID: "SPY", Side: event.OrderSideBuy, Phase: event.OrderSideBuy, OrderType: event.OrderTypeLimit, Quantity: 72500, QuantityScale: event.FractionalQuantityScale, FractionalIncrement: "0.0001", LimitPrice: 101, TargetWeight: 0.98, NetLiquidation: 4000, ReferencePrice: 100, PriceBand: 0.05}
			if strings.Contains(kind, "planned") {
				s.add(event.OrderPlannedEventType, p.SchemaVersion(), "plan", day(0), p)
			}
			if strings.Contains(kind, "released") {
				r := releasedOrder(p.ClientOrderID, event.TradingModeDryRun)
				r.Quantity, r.QuantityScale = p.Quantity, p.QuantityScale
				s.add(event.OrderReleasedEventType, r.SchemaVersion(), "release", day(0), r)
			}
			if strings.Contains(kind, "refused") {
				r := event.OrderRefusedPayload{AccountID: p.AccountID, PlanID: "plan", Tag: p.Tag, ClientOrderID: p.ClientOrderID, InstrumentID: p.InstrumentID, Side: p.Side, Quantity: p.Quantity, QuantityScale: p.QuantityScale, LimitPrice: p.LimitPrice, Reason: event.RefusedOrderSize, Detail: "figures", Figures: figures}
				s.add(event.OrderRefusedEventType, r.SchemaVersion(), "refusal", day(0), r)
			}
			summary, err := report.Summarize(s.out, "2024-01-02", "SPY")
			if err != nil {
				t.Fatal(err)
			}
			text := report.RenderText(summary)
			if !strings.Contains(text, "fractional buy 7.25 SPY") || strings.Contains(text, "72500") {
				t.Fatalf("quantity is not shares:\n%s", text)
			}
		})
	}
}

func TestFractionalRefusedReportText(t *testing.T) {
	for _, strategy := range []string{event.TargetWeightStrategyBoost, event.TargetWeightStrategyMomentum} {
		t.Run(strategy, func(t *testing.T) {
			r := &logRun{t: t}
			o := releasedOrder("fractional", event.TradingModePaper)
			o.Quantity, o.QuantityScale = 72500, event.FractionalQuantityScale
			r.session(0)
			r.add(journal.KindDecision, event.OrderReleasedEventType, o.SchemaVersion(), "release", day(0), o)
			p := event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "7", InstrumentID: o.InstrumentID, Side: o.Side, Status: event.OrderLifecycleStatusSubmitted, Quantity: 75000, QuantityScale: o.QuantityScale, LimitPrice: o.LimitPrice, OccurredAt: day(0).Add(time.Minute)}
			r.add(journal.KindInput, event.OrderLifecycleEventType, p.SchemaVersion(), "ack", p.OccurredAt, p)
			_, unread, err := report.OrderLifecycles(r.out, strategy)
			if err != nil {
				t.Fatal(err)
			}
			if len(unread) != 1 {
				t.Fatalf("unread = %+v", unread)
			}
			text := unread[0].Reason
			if !strings.Contains(text, "buy 7.5 SPY") || !strings.Contains(text, "buy 7.25 SPY") || strings.Contains(text, "72500") || strings.Contains(text, "75000") {
				t.Fatalf("quantity is not shares: %s", text)
			}
		})
	}
}

// The existing Boost fixture's complete text remains byte-identical.
func TestBoostSummaryTextUnchanged(t *testing.T) {
	s, err := report.Summarize(buildDay(t), "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	const want = `joint (boost) daily summary, 2024-01-03
Engine state: normal
Net liquidation 4300.00 as of 2024-01-03 21:00Z
Behind SPY by 2.5 points since 2024-01-02; dollar gap -100.00 (account 4300.00, SPY shadow 4400.00)
Holdings: SPY 39
Targets:
  SPY 98%
Orders:
  tw-b buy 10 SPY limit 101.00: refused (order-size)
Refusals: 1
Fills: none
Costs this month: commissions 0.00, fees 0.00, interest 0.00 (fees and interest not yet recorded by the Flex reader)
Equity vs SPY, 2024-01-02 to 2024-01-03, time-weighted: account +7.50%, SPY +10.00%, difference -2.50 points
Day, 2024-01-02 to 2024-01-03: account +7.50%, SPY +10.00%
Counted: SPY price return only: dividends not counted; deposits and withdrawals not yet recorded: the comparison assumes none, so money you add or take out counts as the account's own gain or loss. Both returns are time-weighted; fees and interest stay in the account's own result.
`
	if got := report.RenderText(s); got != want {
		t.Fatalf("Boost summary changed:\n%s", got)
	}
	raw, err := json.Marshal(s.Orders[0])
	if err != nil {
		t.Fatal(err)
	}
	const wantOrder = `{"ClientOrderID":"tw-b","InstrumentID":"SPY","Side":"buy","Quantity":10,"LimitPrice":101,"Status":"refused","Reason":"order-size","Send":false}`
	if string(raw) != wantOrder {
		t.Fatalf("Boost order JSON changed: %s", raw)
	}
}
