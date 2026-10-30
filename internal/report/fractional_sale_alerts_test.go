package report_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// ADR 0019 Alerting and ADR 0026 D7-F: exercise the alert consumer with a
// synthetic legacy sale plan and a fractional release of the same shares.
// The current engine rejects fractional plans with margin-sale figures;
// this isolates report decoding and does not enable D6 sales for IRAs.
func fractionalSale(t *testing.T) (*stream, event.OrderReleasedPayload) {
	t.Helper()
	s := newIRAStream(t)
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(s.out[0].Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Execution.ShareQuantity = event.ShareQuantityFractional
	cfg.Execution.FractionalIncrement = "0.0001"
	s.rewrite(0, cfg)
	s.out[0].SchemaVersion = cfg.SchemaVersion()
	p := event.OrderPlannedPayload{AccountID: iraInstance, Tag: "incident", ClientOrderID: "sale", InstrumentID: "AAA",
		Side: event.OrderSideSell, Phase: event.OrderSideSell, OrderType: event.OrderTypeLimit,
		Quantity: 20, LimitPrice: 95,
		TargetWeight: 0.98, NetLiquidation: 4000, HeldQuantity: 60, ReferencePrice: 100, PriceBand: 0.05,
		Sale: event.SaleFigures{Kind: event.SaleKindIncident, Trigger: event.SaleTriggerExit, SnapshotID: "broker",
			KeepShares: 40, ToTarget: 20, WithinHolding: 60, NeverBelow: 20}}
	s.add(event.OrderPlannedEventType, p.SchemaVersion(), "plan", monthEnd(1), p)
	r := event.OrderReleasedPayload{AccountID: p.AccountID, PlanID: "plan", Tag: p.Tag, ClientOrderID: p.ClientOrderID,
		InstrumentID: p.InstrumentID, Side: p.Side, Quantity: 200000, QuantityScale: 10000,
		LimitPrice: p.LimitPrice, Mode: event.TradingModePaper, Send: true,
		Figures: event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 1.5, ExposureAfter: 0.98,
			OrderFraction: 0.5, OrdersInDecision: 1, RiskReducing: true, BrokerSet: true, BrokerHolding: 600000}}
	s.add(event.OrderReleasedEventType, r.SchemaVersion(), "release", monthEnd(1), r)
	return s, r
}

func TestFractionalSaleAlerts(t *testing.T) {
	for _, terminal := range []string{"complete", event.OrderLifecycleStatusCanceled, event.OrderLifecycleStatusInvalid} {
		t.Run(terminal, func(t *testing.T) {
			s, r := fractionalSale(t)
			fill := event.TargetWeightFillPayload{AccountID: r.AccountID, ClientOrderID: r.ClientOrderID, FillID: "part",
				InstrumentID: r.InstrumentID, Side: r.Side, Quantity: 35000, QuantityScale: r.QuantityScale,
				Price: 96, Commission: 0.25, FilledAt: monthEnd(1)}
			s.add(event.FillEventType, fill.SchemaVersion(), "part", monthEnd(1), fill)
			s.add(event.FillEventType, fill.SchemaVersion(), "part", monthEnd(1), fill)
			if terminal == "complete" {
				fill.FillID, fill.Quantity = "rest", r.Quantity-fill.Quantity
				s.add(event.FillEventType, fill.SchemaVersion(), "rest", monthEnd(1), fill)
			} else {
				life := event.LimitOrderLifecyclePayload{AccountID: r.AccountID, ClientOrderID: r.ClientOrderID,
					OrderID: "synthetic-order", InstrumentID: r.InstrumentID, Side: r.Side, Quantity: r.Quantity,
					QuantityScale: r.QuantityScale, LimitPrice: r.LimitPrice, Status: terminal, OccurredAt: monthEnd(1)}
				s.add(event.OrderLifecycleEventType, life.SchemaVersion(), "end", monthEnd(1), life)
			}
			alerts := alertsOf(t, s)
			sent, fills, failed := ofKind(alerts, report.KindSaleSent), ofKind(alerts, report.KindSaleFilled), ofKind(alerts, report.KindSaleFailed)
			if len(sent) != 1 || !strings.Contains(sent[0].Body, "sell 20 AAA") {
				t.Errorf("sent = %+v", sent)
			}
			if len(fills) == 0 {
				t.Errorf("no fractional fill alert")
			} else if !strings.Contains(fills[0].Body, "3.5 of 20") || !strings.Contains(fills[0].Body, "16.5 still to fill") {
				t.Errorf("partial = %+v", fills[0])
			}
			if terminal == "complete" {
				if len(fills) != 2 || !strings.Contains(fills[1].Body, "20 of 20 filled, the sale is complete") || len(failed) != 0 {
					t.Errorf("complete fills=%+v failed=%+v", fills, failed)
				}
			} else if len(fills) != 1 || len(failed) != 1 || failed[0].Severity != report.Urgent || !strings.Contains(failed[0].Body, "3.5 of 20 filled") || !strings.Contains(failed[0].Body, "remaining 16.5 by hand") {
				t.Errorf("terminal fills=%+v failed=%+v", fills, failed)
			}
		})
	}
}

func TestFractionalSaleDoesNotCountLegacyFillUnits(t *testing.T) {
	s, r := fractionalSale(t)
	fill := event.TargetWeightFillPayload{AccountID: r.AccountID, ClientOrderID: r.ClientOrderID, FillID: "legacy",
		InstrumentID: r.InstrumentID, Side: r.Side, Quantity: 1, Price: 96, FilledAt: monthEnd(1)}
	s.add(event.FillEventType, fill.SchemaVersion(), "legacy", monthEnd(1), fill)
	if got := ofKind(alertsOf(t, s), report.KindSaleFilled); len(got) != 0 {
		t.Fatalf("mismatched units raised %+v", got)
	}
}
