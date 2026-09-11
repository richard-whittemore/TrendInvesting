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
//
// Bumped to 2 for #12: Kind and CampaignID were added, and Kind is required
// (an empty Kind decodes from a schema-1 record and is not a recognised
// value, so a schema-1 fill is rejected outright rather than silently
// interpreted as an entry — ADR 0015's rule, applied here at the payload
// level, the same way #10 bumped ConfigurationSchemaVersion for
// DollarsPerPoint and RiskAtStopFraction).
//
// NOT bumped for #13's FillKindExit, and NOT bumped again for #14's
// FillKindAdd: unlike #12's Kind and CampaignID, neither adds a new FIELD a
// schema-2 record might decode with an ambiguous zero value — each only adds
// a further recognised value to a field that already exists and is already
// required. A schema-2 record naming "entry" or "stop" decodes and validates
// exactly as before either way; #13 and #14 are each additive, not a
// breaking reinterpretation of anything a schema-2 producer could have
// written.
const FillSchemaVersion uint32 = 2

// The four Kind values FillPayload accepts today.
const (
	// FillKindEntry is the fill that opens a Campaign (#11): it names the
	// ProposalID it executes and must not name a CampaignID, since no
	// Campaign exists yet.
	FillKindEntry = "entry"
	// FillKindStop is the fill that closes a Campaign at its Protective
	// Stop (#12): it names the CampaignID it closes and must not name a
	// ProposalID, since a stop closes a position, not a proposal.
	FillKindStop = "stop"
	// FillKindExit is the fill that closes a Campaign at its Exit Channel
	// (#13, The Turtle Rules p.26): unlike a stop fill, it names BOTH the
	// CampaignID it closes AND the ProposalID of the exit proposal
	// (strategy.exit.proposed, ExitProposalPayload) it executes — an exit,
	// unlike a stop, is always proposed first (ADR 0005 makes it a resting
	// order at the Exit Channel level), so the fill has a proposal to join
	// back to.
	FillKindExit = "exit"
	// FillKindAdd is the fill that adds a further Unit to an open Campaign
	// (#14, The Turtle Rules p.19-20): like an exit fill, and unlike a stop
	// fill, it names BOTH the CampaignID it extends AND the ProposalID of
	// the Add proposal (strategy.add.proposed, AddProposalPayload) it
	// executes — an Add, like an exit, is always proposed first (ADR 0005
	// makes it a resting order at the rung), so the fill has a proposal to
	// join back to.
	FillKindAdd = "add"
)

// FillPayload records one execution: either the entry that opens a Campaign
// or the stop that closes one, discriminated by Kind. One execution-fact
// type rather than a second event per kind, because every kind is the same
// underlying fact — "this quantity executed at this price at this time" —
// and a consumer that groups a journal by event type still sees every
// execution in one place; discriminating by Kind is `docs/development.md`
// principle 5's table-driven spirit applied to a payload rather than a test.
//
// Deliberately out of scope, so nothing here should be read as having been
// decided by this payload:
//
//   - How the fill was arrived at. ADR 0005's resting-order model — gaps fill
//     at the open, same-bar ambiguity resolves pessimistically — belongs to
//     the producer (#18). This payload states what happened, not why: no
//     comparison of a bar's range against a Protective Stop level happens
//     anywhere in this package (see internal/strategy/campaign.go's
//     applyStopFill).
//   - Commissions. ADR 0013 puts them in the cost model applied to the
//     accounting view; a fill states the executed price.
//   - Whether the execution was allowed. Caps (ADR 0008) and the cash rule
//     (ADR 0010) are applied before an order is placed; a fill that arrived is
//     a fact regardless.
type FillPayload struct {
	InstrumentID string `json:"instrument_id"`
	// Kind discriminates what this fill did: FillKindEntry or FillKindStop.
	// Required and closed — an empty or unrecognised value is rejected
	// rather than defaulted, since defaulting to "entry" would silently
	// misinterpret a stop.
	Kind string `json:"kind"`
	// ProposalID is the ID of the trade-proposal envelope this fill
	// executes. Required for FillKindEntry — it is the reconciliation join: a
	// fill naming a proposal the strategy never made is a material
	// reconciliation difference (docs/architecture.md's safety invariants),
	// not something a consumer may absorb — and must be empty for
	// FillKindStop: a stop closes a Campaign, not a proposal.
	ProposalID string `json:"proposal_id"`
	// CampaignID is the ID of the Campaign this fill closes. Required for
	// FillKindStop and must be empty for FillKindEntry, since no Campaign
	// exists yet for an entry fill to name.
	CampaignID string `json:"campaign_id"`
	// FillID is assigned by the producer and is unique per fill. It is the
	// idempotency key: duplicate delivery is expected from any transport, so a
	// consumer needs to distinguish a re-delivery of one execution from a
	// second, genuinely different one. Reusing it for different contents is
	// therefore a producer defect, not a duplicate.
	FillID string `json:"fill_id"`
	// Direction is the POSITION's own direction (CONTEXT.md: long or short),
	// held constant across every fill of one Campaign's life — the entry
	// that opened it and the stop that closes it alike. It is deliberately
	// NOT the order's buy/sell side: a stop fill that closes a long Campaign
	// is a sell execution, but its Direction here is still "long", matching
	// campaignState.direction, because what this field answers is "which
	// Campaign does this fill belong to", not "which side did the venue
	// execute". internal/strategy.applyStopFill checks it against the
	// Campaign's own direction for exactly this reason.
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

// Validate checks that the fill identifies an instrument and itself, that
// Kind is one of the recognised values and that ProposalID/CampaignID are
// present or absent exactly as that Kind requires, that Direction is a
// recognised value, that Quantity is positive, that Price is finite and
// positive, and that FilledAt is present.
func (p FillPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	switch p.Kind {
	case FillKindEntry:
		if p.ProposalID == "" {
			errs = append(errs, errors.New("proposal id is required for an entry fill: it must name the proposal it executes"))
		}
		if p.CampaignID != "" {
			errs = append(errs, fmt.Errorf("campaign id must be empty for an entry fill (got %q): no campaign exists yet for it to name", p.CampaignID))
		}
	case FillKindStop:
		if p.CampaignID == "" {
			errs = append(errs, errors.New("campaign id is required for a stop fill: it must name the campaign it closes"))
		}
		if p.ProposalID != "" {
			errs = append(errs, fmt.Errorf("proposal id must be empty for a stop fill (got %q): a stop closes a campaign, not a proposal", p.ProposalID))
		}
	case FillKindExit:
		if p.CampaignID == "" {
			errs = append(errs, errors.New("campaign id is required for an exit fill: it must name the campaign it closes"))
		}
		if p.ProposalID == "" {
			errs = append(errs, errors.New("proposal id is required for an exit fill: it must name the exit proposal it executes"))
		}
	case FillKindAdd:
		if p.CampaignID == "" {
			errs = append(errs, errors.New("campaign id is required for an add fill: it must name the campaign it extends"))
		}
		if p.ProposalID == "" {
			errs = append(errs, errors.New("proposal id is required for an add fill: it must name the add proposal it executes"))
		}
	default:
		errs = append(errs, fmt.Errorf("kind %q is not a recognised fill kind", p.Kind))
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
