package event_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestOrderReportsReadsAReportAsTheMomentumRunDoes: the one rule by which a
// run that reads fills reads the venue's reports on its released orders. A
// report naming no released order is ErrNotReleased; one that does not
// describe the order, takes it past its quantity in total, reuses a fill id
// for another execution, or arrives once the order is terminal is refused;
// the same execution again is a redelivery that changes nothing.
func TestOrderReportsReadsAReportAsTheMomentumRunDoes(t *testing.T) {
	at := time.Date(2025, 2, 3, 21, 0, 0, 0, time.UTC)
	released := event.OrderReleasedPayload{AccountID: "ira", ClientOrderID: "tw-1", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 10}
	fill := func(id string, quantity int64) event.TargetWeightFillPayload {
		return event.TargetWeightFillPayload{AccountID: "ira", ClientOrderID: "tw-1", FillID: id, InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: quantity, Price: 10, FilledAt: at}
	}
	lifecycle := event.LimitOrderLifecyclePayload{AccountID: "ira", ClientOrderID: "tw-1", OrderID: "7", InstrumentID: "AAA", Side: event.OrderSideBuy,
		Status: event.OrderLifecycleStatusSubmitted, Quantity: 9, LimitPrice: 10 + 1e-9, OccurredAt: at}
	refused := func(err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v, want one mentioning %q", err, want)
		}
	}

	r := event.NewOrderReports()
	r.Release(released)
	again := released
	again.Quantity = 5
	r.Release(again)
	if o, ok := r.Order("tw-1"); !ok || o.Released.Quantity != 9 {
		t.Fatalf("order %+v, want its first release kept", o)
	}
	if _, err := r.Lifecycle(lifecycle); err != nil {
		t.Fatalf("a lifecycle report that describes the order: %v", err)
	}
	other := lifecycle
	other.ClientOrderID = "tw-other"
	if _, err := r.Lifecycle(other); !errors.Is(err, event.ErrNotReleased) {
		t.Errorf("a report naming no released order: %v, want ErrNotReleased", err)
	}
	moved := lifecycle
	moved.Quantity++
	_, err := r.Lifecycle(moved)
	refused(err, "as released")
	sold := fill("f0", 1)
	sold.Side = event.OrderSideSell
	_, _, err = r.Fill(sold)
	refused(err, "describes")
	_, _, err = r.Fill(fill("f0", 10))
	refused(err, "past its")

	if o, redelivery, err := r.Fill(fill("f1", 5)); err != nil || redelivery || o.Filled != 5 || o.Terminal {
		t.Fatalf("first fill: %+v %v %v", o, redelivery, err)
	}
	if _, redelivery, err := r.Fill(fill("f1", 5)); err != nil || !redelivery {
		t.Errorf("the same execution again: redelivery %v, %v; want a redelivery", redelivery, err)
	}
	_, _, err = r.Fill(fill("f1", 1))
	refused(err, "reuses fill id")
	_, _, err = r.Fill(fill("f2", 5))
	refused(err, "past its")
	o, _, err := r.Fill(fill("f3", 4))
	if err != nil || !o.Terminal || len(o.Fills) != 2 {
		t.Fatalf("the fill that completes it: %+v %v, want it terminal with two fills", o, err)
	}
	_, err = r.Lifecycle(lifecycle)
	refused(err, "already terminal")
	_, _, err = r.Fill(fill("f4", 1))
	refused(err, "already terminal")

	r.Release(event.OrderReleasedPayload{AccountID: "ira", ClientOrderID: "tw-2", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 10})
	r.Release(event.OrderReleasedPayload{AccountID: "ira", ClientOrderID: "tw-0", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 1, LimitPrice: 10})
	if got := r.Working(); !slices.Equal(got, []string{"tw-0", "tw-2"}) {
		t.Errorf("working %v, want the orders not terminal, sorted", got)
	}
	cancelled := lifecycle
	cancelled.ClientOrderID, cancelled.Status, cancelled.LimitPrice = "tw-2", event.OrderLifecycleStatusCanceled, 10
	if o, err := r.Lifecycle(cancelled); err != nil || !o.Terminal {
		t.Errorf("a cancellation: %+v %v, want the order terminal", o, err)
	}
}
