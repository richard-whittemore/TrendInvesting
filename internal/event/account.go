package event

import (
	"encoding/json"
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
//
//   - Version 2 added AvailableCash, required. A version-1 record decodes it
//     as the float64 zero, which is indistinguishable from a snapshot that
//     deliberately declares no spare cash — silently treating an old record
//     as "affordable nothing" rather than rejecting it outright — so version
//     1 is rejected (ADR 0015).
const AccountSnapshotSchemaVersion uint32 = 2

// AccountSnapshotPayload carries actual account equity, and the cash
// available to spend, at a point in time.
//
// Equity is deliberately actual equity, never the Notional Account itself:
// CONTEXT.md's "Notional Account" entry exists precisely because the two are
// not the same figure, and internal/strategy's reducer is what derives one
// from the other (ADR 0007).
//
// AvailableCash rides on the same event as Equity, rather than a separate
// one, because ADR 0010 needs exactly the same thing Equity already has: a
// single, chronologically-ordered reading. ADR 0010's cash basis is the cash
// known at the PREVIOUS close — never a running balance updated mid-bar — so
// a producer states AsOf as the instant the figure was true and the reducer
// refuses to spend a figure stamped later than the decision bar's previous
// close (internal/strategy's Reducer.cashAtPreviousClose). A snapshot dated
// after the bar it would fund is a misordered stream, not a usable reading.
type AccountSnapshotPayload struct {
	// AsOf is when this equity and cash figure were true, not when it was
	// recorded — Envelope.RecordedAt carries that. A reducer requires AsOf
	// strictly increasing per account, the same way it requires a completed
	// bar's PeriodEnd strictly increasing per instrument: a duplicate or
	// out-of-order snapshot would silently re-derive Drawdown Steps, or
	// re-apply a stale cash figure, from a stale or repeated reading.
	AsOf time.Time `json:"as_of"`
	// Equity is actual account equity, never the Notional Account it drives
	// (CONTEXT.md: "Notional Account"). It must be finite and positive:
	// .greptile/rules.md's fail-closed rule applies to every
	// account-affecting figure, not only volatility readings.
	Equity float64 `json:"equity"`
	// AvailableCash is the cash on hand to fund a new entry or an Add (ADR
	// 0010). It must be finite and not negative — zero is a legitimate
	// reading (every dollar already deployed) — and is deliberately a
	// separate figure from Equity: Equity includes the value of open
	// positions, which is not spendable cash. It is required: a decoded
	// record that omits the field is refused rather than read as zero (see
	// UnmarshalJSON), since the two are otherwise the same value and the
	// wrong one declines Units that were affordable.
	AvailableCash float64 `json:"available_cash"`
	// Currency is the currency Equity and AvailableCash are stated in,
	// carried explicitly so a multi-currency account (out of scope for this
	// project) cannot silently mix figures once one exists.
	Currency string `json:"currency"`
	// availableCashMissing records that a decoded record carried no
	// available_cash field at all, which json.Unmarshal would otherwise
	// leave indistinguishable from an explicit 0 — a malformed payload would
	// then be read as "no cash" and decline every affordable Unit rather
	// than being refused. Its zero value is "present", so a payload built in
	// Go (a fixture, a producer) needs nothing; only UnmarshalJSON ever sets
	// it.
	availableCashMissing bool
}

// UnmarshalJSON decodes a snapshot and records whether available_cash was
// present, since the field is required from version 2 on and its absence
// decodes as the same float64 zero a genuine no-spare-cash reading does
// (AvailableCash's own doc comment). An explicit 0, and only an explicit 0,
// stays a legitimate reading.
func (p *AccountSnapshotPayload) UnmarshalJSON(data []byte) error {
	// The alias drops this method, so the embedded value decodes every other
	// field by its own tags; the shallower AvailableCash shadows the
	// embedded one, which is what makes presence visible.
	type alias AccountSnapshotPayload
	var decoded struct {
		alias
		AvailableCash *float64 `json:"available_cash"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = AccountSnapshotPayload(decoded.alias)
	if decoded.AvailableCash == nil {
		p.availableCashMissing = true
		return nil
	}
	p.AvailableCash = *decoded.AvailableCash
	return nil
}

// Validate checks that the snapshot identifies when it is as-of, that Equity
// is finite and positive, that AvailableCash is finite and not negative, and
// that Currency is present.
func (p AccountSnapshotPayload) Validate() error {
	var errs []error
	switch {
	case p.AsOf.IsZero():
		errs = append(errs, errors.New("as of is required"))
	case !writableTime(p.AsOf):
		errs = append(errs, errors.New("as of cannot be written as RFC 3339"))
	}
	switch {
	case !isFinite(p.Equity):
		errs = append(errs, errors.New("equity must be finite"))
	case p.Equity <= 0:
		errs = append(errs, errors.New("equity must be positive"))
	}
	switch {
	case p.availableCashMissing:
		errs = append(errs, errors.New("available cash is required: a record that omits it is refused, never read as no spare cash"))
	case !isFinite(p.AvailableCash):
		errs = append(errs, errors.New("available cash must be finite"))
	case p.AvailableCash < 0:
		errs = append(errs, errors.New("available cash must not be negative"))
	}
	if p.Currency == "" {
		errs = append(errs, errors.New("currency is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid account snapshot payload: %w", err)
	}
	return nil
}
