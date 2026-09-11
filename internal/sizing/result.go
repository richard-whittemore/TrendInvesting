package sizing

import (
	"errors"
	"fmt"
)

// AverageMoveInN returns the quantity-weighted average PER-SHARE price move
// of a Campaign's exit, expressed in campaign N:
//
//	move = (exitPrice - entryPrice) / campaignN
//
// This is deliberately NOT the Campaign's aggregate Unit-N result (see
// RealisedResultInUnitN below): it answers "how far did the average share
// move", not "how many Units' worth of a full 1N move did the Campaign
// realise". Two Units each moving a full N report here as 1N — the same
// reading a single Unit moving 1N would give — because this is a per-share
// average, not a sum over Units. A PR #74 review finding (Greptile, "N
// Result Ignores Units") is what separated the two: the field this function
// backs used to be named as though it were the aggregate, which understated
// a multi-Unit Campaign's realised risk and performance when read that way.
//
// entryPrice is the Campaign's own entry price — a single Unit's fill, or,
// for a multi-Unit Campaign, the quantity-weighted average fill price
// across every held Unit (campaignState.entryPrice()); the arithmetic here
// does not know or care which.
//
// campaignN is the CAMPAIGN's frozen N (ADR 0006), never one recomputed
// after entry, mirroring every other campaignN-consuming function in this
// package.
//
// Fails closed (.greptile/rules.md) on a non-finite exitPrice or
// entryPrice, and on a non-finite or non-positive campaignN.
func AverageMoveInN(exitPrice, entryPrice, campaignN float64) (float64, error) {
	errs := []error{checkN(campaignN)}
	if !isFinite(exitPrice) {
		errs = append(errs, errors.New("exit price must be finite"))
	}
	if !isFinite(entryPrice) {
		errs = append(errs, errors.New("entry price must be finite"))
	}
	if err := errors.Join(errs...); err != nil {
		return 0, fmt.Errorf("sizing: cannot derive average move in n: %w", err)
	}
	return (exitPrice - entryPrice) / campaignN, nil
}

// RealisedResultInUnitN returns a Campaign's realised result expressed in
// "one Unit moving 1N" terms (CONTEXT.md: "Unit", "N") — the AGGREGATE
// measure Faith actually uses to describe an outcome ("that trade made
// 2N"), and the counterpart to AverageMoveInN's per-share average:
//
//	result = realisedResult / (unitQuantity x campaignN x dollarsPerPoint)
//
// unitQuantity is the CAMPAIGN's frozen per-Unit share count (ADR 0006) —
// never a particular Unit's own actually-filled quantity. Dividing the
// dollar result by one Unit's worth of a full 1N move is what turns it into
// a count of how many such moves the whole Campaign realised: several
// equal-sized Units each earning a full 1N sum here to the number of
// Units, not to ~1N the way a naive per-share average would understate it
// (the exact defect a PR #74 review finding, "N Result Ignores Units",
// named).
//
// Fails closed on a non-finite realisedResult, a non-positive unitQuantity,
// and a non-finite or non-positive campaignN or dollarsPerPoint.
func RealisedResultInUnitN(realisedResult float64, unitQuantity int64, campaignN, dollarsPerPoint float64) (float64, error) {
	errs := []error{checkN(campaignN)}
	if !isFinite(realisedResult) {
		errs = append(errs, errors.New("realised result must be finite"))
	}
	if unitQuantity <= 0 {
		errs = append(errs, fmt.Errorf("unit quantity must be a positive whole number, got %d", unitQuantity))
	}
	switch {
	case !isFinite(dollarsPerPoint):
		errs = append(errs, errors.New("dollars per point must be finite"))
	case dollarsPerPoint <= 0:
		errs = append(errs, errors.New("dollars per point must be positive"))
	}
	if err := errors.Join(errs...); err != nil {
		return 0, fmt.Errorf("sizing: cannot derive realised result in unit n: %w", err)
	}
	return realisedResult / (float64(unitQuantity) * campaignN * dollarsPerPoint), nil
}
