package report_test

import (
	"encoding/json"
	"os"
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

// ADR 0026 D7-F: adapter records retain exact units; owner surfaces show shares.
func TestFractionalAdapterRecordsInSummary(t *testing.T) {
	s, _ := iraDay(t)
	for i := range s.out {
		e := &s.out[i]
		switch e.Type {
		case event.TargetWeightConfigurationEventType:
			var p event.TargetWeightConfigurationPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			p.Execution.ShareQuantity = event.ShareQuantityFractional
			p.Execution.FractionalIncrement = "0.0001"
			e.SchemaVersion = p.SchemaVersion()
			e.Payload, _ = json.Marshal(p)
		case event.AccountSnapshotEventType:
			var p event.AccountCashSnapshotPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			p.QuantityScale = event.FractionalQuantityScale
			p.Positions = []event.AccountPosition{{InstrumentID: "AAA", Quantity: 123456}}
			e.SchemaVersion = p.SchemaVersion()
			e.Payload, _ = json.Marshal(p)
		case event.FillEventType:
			var p event.TargetWeightFillPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			p.QuantityScale, p.Quantity, p.Commission = event.FractionalQuantityScale, 35000, 0.25
			e.SchemaVersion = p.SchemaVersion()
			e.Payload, _ = json.Marshal(p)
		}
	}
	s.add(event.BrokerSnapshotEventType, event.BrokerSnapshotFractionalVersion, "broker", day(0).Add(time.Hour), event.BrokerSnapshotPayload{
		AccountID: "trad-ira", AsOf: day(0).Add(time.Hour), ReadAt: day(0).Add(time.Hour), NotAsOfClose: "synthetic open order after close",
		QuantityScale: event.FractionalQuantityScale, NetLiquidation: 100000, ReadNetLiquidation: 100000, ExcessLiquidity: 100000,
		Positions: []event.BrokerPosition{}, OpenOrders: []event.BrokerOrder{},
	})
	summary, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if !summary.CashUnchecked || summary.CashChecked {
		t.Fatal("fractional broker read lost its unchecked cash status")
	}
	text := report.RenderText(summary)
	for _, want := range []string{"Holdings: AAA 12.3456", "Fills: buy AAA 3.5 @ 10.00", "commissions 0.25"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
	if summary.RejectedInputs != 0 || summary.Costs.FillsNotRead != 0 {
		t.Fatalf("inputs dropped: %+v", summary)
	}
}

func TestShippedIRAUsesFractionalShares(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/etc/engines/trad-ira.config")
	if err != nil {
		t.Fatal(err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Execution.ShareQuantity != event.ShareQuantityFractional || cfg.Execution.FractionalIncrement != "0.0001" {
		t.Fatalf("IRA execution = %+v; want D7-F with increment 0.0001", cfg.Execution)
	}
}

// ADR 0019: a fractional broker reading still says why it cannot stand for the close.
func TestFractionalBrokerSnapshotExplainsWithheldClose(t *testing.T) {
	s := newStream(t)
	const why = "synthetic open order after close"
	s.broker(0, why, false)
	i := len(s.out) - 1
	var p event.BrokerSnapshotPayload
	if err := json.Unmarshal(s.out[i].Payload, &p); err != nil {
		t.Fatal(err)
	}
	p.QuantityScale = event.FractionalQuantityScale
	s.rewrite(i, p)
	s.out[i].SchemaVersion = p.SchemaVersion()
	s.withheld(0, event.WithheldStaleAccountSnapshot)
	got, err := report.Alerts(s.out, "boost")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Body, why) {
		t.Fatalf("fractional broker reason absent: %+v", got)
	}
}
