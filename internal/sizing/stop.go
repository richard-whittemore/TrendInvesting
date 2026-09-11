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
