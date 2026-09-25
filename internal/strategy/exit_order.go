package strategy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds each held Unit's Exit Order (CONTEXT.md: "Exit Order"):
// the one sell order that Unit rests, for its own shares, and the
// strategy.exit-order.set decision that records every change of its level.
//
// The rule: a long Unit's Exit Order rests at the higher of its own
// Protective Stop and, while an Exit-Channel exit is proposed for its
// Campaign, that exit's level; a tie names the stop. Both are resting sell
// orders at their level under ADR 0005, and a long position sold at the
// higher of two sell levels is sold at the first one price reaches — the
// Protective Stop (The Turtle Rules p.22) or the Exit Channel (p.26) —
// with one order per Unit, so the two can never both sell the same shares.
//
// It is derived only from state the reducer already holds — each Unit's
// protectiveStop and the Campaign's pendingExitProposal — and changes
// neither. strategy.protective-stop.set and strategy.exit.proposed keep
// their exact meaning; this decision is their per-Unit combination, added
// beside them.

// exitOrder is one Unit's Exit Order: where it rests, which level governs,
// and the proposed exit level it was compared against (zero when none is
// proposed).
type exitOrder struct {
	level     float64
	source    string
	exitLevel float64
}

// exitOrderFor applies the rule above to one Unit. pending is the
// instrument's outstanding exit proposal, or nil; one that belongs to a
// different Campaign than campaignID is ignored, since an exit proposal
// closes only the Campaign it names.
func exitOrderFor(u unitState, campaignID string, pending *pendingExitProposalState) exitOrder {
	order := exitOrder{level: u.protectiveStop, source: event.ExitOrderSourceProtectiveStop}
	if pending == nil || pending.campaignID != campaignID {
		return order
	}
	order.exitLevel = pending.level
	// Strictly above: a tie names the stop (event.ExitOrderSourceProtectiveStop).
	if pending.level > u.protectiveStop {
		order.level = pending.level
		order.source = event.ExitOrderSourceExitChannel
	}
	return order
}

// exitOrderChanges returns one strategy.exit-order.set decision for every
// Unit in units whose Exit Order differs from the last one recorded for it
// (unitState.exitOrderLevel/exitOrderSource), in the order units holds them.
// A Unit never recorded — one whose stop has just been set — always
// differs. Nothing is mutated here: the caller records the new levels with
// recordExitOrders on candidate state. transact publishes the recorded levels
// only after every decision of the input has validated.
//
// cause distinguishes the decision's id from any other change to the same
// Unit at the same instant: the fill's id for a fill, "bar" for a completed
// bar, "end-of-stream" for the end of the input stream.
func (r *transition) exitOrderChanges(campaign *campaignState, units []unitState, pending *pendingExitProposalState, asOf time.Time, cause string, input event.Envelope) ([]event.Envelope, error) {
	var emissions []event.Envelope
	for _, u := range units {
		order := exitOrderFor(u, campaign.campaignID, pending)
		if order.level == u.exitOrderLevel && order.source == u.exitOrderSource {
			continue
		}
		envelope, err := r.exitOrderSet(campaign, u, order, asOf, cause, input)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, envelope)
	}
	return emissions, nil
}

// exitOrderSet builds the strategy.exit-order.set decision for one Unit's
// Exit Order at order, for the Unit's own current quantity. It moves no
// state.
func (r *transition) exitOrderSet(campaign *campaignState, u unitState, order exitOrder, asOf time.Time, cause string, input event.Envelope) (event.Envelope, error) {
	payload := event.ExitOrderSetPayload{
		CampaignID:       campaign.campaignID,
		InstrumentID:     campaign.instrumentID,
		UnitIndex:        u.index,
		Level:            order.level,
		Quantity:         u.quantity,
		Source:           order.source,
		ProtectiveStop:   u.protectiveStop,
		ExitChannelLevel: order.exitLevel,
		AsOf:             asOf,
		Rule:             event.RuleExitOrderHigherOfStopAndExitChannel,
		ADR:              event.ADRExitOrderRestsAtTheLevel,
	}
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: instrument %q: campaign %q: built invalid exit order payload for unit %d: %w", campaign.instrumentID, campaign.campaignID, u.index, err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// validated-payload-json (docs/development.md).
		return event.Envelope{}, fmt.Errorf("strategy: marshal exit order payload: %w", err)
	}
	return r.stamp(
		decisionID(fmt.Sprintf("exit-order-set-unit-%d-%s", u.index, cause), campaign.instrumentID, asOf),
		event.ExitOrderSetEventType, event.ExitOrderSetSchemaVersion,
		asOf, input, payloadBytes,
	), nil
}

// recordExitOrders remembers, on every Unit the Campaign holds, the Exit
// Order exitOrderChanges has just reported for it, so the next change is
// measured from it.
func recordExitOrders(campaign *campaignState, pending *pendingExitProposalState) {
	for i := range campaign.units {
		order := exitOrderFor(campaign.units[i], campaign.campaignID, pending)
		campaign.units[i].exitOrderLevel = order.level
		campaign.units[i].exitOrderSource = order.source
	}
}

// emitExitOrderChanges is exitOrderChanges over the Campaign's own held
// Units followed at once by recordExitOrders, for a caller whose state has
// already moved: a completed bar, after its exit proposal was raised or
// expired, and the end of the input stream, after its expiries.
func (r *transition) emitExitOrderChanges(state *instrumentState, asOf time.Time, cause string, input event.Envelope) ([]event.Envelope, error) {
	campaign := state.campaign
	if campaign == nil {
		return nil, nil
	}
	emissions, err := r.exitOrderChanges(campaign, campaign.units, state.pendingExitProposal, asOf, cause, input)
	if err != nil {
		return nil, err
	}
	recordExitOrders(campaign, state.pendingExitProposal)
	return emissions, nil
}
