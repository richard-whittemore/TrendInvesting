package sizing

import (
	"errors"
	"fmt"
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
// Fails closed (.greptile/rules.md: "A zero, negative, or not-yet-warm
// volatility value must fail closed") on a non-finite or non-positive
// entryPrice, campaignN or stopMultiple, on an unrecognised direction, and on
// a derived level at or below zero: a long equity cannot be stopped out at
// or below zero, so such a stop would leave the position unprotected in fact
// while looking protected in the journal — the same rule #10's
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

	level := entryPrice - stopMultiple*campaignN
	if level <= 0 {
		return 0, fmt.Errorf(
			"sizing: cannot derive protective stop level: derived level %v (entry price %v - stop multiple %v x campaign n %v) is not positive: a long position cannot be stopped out at or below zero",
			level, entryPrice, stopMultiple, campaignN)
	}
	return level, nil
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
// "the progression of Protective Stops as Units are added"):
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
	return previousStop + 0.5*campaignN, nil
}
