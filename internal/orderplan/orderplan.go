// Package orderplan is the strategy-independent order planner (ADR 0026,
// point 2): it turns a strategy.targets.decided decision, the account snapshot
// and configuration into planned orders. It knows no strategy rule. Only the
// Boost path, one SPY order for the joint margin account, is planned here.
package orderplan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// BoostInput is what the Boost path plans from: the decision and the ID of
// the envelope that carried it, the account snapshot as of the previous close,
// the instrument's raw previous close (ADR 0004: orders are priced on raw
// prices) and the configured price band. WorkingSells are the shares of the
// instrument already being sold (risklimit.Gate.PendingSells), which the
// account snapshot does not report.
type BoostInput struct {
	Tag            string
	Decision       event.TargetsDecidedPayload
	Snapshot       event.AccountPositionsSnapshotPayload
	ReferencePrice float64
	PriceBand      float64
	WorkingSells   int64
}

// PlanBoost plans Boost's one order (ADR 0026, point 2, "Boost";
// template.py:96-100): only when the decided target differs from the target in
// force, or no target is in force yet (the first decision), one order of
// want x V / p - q whole shares, rounded toward zero, as a limit at the band's
// edge. It reports false when no order is planned: no state change, or a
// difference that rounds to zero shares.
//
// Rounding toward zero never sells below the target on an exit nor buys above
// it on an entry, and a sell never exceeds the holding, since want x V / p is
// never negative. A sell is reduced by WorkingSells, so it and the sells
// already working never together sell below the target (ADR 0019, amended
// 2026-10-15, "never below it"); what is left at or below zero plans nothing,
// and a working sell never turns the order into a buy.
func PlanBoost(in BoostInput) (event.OrderPlannedPayload, bool, error) {
	d := in.Decision
	if err := d.Validate(); err != nil {
		return event.OrderPlannedPayload{}, false, fmt.Errorf("orderplan: %w", err)
	}
	if err := in.Snapshot.Validate(); err != nil {
		return event.OrderPlannedPayload{}, false, fmt.Errorf("orderplan: %w", err)
	}
	if in.Tag == "" {
		return event.OrderPlannedPayload{}, false, errors.New("orderplan: an order must be tagged with the decision it serves")
	}
	if d.Outcome != event.TargetsOutcomeDecided {
		return event.OrderPlannedPayload{}, false, fmt.Errorf("orderplan: a %s decision has no targets to plan", d.Outcome)
	}
	if len(d.Targets) != 1 || d.Targets[0].InstrumentID != d.Boost.InstrumentID {
		return event.OrderPlannedPayload{}, false, fmt.Errorf("orderplan: a Boost decision targets exactly its instrument %s", d.Boost.InstrumentID)
	}
	if in.Snapshot.AccountID != d.AccountID {
		return event.OrderPlannedPayload{}, false, fmt.Errorf("orderplan: snapshot of account %q cannot value a decision for %q", in.Snapshot.AccountID, d.AccountID)
	}
	if !(in.ReferencePrice > 0) || math.IsInf(in.ReferencePrice, 0) {
		return event.OrderPlannedPayload{}, false, errors.New("orderplan: the reference price must be finite and positive")
	}
	if !(in.PriceBand > 0 && in.PriceBand < 1) {
		return event.OrderPlannedPayload{}, false, errors.New("orderplan: the price band must lie strictly between 0 and 1")
	}
	if in.WorkingSells < 0 {
		return event.OrderPlannedPayload{}, false, errors.New("orderplan: working sells must not be negative")
	}
	target := d.Targets[0]
	held := in.Snapshot.Quantity(target.InstrumentID)
	if held < 0 {
		return event.OrderPlannedPayload{}, false, fmt.Errorf("orderplan: account holds %d %s; the planner never plans against a short", held, target.InstrumentID)
	}
	if d.Boost.TargetInForceSet && d.Boost.TargetInForce == target.Weight {
		return event.OrderPlannedPayload{}, false, nil
	}

	desired := target.Weight * in.Snapshot.NetLiquidation / in.ReferencePrice
	delta := math.Trunc(desired - float64(held))
	if math.Abs(delta) >= math.MaxInt64 {
		return event.OrderPlannedPayload{}, false, fmt.Errorf("orderplan: %v shares cannot be ordered", delta)
	}
	if delta == 0 {
		return event.OrderPlannedPayload{}, false, nil
	}
	side, quantity := event.OrderSideBuy, int64(delta)
	if delta < 0 {
		side, quantity = event.OrderSideSell, -quantity-in.WorkingSells
		if quantity <= 0 {
			return event.OrderPlannedPayload{}, false, nil
		}
	}
	order := event.OrderPlannedPayload{
		AccountID:      d.AccountID,
		Tag:            in.Tag,
		ClientOrderID:  ClientOrderID(in.Tag, target.InstrumentID, side),
		InstrumentID:   target.InstrumentID,
		Side:           side,
		Phase:          side,
		OrderType:      event.OrderTypeLimit,
		Quantity:       quantity,
		LimitPrice:     LimitPrice(side, in.ReferencePrice, in.PriceBand),
		TargetWeight:   target.Weight,
		NetLiquidation: in.Snapshot.NetLiquidation,
		HeldQuantity:   held,
		ReferencePrice: in.ReferencePrice,
		PriceBand:      in.PriceBand,
	}
	return order, true, nil
}

// LimitPrice is the band's edge on the order's side of the reference price
// (ADR 0026, point 2, "Order type and price"), rounded to a cent toward the
// reference price so it stays inside the band: a buy's p x (1 + band) rounded
// down, a sell's p x (1 - band) rounded up. It never crosses the reference
// price.
func LimitPrice(side string, reference, band float64) float64 {
	if side == event.OrderSideBuy {
		return math.Max(math.Floor(reference*(1+band)*100)/100, reference)
	}
	return math.Min(math.Ceil(reference*(1-band)*100)/100, reference)
}

// ClientOrderID derives an order's client ID from its tag, instrument and
// phase (ADR 0026, point 2, "Order identity"): "tw-" and the first 24 hex
// digits of SHA-256 over the three, NUL separated. It is a label: what
// prevents a resend is journal-before-send and reconnect matching, not the ID.
func ClientOrderID(tag, instrumentID, phase string) string {
	sum := sha256.Sum256([]byte(tag + "\x00" + instrumentID + "\x00" + phase))
	return "tw-" + hex.EncodeToString(sum[:])[:24]
}
