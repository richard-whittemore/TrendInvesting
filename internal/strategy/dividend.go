package strategy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds a dividend (ADR 0004, ADR 0024): the CorporateActionKindDividend
// kind of market.corporate-action.
//
// ADR 0004 requires dividends "credited as cash events when they occur",
// never folded into either price series: a dividend-adjusted history rewrites
// the breakout levels that actually existed. This mirrors split.go's cash in
// lieu, which is the same shape for the same reason (ADR 0023's own Context),
// but simpler: a dividend changes no Unit's quantity and re-rests no Exit
// Order. It changes nothing this reducer's signal computation reads — no
// channel level, no N, no price the rules see — and its whole effect is one
// decision recording the cash and the Campaign it was credited to.

// applyDividend handles a dividend corporate action (ADR 0024).
//
// # What it records and changes
//
// One strategy.campaign.dividend decision naming the Campaign it was paid
// to, the cash and its currency. Nothing else moves: like a split's cash in
// lieu, the cash reaches spendable cash only through the next previous-close
// snapshot (ADR 0020) — this reducer records the credit; internal/fills'
// simulated account, or a live broker's own cash, is what actually holds it.
//
// # Invariants
//
// A dividend requires an open Campaign to be paid to: the cash is a return on
// shares held, and a payment for an instrument this reducer holds nothing in
// is unexplained and halts (ADR 0019), exactly as a split's shortfall does
// with nothing held. Each dividend must arrive strictly after the last one
// applied to the same instrument, so a redelivered dividend can never credit
// its cash twice; unlike a split, this does not forbid a SECOND, later,
// genuinely different dividend — only a repeat of the same instant.
func (r *transition) applyDividend(payload event.CorporateActionPayload, input event.Envelope) ([]event.Envelope, error) {
	id := payload.InstrumentID
	at := payload.EffectiveAt.Format(time.RFC3339)
	if _, delisted := r.delisted[id]; delisted {
		return nil, fmt.Errorf("strategy: instrument %q: a dividend effective at %s names an instrument already delisted, which is held by nothing and trades no more (ADR 0009)", id, at)
	}
	if r.accountCurrency != "" && payload.Currency != r.accountCurrency {
		return nil, fmt.Errorf("strategy: instrument %q: a dividend effective at %s paid %q, but the account is kept in %q; multi-currency accounts are out of scope", id, at, payload.Currency, r.accountCurrency)
	}

	state, known := r.instrument(id)
	nothingHeld := func() error {
		return fmt.Errorf("strategy: instrument %q: a dividend effective at %s paid %v %s, but there is no open Campaign in it; no Unit this engine holds can explain the payment, which halts (ADR 0019, ADR 0024)",
			id, at, payload.CashAmount, payload.Currency)
	}
	if !known {
		return nil, nothingHeld()
	}
	if payload.EffectiveAt.Before(state.lastPeriodEnd) {
		return nil, fmt.Errorf("strategy: instrument %q: a dividend effective at %s predates the last completed bar %s this reducer has for it; a dividend takes effect between Sessions, after the bars it follows (ADR 0024)",
			id, at, state.lastPeriodEnd.Format(time.RFC3339))
	}
	if !state.lastDividendAt.IsZero() && !payload.EffectiveAt.After(state.lastDividendAt) {
		return nil, fmt.Errorf("strategy: instrument %q: a dividend effective at %s is not after the dividend already applied at %s; each dividend applies once, in order, so none can credit its cash twice (ADR 0024)",
			id, at, state.lastDividendAt.Format(time.RFC3339))
	}
	if state.campaign == nil {
		return nil, nothingHeld()
	}
	if latest := state.campaign.latestFillAt(); payload.EffectiveAt.Before(latest) {
		return nil, fmt.Errorf("strategy: instrument %q: a dividend effective at %s predates the fill at %s campaign %q has already accepted; a dividend on shares not yet held cannot have been paid (ADR 0024)",
			id, at, latest.Format(time.RFC3339), state.campaign.campaignID)
	}

	decisionPayload := event.CampaignDividendPayload{
		CampaignID:        state.campaign.campaignID,
		InstrumentID:      id,
		CorporateActionID: input.ID,
		EffectiveAt:       payload.EffectiveAt,
		CashAmount:        payload.CashAmount,
		Currency:          payload.Currency,
		Rule:              event.RuleDividendCreditedAsCash,
		ADR:               event.ADRDividendCreditedAsCash,
	}
	if err := decisionPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: built invalid campaign dividend payload: %w", id, err)
	}
	decisionBytes, err := json.Marshal(decisionPayload)
	if err != nil {
		// validated-payload-json (docs/development.md).
		return nil, fmt.Errorf("strategy: marshal campaign dividend payload: %w", err)
	}
	emission := r.stamp(
		decisionID("dividend", id, payload.EffectiveAt),
		event.CampaignDividendEventType, event.CampaignDividendSchemaVersion,
		payload.EffectiveAt, input, decisionBytes,
	)

	state.lastDividendAt = payload.EffectiveAt
	return []event.Envelope{emission}, nil
}
