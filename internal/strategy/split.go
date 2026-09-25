package strategy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds a split's cash in lieu (CONTEXT.md: "Cash in lieu"; ADR
// 0023).
//
// The engine's split-adjusted view is adjusted for every split already,
// later ones included (ADR 0004, as amended), so a split changes none of the
// reducer's levels, quantities or N. What a split can change is the broker's
// holding: a broker that cannot deliver a fraction of a share pays cash for
// it instead, and so can hold up to one raw share fewer per Unit than the
// Units at the exact ratio. The split's corporate action states that
// shortfall and the cash, and this is the one route by which it reaches the
// Campaign. Any other difference between the broker and the Units is
// unexplained and halts (ADR 0019): the reducer never adopts a balance.

// applySplit handles a split corporate action (ADR 0023).
//
// # The rule: most recent Unit first, one raw share each
//
// RawSharesLost raw shares come off the Campaign's RawSharesLost most recent
// Units, the most recent first, one raw share (EngineSharesPerRawShare of
// the engine's shares) each. A broker's fraction is less than one share, so
// each Unit's position can be short by at most one whole share once the
// holding is rounded down; a shortfall of more raw shares than the Campaign
// holds Units is not rounding, and fails closed.
//
// # What it records and changes
//
// One strategy.campaign.cash-in-lieu decision, then one
// strategy.exit-order.set per reduced Unit at its unchanged level for its new
// quantity, so the Exit Orders still cover exactly the holding and
// internal/fills re-rests them from these emissions alone. The lost shares
// join the Campaign's closed quantity at their Units' own entry prices, and
// the cash, in points, its closed exit value, exactly as a partial
// stop-out's do (campaignState.lifeAggregate), so the Campaign's exit reports
// its whole life. Nothing else moves: the cash reaches spendable cash only
// through the next previous-close snapshot, like every credit (ADR 0020).
//
// # Invariants
//
// Each split of an instrument applies once, strictly after the last one, and
// never before the instrument's last completed bar or a fill its Campaign has
// accepted, so a redelivered split can never reduce a Unit twice. A split
// that states a shortfall or cash while nothing is held, for a delisted
// instrument, or in a currency other than the account's, fails closed. A
// split stating neither changes nothing and decides nothing.
func (r *transition) applySplit(payload event.CorporateActionPayload, input event.Envelope) ([]event.Envelope, error) {
	id := payload.InstrumentID
	at := payload.EffectiveAt.Format(time.RFC3339)
	if _, delisted := r.delisted[id]; delisted {
		return nil, fmt.Errorf("strategy: instrument %q: a split effective at %s names an instrument already delisted, which is held by nothing and trades no more (ADR 0009)", id, at)
	}
	if r.accountCurrency != "" && payload.Currency != r.accountCurrency {
		return nil, fmt.Errorf("strategy: instrument %q: a split effective at %s paid cash in lieu in %q, but the account is kept in %q; multi-currency accounts are out of scope", id, at, payload.Currency, r.accountCurrency)
	}
	claims := payload.RawSharesLost > 0 || payload.CashInLieu > 0
	nothingHeld := func() error {
		return fmt.Errorf("strategy: instrument %q: a split effective at %s states %d raw share(s) lost and %v cash in lieu, but there is no open Campaign in it; no Unit this engine holds can explain the broker's difference, which halts (ADR 0019, ADR 0023)",
			id, at, payload.RawSharesLost, payload.CashInLieu)
	}

	state, known := r.instrument(id)
	if !known {
		if claims {
			return nil, nothingHeld()
		}
		return nil, nil
	}
	if payload.EffectiveAt.Before(state.lastPeriodEnd) {
		return nil, fmt.Errorf("strategy: instrument %q: a split effective at %s predates the last completed bar %s this reducer has for it; a split takes effect between Sessions, after the bars it follows (ADR 0023)",
			id, at, state.lastPeriodEnd.Format(time.RFC3339))
	}
	if !state.lastSplitAt.IsZero() && !payload.EffectiveAt.After(state.lastSplitAt) {
		return nil, fmt.Errorf("strategy: instrument %q: a split effective at %s is not after the split already applied at %s; each split applies once, in order, so none can reduce a Unit twice (ADR 0023)",
			id, at, state.lastSplitAt.Format(time.RFC3339))
	}
	campaign := state.campaign
	if campaign == nil {
		if claims {
			return nil, nothingHeld()
		}
		state.lastSplitAt = payload.EffectiveAt
		return nil, nil
	}
	if latest := campaign.latestFillAt(); payload.EffectiveAt.Before(latest) {
		return nil, fmt.Errorf("strategy: instrument %q: a split effective at %s predates the fill at %s campaign %q has already accepted; the split cannot have changed shares a later fill already traded (ADR 0023)",
			id, at, latest.Format(time.RFC3339), campaign.campaignID)
	}
	if !claims {
		state.lastSplitAt = payload.EffectiveAt
		return nil, nil
	}
	held := len(campaign.units)
	if payload.RawSharesLost > int64(held) {
		return nil, fmt.Errorf("strategy: instrument %q: a split effective at %s states %d raw share(s) lost, but campaign %q holds %d Unit(s); cash in lieu is at most one raw share per Unit, so the rest of the difference is unexplained and halts (ADR 0023, ADR 0019)",
			id, at, payload.RawSharesLost, campaign.campaignID, held)
	}

	perRaw := payload.EngineSharesPerRawShare
	lost := int(payload.RawSharesLost)
	reduced := campaign.units[held-lost:]
	reductions := make([]event.UnitReduction, 0, lost)
	for i := len(reduced) - 1; i >= 0; i-- {
		u := reduced[i]
		if u.quantity <= perRaw {
			return nil, fmt.Errorf("strategy: instrument %q: a split effective at %s would take one raw share of %d engine shares off unit %d of campaign %q, which holds %d; a reduced Unit keeps at least one share, so the difference is unexplained and halts (ADR 0023)",
				id, at, perRaw, u.index, campaign.campaignID, u.quantity)
		}
		reductions = append(reductions, event.UnitReduction{UnitIndex: u.index, QuantityBefore: u.quantity, QuantityAfter: u.quantity - perRaw})
	}
	engineLost := int64(lost) * perRaw
	before := campaign.filledQuantity()
	decisionPayload := event.CampaignCashInLieuPayload{
		CampaignID:              campaign.campaignID,
		InstrumentID:            id,
		CorporateActionID:       input.ID,
		EffectiveAt:             payload.EffectiveAt,
		NewShares:               payload.NewShares,
		OldShares:               payload.OldShares,
		EngineSharesPerRawShare: perRaw,
		RawSharesLost:           payload.RawSharesLost,
		EngineSharesLost:        engineLost,
		CashInLieu:              payload.CashInLieu,
		Currency:                payload.Currency,
		Reductions:              reductions,
		QuantityBefore:          before,
		QuantityAfter:           before - engineLost,
		Rule:                    event.RuleCashInLieuMostRecentUnitsFirst,
		ADR:                     event.ADRSplitCashInLieu,
	}
	if err := decisionPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: built invalid cash in lieu payload: %w", id, err)
	}
	decisionBytes, err := json.Marshal(decisionPayload)
	if err != nil {
		// validated-payload-json (docs/development.md).
		return nil, fmt.Errorf("strategy: marshal cash in lieu payload: %w", err)
	}
	emissions := []event.Envelope{r.stamp(
		decisionID("cash-in-lieu", id, payload.EffectiveAt),
		event.CampaignCashInLieuEventType, event.CampaignCashInLieuSchemaVersion,
		payload.EffectiveAt, input, decisionBytes,
	)}

	// The candidate Campaign moves here; transact publishes it only once
	// every emission below has validated.
	var lostEntryWeightedSum float64
	for i := range reduced {
		lostEntryWeightedSum += sizing.Product(float64(perRaw), reduced[i].fillPrice)
		reduced[i].quantity -= perRaw
	}
	campaign.closedQuantity += engineLost
	campaign.closedEntryWeightedSum += lostEntryWeightedSum
	campaign.closedExitWeightedSum += payload.CashInLieu / r.dollarsPerPoint
	for _, u := range reduced {
		envelope, err := r.exitOrderSet(campaign, u, exitOrderFor(u, campaign.campaignID, state.pendingExitProposal), payload.EffectiveAt, "split", input)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, envelope)
	}
	state.lastSplitAt = payload.EffectiveAt
	return emissions, nil
}

// latestFillAt is the latest fill this Campaign has accepted: its most
// recent Unit's, or a later closing fill's.
func (c *campaignState) latestFillAt() time.Time {
	latest := c.lastUnit().filledAt
	if c.lastCloseFillAt.After(latest) {
		latest = c.lastCloseFillAt
	}
	return latest
}
