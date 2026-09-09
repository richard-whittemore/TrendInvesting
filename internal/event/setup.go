package event

import (
	"errors"
	"fmt"
	"time"
)

// SetupEvaluatedEventType identifies the Setup-evaluated decision payload
// for the Envelope's Type field.
const SetupEvaluatedEventType = "strategy.setup.evaluated"

// SetupEvaluatedSchemaVersion is the current schema version of
// SetupEvaluatedPayload, for the Envelope's SchemaVersion field.
const SetupEvaluatedSchemaVersion uint32 = 1

// SetupEvaluatedPayload carries the outcome of evaluating one instrument's
// Setup (CONTEXT.md: "Setup") on one completed bar: the current value of N
// (CONTEXT.md: "N") and whether it is a usable volatility reading.
//
// NReady means "N is a usable volatility reading", which is stronger than
// "bar-count warm-up (CONTEXT.md: 'Completed bar') is complete": twenty flat
// bars (high==low==close) legitimately complete warm-up with N==0, and a
// volatility-normalised sizing step could not safely divide by that. NReady
// is therefore false in both cases — warm-up incomplete, or warm-up complete
// but N==0 — and Validate rejects the contradictory combination NReady==true
// with N==0 outright, so it can never reach a sizing calculation.
//
// This payload is deliberately not a Signal (CONTEXT.md: "Signal" is a Tier
// A event): #8 evaluates and reports N only, and emits this event whether or
// not N is ready, so the warm-up itself is observable. NReady false means no
// decision may be taken from N yet; a consumer must check it before using N
// for anything, including reading N as "not volatile" — N is 0 while not
// ready (see indicator.WilderAverage), not a genuine reading of zero
// volatility.
type SetupEvaluatedPayload struct {
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	N            float64   `json:"n"`
	NReady       bool      `json:"n_ready"`
}

// Validate checks that the payload identifies an instrument and period, and
// that N is a finite, non-negative number that is never combined with
// NReady==true while zero.
//
// True Range, and therefore its Wilder average, is never negative (bar.go's
// PriceView.validate already enforces high >= low), so a negative N always
// indicates a defect upstream rather than a legitimate reading. NReady==true
// with N==0 is rejected for the reason on the type's doc comment: N is not a
// usable volatility reading at zero, so a producer must never claim it is
// ready — the reducer that builds this payload is expected to report
// NReady==false for a flat instrument instead of hitting this check, but
// Validate enforces the invariant regardless of which producer built the
// payload.
func (p SetupEvaluatedPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.PeriodEnd.IsZero() {
		errs = append(errs, errors.New("period end is required"))
	}
	switch {
	case !isFinite(p.N):
		errs = append(errs, errors.New("n must be finite"))
	case p.N < 0:
		errs = append(errs, errors.New("n must not be negative"))
	case p.NReady && p.N == 0:
		errs = append(errs, errors.New("n must be positive when ready"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid setup evaluated payload: %w", err)
	}
	return nil
}
