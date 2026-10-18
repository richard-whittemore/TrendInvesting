package targetweight_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestBoostRecordsAReportOnlyForAnOrderItReleased: a Boost run records the
// venue's report about an order it released and decides nothing (ADR 0022).
// A report naming an order it did not release, or naming it with other
// figures, or at the Turtle reducer's lifecycle version, is refused
// (docs/development.md principle 4; ADR 0015).
func TestBoostRecordsAReportOnlyForAnOrderItReleased(t *testing.T) {
	r := simulate(t, options{})
	var order event.OrderReleasedPayload
	decode(t, ofType(r.decisions, event.OrderReleasedEventType)[0], &order)
	s := &stream{hash: r.hash, sequence: uint64(len(r.inputs))}
	at := sessionEnd(len(syntheticCloses())).Add(time.Hour)
	report := func(mutate func(*event.LimitOrderLifecyclePayload)) event.LimitOrderLifecyclePayload {
		p := event.LimitOrderLifecyclePayload{AccountID: order.AccountID, ClientOrderID: order.ClientOrderID, OrderID: "7", InstrumentID: order.InstrumentID,
			Side: order.Side, Status: event.OrderLifecycleStatusSubmitted, Quantity: order.Quantity, LimitPrice: order.LimitPrice, OccurredAt: at}
		if mutate != nil {
			mutate(&p)
		}
		return p
	}
	apply := func(version uint32, p event.LimitOrderLifecyclePayload) ([]event.Envelope, error) {
		return r.recorder.Apply(context.Background(), s.envelope(t, event.OrderLifecycleEventType, version, "lifecycle", at, p))
	}

	if out, err := apply(event.OrderLifecycleLimitVersion, report(nil)); err != nil || len(out) != 0 {
		t.Fatalf("the report about a released order: %v, %d decisions; want it recorded, deciding nothing", err, len(out))
	}
	// The venue's echo of the limit need not be bit-equal to the engine's
	// (.greptile/rules.md, "Numbers and money"): within half the cent the
	// limit is rounded to, it is the same price.
	echoed := report(func(p *event.LimitOrderLifecyclePayload) { p.LimitPrice = order.LimitPrice + 1e-9 })
	if _, err := apply(event.OrderLifecycleLimitVersion, echoed); err != nil {
		t.Fatalf("a report whose limit differs from the released one by a rounding error: %v, want it recorded", err)
	}
	for name, c := range map[string]struct {
		version uint32
		mutate  func(*event.LimitOrderLifecyclePayload)
		want    string
	}{
		"an order not released": {event.OrderLifecycleLimitVersion, func(p *event.LimitOrderLifecyclePayload) { p.ClientOrderID = "tw-other" }, "did not release"},
		"other figures":         {event.OrderLifecycleLimitVersion, func(p *event.LimitOrderLifecyclePayload) { p.Quantity++ }, "was released as"},
		"a limit a cent away":   {event.OrderLifecycleLimitVersion, func(p *event.LimitOrderLifecyclePayload) { p.LimitPrice = order.LimitPrice + 0.01 }, "was released as"},
		"the Turtle version":    {event.OrderLifecycleSchemaVersion, nil, "schema version"},
	} {
		if _, err := apply(c.version, report(c.mutate)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, c.want)
		}
	}
}

// TestBoostRecordsAReportOnTheSaleBackTo98: the sale back to 98% is an order
// Boost released like any other, so the venue's report about it is recorded
// (ADR 0022; ADR 0019, "The sale back to 98%").
func TestBoostRecordsAReportOnTheSaleBackTo98(t *testing.T) {
	r := simulate(t, options{brokers: true, kill: map[int]bool{19: true}})
	sold := sales(t, r.decisions)
	if len(sold) != 1 {
		t.Fatalf("one sale back to 98%%: %+v", sold)
	}
	var sale event.OrderReleasedPayload
	for _, e := range ofType(r.decisions, event.OrderReleasedEventType) {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		if p.ClientOrderID == sold[0].ClientOrderID {
			sale = p
		}
	}
	if sale.ClientOrderID == "" {
		t.Fatalf("the sale %s was not released", sold[0].ClientOrderID)
	}
	s := &stream{hash: r.hash, sequence: uint64(len(r.inputs))}
	at := sessionEnd(len(syntheticCloses())).Add(time.Hour)
	report := event.LimitOrderLifecyclePayload{AccountID: sale.AccountID, ClientOrderID: sale.ClientOrderID, OrderID: "8", InstrumentID: sale.InstrumentID,
		Side: sale.Side, Status: event.OrderLifecycleStatusSubmitted, Quantity: sale.Quantity, LimitPrice: sale.LimitPrice, OccurredAt: at}
	if out, err := r.recorder.Apply(context.Background(), s.envelope(t, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "lifecycle:sale", at, report)); err != nil || len(out) != 0 {
		t.Fatalf("the report about the released sale: %v, %d decisions; want it recorded, deciding nothing", err, len(out))
	}
}
