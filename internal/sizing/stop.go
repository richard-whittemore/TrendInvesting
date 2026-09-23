package sizing

import (
	"errors"
	"fmt"
	"math"
)

// DirectionLong is the only Direction this package accepts today, mirroring
// event.DirectionLong (pinned equal by
// internal/strategy.TestSizingDirectionConstantsMatchTheEventContract, the
// same seam TestSizingModeConstantsMatchTheEventContract already pins for
// Mode). internal/sizing deliberately imports nothing from internal/event
// (see the package comment), so it declares its own copy rather than
// importing the wire contract's.
const DirectionLong = "long"

// ProtectiveStopLevel returns the price at which a Campaign's Protective
// Stop sits (CONTEXT.md: "Protective Stop"): entryPrice less stopMultiple
// campaign N, for a long Campaign.
//
//	level = entryPrice - stopMultiple x campaignN
//
// The Turtle Rules p.22, Faith's single-Unit Crude example: entry 28.30, N
// 1.20, the Baseline's Stop Multiple of 2 (ADR 0003) -> a stop of 25.90 (see
// TestProtectiveStopLevelCrudeGolden, which reproduces it to the exact
// float64 bit pattern).
//
// campaignN is the CAMPAIGN's frozen N (ADR 0006), never one recomputed
// after entry: the whole Stop Ladder is computed from the value frozen at
// first entry, and a caller that passed a recomputed N here would silently
// reintroduce the feedback loop ADR 0006 exists to prevent.
//
// direction is only ever DirectionLong today: the Baseline is long-only, and
// a short Campaign's stop sits ABOVE entry, a different formula this
// function does not implement. Any other value fails closed rather than
// guessing which formula was meant.
//
// Fails closed (.greptile/rules.md) on a non-finite or non-positive
// entryPrice, campaignN or stopMultiple, on an unrecognised direction, and on
// a derived level at or below zero: a long equity cannot be stopped out at
// or below zero, so such a stop would leave the position unprotected in fact
// while looking protected in the journal — the same rule
// DeclineReasonStopIntentNotPositive encodes for a proposal's stop intent,
// and CampaignOpenedPayload.Validate enforces again for the Campaign that
// results from a fill.
func ProtectiveStopLevel(entryPrice, campaignN, stopMultiple float64, direction string) (float64, error) {
	errs := []error{
		checkEntryPrice(entryPrice),
		checkN(campaignN),
		checkStopMultiple(stopMultiple),
	}
	if direction != DirectionLong {
		errs = append(errs, fmt.Errorf("direction %q is not implemented: only %q is supported (the baseline is long-only, and a short campaign's stop sits above entry, a different formula)", direction, DirectionLong))
	}
	if err := errors.Join(errs...); err != nil {
		return 0, fmt.Errorf("sizing: cannot derive protective stop level: %w", err)
	}

	level := entryPrice - Product(stopMultiple, campaignN)
	if level <= 0 {
		return 0, fmt.Errorf(
			"sizing: cannot derive protective stop level: derived level %v (entry price %v - stop multiple %v x campaign n %v) is not positive: a long position cannot be stopped out at or below zero",
			level, entryPrice, stopMultiple, campaignN)
	}
	return level, nil
}

// StopKind distinguishes how a Unit's current Protective Stop level came to
// be, because ValidStopLevel's rule differs by which: a Unit's own FIRST
// stop must sit strictly below its entry, while a stop the Stop Ladder has
// RAISED (RaisedStop, The Turtle Rules p.22-23) may reach or pass entry once
// enough half-N raises have made the Unit risk-free, open risk clamped at
// zero (AggregateOpenRisk's own doc comment works the arithmetic).
//
// Declared here rather than imported from internal/event's
// ProtectiveStopReasonInitial/ProtectiveStopReasonAddLadder, for the same
// package-boundary reason DirectionLong is this package's own constant (see
// its doc comment): internal/sizing imports nothing from internal/event.
type StopKind int

const (
	// StopKindInitial is a Unit's own first Protective Stop, set the moment
	// its fill was accepted.
	StopKindInitial StopKind = iota
	// StopKindRaised is an earlier Unit's stop after the Stop Ladder has
	// raised it at least once.
	StopKindRaised
)

// ValidStopLevel reports whether level is a legitimate Protective Stop for a
// Unit that entered at entryPrice (CONTEXT.md: "Protective Stop" — "Every
// open Campaign has one at all times"), given whether level is the Unit's
// own initial stop or one the Stop Ladder has raised (see StopKind).
//
// entryPrice and level must each be finite and positive: a long position
// cannot be stopped out at or below zero, so a level there would leave the
// position unprotected in fact while looking protected in a journal. Beyond
// that, the rule is conditional on kind:
//
//   - StopKindInitial: level must sit STRICTLY BELOW entryPrice (The Turtle
//     Rules p.22 gives no stop at entry) — a stop can only ever REACH entry
//     by later being raised, never as its own first level.
//   - StopKindRaised: level may sit AT OR ABOVE entryPrice, a legitimate
//     break-even or profit-protecting level once enough half-N raises have
//     made the Unit risk-free (CONTEXT.md's "risk-free"; AggregateOpenRisk
//     treats such a Unit's own contribution as exactly zero, never an
//     error).
//
// This is the one place the rule is stated: event.ProtectiveStopSetPayload.Validate,
// event.CampaignOpenedPayload.Validate and event.CampaignEvaluatedPayload.Validate,
// and internal/strategy's reducer invariant that every open Campaign's Units
// carry a usable stop, each call this rather than re-typing the comparison —
// a rule asserted independently in four places is a rule that can drift the
// moment one of them is updated and the others are not.
//
// Returns an error, not a bool, because every caller here needs to produce a
// message a journal reader can act on; a caller wraps the returned error
// with its own context (which field, which Unit index) rather than
// discarding it, so a failure still names which validation seam refused.
func ValidStopLevel(entryPrice, level float64, kind StopKind) error {
	var errs []error
	switch {
	case !isFinite(entryPrice):
		errs = append(errs, errors.New("entry price must be finite"))
	case entryPrice <= 0:
		errs = append(errs, errors.New("entry price must be positive"))
	}
	switch {
	case !isFinite(level):
		errs = append(errs, errors.New("protective stop level must be finite"))
	case level <= 0:
		errs = append(errs, errors.New("protective stop level must be positive: a long position cannot be stopped out at or below zero"))
	default:
		switch kind {
		case StopKindInitial:
			if isFinite(entryPrice) && level >= entryPrice {
				errs = append(errs, fmt.Errorf(
					"protective stop level %v must be below the entry price %v for an initial stop: a stop only reaches entry once the stop ladder has raised it",
					level, entryPrice))
			}
		case StopKindRaised:
			// No further constraint: a raised stop may legitimately sit at
			// or above entry (see the doc comment above).
		default:
			// Fail closed rather than silently applying the more permissive
			// StopKindRaised rule to a kind this package never declared: an
			// unrecognised kind — including a zero value from a field that
			// was never set, or a future third kind nobody has taught this
			// switch yet — must be refused, not defaulted, or this
			// predicate's entire reason to exist (one statement of the
			// rule, everywhere) is undone by exactly the values that most
			// need it enforced.
			errs = append(errs, fmt.Errorf("stop kind %d is not a declared StopKind: only StopKindInitial and StopKindRaised are valid", kind))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("sizing: invalid protective stop level: %w", err)
	}
	return nil
}

func checkEntryPrice(v float64) error {
	switch {
	case !isFinite(v):
		return errors.New("entry price must be finite")
	case v <= 0:
		return errors.New("entry price must be positive")
	}
	return nil
}

// RaisedStop returns the level an EARLIER Unit's Protective Stop rises to
// when a further Unit is added to the Campaign (CONTEXT.md: "Stop Ladder" —
// "The progression of Protective Stops as Units are added"):
//
//	level = previousStop + 0.5 x campaignN
//
// The Turtle Rules p.22-23: "if additional units were added, the stops for
// earlier units were raised by 1/2 N. This generally meant that all the
// stops for the entire position would be placed at 2 N from the most
// recently added unit. However, in cases where later units were placed at
// larger spacing either because of fast markets causing skid, or because of
// opening gaps, there would be differences in the stops."
//
// This function implements the FIRST sentence literally — "raise the
// earlier Unit's OWN stop by half N" — never the second sentence's
// paraphrase ("set every stop to 2N below the newest fill"). The two
// readings coincide whenever every Unit landed exactly on its own rung
// (Faith's normal Crude table, The Turtle Rules p.22), because the rungs
// themselves are exactly 1/2N apart; they diverge the moment a later Unit
// fills away from its rung — a gap or fast-market skid (p.23's own Crude
// gap table) — where the newest Unit's own stop sits farther from the
// earlier Units' than 1/2N per rung would predict, while the earlier Units'
// stops still only ever rise by the standard 1/2N. Callers apply this to
// EVERY earlier Unit's own current stop, once per Add (internal/strategy's
// applyAddFill), never to a single Campaign-level figure — a Campaign has no
// one "the stop" once levels diverge (see internal/sizing's package
// comment and campaignState.protectiveStop's own doc comment for why the
// minimum, not a shared level, is what a Campaign reports as a whole).
//
// previousStop is the Unit's own CURRENT stop (its own initial level from
// sizing.ProtectiveStopLevel, or a level this function has already raised
// once or more) — never recomputed from the Unit's original fill price and
// how many Adds have happened since, so a Unit's stop history is a sequence
// of exact raises, not a formula re-evaluated from scratch each time.
// campaignN is the CAMPAIGN's frozen N (ADR 0006), mirroring every other
// campaignN-consuming function in this package.
//
// Fails closed (.greptile/rules.md) on a non-finite or non-positive
// previousStop or campaignN.
func RaisedStop(previousStop, campaignN float64) (float64, error) {
	errs := []error{checkN(campaignN)}
	switch {
	case !isFinite(previousStop):
		errs = append(errs, errors.New("previous stop must be finite"))
	case previousStop <= 0:
		errs = append(errs, errors.New("previous stop must be positive"))
	}
	if err := errors.Join(errs...); err != nil {
		return 0, fmt.Errorf("sizing: cannot derive raised stop: %w", err)
	}
	return finiteResult("raised stop", previousStop+Product(0.5, campaignN))
}

// LowestProtectiveStop is a Campaign's Protective Stop: the lowest of the
// Units' own stops, which is the level at which its protection is FIRST
// breached (CONTEXT.md: "Protective Stop").
//
// It exists so that level is derived once. Three seams computed it from the
// same figures — the Campaign's own reported stop, the level in force for
// the Units a stop fill named, and the validator that re-checks the first
// against the payload's Units — and they did not agree about the degenerate
// cases. Two took a plain running minimum, and the third skipped stops that
// were not finite and positive.
//
// A plain running minimum is not order-independent. NaN compares false
// against everything, so it is ignored when it sits anywhere but first and
// returned when it sits first: the same Units stored in a different order
// gave different answers. A negative stop, meanwhile, was silently taken as
// the lowest. Neither is a level anything should act on, and which one came
// out depended on how the slice happened to be built.
//
// So this fails closed on any stop that is not finite and positive rather
// than choosing between ignoring it and returning it. The reducer's own
// per-bar invariant already refuses such a Unit (checkCampaignHasAProtectiveStop),
// which is an argument for this never firing, not an argument for it being
// absent: that invariant runs at the start of a bar, and a Unit added by a
// fill within one is not covered until the next.
func LowestProtectiveStop(stops []float64) (float64, error) {
	if len(stops) == 0 {
		return 0, errors.New("sizing: a campaign's protective stop needs at least one unit's own stop")
	}
	lowest := math.Inf(1)
	for i, stop := range stops {
		if !isFinite(stop) || stop <= 0 {
			return 0, fmt.Errorf("sizing: unit %d has %v, which is not a usable protective stop: every unit's own stop must be finite and positive before the campaign's lowest can be stated", i, stop)
		}
		if stop < lowest {
			lowest = stop
		}
	}
	return lowest, nil
}
