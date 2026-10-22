package event_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validRetired() event.OrderRetiredPayload {
	return event.OrderRetiredPayload{
		AccountID: "DU0000001", ClientOrderID: "tw-paper-1", InstrumentID: "SPY", Side: event.OrderSideBuy,
		Quantity: 9, LimitPrice: 401.5, Reason: event.OrderRetiredSessionOver,
		Detail:    "order tw-paper-1 was released for the session of 2026-10-16, not today's (2026-10-17)",
		RetiredBy: event.OrderRetiredByAdapter, RetiredAt: time.Date(2026, time.October, 17, 13, 30, 0, 0, time.UTC),
	}
}

// TestOrderRetiredValidate: a retirement names the order as it was
// released, one of the two reasons (the session over, retired by the
// adapter; or the owner's retirement, named by the owner), what refused it
// or why the owner retired it, and when, to the whole second the adapter
// stamps (the owner's decision of 2026-10-22, ADR 0026, point 2, "Boost",
// amended 2026-10-22).
func TestOrderRetiredValidate(t *testing.T) {
	v := validRetired()
	if err := v.Validate(); err != nil {
		t.Fatalf("the fixture is invalid: %v", err)
	}
	owner := v
	owner.Reason, owner.RetiredBy, owner.Detail = event.OrderRetiredByOwnerReason, "richard", "no IBKR contract for SPY"
	if err := owner.Validate(); err != nil {
		t.Fatalf("the owner's retirement is invalid: %v", err)
	}
	type P = event.OrderRetiredPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "client order id", v, func(p *P) { p.ClientOrderID = "" }, "client order id")
	expectInvalid(t, "instrument", v, func(p *P) { p.InstrumentID = "" }, "instrument")
	expectInvalid(t, "side", v, func(p *P) { p.Side = "short" }, "side")
	expectInvalid(t, "quantity", v, func(p *P) { p.Quantity = 0 }, "quantity")
	expectInvalid(t, "limit", v, func(p *P) { p.LimitPrice = 0 }, "limit price")
	expectInvalid(t, "no reason", v, func(p *P) { p.Reason = "" }, "reason")
	expectInvalid(t, "an unknown reason", v, func(p *P) { p.Reason = "cancelled" }, "reason")
	expectInvalid(t, "detail", v, func(p *P) { p.Detail = "" }, "detail")
	expectInvalid(t, "the session over by anyone but the adapter", v, func(p *P) { p.RetiredBy = "richard" }, "adapter")
	expectInvalid(t, "the owner's retirement naming no one", owner, func(p *P) { p.RetiredBy = "" }, "retired by")
	expectInvalid(t, "the owner's retirement by the adapter", owner, func(p *P) { p.RetiredBy = event.OrderRetiredByAdapter }, "owner")
	expectInvalid(t, "no time", v, func(p *P) { p.RetiredAt = time.Time{} }, "retired at")
	expectInvalid(t, "a fraction of a second", v, func(p *P) { p.RetiredAt = p.RetiredAt.Add(time.Millisecond) }, "whole second")
}

// TestOrderRetiredDescribesTheReleaseItNames: a retirement is read only for
// the order it names as released: the same client order ID, account,
// instrument, side, quantity and limit price, the limit within
// LimitPriceTolerance as a venue's report is read.
func TestOrderRetiredDescribesTheReleaseItNames(t *testing.T) {
	p := validRetired()
	o := event.OrderReleasedPayload{AccountID: "DU0000001", ClientOrderID: "tw-paper-1", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 401.5}
	if !p.Describes(o) {
		t.Fatal("the retirement does not describe the order it names")
	}
	o.LimitPrice = 401.504
	if !p.Describes(o) {
		t.Error("a limit within the tolerance is the same limit")
	}
	for name, change := range map[string]func(*event.OrderReleasedPayload){
		"another order":   func(o *event.OrderReleasedPayload) { o.ClientOrderID = "tw-paper-2" },
		"another account": func(o *event.OrderReleasedPayload) { o.AccountID = "DU0000002" },
		"another side":    func(o *event.OrderReleasedPayload) { o.Side = event.OrderSideSell },
		"another size":    func(o *event.OrderReleasedPayload) { o.Quantity = 10 },
		"another limit":   func(o *event.OrderReleasedPayload) { o.LimitPrice = 401.52 },
	} {
		q := o
		change(&q)
		if p.Describes(q) {
			t.Errorf("%s: the retirement describes %+v", name, q)
		}
	}
}

func TestOrderRetiredRoundTrips(t *testing.T) {
	raw, err := json.Marshal(validRetired())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reason":"session over"`, `"retired_by":"adapter"`, `"retired_at":"2026-10-17T13:30:00Z"`, `"client_order_id":"tw-paper-1"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("encoded %s, want %s", raw, want)
		}
	}
	if event.OrderRetiredEventType != "execution.order.retired" || event.OrderRetiredSchemaVersion != 1 {
		t.Errorf("type %s version %d", event.OrderRetiredEventType, event.OrderRetiredSchemaVersion)
	}
}
