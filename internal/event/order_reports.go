package event

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ErrNotReleased is OrderReports' refusal of a report naming an order the
// run never released.
var ErrNotReleased = errors.New("which this run never released")

// OrderReports is the rule by which a run that reads fills (the momentum
// run) reads the venue's reports on the orders it released. The engine
// applies it, and the reporter reads the log by it, so a report the engine
// refused is never taken as read (ADR 0022; ADR 0019, "Alerting").
//
// A report is read when it names a released order that is not yet terminal
// and describes it: a lifecycle report its account, instrument, side,
// quantity and limit (LimitOrderLifecyclePayload's Describes); a fill its
// account, instrument and side, taking the order no further than its
// quantity in total, under a fill id no other execution has used, since a
// fill id names one execution across the account (TargetWeightFillPayload).
// The same execution delivered again (SameExecution) is a redelivery and
// changes nothing. An order becomes terminal on a lifecycle report that is
// terminal (cancelled or invalid; LimitOrderLifecyclePayload's Terminal), on
// the fill that completes its quantity, on the owner's clearance (Clear), or
// on its retirement as an order no venue received (Retire).
type OrderReports struct {
	orders map[string]*ReportedOrder
	fills  map[string]TargetWeightFillPayload
}

// ReportedOrder is one released order and what the venue has reported of it.
type ReportedOrder struct {
	Released OrderReleasedPayload
	Filled   int64
	Terminal bool
	Fills    map[string]TargetWeightFillPayload // its fills read, by fill id
	// Venue is set once a lifecycle report or a fill on it is read: the
	// venue received it, so it can no longer be retired as never sent.
	Venue bool
}

// NewOrderReports returns the rule with no order released.
func NewOrderReports() *OrderReports {
	return &OrderReports{orders: map[string]*ReportedOrder{}, fills: map[string]TargetWeightFillPayload{}}
}

// Release starts reading reports on o. A client order ID names one order, so
// a later release under it keeps the first.
func (r *OrderReports) Release(o OrderReleasedPayload) {
	if _, seen := r.orders[o.ClientOrderID]; !seen {
		r.orders[o.ClientOrderID] = &ReportedOrder{Released: o, Fills: map[string]TargetWeightFillPayload{}}
	}
}

// Order returns the released order under a client order ID.
func (r *OrderReports) Order(clientOrderID string) (*ReportedOrder, bool) {
	o, ok := r.orders[clientOrderID]
	return o, ok
}

// Working returns the client order IDs of the released orders not yet
// terminal, sorted.
func (r *OrderReports) Working() []string {
	var out []string
	for _, id := range slices.Sorted(maps.Keys(r.orders)) {
		if !r.orders[id].Terminal {
			out = append(out, id)
		}
	}
	return out
}

// Clear reads the owner's clearance of the rebalance in progress, or says
// why it is not read: an order must be working, and the clearance must be
// for the account of the orders working, name the rebalance they serve
// (their Tag) and every one of them, exactly, each dealt with at the broker
// (RebalanceClearedPayload). Once read, each order named is terminal, so a
// later venue report on it is refused as on a terminal order (ADR 0026,
// point 5).
func (r *OrderReports) Clear(p RebalanceClearedPayload) error {
	working := r.Working()
	if len(working) == 0 {
		return errors.New("no rebalance is in progress, every order released is terminal")
	}
	for _, id := range working {
		o := r.orders[id].Released
		switch {
		case o.AccountID != p.AccountID:
			return fmt.Errorf("is for account %q, but order %s is %q's", p.AccountID, id, o.AccountID)
		case o.Tag != p.DecisionID:
			return fmt.Errorf("names %s, but the rebalance in progress is %s", p.DecisionID, o.Tag)
		}
	}
	if !slices.Equal(p.ClientOrderIDs, working) {
		return fmt.Errorf("names orders %v, but the orders not terminal are %v: name exactly those, each dealt with at the broker", p.ClientOrderIDs, working)
	}
	for _, id := range working {
		r.orders[id].Terminal = true
	}
	return nil
}

// Lifecycle reads a lifecycle report, or says why it is not read. The
// returned order's Terminal says whether the report made it terminal.
func (r *OrderReports) Lifecycle(p LimitOrderLifecyclePayload) (*ReportedOrder, error) {
	o, err := r.open(p.ClientOrderID)
	if err != nil {
		return nil, err
	}
	if !p.Describes(o.Released) {
		q := o.Released
		return nil, fmt.Errorf("reports %s %s %s %s at %v, which describes no order %s as released (%s %s %s %s at %v)",
			p.AccountID, p.Side, FormatQuantity(p.Quantity, p.QuantityScale), p.InstrumentID, p.LimitPrice, q.ClientOrderID, q.AccountID, q.Side, FormatQuantity(q.Quantity, q.QuantityScale), q.InstrumentID, q.LimitPrice)
	}
	o.Venue = true
	if p.Terminal() {
		o.Terminal = true
	}
	return o, nil
}

// Retire reads the retirement of a released order the adapter never sent,
// or says why it is not read: the session it was released for ended before
// it could be sent, or the owner retired an order the adapter can never
// send (the owner's decision of 2026-10-22; ADR 0026, point 2, "Boost",
// amended 2026-10-22). It must name a released order that is not terminal,
// describe it as released (OrderRetiredPayload's Describes), and name one no
// venue has reported on: a lifecycle report or a fill shows the venue
// received it, so it was sent. Once read the order is terminal with nothing
// filled, so a later venue report on it is refused as on any terminal order.
func (r *OrderReports) Retire(p OrderRetiredPayload) (*ReportedOrder, error) {
	o, err := r.open(p.ClientOrderID)
	if err != nil {
		return nil, err
	}
	q := o.Released
	switch {
	case !p.Describes(q):
		return nil, fmt.Errorf("retires %s %s %d %s at %v, which describes no order %s as released (%s %s %d %s at %v)",
			p.AccountID, p.Side, p.Quantity, p.InstrumentID, p.LimitPrice, q.ClientOrderID, q.AccountID, q.Side, q.Quantity, q.InstrumentID, q.LimitPrice)
	case o.Venue:
		return nil, fmt.Errorf("retires order %s, which the venue has reported on, so it was sent: only an order no venue received is retired", q.ClientOrderID)
	}
	o.Terminal = true
	return o, nil
}

// Fill reads a fill, or says why it is not read. redelivery is true for the
// same execution read before, which changes nothing; otherwise the returned
// order's Terminal says whether the fill completed it.
func (r *OrderReports) Fill(p TargetWeightFillPayload) (o *ReportedOrder, redelivery bool, err error) {
	if seen, dup := r.fills[p.FillID]; dup {
		if seen.SameExecution(p) {
			return r.orders[seen.ClientOrderID], true, nil
		}
		return nil, false, fmt.Errorf("reuses fill id %s for a different execution", p.FillID)
	}
	if o, err = r.open(p.ClientOrderID); err != nil {
		return nil, false, err
	}
	q := o.Released
	if p.QuantityScale != q.QuantityScale {
		return nil, false, fmt.Errorf("quantity scale %d differs from released order %s scale %d", p.QuantityScale, q.ClientOrderID, q.QuantityScale)
	}
	if p.Quantity <= 0 {
		return nil, false, errors.New("fill quantity must be positive")
	}
	if p.AccountID != q.AccountID || p.InstrumentID != q.InstrumentID || p.Side != q.Side {
		return nil, false, fmt.Errorf("describes %s %s %s, but order %s is %s %s %s", p.AccountID, p.Side, p.InstrumentID, q.ClientOrderID, q.AccountID, q.Side, q.InstrumentID)
	}
	// Against what remains, not the sum: the sum can wrap past int64.
	if p.Quantity > q.Quantity-o.Filled {
		return nil, false, fmt.Errorf("takes order %s past its %d shares", q.ClientOrderID, q.Quantity)
	}
	r.fills[p.FillID] = p
	o.Venue = true
	o.Fills[p.FillID] = p
	o.Filled += p.Quantity
	o.Terminal = o.Filled == q.Quantity
	return o, false, nil
}

// open returns the released order a report names, refusing one never
// released or already terminal.
func (r *OrderReports) open(clientOrderID string) (*ReportedOrder, error) {
	o, ok := r.orders[clientOrderID]
	switch {
	case !ok:
		return nil, fmt.Errorf("names order %s, %w", clientOrderID, ErrNotReleased)
	case o.Terminal:
		return nil, fmt.Errorf("reports on order %s, which is already terminal", clientOrderID)
	}
	return o, nil
}
