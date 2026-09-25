package strategy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds a symbol change (ADR 0024): the CorporateActionKindSymbolChange
// kind of market.corporate-action.
//
// # Instrument identity
//
// This reducer's instrument id is already an opaque, producer-assigned
// identity, never the ticker itself — CompletedBarPayload.InstrumentID and
// every other payload's own field carry no separate ticker or symbol at all,
// and nothing in this package derives one from the other (grep the module:
// there is no "symbol" or "ticker" field anywhere in internal/event or
// internal/strategy). A symbol change is therefore a statement that ONE
// instrument's identity moves from one id to another — never that a ticker
// changed while some other, unrelated key stayed fixed.
//
// The whole of an instrument's tracked state — its open Campaign with its
// frozen values (ADR 0006), its indicator history (N, the Entry/Exit
// Channels, the ranking windows), its universe classification (ADR 0009) and
// every resting order (its pending proposals and their ADR 0020 holds) —
// lives in one *instrumentState, published under the instrument id as the
// map key (Reducer.instruments). A symbol change moves that SAME value to a
// new key; it copies nothing and re-derives nothing, so there is no field
// this reducer could fail to carry across by forgetting to name it. This is
// the opposite shape from a delisting followed by a new instrument: nothing
// closes, and the Campaign's identity (CampaignID) does not change.
//
// The old id is retired (Reducer.renamed): once this reducer has moved an
// id's state away, a later bar naming it fails closed (reducer.go's
// applyCompletedBar) rather than silently opening a fresh, zero-history
// instrument under a name that used to mean something else — the identical
// concern ADR 0019 states for reconciliation: "a symbol rename must not
// merge unrelated instruments." The same concern runs the other way at the
// destination: a symbol change into an id this reducer already tracks is
// refused below, so two instruments' histories can never be merged by a
// producer's mistake.

// applySymbolChange handles a symbol change corporate action (ADR 0024).
//
// # What it records and changes
//
// When the old id is known, one strategy.instrument.symbol-changed decision,
// then the whole of its instrument state — Campaign, indicators, universe
// classification, pending proposals and their holds — moves to the new id in
// this same transaction. Every hold's own InstrumentID (hold.go) is
// re-pointed at the new id too, so ADR 0008's per-instrument cap accounting
// continues to recognise a standing hold as this same instrument's after the
// change; the hold itself already carries no other instrument-shaped field.
//
// When the old id is genuinely unknown — no bar has ever been accepted for
// it — there is nothing to move, and nothing is journalled, mirroring
// applyDelisting's identical no-op for an instrument this reducer never saw.
// The old id is still retired, so a later bar under it fails closed rather
// than opening a fresh Setup.
//
// # Invariants
//
// The new id must not already name an instrument this reducer tracks: two
// live instruments merging under one id would silently combine unrelated
// Campaigns (ADR 0019's own rule, quoted above). Neither id may already be
// delisted. A symbol change takes effect between Sessions, at or after the
// old instrument's last completed bar, and before either id's bar in the
// currently open Session — the ADR 0010/0021 ordering every corporate action
// keeps (applyCorporateAction's own barReceivedInOpenSession check covers the
// old id; this function checks the new id too, since a producer could
// otherwise deliver the change after the new id's own first bar of the same
// Session).
func (r *transition) applySymbolChange(payload event.CorporateActionPayload, input event.Envelope) ([]event.Envelope, error) {
	oldID, newID := payload.InstrumentID, payload.NewInstrumentID
	at := payload.EffectiveAt.Format(time.RFC3339)

	if _, delisted := r.delisted[oldID]; delisted {
		return nil, fmt.Errorf("strategy: instrument %q: a symbol change effective at %s names an instrument already delisted, which trades no more (ADR 0009)", oldID, at)
	}
	if _, delisted := r.delisted[newID]; delisted {
		return nil, fmt.Errorf("strategy: instrument %q: a symbol change effective at %s would continue %q under %q, which is already delisted (ADR 0009)", oldID, at, oldID, newID)
	}
	if r.barReceivedInOpenSession(newID) {
		return nil, fmt.Errorf("strategy: instrument %q: a symbol change effective at %s would continue it under %q, whose bar the open Session ending %s already holds; state it between Sessions (ADR 0021)",
			oldID, at, newID, r.sessionPeriodEnd.Format(time.RFC3339))
	}
	if _, alreadyTracked := r.instrument(newID); alreadyTracked {
		return nil, fmt.Errorf("strategy: instrument %q: a symbol change effective at %s would continue it under %q, which this reducer already tracks as its own instrument; a symbol change must never merge two instruments (ADR 0019, ADR 0024)",
			oldID, at, newID)
	}

	state, known := r.instrument(oldID)
	if !known {
		r.renamed[oldID] = newID
		return nil, nil
	}
	if payload.EffectiveAt.Before(state.lastPeriodEnd) {
		return nil, fmt.Errorf("strategy: instrument %q: a symbol change effective at %s predates the last completed bar %s this reducer has for it; a symbol change takes effect between Sessions, after the bars it follows (ADR 0024)",
			oldID, at, state.lastPeriodEnd.Format(time.RFC3339))
	}

	var campaignID string
	if state.campaign != nil {
		campaignID = state.campaign.campaignID
	}
	decisionPayload := event.InstrumentSymbolChangedPayload{
		InstrumentID:      oldID,
		NewInstrumentID:   newID,
		CorporateActionID: input.ID,
		EffectiveAt:       payload.EffectiveAt,
		CampaignID:        campaignID,
		Rule:              event.RuleSymbolChangeCarriesInstrumentState,
		ADR:               event.ADRSymbolChangeCarriesInstrumentState,
	}
	if err := decisionPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: instrument %q: built invalid instrument symbol changed payload: %w", oldID, err)
	}
	decisionBytes, err := json.Marshal(decisionPayload)
	if err != nil {
		// validated-payload-json (docs/development.md).
		return nil, fmt.Errorf("strategy: marshal instrument symbol changed payload: %w", err)
	}
	emission := r.stamp(
		decisionID("symbol-changed", oldID, payload.EffectiveAt),
		event.InstrumentSymbolChangedEventType, event.InstrumentSymbolChangedSchemaVersion,
		payload.EffectiveAt, input, decisionBytes,
	)

	// --- State moves only now, after the one payload above has validated.
	//
	// campaignState.instrumentID is a denormalised copy of the map key,
	// carried so campaign.go/exit_order.go can build a proposal or an Exit
	// Order's payload, a decisionID, a cap check (capExceeded) or a hold
	// (placeHold) without threading the instrument id through every call.
	// Left at the old id, every one of those would keep citing the retired
	// id for the rest of this Campaign's life, so it moves in the same step
	// as the map key.
	if state.campaign != nil {
		state.campaign.instrumentID = newID
	}
	for i := range r.holds {
		if r.holds[i].instrumentID == oldID {
			r.holds[i].instrumentID = newID
		}
	}
	if record, ok := r.classifications[oldID]; ok {
		delete(r.classifications, oldID)
		r.classifications[newID] = record
	}
	r.renameInstrument(oldID, newID, state)
	r.renamed[oldID] = newID

	return []event.Envelope{emission}, nil
}
