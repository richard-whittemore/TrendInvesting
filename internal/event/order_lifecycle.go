package event

import (
	"errors"
	"fmt"
	"time"
)

// OrderLifecycleEventType identifies the input a venue's adapter sends when
// one of this system's orders changes state WITHOUT executing: the venue
// acknowledged it, amended it, is cancelling it, cancelled it, or refused
// it.
//
// Namespaced "execution.", like FillEventType: it is an external fact about
// what an execution venue did, never a decision this system made. It is
// deliberately a separate type from FillEventType rather than a status on
// it. docs/architecture.md's invariant is that "positions, protective stops,
// and pyramid state change only from recorded brokerage events", and the
// only such event that may move a position is a fill; a lifecycle change
// moves none. Keeping the two apart means no reader of a journal, and no
// reducer, can mistake one for the other.
//
// ADR 0019 needs these facts journalled: expected broker orders are derived
// "from journalled acknowledgements and lifecycle events, not unsubmitted
// proposals", and "an acknowledged, journalled lifecycle transition can
// explain" a change in the broker's open orders. The reducer records this
// input and decides nothing from it (internal/strategy's
// applyOrderLifecycle); reconciliation is what will read it.
const OrderLifecycleEventType = "execution.order.lifecycle"

// OrderLifecycleSchemaVersion is the current schema version of
// OrderLifecyclePayload.
const OrderLifecycleSchemaVersion uint32 = 1

// The closed set of statuses OrderLifecyclePayload.Status may name: every
// state change of an order that is not an execution. Validate rejects any
// other value, so a status a producer invents is never silently misread,
// and a fill in particular can never arrive disguised as one.
const (
	// OrderLifecycleStatusSubmitted is the venue's acknowledgement that it
	// accepted the order and it is working.
	OrderLifecycleStatusSubmitted = "submitted"
	// OrderLifecycleStatusUpdated is the venue's acknowledgement of an
	// amendment: the order now works at StopPrice.
	OrderLifecycleStatusUpdated = "updated"
	// OrderLifecycleStatusCancelPending means a cancellation was requested
	// and the order may still execute until the venue confirms it.
	OrderLifecycleStatusCancelPending = "cancel-pending"
	// OrderLifecycleStatusCanceled means the order no longer works: this
	// system cancelled it, or the venue expired it (a DAY order at the end of
	// its session).
	OrderLifecycleStatusCanceled = "canceled"
	// OrderLifecycleStatusInvalid means the venue refused the order; it never
	// worked.
	OrderLifecycleStatusInvalid = "invalid"
)

// OrderLifecyclePayload records one state change of one order, as the venue
// reported it, in the order the venue reported it.
//
// It carries the order's own figures as the venue stated them at that
// moment — its signed quantity and stop price — so a reader can see which
// order changed and to what, without joining back to the decision that
// placed it. Tag is that join: the id of the decision the order carries
// (the proposal, or the strategy.exit-order.set in force).
type OrderLifecyclePayload struct {
	InstrumentID string `json:"instrument_id"`
	// OrderID is the venue's own identifier for the order, as text so any
	// venue's scheme fits.
	OrderID string `json:"order_id"`
	// Tag is the id of the decision the order carries.
	Tag string `json:"tag"`
	// Status is one of the OrderLifecycleStatus* constants.
	Status string `json:"status"`
	// Quantity is the order's signed share count: positive to buy, negative
	// to sell. Never zero: an order for nothing is not an order.
	Quantity int64 `json:"quantity"`
	// StopPrice is the level the order works at after this change. Every
	// order this system places is a stop order at a stated level (ADR 0005),
	// so it is required and positive.
	StopPrice float64 `json:"stop_price"`
	// OccurredAt is when the venue reported the change: simulated time in a
	// backtest, a real timestamp live. Never read from a wall clock in the
	// domain.
	OccurredAt time.Time `json:"occurred_at"`
	// Message is the venue's own text for the change, if it gave one, such as
	// why it refused or expired the order. Optional.
	Message string `json:"message"`
}

// Validate checks that the change names its instrument, order and tag, that
// Status is one of the enumerated constants, that Quantity is not zero,
// that StopPrice is finite and positive, and that OccurredAt is present and
// writable as RFC 3339.
func (p OrderLifecyclePayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.OrderID == "" {
		errs = append(errs, errors.New("order id is required"))
	}
	if p.Tag == "" {
		errs = append(errs, errors.New("tag is required: it names the decision the order carries"))
	}
	switch p.Status {
	case OrderLifecycleStatusSubmitted, OrderLifecycleStatusUpdated, OrderLifecycleStatusCancelPending,
		OrderLifecycleStatusCanceled, OrderLifecycleStatusInvalid:
		// recognised
	case "":
		errs = append(errs, errors.New("status is required"))
	default:
		errs = append(errs, fmt.Errorf("status %q is not a recognised order lifecycle status; an execution is an %s, never a lifecycle change", p.Status, FillEventType))
	}
	if p.Quantity == 0 {
		errs = append(errs, errors.New("quantity must not be zero: an order for nothing is not an order"))
	}
	switch {
	case !isFinite(p.StopPrice):
		errs = append(errs, errors.New("stop price must be finite"))
	case p.StopPrice <= 0:
		errs = append(errs, errors.New("stop price must be positive: every order rests at a stated level (ADR 0005)"))
	}
	switch {
	case p.OccurredAt.IsZero():
		errs = append(errs, errors.New("occurred at is required"))
	case !writableTime(p.OccurredAt):
		errs = append(errs, errors.New("occurred at cannot be written as RFC 3339"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order lifecycle payload: %w", err)
	}
	return nil
}
