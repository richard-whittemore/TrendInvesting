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
// comment). Its payload is read through event.UpcastCorporateActionPayload,
// which reads a schema-1 delisting forward and refuses any schema it does
// not know (ADR 0015).
//
// Dispatch is by Kind even though event.CorporateActionPayload.Validate
// recognises only the kinds handled below, so that a future payload version
// which recognises another kind before this reducer implements it fails
// closed here rather than silently doing nothing with a corporate action it
// does not understand (docs/development.md principle 4).
func (r *transition) applyCorporateAction(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received a corporate action before a configuration event; failing closed")
	}
	payload, err := event.UpcastCorporateActionPayload(envelope.SchemaVersion, envelope.Payload)
	if err != nil {
		return nil, fmt.Errorf("strategy: %w", err)
	}

	// The open Session has not yet decided this instrument's Add or entry
	// (ADR 0021), so a fact about its listing cannot be ordered against that
	// decision. A producer states such a fact between Sessions.
	if r.barReceivedInOpenSession(payload.InstrumentID) {
		return nil, fmt.Errorf("strategy: instrument %q: a corporate action arrived after its bar in the Session ending %s and before the Session closes; state it between Sessions (ADR 0021)",
			payload.InstrumentID, r.sessionPeriodEnd.Format(time.RFC3339))
	}
	switch payload.Kind {
	case event.CorporateActionKindDelisting:
		return r.applyDelisting(payload, envelope)
	case event.CorporateActionKindSplit:
		return r.applySplit(payload, envelope)
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
// completed bar this reducer has accepted for the instrument. ADR 0004, as
// amended: a Campaign's money is computed entirely within the one price view
// its own fills were priced in, and internal/fills prices every fill on the
// split-adjusted view. This is the only exit that reaches a bar price
// directly instead of through a fill, and so the only place the exit price
// and the entry prices it is subtracted from could come from different
// views.
//
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
// Whichever proposal is outstanding for this instrument — an entry proposal,
// or an exit or Add proposal raised by the SAME bar whose close is now the
// last available price — is cancelled here rather than left to ADR 0011's
// ordinary next-bar expiry, because a delisted instrument produces no further
// bar for that expiry to ever run on. Cancelling rather than letting a fill
// for it arrive later is what makes the outcome unambiguous: exactly one
// Campaign-exited event, reason delisting, never a race with whatever the
// same bar also proposed — and, for an entry proposal, no Campaign at all.
//
// # The invariant, and what enforces it
//
// A delisted instrument cannot un-delist and cannot be traded again in this
// run. Three things together make that true rather than merely stated:
// r.delisted records the fact the moment a delisting arrives, whether or not
// there was anything to close; applyCompletedBar then evaluates no Setup for
// the instrument; and applyFill refuses any new execution naming it.
//
// The record is made in EVERY case, including the ones that emit nothing —
// which is why the guard clauses below write to r.delisted before returning.
// A delisting with no Campaign and no proposal outstanding is still a no-op
// as far as the journal is concerned: no event, and no error — but it is a
// no-op only once its own chronology has cleared the instrument's last
// completed bar; the tombstone it records is exactly as terminal as the one
// that closes a Campaign, so it is checked to the same standard. Every other
// input this reducer accepts about an already-closed or never-open Campaign
// fails closed (applyStopFill, applyExitFill, applyFillToOpenCampaign),
// because a fill or a proposal for a Campaign this reducer does not hold is a
// reconciliation failure. A delisting notice is different in kind: it
// originates from the instrument's own listing, not from anything this system
// proposed, and it may legitimately name an instrument this strategy was
// never in a Campaign for (never eligible, already exited on its own, or
// simply never signalled). A repeated or late-arriving notice states no new
// fact either, since the first one was terminal.
func (r *transition) applyDelisting(payload event.CorporateActionPayload, input event.Envelope) ([]event.Envelope, error) {
	if _, alreadyDelisted := r.delisted[payload.InstrumentID]; alreadyDelisted {
		return nil, nil
	}

	state, known := r.instrument(payload.InstrumentID)
	if !known {
		// Genuinely unknown: no completed bar has ever been accepted for the
		// instrument, so there is no last completed bar for the chronology
		// check below to compare against, and nothing to close or cancel. The
		// fact is recorded all the same, which is the whole difference
		// between this no-op and the one that let a delisted instrument be
		// entered. Applying the chronology check anyway would make a stale
		// notice for an instrument this strategy never traded halt the run.
		r.delisted[payload.InstrumentID] = payload.EffectiveAt
		return nil, nil
	}

	// The last available price, and every expiry stamped below, are only as of
	// the last completed bar this reducer has actually accepted for the
	// instrument (see this function's own doc comment); a delisting stated to
	// take effect BEFORE that bar closed would be closing the campaign against
	// a price from the future relative to its own stated moment.
	//
	// Checked here, for every KNOWN instrument, before deciding whether there
	// is any outstanding business to close — not only on the path that has
	// some. A known but idle instrument's chronology can be violated exactly
	// as an active one's can, and the no-op below records r.delisted just as
	// terminally: absorbing a stale notice there would let an event history
	// contradict itself (an earlier SetupEvaluated event for a bar after the
	// stated delisting) and would permanently block every later, correct
	// notice and every bar that follows, with no way to repair it.
	//
	// state.hasPreviousClose is unreachable false here: state only exists in
	// r.instruments once applyCompletedBar's advance block has run for it,
	// which sets hasPreviousClose unconditionally before returning. Guarded
	// anyway, matching this package's fail-closed style.
	if !state.hasPreviousClose || payload.EffectiveAt.Before(state.lastPeriodEnd) {
		return nil, fmt.Errorf("strategy: instrument %q: delisting effective at %s predates the last completed bar %s this reducer has for it; the last available price is that bar's own close and cannot be read before it exists",
			payload.InstrumentID, payload.EffectiveAt.Format(time.RFC3339), state.lastPeriodEnd.Format(time.RFC3339))
	}

	if !state.hasOutstandingBusiness() {
		// Nothing to close and nothing to cancel, so nothing is journalled —
		// but the fact is recorded all the same, exactly as it is for a
		// genuinely unknown instrument above.
		r.delisted[payload.InstrumentID] = payload.EffectiveAt
		return nil, nil
	}

	// --- Every payload this transition will be journalled as is built and
	// validated HERE, before any state moves — the same discipline
	// openCampaign/applyStopFill/applyExitFill all follow, and the reason
	// expireAddProposalForStop deliberately leaves its own state untouched.
	//
	// All three proposal kinds are checked unconditionally, mirroring
	// applyCompletedBar's own "all checks are unconditional here so none is
	// skipped by construction" — not because more than one could ever be
	// outstanding at once (an entry proposal is always cleared before a
	// Campaign exists, and ADR 0010's exit precedence keeps exit and Add
	// proposals mutually exclusive), but so that no path depends on that
	// invariant to be exercised.
	var cancelled []event.Envelope
	if pending := state.pendingExitProposal; pending != nil {
		envelope, err := r.emitDelistingExpiry(delistingExpiry{
			idKind:         "exit-proposal-expired-by-delisting",
			kind:           event.ProposalKindExit,
			rule:           event.RuleExitProposalSupersededByDelisting,
			proposalID:     pending.proposalID,
			periodEnd:      pending.periodEnd,
			earliestFillAt: pending.earliestFillAt,
			quantity:       pending.quantity,
			level:          pending.level,
		}, payload, input)
		if err != nil {
			return nil, err
		}
		cancelled = append(cancelled, envelope)
	}
	if pending := state.pendingAddProposal; pending != nil {
		envelope, err := r.emitDelistingExpiry(delistingExpiry{
			idKind:         "add-proposal-expired-by-delisting",
			kind:           event.ProposalKindAdd,
			rule:           event.RuleAddProposalSupersededByDelisting,
			proposalID:     pending.proposalID,
			periodEnd:      pending.periodEnd,
			earliestFillAt: pending.earliestFillAt,
			quantity:       pending.quantity,
			level:          pending.level,
		}, payload, input)
		if err != nil {
			return nil, err
		}
		cancelled = append(cancelled, envelope)
	}
	if pending := state.pendingProposal; pending != nil {
		envelope, err := r.emitDelistingExpiry(delistingExpiry{
			idKind:         "entry-proposal-expired-by-delisting",
			kind:           event.ProposalKindEntry,
			rule:           event.RuleEntryProposalSupersededByDelisting,
			proposalID:     pending.proposalID,
			signalID:       pending.signalID,
			periodEnd:      pending.periodEnd,
			earliestFillAt: pending.earliestFillAt,
			quantity:       pending.quantity,
			level:          pending.entryLevel,
		}, payload, input)
		if err != nil {
			return nil, err
		}
		cancelled = append(cancelled, envelope)
	}

	var exit []event.Envelope
	if state.campaign != nil {
		envelope, err := r.closeCampaignForDelisting(state, payload, input)
		if err != nil {
			return nil, err
		}
		exit = append(exit, envelope)
	}

	// --- State moves only now, after every payload above has validated.
	r.delisted[payload.InstrumentID] = payload.EffectiveAt
	if state.campaign != nil {
		state.campaign = nil
		state.lastClosingFillAt = payload.EffectiveAt
	}
	// A cancelled proposal can no longer fill, so its hold is released (ADR
	// 0020, as amended 2026-09-24).
	for _, pending := range []string{pendingProposalID(state), pendingAddProposalID(state)} {
		if pending == "" {
			continue
		}
		if err := r.releaseHold(pending); err != nil {
			return nil, err
		}
	}
	state.pendingProposal = nil
	state.pendingExitProposal = nil
	state.pendingAddProposal = nil

	emissions := make([]event.Envelope, 0, len(cancelled)+len(exit))
	emissions = append(emissions, cancelled...)
	emissions = append(emissions, exit...)
	return emissions, nil
}

// hasOutstandingBusiness reports whether a delisting for this instrument has
// anything to resolve: an open Campaign to close, or a proposal of any kind
// to cancel. It is the condition applyDelisting distinguishes a recorded
// no-op from a transition by.
func (s *instrumentState) hasOutstandingBusiness() bool {
	return s.campaign != nil ||
		s.pendingProposal != nil ||
		s.pendingExitProposal != nil ||
		s.pendingAddProposal != nil
}

// closeCampaignForDelisting builds the Campaign-exited event a delisting
// forces, at the last available price (ADR 0009; see applyDelisting's doc
// comment for which price that is and why the payload carries none of its
// own). It moves no state: the caller commits, once every payload for the
// transition has validated.
func (r *transition) closeCampaignForDelisting(state *instrumentState, payload event.CorporateActionPayload, input event.Envelope) (event.Envelope, error) {
	campaign := state.campaign

	if payload.EffectiveAt.Before(campaign.openedAt) {
		return event.Envelope{}, fmt.Errorf("strategy: instrument %q: delisting effective at %s predates campaign %q's own opening fill at %s; a campaign cannot be closed before it opened",
			payload.InstrumentID, payload.EffectiveAt.Format(time.RFC3339), campaign.campaignID, campaign.openedAt.Format(time.RFC3339))
	}
	// The identical check applyStopFill/applyExitFill apply to their own
	// closing fill: a delisting closing whatever Units survived an earlier
	// partial stop must not claim a moment before that earlier closing
	// fill's own (campaignState.lastCloseFillAt's own doc comment).
	if !campaign.lastCloseFillAt.IsZero() && payload.EffectiveAt.Before(campaign.lastCloseFillAt) {
		return event.Envelope{}, fmt.Errorf("strategy: instrument %q: delisting effective at %s predates campaign %q's most recently accepted closing fill at %s; a later closing event cannot have happened before an earlier one",
			payload.InstrumentID, payload.EffectiveAt.Format(time.RFC3339), campaign.campaignID, campaign.lastCloseFillAt.Format(time.RFC3339))
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
		return event.Envelope{}, fmt.Errorf("strategy: instrument %q: delisting cannot compute the average move in n: %w", payload.InstrumentID, err)
	}
	realisedResultInUnitN, err := sizing.RealisedResultInUnitN(realisedResult, campaign.unitQuantity, campaign.campaignN, r.dollarsPerPoint)
	if err != nil {
		// Unreachable: campaign.unitQuantity and campaign.campaignN were both
		// required positive when the Campaign opened, and r.dollarsPerPoint
		// by ConfigurationPayload.Validate.
		return event.Envelope{}, fmt.Errorf("strategy: instrument %q: delisting cannot compute the realised result in unit n: %w", payload.InstrumentID, err)
	}

	delistingStopLevel, err := campaign.protectiveStop()
	if err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: instrument %q: delisting: %w", payload.InstrumentID, err)
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
		ProtectiveStopLevel:   delistingStopLevel,
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
		return event.Envelope{}, fmt.Errorf("strategy: instrument %q: delisting would close campaign %q with an invalid exit: %w", payload.InstrumentID, campaign.campaignID, err)
	}
	exitedPayloadBytes, err := json.Marshal(exitedPayload)
	if err != nil {
		// Unreachable: json.Marshal of the payload Validate has just accepted;
		// the only thing it could refuse is a NaN or an infinity in a float64
		// field, and Validate has already tested every one for finiteness.
		return event.Envelope{}, fmt.Errorf("strategy: marshal campaign exited payload: %w", err)
	}
	return r.stamp(
		decisionID("campaign-exited", payload.InstrumentID, payload.EffectiveAt),
		event.CampaignExitedEventType, event.CampaignExitedSchemaVersion,
		payload.EffectiveAt, input, exitedPayloadBytes,
	), nil
}

// delistingExpiry is what the three pending-proposal kinds differ in when a
// delisting cancels them; the reason, the instant, the event type and the
// schema are identical across them, which is why one builder serves all three
// (end_of_stream.go's endOfStreamExpiry makes the same choice for the
// end-of-stream expiries).
type delistingExpiry struct {
	idKind         string
	kind           string
	rule           string
	proposalID     string
	signalID       string
	periodEnd      time.Time
	earliestFillAt time.Time
	quantity       int64
	level          float64
}

// emitDelistingExpiry builds the terminal event for one proposal a delisting
// cancels — the delisting-side counterpart of expireAddProposalForStop
// (campaign.go), for the identical reason: ADR 0011's ordinary next-bar
// expiry never runs for a delisted instrument, since there is no next bar.
//
// It moves no state, for the same reason expireAddProposalForStop does not:
// the caller clears the proposal only once every payload the transition
// produces has validated.
func (r *transition) emitDelistingExpiry(expiry delistingExpiry, action event.CorporateActionPayload, input event.Envelope) (event.Envelope, error) {
	payload := event.ProposalExpiredPayload{
		InstrumentID:   action.InstrumentID,
		Kind:           expiry.kind,
		ProposalID:     expiry.proposalID,
		SignalID:       expiry.signalID,
		PeriodEnd:      expiry.periodEnd,
		ExpiredAt:      action.EffectiveAt,
		EarliestFillAt: expiry.earliestFillAt,
		Rule:           expiry.rule,
		ADR:            event.ADRSignalExpiry,
		Reason:         event.ExpiryReasonSupersededByDelisting,
		Quantity:       expiry.quantity,
		Level:          expiry.level,
	}
	if err := payload.Validate(); err != nil {
		// Unreachable: every field is copied from the pending proposal this
		// reducer itself recorded, which passed the same contract when it was
		// proposed, or is action.EffectiveAt, which the caller has already
		// checked is not before state.lastPeriodEnd — at the earliest the
		// proposal's own bar, and so strictly after the bar before it, which
		// is expiry.earliestFillAt.
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal expired payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Unreachable, for the same reason expireAddProposalForStop's marshal
		// guard is.
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal expired payload: %w", err)
	}
	// Keyed to the corporate action that superseded the proposal rather than
	// to the bar that raised it, and to its own kind: a delisting is terminal,
	// so one instrument produces at most one expiry of each kind from it.
	return r.stamp(
		decisionID(expiry.idKind, action.InstrumentID, action.EffectiveAt),
		event.ProposalExpiredEventType, event.ProposalExpiredSchemaVersion,
		action.EffectiveAt, input, payloadBytes,
	), nil
}
