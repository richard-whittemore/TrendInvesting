package risklimit

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// Reconciliation is the gate's answer to one account.reconciliation input:
// the classification to journal under ResultID, and State when it moved the
// account's engine state, at StateSchemaVersion, to be journalled under
// StateID.
type Reconciliation struct {
	Result             event.ReconciliationResultPayload
	ResultID           string
	State              *event.EngineStatePayload
	StateSchemaVersion uint32
	StateID            string
}

// SetBook records the journal's book that later reconciliations start from:
// the latest account snapshot, which the planner sizes from (ADR 0026, point
// 2). The changes journalled since stay recorded; the classifier applies
// those after the book's time.
func (g *Gate) SetBook(b reconcile.Book) { g.book = &b }

// ObserveCorporateAction records a journalled market.corporate-action,
// envelope id, as a change a reconciliation's book is bridged by: a
// dividend's posted cash or a split's new holding (ADRs 0023 and 0024). Any
// other kind is recorded too, so a reconciliation it falls inside is
// unverifiable rather than blind to it. A corporate action is one fact under
// its envelope ID: a redelivery of the same fact changes nothing, and the ID
// stating a different one is refused and leaves what was recorded, since a
// dividend or a split counted twice would explain a difference the broker
// never made (ADR 0019, "Explained means").
func (g *Gate) ObserveCorporateAction(id string, p event.CorporateActionPayload) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("risklimit: %w", err)
	}
	if id == "" {
		return errors.New("risklimit: a corporate action needs its envelope ID")
	}
	if seen, ok := g.corporateActions[id]; ok {
		if sameCorporateAction(seen, p) {
			return nil
		}
		return fmt.Errorf("risklimit: corporate action %s is reused for a different action", id)
	}
	g.corporateActions[id] = p
	g.changes = append(g.changes, reconcile.CorporateActionChange(id, p))
	return nil
}

// sameCorporateAction reports whether a and b state the same action: every
// term equal, and the same effective instant whatever its time zone.
func sameCorporateAction(a, b event.CorporateActionPayload) bool {
	if !a.EffectiveAt.Equal(b.EffectiveAt) {
		return false
	}
	a.EffectiveAt, b.EffectiveAt = time.Time{}, time.Time{}
	return a == b
}

// Reconcile classifies a recorded account.reconciliation, envelope id,
// against the journal's book and every change since it (reconcile.Classify),
// and moves the engine state as ADR 0019 (amended 2026-09-24) sets:
//
//   - matched or explained changes nothing: a later match never returns a
//     Degraded or Halted account to Normal;
//   - unexplained degrades the account, its scope widened by exactly the
//     unexplained instruments and cash components; a cash-only difference
//     freezes no instrument and blocks buys;
//   - unverifiable halts the whole account.
//
// A state record is returned only when the state or its scope changes, and
// it states the whole scope; Halted is never loosened. The first record out
// of Normal is the incident.
func (g *Gate) Reconcile(id string, p event.ReconciliationPayload) (Reconciliation, error) {
	if err := p.Validate(); err != nil {
		return Reconciliation{}, fmt.Errorf("risklimit: %w", err)
	}
	if id == "" {
		return Reconciliation{}, errors.New("risklimit: a reconciliation needs its envelope ID")
	}
	if p.AccountID != g.accountID {
		return Reconciliation{}, fmt.Errorf("risklimit: reconciliation for account %q; this gate is %q", p.AccountID, g.accountID)
	}
	r := reconcile.Classify(g.book, g.changes, id, p)
	out := Reconciliation{Result: r, ResultID: "reconciliation-result:" + g.accountID + ":" + id}
	var state *event.EngineStatePayload
	switch {
	case r.Action == event.ReconciliationActionHalt && g.state != StateHalted:
		state = &event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonReconciliationUnverifiable, Detail: "reconciliation " + p.ReconciliationID + " " + r.Detail}
		g.scope, g.cashScope = nil, nil
		out.StateID = g.move(StateHalted, id)
	case r.Action == event.ReconciliationActionDegrade && g.state != StateHalted:
		scope, cash := union(g.scope, r.ScopeInstrumentIDs), union(g.cashScope, r.ScopeCashComponents)
		if g.state == StateDegraded && slices.Equal(scope, g.scope) && slices.Equal(cash, g.cashScope) {
			break
		}
		g.scope, g.cashScope = scope, cash
		state = &event.EngineStatePayload{
			State: event.EngineStateDegraded, Reason: event.EngineStateReasonReconciliationUnexplained,
			Detail:             "reconciliation " + p.ReconciliationID + " " + r.Detail,
			ScopeInstrumentIDs: slices.Clone(scope), ScopeCashComponents: slices.Clone(cash),
		}
		out.StateID = g.move(StateDegraded, id)
	}
	if state != nil {
		out.State, out.StateSchemaVersion = state, event.EngineStateScopedVersion
	}
	return out, nil
}
