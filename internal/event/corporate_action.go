package event

import (
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
// CorporateActionPayload, for the Envelope's SchemaVersion field.
const MarketCorporateActionSchemaVersion uint32 = 1

// CorporateActionKindDelisting is the one recognised Kind today: an
// instrument stopped trading (CONTEXT.md: "Delisting Exit"; ADR 0009). Kind
// is a closed set — Validate rejects anything else — so that a future
// corporate action (a merger, a spinoff, a ticker change) is a new
// recognised value added deliberately, on both this payload and the reducer
// that dispatches on it (internal/strategy/delisting.go), never a string a
// producer could send today and have silently misread as a delisting.
const CorporateActionKindDelisting = "delisting"

// CorporateActionPayload records a fact about an instrument's own listing,
// external to any decision this system made or any execution a venue
// reported (see the event type's own doc comment).
//
// It deliberately carries no price. The "last available price" a Delisting
// Exit closes at (CONTEXT.md: "Delisting Exit") is not a new fact this
// payload needs to state: it is the split-adjusted close of the last
// completed bar the reducer has already accepted for this instrument (ADR
// 0004 — signal computation, and everything downstream of it, runs on the
// split-adjusted view only), which internal/strategy already holds in its
// own per-instrument state. Carrying a second, independent price here would
// let a producer state a delisting price that disagrees with the bars it
// also sent, with no way for the reducer to tell which one is wrong.
type CorporateActionPayload struct {
	InstrumentID string `json:"instrument_id"`
	// Kind is CorporateActionKindDelisting today (see that constant's own
	// doc comment).
	Kind string `json:"kind"`
	// EffectiveAt is when the corporate action takes effect: for a
	// delisting, the moment the instrument stopped trading. This is why a
	// Delisting Exit's CampaignExitedPayload.ExitedAt is this value and not
	// a fill's own timestamp — a delisting has no execution to reconcile
	// against (ExitProposalPayload's own doc comment: "a Campaign is forced
	// closed, never proposed first"). It must not precede the period end of
	// the last completed bar the reducer holds for the instrument, since
	// that bar's close is the last available price this action closes at —
	// see internal/strategy/delisting.go's own chronology check.
	EffectiveAt time.Time `json:"effective_at"`
}

// Validate checks that the payload identifies an instrument, that Kind is
// the one recognised value, and that EffectiveAt is present.
func (p CorporateActionPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	switch p.Kind {
	case CorporateActionKindDelisting:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("kind %q is not a recognised corporate action kind", p.Kind))
	}
	if p.EffectiveAt.IsZero() {
		errs = append(errs, errors.New("effective at is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid corporate action payload: %w", err)
	}
	return nil
}
