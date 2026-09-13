package sizing

import (
	"errors"
	"fmt"
)

// NextAddLevel returns the price at which the NEXT Unit of a Campaign would
// be added (CONTEXT.md: "Add Ladder"): the ACTUAL fill price of the Unit
// immediately before it, plus half the campaign N, for a long Campaign.
//
//	level = previousFill + 0.5 x campaignN
//
// The Turtle Rules p.19: "add 1 Unit every 1/2N measured from the actual
// fill of the previous Unit ... up to the maximum; slippage on the first
// fill pushes later adds out accordingly." previousFill is deliberately the
// price that ACTUALLY filled, never the intended or proposed level, so that
// slippage on one fill shifts every later rung — ADR 0013 measures the Add
// Ladder from the slipped fill for the identical reason
// sizing.ProtectiveStopLevel is measured from the actual entry rather than
// the proposal's intent.
//
// Faith's printed Gold ladder [T p.20], N = 2.50, first fill 310.00:
//
//	310.00 -> 311.25 -> 312.50 -> 313.75  (each +1.25 = 0.5 x 2.50)
//
// campaignN is the CAMPAIGN's frozen N (ADR 0006), never one recomputed
// after entry — the same discipline ProtectiveStopLevel's own doc comment
// states, for the identical reason: "recompute N at each Add" is a declared
// Variant (ADR 0006), not something this function may do silently.
//
// direction is only ever DirectionLong today, mirroring ProtectiveStopLevel:
// a short Campaign's Add Ladder would descend rather than ascend, a
// different formula this function does not implement.
//
// Fails closed (.greptile/rules.md) on a non-finite or non-positive
// previousFill or campaignN, and on an unrecognised direction.
func NextAddLevel(previousFill, campaignN float64, direction string) (float64, error) {
	errs := []error{
		checkEntryPrice(previousFill),
		checkN(campaignN),
	}
	if direction != DirectionLong {
		errs = append(errs, fmt.Errorf("direction %q is not implemented: only %q is supported (the baseline is long-only, and a short campaign's add ladder descends rather than ascends, a different formula)", direction, DirectionLong))
	}
	if err := errors.Join(errs...); err != nil {
		return 0, fmt.Errorf("sizing: cannot derive next add level: %w", err)
	}
	return previousFill + float64(0.5*campaignN), nil
}

// AddLadder returns the whole INTENDED Add Ladder for a Campaign: firstFill,
// and then maxUnits-1 further rungs, each half a campaign N above the one
// before it, assuming every fill lands EXACTLY on its own rung.
//
// This is deliberately the ladder Faith prints [T p.19-20] — the one
// computable at entry from firstFill and campaignN alone (ADR 0006) — not
// the ladder a live Campaign actually adds along, which is measured from
// each Unit's ACTUAL fill (NextAddLevel above) and so diverges from this one
// the moment any fill slips. It exists for the two things that legitimately
// want the exact-fill assumption: reproducing Faith's Gold and Crude goldens
// directly, and journaling the intended ladder at the moment a Campaign
// opens, before any Add fill has had a chance to slip.
//
// Faith's printed Crude ladder [T p.20], N = 1.20, first fill 28.30:
//
//	28.30 -> 28.90 -> 29.50 -> 30.10  (each +0.60 = 0.5 x 1.20)
//
// maxUnits must be at least 1 (the returned slice always starts with
// firstFill, Unit 1); the returned slice has exactly maxUnits entries.
//
// Fails closed on a non-finite or non-positive firstFill or campaignN, on an
// unrecognised direction, and on maxUnits less than 1.
func AddLadder(firstFill, campaignN float64, maxUnits int, direction string) ([]float64, error) {
	errs := []error{
		checkEntryPrice(firstFill),
		checkN(campaignN),
	}
	if direction != DirectionLong {
		errs = append(errs, fmt.Errorf("direction %q is not implemented: only %q is supported (the baseline is long-only, and a short campaign's add ladder descends rather than ascends, a different formula)", direction, DirectionLong))
	}
	if maxUnits < 1 {
		errs = append(errs, fmt.Errorf("max units must be at least 1, got %d", maxUnits))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("sizing: cannot derive add ladder: %w", err)
	}

	ladder := make([]float64, maxUnits)
	ladder[0] = firstFill
	for i := 1; i < maxUnits; i++ {
		// Each rung is derived from the PRECEDING rung in this slice, exactly
		// as NextAddLevel derives a real rung from the preceding Unit's
		// actual fill — the two agree bit for bit whenever every fill lands
		// exactly on its own rung, which is the assumption this function
		// deliberately makes (see the type's doc comment).
		ladder[i] = ladder[i-1] + float64(0.5*campaignN)
	}
	return ladder, nil
}
