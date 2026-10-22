package risklimit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// TestARecoveryIsRestoredWholeOrNotAtAll: a recovered decision's orders,
// sale attempt, retired sales, own live sale and working orders are
// restored together or not at all, so a decision refused for its own live
// sale's attempt number, or for its last working order, leaves the gate as
// it was, and the same decision with the right attempt is then restored in
// full (ADR 0026, point 2, "Order identity", and point 5: recovered, never
// recomputed; fail closed, AGENTS.md).
func TestARecoveryIsRestoredWholeOrNotAtAll(t *testing.T) {
	const incident = "engine-state:joint:1"
	watched := risklimit.RestoredOrder{Order: event.OrderReleasedPayload{AccountID: "joint", ClientOrderID: "tw-watched", InstrumentID: "SPY",
		Side: event.OrderSideSell, Quantity: 10, LimitPrice: 100}, VenueOrderID: "ib-watched"}
	retired := risklimit.RestoredOrder{Order: event.OrderReleasedPayload{AccountID: "joint", ClientOrderID: orderplan.SaleAttemptClientOrderID(incident, "SPY", 0),
		InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 5, LimitPrice: 99}, Ended: at}
	own := risklimit.RestoredOwnSale{RestoredOrder: risklimit.RestoredOrder{Order: event.OrderReleasedPayload{AccountID: "joint", Tag: incident,
		ClientOrderID: orderplan.SaleAttemptClientOrderID(incident, "SPY", 1), InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 5, LimitPrice: 98},
		VenueOrderID: "ib-own"}, Trigger: event.SaleTriggerMarginUrgent, Attempt: 2}
	working := risklimit.RestoredOrder{Order: event.OrderReleasedPayload{AccountID: "joint", ClientOrderID: "tw-working", InstrumentID: "SPY",
		Side: event.OrderSideSell, Quantity: 4, LimitPrice: 101}, VenueOrderID: "ib-working"}
	recovery := func(attempt int, working risklimit.RestoredOrder) risklimit.Recovery {
		o := own
		o.Attempt = attempt
		return risklimit.Recovery{
			Released:     []risklimit.RestoredOrder{watched, retired},
			SaleAttempt:  &risklimit.RestoredSaleAttempt{IncidentID: incident},
			RetiredSales: []risklimit.RetiredSaleAttempt{{Tag: incident, InstrumentID: "SPY", Attempt: 1}},
			OwnSale:      &o,
			Working:      []risklimit.RestoredOrder{working},
		}
	}
	// A working sell under the own sale's venue order ID: the last part
	// restored is the one refused.
	shared := working
	shared.VenueOrderID = own.VenueOrderID
	g := gate(t, limits(), event.TradingModePaper)
	for name, c := range map[string]struct {
		r    risklimit.Recovery
		want string
	}{
		"its own sale recorded as attempt 3":           {recovery(3, working), "attempt 3"},
		"a working sell under its own sale's venue ID": {recovery(2, shared), "venue order ID names one order"},
	} {
		if err := g.RestoreRecovery(c.r); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("a recovery with %s was restored: %v", name, err)
		}
		if got := g.RestoredIncident(); got != "" {
			t.Errorf("the recovery refused for %s restored incident %q's sale attempt", name, got)
		}
		if got := g.PendingSells("SPY"); got != 0 {
			t.Errorf("the recovery refused for %s left %d shares being sold, want none: no order of it is restored", name, got)
		}
	}
	if err := g.RestoreRecovery(recovery(2, working)); err != nil {
		t.Fatalf("the recovery with the right attempt, after the refusal: %v", err)
	}
	if live, ok := g.OwnLiveSale(incident, "SPY"); !ok || live.Attempt != 2 || live.VenueOrderID != "ib-own" {
		t.Errorf("the recovered gate's own live sale is %+v %v, want attempt 2 under venue order ib-own", live, ok)
	}
	if got, want := g.PendingSells("SPY"), watched.Order.Quantity+own.Order.Quantity+working.Order.Quantity; got != want {
		t.Errorf("the recovered gate counts %d shares being sold, want %d: the watched, own and working sells", got, want)
	}
}

// TestARestoredOwnSaleThatEndedStatesHowItEnded: a run's own sale that a
// venue ended is restored with how it ended, canceled or invalid, and only
// then, so a recovered decision whose own sale states an end without how, a
// way it ended that ends nothing, or how with no end, is refused whole, and
// no part of it is restored (ADR 0022; ADR 0026, point 5: recovered, never
// recomputed; fail closed, AGENTS.md).
func TestARestoredOwnSaleThatEndedStatesHowItEnded(t *testing.T) {
	const incident = "engine-state:joint:1"
	own := risklimit.RestoredOwnSale{RestoredOrder: risklimit.RestoredOrder{Order: event.OrderReleasedPayload{AccountID: "joint", Tag: incident,
		ClientOrderID: orderplan.SaleAttemptClientOrderID(incident, "SPY", 0), InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 5, LimitPrice: 98},
		VenueOrderID: "ib-own", Ended: at}, Trigger: event.SaleTriggerMarginUrgent, Attempt: 1}
	for name, c := range map[string]struct {
		ended  bool
		status string
	}{
		"an end without how":       {true, ""},
		"an end as submitted":      {true, event.OrderLifecycleStatusSubmitted},
		"how it ended with no end": {false, event.OrderLifecycleStatusCanceled},
		"an end as cancel-pending": {true, event.OrderLifecycleStatusCancelPending},
	} {
		o := own
		o.EndStatus = c.status
		if !c.ended {
			o.Ended = time.Time{}
		}
		g := gate(t, limits(), event.TradingModePaper)
		err := g.RestoreRecovery(risklimit.Recovery{SaleAttempt: &risklimit.RestoredSaleAttempt{IncidentID: incident}, OwnSale: &o})
		if err == nil || !strings.Contains(err.Error(), "ended") {
			t.Errorf("a recovery whose own sale states %s was restored: %v", name, err)
		}
		if got := g.RestoredIncident(); got != "" {
			t.Errorf("the recovery refused for %s restored incident %q's sale attempt", name, got)
		}
		if _, ok := g.OwnReleasedSale(incident, "SPY"); ok {
			t.Errorf("the recovery refused for %s restored the sale", name)
		}
	}
	o := own
	o.EndStatus = event.OrderLifecycleStatusCanceled
	g := gate(t, limits(), event.TradingModePaper)
	if err := g.RestoreRecovery(risklimit.Recovery{SaleAttempt: &risklimit.RestoredSaleAttempt{IncidentID: incident}, OwnSale: &o}); err != nil {
		t.Fatalf("a recovery whose own sale was canceled: %v", err)
	}
	s, ok := g.OwnReleasedSale(incident, "SPY")
	if !ok || s.Live() || !s.Ended.Equal(at) || s.EndStatus != event.OrderLifecycleStatusCanceled || s.VenueOrderID != "ib-own" {
		t.Errorf("the restored canceled sale is %+v %v, want it ended at %s, canceled, under venue order ib-own", s, ok, at)
	}
	if _, live := g.OwnLiveSale(incident, "SPY"); live {
		t.Error("the restored canceled sale is reported live")
	}
	if got := g.PendingSells("SPY"); got != 0 {
		t.Errorf("the restored canceled sale leaves %d shares being sold, want none", got)
	}
}
