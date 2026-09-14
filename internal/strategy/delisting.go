package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds the Delisting Exit (CONTEXT.md: "Delisting Exit"; ADR
// 0009): the third and last of the three ways a Campaign can end, alongside
// the Exit Channel (evaluateCampaign/applyExitFill) and the Protective Stop
// (applyStopFill), both in reducer.go/campaign.go.
//
// It is a DIRECT close, never a proposal that awaits its own fill — the
// opposite shape from the other two. event.ExitProposalPayload's own doc
// comment states the reason: "a Campaign is forced closed, never proposed
// first". An entry, an Add, an exit and a stop are all, in the end, a
// recorded FILL that this reducer reconciles against something it already
// proposed or is already protecting (ADR 0005 owns deciding whether and at
// what price a resting order executed). A delisting has no order behind it
// and nothing for a resting order to fill against: the instrument's own
// market is gone. So there is nothing here for internal/fills to decide from
// bar data, and nothing here waits for a fill event that could never arrive.

// applyCorporateAction handles event.MarketCorporateActionEventType: a fact
// about an instrument's own listing, external to any decision this system
// made and to any execution a venue reported (see that event type's own doc
// comment). Like applyConfiguration and applyCompletedBar, an unexpected
// schema version is rejected before decoding (ADR 0015).
//
// Dispatch is by Kind even though event.CorporateActionPayload.Validate
// recognises only event.CorporateActionKindDelisting today, so that a future
// payload version which recognises a SECOND kind before this reducer
// implements it fails closed here rather than silently doing nothing with a
// corporate action it does not understand (docs/development.md principle 4).
func (r *Reducer) applyCorporateAction(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received a corporate action before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.MarketCorporateActionSchemaVersion {
		return nil, fmt.Errorf("strategy: corporate action payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.MarketCorporateActionSchemaVersion)
	}

	var payload event.CorporateActionPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, fmt.Errorf("strategy: decode corporate action payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid corporate action payload: %w", err)
	}

	switch payload.Kind {
	case event.CorporateActionKindDelisting:
		return r.applyDelisting(payload, envelope)
	default:
		return nil, fmt.Errorf("strategy: corporate action kind %q is not implemented by this reducer", payload.Kind)
	}
}

// applyDelisting handles a Delisting Exit: an instrument stopped trading, and
// any open Campaign in it is forced closed at the last available price (ADR
// 0009).
//
// # The last available price
//
// state.previousClose — the split-adjusted close of the most recently
// completed bar this reducer has accepted for the instrument (ADR 0004: every
// downstream calculation reads the split-adjusted view, never the raw one).
// This is state the reducer already owns from applyCompletedBar's advance
// block, not a new figure the corporate action states:
// event.CorporateActionPayload deliberately carries no price of its own (see
// its own doc comment), so a producer cannot state a delisting price that
// disagrees with the bars it also sent, and the reducer never has to choose
// between two numbers that might conflict.
//
// # A direct close, not a proposal
//
// evaluateCampaign's Exit-Channel path and applyStopFill's Protective-Stop
// path both react to a recorded FILL: an external fact about an order's
// execution that this reducer reconciles against something it already
// proposed or was already protecting (ADR 0005 owns deciding whether and at
// what price a resting order executed from bar data — see
// internal/fills). A delisting has no order and no execution to reconcile
// against: the instrument's market is simply gone, so there is nothing for a
// resting order to fill against and nothing here for internal/fills to
// decide. event.ExitProposalPayload's own doc comment states this plainly:
// "a Campaign is forced closed, never proposed first."
//
// # Delisting wins
//
// If an exit or an Add proposal is outstanding for this instrument's
// Campaign — raised by the SAME bar whose close is now the last available
// price — it is cancelled here rather than left to ADR 0011's ordinary
// next-bar expiry, because a delisted instrument produces no further bar for
// that expiry to ever run on (see expireExitProposalForDelisting/
// expireAddProposalForDelisting). Cancelling it rather than letting a fill
// for it arrive later is what makes the outcome unambiguous: exactly one
// Campaign-exited event, reason delisting, never a race with whatever the
// same bar also proposed.
//
// # No open Campaign is a no-op, not an error
//
// Every other input this reducer accepts about an already-closed or
// never-open Campaign fails closed (applyStopFill, applyExitFill,
// applyFillToOpenCampaign): a fill or a proposal for a Campaign this reducer
// does not hold is a reconciliation failure, because the producer that sent
// it should have known better. A delisting notice is different in kind: it
// originates from the instrument's own listing, not from anything this
// system proposed, and it may legitimately name an instrument this strategy
// was never in a Campaign for (never eligible, already exited on its own, or
// simply never signalled). Absorbing it silently is therefore the deliberate
// exception fail-closed elsewhere in this file is not — the named invariant
// is that a delisted instrument cannot un-delist and cannot be traded again
// in this run, so a repeated or late-arriving delisting notice for it states
// no new fact this reducer needs to act on.
func (r *Reducer) applyDelisting(payload event.CorporateActionPayload, input event.Envelope) ([]event.Envelope, error) {
	state, known := r.instruments[payload.InstrumentID]
	if !known || state.campaign == nil {
		return nil, nil
	}
	campaign := state.campaign

	if payload.EffectiveAt.Before(campaign.openedAt) {
		return nil, fmt.Errorf("strategy: instrument %q: delisting effective at %s predates campaign %q's own opening fill at %s; a campaign cannot be closed before it opened",
			payload.InstrumentID, payload.EffectiveAt.Format(time.RFC3339), campaign.campaignID, campaign.openedAt.Format(time.RFC3339))
	}
	// The identical check applyStopFill/applyExitFill apply to their own
	// closing fill: a delisting closing whatever Units survived an earlier
	// partial stop must not claim a moment before that earlier closing
	// fill's own (campaignState.lastCloseFillAt's own doc comment).
	if !campaign.lastCloseFillAt.IsZero() && payload.EffectiveAt.Before(campaign.lastCloseFillAt) {
		return nil, fmt.Errorf("strategy: instrument %q: delisting effective at %s predates campaign %q's most recently accepted closing fill at %s; a later closing event cannot have happened before an earlier one",
			payload.InstrumentID, payload.EffectiveAt.Format(time.RFC3339), campaign.campaignID, campaign.lastCloseFillAt.Format(time.RFC3339))
	}
	// The last available price is only as of the last completed bar this
	// reducer has actually accepted for the instrument (see this function's
	// own doc comment); a delisting stated to take effect BEFORE that bar
	// closed would be closing the campaign against a price from the future
	// relative to its own stated moment.
	//
	// state.hasPreviousClose is unreachable false here: an open Campaign
	// requires at least one accepted entry fill, which itself requires a
	// completed bar to have produced the proposal it executed, so a
	// previous close always exists whenever state.campaign is non-nil.
	// Guarded anyway, matching this package's fail-closed style.
	if !state.hasPreviousClose || payload.EffectiveAt.Before(state.lastPeriodEnd) {
		return nil, fmt.Errorf("strategy: instrument %q: delisting effective at %s predates the last completed bar %s this reducer has for it; the last available price is that bar's own close and cannot be read before it exists",
			payload.InstrumentID, payload.EffectiveAt.Format(time.RFC3339), state.lastPeriodEnd.Format(time.RFC3339))
	}
	lastAvailablePrice := state.previousClose

	// Whole-life aggregation over whatever this Campaign still holds,
	// combined with any earlier partial stop's own accumulated share —
	// campaignState.lifeAggregate's own doc comment, and applyExitFill's
	// identical use of it for the Exit-Channel path this mirrors.
	var thisEntryWeightedSum float64
	for _, u := range campaign.units {
		thisEntryWeightedSum += sizing.Product(float64(u.quantity), u.fillPrice)
	}
	thisQuantity := campaign.filledQuantity()
	quantity, entryPrice, exitPrice := campaign.lifeAggregate(thisQuantity, thisEntryWeightedSum, lastAvailablePrice)
	realisedResult := float64(quantity) * (exitPrice - entryPrice) * r.dollarsPerPoint
	averageMoveInN, err := sizing.AverageMoveInN(exitPrice, entryPrice, campaign.campaignN)
	if err != nil {
		// Unreachable: campaign.campaignN was required positive when the
		// Campaign opened (CampaignOpenedPayload.Validate) and is never
		// recomputed while it is open (ADR 0006).
		return nil, fmt.Errorf("strategy: instrument %q: delisting cannot compute the average move in n: %w", payload.InstrumentID, err)
	}
	realisedResultInUnitN, err := sizing.RealisedResultInUnitN(realisedResult, campaign.unitQuantity, campaign.campaignN, r.dollarsPerPoint)
	if err != nil {
		// Unreachable: campaign.unitQuantity and campaign.campaignN were both
		// required positive when the Campaign opened, and r.dollarsPerPoint
		// by ConfigurationPayload.Validate.
		return nil, fmt.Errorf("strategy: instrument %q: delisting cannot compute the realised result in unit n: %w", payload.InstrumentID, err)
	}

	exitedPayload := event.CampaignExitedPayload{
		CampaignID:            campaign.campaignID,
		InstrumentID:          payload.InstrumentID,
		FillID:                input.ID,
		ExitedAt:              payload.EffectiveAt,
		Reason:                event.ExitReasonDelisting,
		EntryPrice:            entryPrice,
		ExitPrice:             exitPrice,
		Quantity:              quantity,
		CampaignN:             campaign.campaignN,
		DollarsPerPoint:       r.dollarsPerPoint,
		UnitQuantity:          campaign.unitQuantity,
		ProtectiveStopLevel:   campaign.protectiveStop(),
		RealisedResult:        realisedResult,
		AverageMoveInN:        averageMoveInN,
		RealisedResultInUnitN: realisedResultInUnitN,
		Units:                 campaign.unitsOpened,
		Rule:                  event.RuleCampaignExitedByDelisting,
		ADR:                   event.ADRDelistingForcesExit,
	}
	if err := exitedPayload.Validate(); err != nil {
		// Unreachable: built field by field from figures the Campaign's own
		// history has already validated (CampaignOpenedPayload.Validate,
		// CampaignUnitAddedPayload.Validate) or that ConfigurationPayload.Validate
		// required, using the SAME expression order lifeAggregate/AverageMoveInN/
		// RealisedResultInUnitN compute, so the producer and this Validate call
		// agree bit for bit rather than approximately.
		return nil, fmt.Errorf("strategy: instrument %q: delisting would close campaign %q with an invalid exit: %w", payload.InstrumentID, campaign.campaignID, err)
	}
	exitedPayloadBytes, err := json.Marshal(exitedPayload)
	if err != nil {
		// Unreachable: json.Marshal of the payload Validate has just accepted;
		// the only thing it could refuse is a NaN or an infinity in a float64
		// field, and Validate has already tested every one for finiteness.
		return nil, fmt.Errorf("strategy: marshal campaign exited payload: %w", err)
	}
	exitEnvelope := r.stamp(
		decisionID("campaign-exited", payload.InstrumentID, payload.EffectiveAt),
		event.CampaignExitedEventType, event.CampaignExitedSchemaVersion,
		payload.EffectiveAt, input, exitedPayloadBytes,
	)

	// Delisting wins (see this function's own doc comment): whichever of an
	// exit or an Add proposal is outstanding is cancelled here, explicitly,
	// rather than left to ADR 0011's ordinary next-bar expiry — a delisted
	// instrument produces no further bar for that expiry to ever run on.
	// Built and validated BEFORE any state moves, the same discipline
	// applyStopFill's own expireAddProposalForStop follows. Checked
	// unconditionally for both kinds, mirroring applyCompletedBar's own
	// "all checks are unconditional here so none is skipped by construction"
	// — not because both could ever be outstanding at once (ADR 0010's exit
	// precedence means at most one of the two ever is), but so that neither
	// path depends on that invariant to be exercised.
	var cancelled []event.Envelope
	if state.pendingExitProposal != nil {
		env, err := r.expireExitProposalForDelisting(state, payload, input)
		if err != nil {
			return nil, err
		}
		cancelled = append(cancelled, env)
	}
	if state.pendingAddProposal != nil {
		env, err := r.expireAddProposalForDelisting(state, payload, input)
		if err != nil {
			return nil, err
		}
		cancelled = append(cancelled, env)
	}

	// State moves only now, after every payload this transition will be
	// journalled as has validated — the same discipline
	// openCampaign/applyStopFill/applyExitFill all follow.
	state.campaign = nil
	state.pendingExitProposal = nil
	state.pendingAddProposal = nil
	state.lastClosingFillAt = payload.EffectiveAt

	emissions := make([]event.Envelope, 0, len(cancelled)+1)
	emissions = append(emissions, cancelled...)
	emissions = append(emissions, exitEnvelope)
	return emissions, nil
}

// expireExitProposalForDelisting cancels an outstanding exit proposal the
// instant a delisting forces the same Campaign closed — the delisting-side
// counterpart of expireAddProposalForStop (campaign.go), for the identical
// reason: ADR 0011's ordinary next-bar expiry never runs for a delisted
// instrument, since there is no next bar.
func (r *Reducer) expireExitProposalForDelisting(state *instrumentState, action event.CorporateActionPayload, input event.Envelope) (event.Envelope, error) {
	pending := state.pendingExitProposal
	payload := event.ProposalExpiredPayload{
		InstrumentID:   action.InstrumentID,
		Kind:           event.ProposalKindExit,
		ProposalID:     pending.proposalID,
		SignalID:       "",
		PeriodEnd:      pending.periodEnd,
		ExpiredAt:      action.EffectiveAt,
		EarliestFillAt: pending.earliestFillAt,
		Rule:           event.RuleExitProposalSupersededByDelisting,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByDelisting,
		Quantity:       pending.quantity,
		Level:          pending.level,
	}
	if err := payload.Validate(); err != nil {
		// Unreachable: every field is copied from the pending proposal this
		// reducer itself recorded, which passed the same contract when it was
		// proposed, or is action.EffectiveAt, which the caller has already
		// checked is not before state.lastPeriodEnd — later than
		// pending.periodEnd's own predecessor bar, and so later than
		// pending.earliestFillAt.
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal expired payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable, for the same reason expireAddProposalForStop's marshal
		// guard is.
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal expired payload: %w", err)
	}
	return r.stamp(
		decisionID("exit-proposal-expired-by-delisting", action.InstrumentID, action.EffectiveAt),
		event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion,
		action.EffectiveAt, input, payloadBytes,
	), nil
}

// expireAddProposalForDelisting is
// expireExitProposalForDelisting's Add-side counterpart.
func (r *Reducer) expireAddProposalForDelisting(state *instrumentState, action event.CorporateActionPayload, input event.Envelope) (event.Envelope, error) {
	pending := state.pendingAddProposal
	payload := event.ProposalExpiredPayload{
		InstrumentID:   action.InstrumentID,
		Kind:           event.ProposalKindAdd,
		ProposalID:     pending.proposalID,
		SignalID:       "",
		PeriodEnd:      pending.periodEnd,
		ExpiredAt:      action.EffectiveAt,
		EarliestFillAt: pending.earliestFillAt,
		Rule:           event.RuleAddProposalSupersededByDelisting,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByDelisting,
		Quantity:       pending.quantity,
		Level:          pending.level,
	}
	if err := payload.Validate(); err != nil {
		// Unreachable, for the identical reason
		// expireExitProposalForDelisting's own Validate guard is.
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal expired payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable, for the same reason expireAddProposalForStop's marshal
		// guard is.
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal expired payload: %w", err)
	}
	return r.stamp(
		decisionID("add-proposal-expired-by-delisting", action.InstrumentID, action.EffectiveAt),
		event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion,
		action.EffectiveAt, input, payloadBytes,
	), nil
}
