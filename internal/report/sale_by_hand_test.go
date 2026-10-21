package report_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// handSaleSteps are the phrases of docs/manual/states-and-kill-switch.md,
// "Selling by hand safely", a "sell by hand" page must carry: stop the
// engine and the adapter, cancel every open order on the instrument, confirm
// none is open, sell, and come back as a fresh run whose approval states the
// book after the sale.
var handSaleSteps = []string{
	"stop the engine and the adapter",
	"cancel every open order on the instrument",
	"confirm there that none is open",
	"only then sell",
	"fresh run",
	"states the book as it is after your sale",
	"never resume the stopped run",
}

func carriesHandSaleSteps(t *testing.T, name, body string) {
	t.Helper()
	for _, step := range handSaleSteps {
		if !strings.Contains(body, step) {
			t.Errorf("%s: the page does not say %q:\n%s", name, step, body)
		}
	}
}

// saleRefused is the gate's refusal of a planned sale back to 98% (the
// safety version, which carries the broker figures).
func (s *stream) saleRefused(n int, clientID, reason string) {
	s.add(event.OrderRefusedEventType, event.OrderRefusedSafetyVersion, "refuse:"+clientID, day(n),
		event.OrderRefusedPayload{AccountID: "joint", PlanID: "plan:" + clientID, Tag: "alarm:u", ClientOrderID: clientID,
			InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 20, LimitPrice: 95, Reason: reason, Detail: "broker holds 10",
			Figures: event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 1.5, ExposureAfter: 0.98,
				OrderFraction: 0.5, OrdersInDecision: 1, RiskReducing: true, BrokerSet: true, BrokerHolding: 10}})
}

// TestANonPositiveWithheldSalePagesSellByHand: a sale back to 98% withheld
// because IBKR reported a net liquidation or mark of zero or less is decided
// again from the next positive snapshot, but until one comes nothing sells,
// so the owner is paged with the hand-sale steps (docs/manual/
// states-and-kill-switch.md, "Selling by hand safely"): stopping the engine
// and the adapter first keeps the decided-again sale from going out beside
// the owner's. One page per sale: the same sale withheld again on the next
// snapshot is the same incident.
func TestANonPositiveWithheldSalePagesSellByHand(t *testing.T) {
	s := newStream(t)
	s.saleWithheld(0, "sale-1", event.SaleWithheldNotPositive)
	s.add(event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, "sale-withheld:again", day(1),
		event.SaleWithheldPayload{AccountID: "joint", Tag: "alarm:u", ClientOrderID: "sale-1", InstrumentID: "SPY",
			Kind: event.SaleKindUrgent, Trigger: event.SaleTriggerMarginUrgent, Reason: event.SaleWithheldNotPositive, Detail: "still zero", SnapshotID: "broker:v"})
	got := ofKind(alertsOf(t, s), report.KindSaleByHand)
	if len(got) != 1 {
		t.Fatalf("by-hand alerts = %+v, want one for the sale", got)
	}
	a := got[0]
	if a.Severity != report.Urgent || !contains(a.Title, "sell by hand") || !contains(a.Title, event.SaleWithheldNotPositive) || a.AccountID != "joint" {
		t.Errorf("alert = %+v", a)
	}
	if !contains(a.Body, "zero or less") || !contains(a.Body, "decided again") {
		t.Errorf("the page does not say why or that the program may still sell:\n%s", a.Body)
	}
	carriesHandSaleSteps(t, "non-positive", a.Body)
}

// TestANonPositiveWithholdUnderANewAlarmPagesAgain: the once-per-sale rule
// is per alarm, so a later alarm's non-positive withhold of the same
// instrument is a new incident and pages again with its own key (ADR 0026,
// amended 2026-10-17: a new alarm is minted on every change of the alarm's
// level into urgent).
func TestANonPositiveWithholdUnderANewAlarmPagesAgain(t *testing.T) {
	s := newStream(t)
	s.saleWithheld(0, "sale-1", event.SaleWithheldNotPositive)
	s.add(event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, "sale-withheld:other-alarm", day(3),
		event.SaleWithheldPayload{AccountID: "joint", Tag: "alarm:v", ClientOrderID: "sale-2", InstrumentID: "SPY",
			Kind: event.SaleKindUrgent, Trigger: event.SaleTriggerMarginUrgent, Reason: event.SaleWithheldNotPositive, Detail: "zero again", SnapshotID: "broker:w"})
	got := ofKind(alertsOf(t, s), report.KindSaleByHand)
	if len(got) != 2 || got[0].Key == got[1].Key {
		t.Fatalf("by-hand alerts = %+v, want one per alarm with distinct keys", got)
	}
}

// TestASaleRefusedAsBrokerHoldingPagesSellByHand: the sale back to 98%
// refused at the gate as broker-holding spends its one attempt (ADR 0019,
// amended 2026-10-15), so beside the refusal alert the owner is paged to
// sell by hand, with the hand-sale steps. An ordinary order refused for the
// same reason is not a sale and raises only the refusal.
func TestASaleRefusedAsBrokerHoldingPagesSellByHand(t *testing.T) {
	s := newStream(t)
	s.salePlanned(0, "sale-1")
	s.saleRefused(0, "sale-1", event.RefusedBrokerHolding)
	got := alertsOf(t, s)
	if refusals := ofKind(got, report.KindRefusal); len(refusals) != 1 {
		t.Errorf("refusal alerts = %+v, want the refusal still", refusals)
	}
	hand := ofKind(got, report.KindSaleByHand)
	if len(hand) != 1 {
		t.Fatalf("by-hand alerts = %+v", hand)
	}
	a := hand[0]
	if a.Severity != report.Urgent || !contains(a.Title, "sell by hand") || !contains(a.Title, "SPY") || !contains(a.Body, event.RefusedBrokerHolding) ||
		!contains(a.Body, "sale-1") || !contains(a.Body, "will not try again") || !contains(a.Body, "broker holds 10") {
		t.Errorf("alert = %+v", a)
	}
	carriesHandSaleSteps(t, "broker-holding", a.Body)

	s = newStream(t)
	s.planned(0, "tw-a", 3)
	s.saleRefused(0, "tw-a", event.RefusedBrokerHolding)
	if hand := ofKind(alertsOf(t, s), report.KindSaleByHand); len(hand) != 0 {
		t.Errorf("an ordinary order's refusal raised %+v", hand)
	}
	s = newStream(t)
	s.salePlanned(0, "sale-1")
	s.saleRefused(0, "sale-1", event.RefusedSaleAttempted)
	if hand := ofKind(alertsOf(t, s), report.KindSaleByHand); len(hand) != 0 {
		t.Errorf("a sale refused for another reason raised %+v", hand)
	}
}
