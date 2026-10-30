package event

import (
	"errors"
	"fmt"
	"time"
)

// OrderRetiredEventType identifies the input the IBKR adapter sends when a
// released order it can never send is retired: the paper sender refused it
// before any write-ahead mark, so it never left the adapter and no venue
// ever received it (the owner's decision of 2026-10-22; ADR 0026, point 2,
// "Boost", amended 2026-10-22).
//
// Namespaced "execution." because it is a fact about the order's execution,
// recorded by the adapter, never a decision this system's strategy made. It
// is deliberately a type of its own rather than a lifecycle status: a
// lifecycle report is the venue's word on an order it received, and an
// order retired here was never received by any venue, so no reader can take
// a retirement for a venue's cancel or refusal (ADR 0022).
const OrderRetiredEventType = "execution.order.retired"

// OrderRetiredSchemaVersion is the current schema version of
// OrderRetiredPayload.
const OrderRetiredSchemaVersion uint32 = 1

// The closed set of reasons an order is retired, and who retires it.
const (
	// OrderRetiredSessionOver: the order was released for a session that is
	// over before the adapter could send it (a DAY limit order is for its
	// own session only, ADR 0026, point 2, "Order type and price"). The
	// adapter retires it itself, as OrderRetiredByAdapter.
	OrderRetiredSessionOver = "session over"
	// OrderRetiredByOwnerReason: the adapter can never send the order as it
	// stands (not a limit order, a fractional quantity, or no IBKR contract
	// ID), so it stopped at it, and the owner retired it with the relay's
	// retire command once the cause was found; RetiredBy names the owner.
	OrderRetiredByOwnerReason = "owner retired"
	// OrderRetiredByAdapter is RetiredBy for a retirement the adapter made
	// itself.
	OrderRetiredByAdapter = "adapter"
)

// OrderRetiredPayload retires one released order no venue received. It
// names the order as it was released, so a reader joins it to that release
// as it joins a venue's report (Describes), and says why, what refused it
// or why the owner retired it, who retired it, and when.
type OrderRetiredPayload struct {
	QuantityScale int64   `json:"quantity_scale,omitempty"`
	AccountID     string  `json:"account_id"`
	ClientOrderID string  `json:"client_order_id"`
	InstrumentID  string  `json:"instrument_id"`
	Side          string  `json:"side"`
	Quantity      int64   `json:"quantity"`
	LimitPrice    float64 `json:"limit_price"`
	// Reason is OrderRetiredSessionOver or OrderRetiredByOwnerReason.
	Reason string `json:"reason"`
	// Detail is the sender's refusal, for the session over, or the owner's
	// reason, for the owner's retirement.
	Detail string `json:"detail"`
	// RetiredBy is OrderRetiredByAdapter for the session over, and the
	// owner's name for the owner's retirement.
	RetiredBy string `json:"retired_by"`
	// RetiredAt is when the adapter refused the order or the owner retired
	// it, to the whole second the adapter stamps its inputs with.
	RetiredAt time.Time `json:"retired_at"`
}

// Describes reports whether p retires the released order o as it was
// released: the same client order ID, account, instrument, side and
// quantity, and the same limit price (SameLimitPrice), as a venue's
// lifecycle report is read (LimitOrderLifecyclePayload's Describes).
func (p OrderRetiredPayload) Describes(o OrderReleasedPayload) bool {
	return p.ClientOrderID == o.ClientOrderID && p.AccountID == o.AccountID && p.InstrumentID == o.InstrumentID &&
		p.Side == o.Side && p.QuantityScale == o.QuantityScale && p.Quantity == o.Quantity && SameLimitPrice(p.LimitPrice, o.LimitPrice)
}

// Validate checks the order's identity, side, quantity and limit, that the
// reason is one of the two and who retired it fits it (the adapter for the
// session over, a named owner for the owner's retirement), that the detail
// says why, and that the time is a whole second.
func (p OrderRetiredPayload) Validate() error {
	if err := quantityScaleError(p.QuantityScale); err != nil {
		return err
	}
	errs := orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)
	switch p.Reason {
	case OrderRetiredSessionOver:
		if p.RetiredBy != OrderRetiredByAdapter {
			errs = append(errs, fmt.Errorf("an order retired for %q is retired by the %s itself, not %q", p.Reason, OrderRetiredByAdapter, p.RetiredBy))
		}
	case OrderRetiredByOwnerReason:
		switch p.RetiredBy {
		case "":
			errs = append(errs, errors.New("retired by is required: the owner's retirement names the owner"))
		case OrderRetiredByAdapter:
			errs = append(errs, fmt.Errorf("an order %s is retired by the owner, never by the %s", p.Reason, OrderRetiredByAdapter))
		}
	case "":
		errs = append(errs, errors.New("reason is required"))
	default:
		errs = append(errs, fmt.Errorf("reason %q is not %q or %q", p.Reason, OrderRetiredSessionOver, OrderRetiredByOwnerReason))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required: what refused the order, or why the owner retired it"))
	}
	errs = append(errs, wholeSecondTime("retired at", p.RetiredAt)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order retired payload: %w", err)
	}
	return nil
}

// OrderRetiredFractionalVersion identifies an exact fractional release (ADR 0015; ADR 0026, D7-F).
const OrderRetiredFractionalVersion uint32 = 2

// OrderRetiredVersions preserves legacy retirement inputs (ADR 0015).
func OrderRetiredVersions() []uint32 { return []uint32{1, 2} }

// SchemaVersion states the release quantity units (ADR 0015).
func (p OrderRetiredPayload) SchemaVersion() uint32 {
	if p.QuantityScale != 0 {
		return OrderRetiredFractionalVersion
	}
	return OrderRetiredSchemaVersion
}

// ValidateSchema refuses quantity-unit mismatches (ADR 0015).
func (p OrderRetiredPayload) ValidateSchema(v uint32) error {
	if v < 1 || v > 2 {
		return fmt.Errorf("unsupported retirement version %d", v)
	}
	if err := quantitySchema(p.QuantityScale, v, 2); err != nil {
		return err
	}
	return p.Validate()
}
