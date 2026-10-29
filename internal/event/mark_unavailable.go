package event

import (
	"errors"
	"fmt"
	"time"
)

// MarkUnavailableEventType records the adapter's failed contract lookup,
// preserving the cause of a missing mark for the month's withheld pick
// (ADR 0026, amendment of 2026-10-29). It supplies no price.
const MarkUnavailableEventType = "market.mark.unavailable"

// MarkUnavailableSchemaVersion is the first payload shape (ADR 0015).
const MarkUnavailableSchemaVersion uint32 = 1

// Mark failure kinds distinguish a timed-out lookup from a definitive
// refusal. Unknown is used only by a decision with no adapter evidence
// (ADR 0026, amendment of 2026-10-29).
const (
	MarkFailureTransient = "transient"
	MarkFailureFinal     = "final"
	MarkFailureUnknown   = "unknown"
)

// MarkUnavailablePayload preserves the adapter's reason and classification
// at the same valuation time as its marks (ADR 0026, point 3, amended
// 2026-10-29). A subsequent observation can resolve the name.
type MarkUnavailablePayload struct {
	InstrumentID string    `json:"instrument_id"`
	AsOf         time.Time `json:"as_of"`
	Source       string    `json:"source"`
	Detail       string    `json:"detail"`
	Failure      string    `json:"failure"`
}

// Validate requires a named, dated observation with its recorded reason;
// only an observed transient or final failure is evidence.
func (p MarkUnavailablePayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	errs = append(errs, requiredTime("as of", p.AsOf)...)
	if p.Source == "" {
		errs = append(errs, errors.New("source is required"))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required"))
	}
	if p.Failure != MarkFailureTransient && p.Failure != MarkFailureFinal {
		errs = append(errs, errors.New("mark failure must be transient or final"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid mark unavailable payload: %w", err)
	}
	return nil
}

// WithheldPick names an intended purchase whose allocation stays in cash
// for this month; it does not change the declared target weights (ADR 0026,
// amendment of 2026-10-29).
type WithheldPick struct {
	InstrumentID string `json:"instrument_id"`
	Reason       string `json:"reason"`
	Detail       string `json:"detail"`
	Failure      string `json:"failure"`
}

// withheldPickErrors checks the closed per-pick refusal set and its target
// identity; whole-rebalance refusals cannot carry per-pick refusals.
func (p TargetsDecidedPayload) withheldPickErrors() []error {
	var errs []error
	if len(p.WithheldPicks) > 0 && (p.StrategyID != TargetWeightStrategyMomentum || p.Outcome != TargetsOutcomeDecided) {
		errs = append(errs, errors.New("withheld picks belong only to a decided momentum outcome"))
	}
	for i, pick := range p.WithheldPicks {
		if pick.InstrumentID == "" {
			errs = append(errs, errors.New("withheld pick instrument id is required"))
		}
		if i > 0 && pick.InstrumentID <= p.WithheldPicks[i-1].InstrumentID {
			errs = append(errs, errors.New("withheld picks must be strictly ascending by instrument"))
		}
		targeted := false
		for _, target := range p.Targets {
			if target.InstrumentID == pick.InstrumentID && target.Weight > 0 {
				targeted = true
				break
			}
		}
		if !targeted {
			errs = append(errs, fmt.Errorf("withheld pick %s has no positive target", pick.InstrumentID))
		}
		if pick.Reason != RefusedMissingMark {
			errs = append(errs, errors.New("withheld pick reason must be missing-mark"))
		}
		if pick.Detail == "" {
			errs = append(errs, errors.New("withheld pick detail is required"))
		}
		if pick.Failure != MarkFailureTransient && pick.Failure != MarkFailureFinal && pick.Failure != MarkFailureUnknown {
			errs = append(errs, errors.New("withheld pick failure must be transient, final or unknown"))
		}
	}
	return errs
}
