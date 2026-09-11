package event

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// CashMovementEventType identifies the cash-movement input payload for the
// Envelope's Type field: a deposit into, or withdrawal from, the account
// (ADR 0007; #17). Produced by fixtures for this ticket; a brokerage or
// ledger integration produces it later, the same relationship
// AccountSnapshotPayload has to the LEAN adapter/broker.
const CashMovementEventType = "account.cash-movement"

// CashMovementSchemaVersion is the current schema version of
// CashMovementPayload, for the Envelope's SchemaVersion field.
const CashMovementSchemaVersion uint32 = 1

// CashMovementPayload carries one deposit or withdrawal.
//
// EquityBefore — actual equity immediately BEFORE this movement — is
// carried on the event itself rather than left for the reducer to read off
// the last account snapshot: #17's decision is that the "neither triggers
// nor masks a Drawdown Step" rule (ADR 0007) must be checkable from the
// event alone, without requiring a same-instant snapshot to exist. See
// event_test's TestCashMovementPayloadValidate for the fail-closed cases
// this carries, including a withdrawal that would take equity to zero or
// below.
type CashMovementPayload struct {
	// AsOf is when this movement occurred, sharing ADR 0007 rule 4's one
	// per-account timeline with AccountSnapshotPayload.AsOf: a reducer
	// requires AsOf strictly increasing across BOTH event types together
	// (#17's decision — a cash movement and a snapshot may not share an
	// AsOf), the same way it requires a snapshot's own AsOf strictly
	// increasing.
	AsOf time.Time `json:"as_of"`
	// Amount is the movement: positive for a deposit, negative for a
	// withdrawal. Must be finite and non-zero — a "movement" of nothing is
	// not a movement.
	Amount float64 `json:"amount"`
	// EquityBefore is actual account equity immediately before this
	// movement (see the type's own doc comment for why it is carried here
	// rather than read from the last snapshot). Must be finite and
	// positive, and EquityBefore+Amount must be strictly positive: a
	// withdrawal that would take equity to zero or below fails closed (see
	// Validate).
	EquityBefore float64 `json:"equity_before"`
	// Currency is the currency Amount and EquityBefore are stated in,
	// mirroring AccountSnapshotPayload.Currency.
	Currency string `json:"currency"`
}

// Validate checks that the movement identifies when it occurred, that
// Amount is finite and non-zero, that EquityBefore is finite and positive,
// that EquityBefore+Amount is finite (two finite inputs can still overflow
// to +Inf — Greptile PR #71 finding) and strictly positive (a withdrawal to
// zero or below fails closed rather than being silently accepted), and that
// Currency is present.
func (p CashMovementPayload) Validate() error {
	var errs []error
	if p.AsOf.IsZero() {
		errs = append(errs, errors.New("as of is required"))
	}

	amountFinite := isFinite(p.Amount)
	switch {
	case !amountFinite:
		errs = append(errs, errors.New("amount must be finite"))
	case p.Amount == 0:
		errs = append(errs, errors.New("amount must be non-zero"))
	}

	equityBeforeFinite := isFinite(p.EquityBefore)
	switch {
	case !equityBeforeFinite:
		errs = append(errs, errors.New("equity before must be finite"))
	case p.EquityBefore <= 0:
		errs = append(errs, errors.New("equity before must be positive"))
	}

	if amountFinite && equityBeforeFinite {
		equityAfter := p.EquityBefore + p.Amount
		switch {
		case math.IsNaN(equityAfter) || math.IsInf(equityAfter, 0):
			// Two finite inputs (EquityBefore and Amount are both already
			// checked finite above) can still sum to +Inf: failing closed
			// here, before the <= 0 check below, matters because +Inf is
			// NOT <= 0 and would otherwise pass it silently.
			errs = append(errs, fmt.Errorf(
				"equity before %v plus amount %v is not finite (%v); failing closed",
				p.EquityBefore, p.Amount, equityAfter))
		case equityAfter <= 0:
			errs = append(errs, fmt.Errorf(
				"a withdrawal of %v from equity %v would take equity to %v, at or below zero; failing closed",
				p.Amount, p.EquityBefore, equityAfter))
		}
	}

	if p.Currency == "" {
		errs = append(errs, errors.New("currency is required"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid cash movement payload: %w", err)
	}
	return nil
}
