package event

import (
	"errors"
	"fmt"
	"time"
)

// FillEventType identifies the execution-fill payload for the Envelope's Type
// field.
//
// It is deliberately namespaced "execution.", not "strategy.": a fill is an
// external fact about what an execution venue did, never a decision this
// system made. docs/architecture.md states the invariant it exists to carry —
// "Go never assumes an intended order was filled. Positions, protective stops,
// and pyramid state change only from recorded brokerage events" — and
// .greptile/rules.md names the opposite ("request-driven state") as one of the
// failure modes that already occurred in the predecessor prototype.
//
// The producer differs by slice and none of them is the reducer: fixtures
// first, then #18's intraday fill simulator (ADR 0005), then the LEAN adapter
// in slice 2 (#30). A consumer must treat every fill as a fact it did not
// produce and cannot re-derive.
const FillEventType = "execution.fill"

// FillSchemaVersion is the current schema version of FillPayload, for the
// Envelope's SchemaVersion field.
const FillSchemaVersion uint32 = 1

// FillPayload records one execution against one trade proposal.
//
// Deliberately out of scope, so nothing here should be read as having been
// decided by this payload:
//
//   - How the fill was arrived at. ADR 0005's resting-order model — gaps fill
//     at the open, same-bar ambiguity resolves pessimistically — belongs to
//     the producer (#18). This payload states what happened, not why.
//   - Commissions. ADR 0013 puts them in the cost model applied to the
//     accounting view; a fill states the executed price.
//   - Whether the execution was allowed. Caps (ADR 0008) and the cash rule
//     (ADR 0010) are applied before an order is placed; a fill that arrived is
//     a fact regardless.
type FillPayload struct {
	InstrumentID string `json:"instrument_id"`
	// ProposalID is the ID of the trade-proposal envelope this fill executes.
	// It is the reconciliation join: a fill naming a proposal the strategy
	// never made is a material reconciliation difference
	// (docs/architecture.md's safety invariants), not something a consumer may
	// absorb.
	ProposalID string `json:"proposal_id"`
	// FillID is assigned by the producer and is unique per fill. It is the
	// idempotency key: duplicate delivery is expected from any transport, so a
	// consumer needs to distinguish a re-delivery of one execution from a
	// second, genuinely different one. Reusing it for different contents is
	// therefore a producer defect, not a duplicate.
	FillID    string `json:"fill_id"`
	Direction string `json:"direction"`
	// Quantity is the whole number of shares or contracts that actually
	// executed, which may be fewer than the proposal asked for. It is always
	// positive: nothing executing is not a fill.
	Quantity int64 `json:"quantity"`
	// Price is the actual executed price, with the producer's slippage already
	// applied (ADR 0013: slippage is 0.05 N per fill, applied against the
	// trader, and Add Ladders are measured from the *slipped* fill as Faith
	// specifies [T p.19]). A consumer must never substitute the level the
	// Signal fired at.
	Price float64 `json:"price"`
	// FilledAt is when the execution happened: the bar period end in a
	// backtest, a real timestamp live. Time arrives in the event and is never
	// read from a wall clock in the domain (.golangci.yml forbids time.Now in
	// internal/), so this is what any decision caused by the fill is stamped
	// with.
	FilledAt time.Time `json:"filled_at"`
}

// Validate checks that the fill identifies an instrument, the proposal it
// executes and itself, that Direction is a recognised value, that Quantity is
// positive, that Price is finite and positive, and that FilledAt is present.
func (p FillPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.ProposalID == "" {
		errs = append(errs, errors.New("proposal id is required: a fill must name the proposal it executes"))
	}
	if p.FillID == "" {
		errs = append(errs, errors.New("fill id is required: it is the key that makes a duplicate delivery idempotent"))
	}
	switch p.Direction {
	case DirectionLong:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("direction %q is not a recognised direction", p.Direction))
	}
	if p.Quantity <= 0 {
		errs = append(errs, fmt.Errorf("quantity must be a positive whole number, got %d: nothing executing is not a fill", p.Quantity))
	}
	switch {
	case !isFinite(p.Price):
		errs = append(errs, errors.New("price must be finite"))
	case p.Price <= 0:
		errs = append(errs, errors.New("price must be positive"))
	}
	if p.FilledAt.IsZero() {
		errs = append(errs, errors.New("filled at is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid fill payload: %w", err)
	}
	return nil
}
