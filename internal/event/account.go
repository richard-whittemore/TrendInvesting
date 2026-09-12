package event

import (
	"errors"
	"fmt"
	"time"
)

// AccountSnapshotEventType identifies the account-snapshot payload for the
// Envelope's Type field: a reading of actual account equity at a point in
// time, which drives ADR 0007's Notional Account and Drawdown Step ladder
// (CONTEXT.md: "Notional Account", "Drawdown Step"). Produced by fixtures
// today; the LEAN adapter/broker produces it later.
const AccountSnapshotEventType = "account.snapshot"

// AccountSnapshotSchemaVersion is the current schema version of
// AccountSnapshotPayload, for the Envelope's SchemaVersion field.
const AccountSnapshotSchemaVersion uint32 = 1

// AccountSnapshotPayload carries actual account equity at a point in time.
//
// This is deliberately actual equity, never the Notional Account itself:
// CONTEXT.md's "Notional Account" entry exists precisely because the two are
// not the same figure, and internal/strategy's reducer is what derives one
// from the other (ADR 0007).
type AccountSnapshotPayload struct {
	// AsOf is when this equity figure was true, not when it was recorded —
	// Envelope.RecordedAt carries that. A reducer requires AsOf strictly
	// increasing per account, the same way it requires a completed bar's
	// PeriodEnd strictly increasing per instrument: a duplicate or
	// out-of-order snapshot would silently re-derive Drawdown Steps from a
	// stale or repeated reading.
	AsOf time.Time `json:"as_of"`
	// Equity is actual account equity, never the Notional Account it drives
	// (CONTEXT.md: "Notional Account"). It must be finite and positive:
	// .greptile/rules.md's fail-closed rule applies to every
	// account-affecting figure, not only volatility readings.
	Equity float64 `json:"equity"`
	// Currency is the currency Equity is stated in, carried explicitly so a
	// multi-currency account (out of scope for this project) cannot silently
	// mix figures once one exists.
	Currency string `json:"currency"`
}

// Validate checks that the snapshot identifies when it is as-of, that Equity
// is finite and positive, and that Currency is present.
func (p AccountSnapshotPayload) Validate() error {
	var errs []error
	if p.AsOf.IsZero() {
		errs = append(errs, errors.New("as of is required"))
	}
	switch {
	case !isFinite(p.Equity):
		errs = append(errs, errors.New("equity must be finite"))
	case p.Equity <= 0:
		errs = append(errs, errors.New("equity must be positive"))
	}
	if p.Currency == "" {
		errs = append(errs, errors.New("currency is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid account snapshot payload: %w", err)
	}
	return nil
}
