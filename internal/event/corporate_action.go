package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MarketCorporateActionEventType identifies the corporate-action payload for
// the Envelope's Type field: a fact about an instrument's own listing, not a
// decision this system made and not a venue's report of an execution.
//
// Namespaced "market.", matching CompletedBarEventType (market.bar.completed):
// like a bar, this is a fact about the market that the strategy did not
// produce and cannot re-derive — the opposite of "execution.", which names a
// venue's own report of what it did with an order (FillEventType's own doc
// comment), and the opposite of "strategy.", which names this system's own
// decisions.
const MarketCorporateActionEventType = "market.corporate-action"

// MarketCorporateActionSchemaVersion is the current schema version of
// CorporateActionPayload, for the Envelope's SchemaVersion field (ADR 0015):
//
//   - 1: a delisting, the only Kind, with no terms of its own.
//   - 2: adds the split Kind and its terms (ADR 0023). A schema-1 record is
//     read forward by UpcastCorporateActionPayload.
const MarketCorporateActionSchemaVersion uint32 = 2

// CorporateActionKindDelisting is an instrument that stopped trading
// (CONTEXT.md: "Delisting Exit"; ADR 0009). Kind is a closed set — Validate
// rejects anything else — so that a future corporate action (a merger, a
// spinoff, a ticker change) is a new recognised value added deliberately, on
// both this payload and the reducer that dispatches on it
// (internal/strategy/delisting.go), never a string a producer could send
// today and have silently misread as a known kind.
const CorporateActionKindDelisting = "delisting"

// CorporateActionKindSplit is a split whose broker paid cash for the
// fractional shares it could not deliver (CONTEXT.md: "Cash in lieu"; ADR
// 0023). The engine's split-adjusted view is adjusted for every split
// already (ADR 0004, as amended), so the split changes none of the engine's
// figures: the payload carries only what the split did not do exactly — the
// whole raw shares lost and the cash paid for them.
const CorporateActionKindSplit = "split"

// CorporateActionPayload records a fact about an instrument's own listing,
// external to any decision this system made or any execution a venue
// reported (see the event type's own doc comment).
//
// It deliberately carries no price. The "last available price" a Delisting
// Exit closes at (CONTEXT.md: "Delisting Exit") is not a new fact this
// payload needs to state: it is the split-adjusted close of the last
// completed bar the reducer has already accepted for this instrument (ADR
// 0004, as amended — a Campaign's money stays in the one price view its own
// fills were priced in, which in this build is the split-adjusted view),
// which internal/strategy already holds in its own per-instrument state.
// Carrying a second, independent price here would let a producer state a
// delisting price that disagrees with the bars it also sent, with no way for
// the reducer to tell which one is wrong.
//
// The split terms are omitted when zero, so a delisting encodes to exactly
// the bytes it did at schema 1; a delisting carrying any of them is invalid.
type CorporateActionPayload struct {
	InstrumentID string `json:"instrument_id"`
	// Kind is one of the CorporateActionKind* constants.
	Kind string `json:"kind"`
	// EffectiveAt is when the corporate action takes effect: for a
	// delisting, the moment the instrument stopped trading. This is why a
	// Delisting Exit's CampaignExitedPayload.ExitedAt is this value and not
	// a fill's own timestamp — a delisting has no execution to reconcile
	// against (ExitProposalPayload's own doc comment: "a Campaign is forced
	// closed, never proposed first"). It must not precede the period end of
	// the last completed bar the reducer holds for the instrument, since
	// that bar's close is the last available price this action closes at —
	// see internal/strategy/delisting.go's own chronology check. A split is
	// held to the same bound (internal/strategy/split.go).
	EffectiveAt time.Time `json:"effective_at"`
	// NewShares and OldShares are a split's ratio: NewShares new shares for
	// every OldShares old ones (7 and 1 for a 7-for-1).
	NewShares int64 `json:"new_shares,omitempty"`
	OldShares int64 `json:"old_shares,omitempty"`
	// EngineSharesPerRawShare is how many of the engine's split-adjusted
	// shares one raw share is AFTER the split: the whole number a producer
	// converts the engine's quantities to raw shares by (ADR 0004).
	EngineSharesPerRawShare int64 `json:"engine_shares_per_raw_share,omitempty"`
	// RawSharesLost is the whole raw shares, after the split, the broker
	// holds fewer than the engine's Units at the exact ratio: the shares it
	// paid CashInLieu for instead (ADR 0023).
	RawSharesLost int64 `json:"raw_shares_lost,omitempty"`
	// CashInLieu is the cash the broker paid for the fractional shares, in
	// Currency. A split that lost a share must have paid for it.
	CashInLieu float64 `json:"cash_in_lieu,omitempty"`
	// Currency is the currency CashInLieu is stated in, mirroring
	// CashMovementPayload.Currency.
	Currency string `json:"currency,omitempty"`
}

// Validate checks that the payload identifies an instrument, that Kind is a
// recognised value, that EffectiveAt is present, and that the split terms
// are present and consistent for a split and absent for a delisting.
func (p CorporateActionPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	switch p.Kind {
	case CorporateActionKindDelisting:
		if p.NewShares != 0 || p.OldShares != 0 || p.EngineSharesPerRawShare != 0 ||
			p.RawSharesLost != 0 || p.CashInLieu != 0 || p.Currency != "" {
			errs = append(errs, errors.New("a delisting carries no split terms"))
		}
	case CorporateActionKindSplit:
		errs = append(errs, p.splitTermErrors()...)
	default:
		errs = append(errs, fmt.Errorf("kind %q is not a recognised corporate action kind", p.Kind))
	}
	switch {
	case p.EffectiveAt.IsZero():
		errs = append(errs, errors.New("effective at is required"))
	case !writableTime(p.EffectiveAt):
		errs = append(errs, errors.New("effective at cannot be written as RFC 3339"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid corporate action payload: %w", err)
	}
	return nil
}

// splitTermErrors checks a split's own terms (ADR 0023).
func (p CorporateActionPayload) splitTermErrors() []error {
	var errs []error
	errs = append(errs, splitRatioErrors(p.NewShares, p.OldShares, p.EngineSharesPerRawShare)...)
	if p.RawSharesLost < 0 {
		errs = append(errs, fmt.Errorf("raw shares lost must not be negative, got %d", p.RawSharesLost))
	}
	switch {
	case !isFinite(p.CashInLieu) || p.CashInLieu < 0:
		errs = append(errs, fmt.Errorf("cash in lieu must be finite and not negative, got %v", p.CashInLieu))
	case p.RawSharesLost > 0 && p.CashInLieu == 0:
		errs = append(errs, fmt.Errorf("%d raw share(s) lost with no cash in lieu paid: a share taken with nothing paid is not cash in lieu (ADR 0023)", p.RawSharesLost))
	}
	if p.Currency == "" {
		errs = append(errs, errors.New("currency is required for a split"))
	}
	return errs
}

// splitRatioErrors checks a split's ratio and the engine's conversion after
// it, shared by the corporate action and the decision that restates them.
func splitRatioErrors(newShares, oldShares, enginePerRaw int64) []error {
	var errs []error
	if newShares < 1 {
		errs = append(errs, fmt.Errorf("new shares must be a positive whole number, got %d", newShares))
	}
	if oldShares < 1 {
		errs = append(errs, fmt.Errorf("old shares must be a positive whole number, got %d", oldShares))
	}
	if newShares >= 1 && newShares == oldShares {
		errs = append(errs, fmt.Errorf("a %d-for-%d split changes no share", newShares, oldShares))
	}
	if enginePerRaw < 1 {
		errs = append(errs, fmt.Errorf("engine shares per raw share must be a positive whole number, got %d", enginePerRaw))
	}
	return errs
}

// corporateActionV1 is the schema-1 shape: a delisting, with no terms.
type corporateActionV1 struct {
	InstrumentID string    `json:"instrument_id"`
	Kind         string    `json:"kind"`
	EffectiveAt  time.Time `json:"effective_at"`
}

// UpcastCorporateActionPayload decodes a market.corporate-action payload
// recorded at schemaVersion into the current CorporateActionPayload, and
// validates it (ADR 0015).
//
// Schema 1 recognised only a delisting and carried no other field, so a
// schema-1 record is read as the schema-2 delisting it is. One that names
// another kind, or any field schema 1 did not have, is not a schema-1
// record and is refused, as is every schema this build does not know, older
// or newer: an upcaster translates what an old schema said, never guesses.
// Unknown fields are refused at every version, so a producer's addition is
// never silently dropped.
func UpcastCorporateActionPayload(schemaVersion uint32, payload []byte) (CorporateActionPayload, error) {
	var decoded CorporateActionPayload
	switch {
	case schemaVersion == 1:
		var old corporateActionV1
		if err := decodeStrict(payload, &old); err != nil {
			return CorporateActionPayload{}, fmt.Errorf("decode schema-1 corporate action payload: %w", err)
		}
		if old.Kind != CorporateActionKindDelisting {
			return CorporateActionPayload{}, fmt.Errorf("a schema-1 corporate action can only be a %q, got kind %q", CorporateActionKindDelisting, old.Kind)
		}
		decoded = CorporateActionPayload{InstrumentID: old.InstrumentID, Kind: old.Kind, EffectiveAt: old.EffectiveAt}
	case schemaVersion == MarketCorporateActionSchemaVersion:
		if err := decodeStrict(payload, &decoded); err != nil {
			return CorporateActionPayload{}, fmt.Errorf("decode corporate action payload: %w", err)
		}
	case schemaVersion > MarketCorporateActionSchemaVersion:
		return CorporateActionPayload{}, fmt.Errorf("corporate action payload schema version %d was produced by a newer build than this one (%d); it cannot be read (ADR 0015)", schemaVersion, MarketCorporateActionSchemaVersion)
	default:
		return CorporateActionPayload{}, fmt.Errorf("corporate action payload schema version %d has no upcaster (ADR 0015)", schemaVersion)
	}
	if err := decoded.Validate(); err != nil {
		return CorporateActionPayload{}, err
	}
	return decoded, nil
}

// decodeStrict decodes payload into into, refusing any field into does not
// declare and anything after the one JSON value.
func decodeStrict(payload []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data after the payload")
	}
	return nil
}
