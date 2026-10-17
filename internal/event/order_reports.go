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
// terminal (cancelled or invalid; LimitOrderLifecyclePayload's Terminal) or
// on the fill that completes its quantity.
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

// Lifecycle reads a lifecycle report, or says why it is not read. The
// returned order's Terminal says whether the report made it terminal.
func (r *OrderReports) Lifecycle(p LimitOrderLifecyclePayload) (*ReportedOrder, error) {
	o, err := r.open(p.ClientOrderID)
	if err != nil {
		return nil, err
	}
	if !p.Describes(o.Released) {
		q := o.Released
		return nil, fmt.Errorf("reports %s %s %d %s at %v, which describes no order %s as released (%s %s %d %s at %v)",
			p.AccountID, p.Side, p.Quantity, p.InstrumentID, p.LimitPrice, q.ClientOrderID, q.AccountID, q.Side, q.Quantity, q.InstrumentID, q.LimitPrice)
	}
	if p.Terminal() {
		o.Terminal = true
	}
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
	if p.AccountID != q.AccountID || p.InstrumentID != q.InstrumentID || p.Side != q.Side {
		return nil, false, fmt.Errorf("describes %s %s %s, but order %s is %s %s %s", p.AccountID, p.Side, p.InstrumentID, q.ClientOrderID, q.AccountID, q.Side, q.InstrumentID)
	}
	// Against what remains, not the sum: the sum can wrap past int64.
	if p.Quantity > q.Quantity-o.Filled {
		return nil, false, fmt.Errorf("takes order %s past its %d shares", q.ClientOrderID, q.Quantity)
	}
	r.fills[p.FillID] = p
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
