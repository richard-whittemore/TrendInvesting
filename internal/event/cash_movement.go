package event

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// CashMovementEventType identifies the cash-movement input payload for the
// Envelope's Type field: a deposit into, or withdrawal from, the account
// (ADR 0007). Version 1 is produced by fixtures, and read by the Turtle
// reducer; version 2 by the IBKR Flex reader (adapter/ibkr/flex.py), from
// the broker's own cash transactions.
const CashMovementEventType = "account.cash-movement"

// CashMovementSchemaVersion is version 1 of CashMovementPayload, the version
// the Turtle reducer reads.
const CashMovementSchemaVersion uint32 = 1

// CashMovementTransferVersion is version 2 of CashMovementPayload, the
// version the IBKR Flex reader writes (ADR 0015):
//
//   - Version 2 adds TransactionID, the broker's own ID for the transfer,
//     which the reader dedupes on across runs. Version 1 carries none; each
//     version is refused what it does not state (ValidateSchema).
const CashMovementTransferVersion uint32 = 2

// CashMovementVersions returns the versions a cash movement is read at.
func CashMovementVersions() []uint32 {
	return []uint32{CashMovementSchemaVersion, CashMovementTransferVersion}
}

// CashMovementPayload carries one deposit or withdrawal.
//
// EquityBefore — actual equity immediately BEFORE this movement — is
// carried on the event itself rather than left for the reducer to read off
// the last account snapshot: the "neither triggers nor masks a Drawdown
// Step" rule (ADR 0007) must be checkable from the event alone, without
// requiring a same-instant snapshot to exist.
type CashMovementPayload struct {
	// AsOf is when this movement occurred, sharing ADR 0007 rule 4's one
	// per-account timeline with AccountSnapshotPayload.AsOf: a reducer
	// requires AsOf strictly increasing across BOTH event types together — a
	// cash movement and a snapshot may not share an AsOf — the same way it
	// requires a snapshot's own AsOf strictly increasing.
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
	// TransactionID, from version 2, is the broker's own ID for the
	// transfer (IBKR's transactionID in its Cash Transactions), unique per
	// account; empty, and left out, at version 1.
	TransactionID string `json:"transaction_id,omitempty"`
}

// Validate checks that the movement identifies when it occurred, that
// Amount is finite and non-zero, that EquityBefore is finite and positive,
// that EquityBefore+Amount is finite (two finite inputs can still overflow
// to +Inf) and strictly positive (a withdrawal to zero or below fails closed
// rather than being silently accepted), and that Currency is present.
func (p CashMovementPayload) Validate() error {
	var errs []error
	switch {
	case p.AsOf.IsZero():
		errs = append(errs, errors.New("as of is required"))
	case !writableTime(p.AsOf):
		errs = append(errs, errors.New("as of cannot be written as RFC 3339"))
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

// ValidateSchema is Validate at the version the movement arrived at: version
// 2 requires the broker's transaction ID and version 1 carries none
// (CashMovementTransferVersion).
func (p CashMovementPayload) ValidateSchema(version uint32) error {
	if err := p.Validate(); err != nil {
		return err
	}
	switch version {
	case CashMovementSchemaVersion:
		if p.TransactionID != "" {
			return errors.New("invalid cash movement payload: version 1 carries no transaction id; that is version 2's")
		}
	case CashMovementTransferVersion:
		if p.TransactionID == "" {
			return errors.New("invalid cash movement payload: transaction id is required at version 2")
		}
	default:
		return fmt.Errorf("invalid cash movement payload: schema version %d is not one of %v", version, CashMovementVersions())
	}
	return nil
}
