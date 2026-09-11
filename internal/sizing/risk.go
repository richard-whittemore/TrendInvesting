package sizing

import (
	"errors"
	"fmt"
)

// UnitOpenRisk is one held Unit's own entry price, current Protective Stop,
// and quantity — exactly what AggregateOpenRisk needs to compute that Unit's
// own contribution to a Campaign's aggregate open risk. It carries no
// identity of its own (no Unit index, no fill id): callers that need to
// report per-Unit identity alongside the arithmetic (internal/event's
// CampaignEvaluatedPayload.Units) keep it on their own richer type and build
// a slice of these purely for this computation.
type UnitOpenRisk struct {
	EntryPrice     float64
	ProtectiveStop float64
	Quantity       int64
}

// AggregateOpenRisk returns a Campaign's aggregate open risk — the capital a
// stop hitting every held Unit at its OWN current level would cost — as the
// sum, over every held Unit, of:
//
//	(EntryPrice - ProtectiveStop) x Quantity x dollarsPerPoint
//
// This is .greptile/rules.md's "risk multiplication when pyramiding" failure
// mode, fixed by construction: "each added Unit must not be granted a fresh
// full risk budget" and "aggregate open risk ... must be computed from each
// Unit's own entry and its own current stop, not assumed uniform" (issue
// #15). Summing EACH Unit's own entry and OWN current stop is the whole
// point — a single Campaign-level entry/stop pair is not enough to compute
// this once the Stop Ladder (RaisedStop) has raised some Units' stops and
// not others, which is exactly what happens the moment a later Unit fills
// away from its own rung (The Turtle Rules p.23's Crude gap table). The
// **negative** this function exists to reject: a Campaign that gave every
// added Unit a fresh, un-raised 2N stop (rather than raising the earlier
// Units' stops as the Stop Ladder requires) would report an aggregate of
// stopMultiple x campaignN x quantity x dollarsPerPoint PER UNIT — n Units
// summing to n times the intended per-trade risk, the prototype's own bug —
// and this function will faithfully compute exactly that inflated figure if
// handed those (wrong) stops, which is why the reducer must never construct
// UnitOpenRisk from anything but each Unit's own CURRENT (correctly raised)
// stop, and why event.CampaignEvaluatedPayload.Validate calls this same
// function to catch a payload that claims a smaller, "ladder" aggregate
// while its own listed Units carry the inflated, unraised figures (the
// #65 discipline: one shared function, called by both the producer and the
// validator, so the two cannot silently disagree about which arithmetic is
// "the" aggregate).
//
// Fails closed (.greptile/rules.md) on: no Units at all (there is no
// Campaign to report risk for); a non-finite or non-positive
// dollarsPerPoint; and, per Unit, a non-finite or non-positive EntryPrice, a
// non-finite or non-positive ProtectiveStop, a ProtectiveStop at or above
// its own Unit's EntryPrice (a long position's stop always sits below
// entry — CONTEXT.md: "Protective Stop"; such a Unit contributes negative or
// zero risk, which is not a legitimate reading), or a non-positive
// Quantity.
func AggregateOpenRisk(units []UnitOpenRisk, dollarsPerPoint float64) (float64, error) {
	var errs []error
	if len(units) == 0 {
		errs = append(errs, errors.New("at least one unit is required: there is no campaign to report aggregate open risk for otherwise"))
	}
	switch {
	case !isFinite(dollarsPerPoint):
		errs = append(errs, errors.New("dollars per point must be finite"))
	case dollarsPerPoint <= 0:
		errs = append(errs, errors.New("dollars per point must be positive"))
	}

	var total float64
	for i, u := range units {
		entryFinite := isFinite(u.EntryPrice)
		switch {
		case !entryFinite:
			errs = append(errs, fmt.Errorf("unit %d: entry price must be finite", i))
		case u.EntryPrice <= 0:
			errs = append(errs, fmt.Errorf("unit %d: entry price must be positive", i))
			entryFinite = false
		}

		stopFinite := isFinite(u.ProtectiveStop)
		switch {
		case !stopFinite:
			errs = append(errs, fmt.Errorf("unit %d: protective stop must be finite", i))
		case u.ProtectiveStop <= 0:
			errs = append(errs, fmt.Errorf("unit %d: protective stop must be positive", i))
			stopFinite = false
		case entryFinite && u.ProtectiveStop >= u.EntryPrice:
			errs = append(errs, fmt.Errorf("unit %d: protective stop %v must be below entry price %v for a long position", i, u.ProtectiveStop, u.EntryPrice))
			stopFinite = false
		}

		if u.Quantity <= 0 {
			errs = append(errs, fmt.Errorf("unit %d: quantity must be a positive whole number, got %d", i, u.Quantity))
			continue
		}
		if entryFinite && stopFinite {
			total += (u.EntryPrice - u.ProtectiveStop) * float64(u.Quantity) * dollarsPerPoint
		}
	}

	if err := errors.Join(errs...); err != nil {
		return 0, fmt.Errorf("sizing: cannot derive aggregate open risk: %w", err)
	}
	return total, nil
}
