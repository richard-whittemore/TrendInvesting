package risklimit

import (
	"errors"
	"fmt"
	"maps"
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
// those after the book's time. It checks nothing: the engine sets its book
// through CheckBook.
func (g *Gate) SetBook(b reconcile.Book) {
	g.book, g.classified, g.latestReadAsOf, g.reported, g.reportedAsOf = &b, nil, time.Time{}, nil, nil
}

// CheckBook checks b, an account snapshot about to become the book, against
// the book in force and every change journalled since it
// (reconcile.Holdings), moves the engine state as Reconcile does, and then
// sets b as the book (ADR 0019, amended 2026-10-21): a holding no journalled
// fill explains degrades the account on its instrument, and b still becomes
// the book the planner sizes from, the Degraded scope freezing what differs.
// It reports whether the check is to be journalled:
//
//   - never for the run's first book, which has none to be checked against
//     (the IBKR adapter checks that reading against the previous close, or
//     the starting book the Run approval states; ADR 0019, amended
//     2026-10-18);
//   - never when the check continues: a snapshot the fills explain is the
//     ordinary close, and recording nothing keeps every such journal's bytes;
//   - never when b's holdings are exactly the positions a reconciliation
//     already classified against the same book and the same changes: the
//     IBKR adapter sends each reading's reconciliation ahead of its snapshot,
//     and that reconciliation's result, matched or not, already stands for
//     those holdings, so one difference is never reported twice. A new book,
//     or a fill or corporate action recorded after that reconciliation, or
//     dated between its reading and b's (whichever came first), ends its
//     standing, so the snapshot is then checked (classifiedStandsFor);
//   - never for a difference already reported against the same book: an
//     unexplained result whose every instrument was already reported with
//     the same residual, and whose cash is not in doubt, repeats a journalled
//     result, so it is not journalled or paged again however many fills of
//     other instruments came between. A new instrument or a changed residual
//     is a new difference and is reported (ADR 0019, amended 2026-10-21,
//     point 6: one difference, one report).
//
// An unverifiable snapshot halts and is journalled, unless the account is
// already Halted on every one of its reasons (alreadyHaltedOn) and every
// unexplained holding it carries was already reported (alreadyReported): a
// Halt is suppressed only when both hold, so a Halt for a new change or a new
// unexplained holding is always journalled. Neither the classified-holdings
// case nor the reported-difference case alone suppresses a Halt.
func (g *Gate) CheckBook(b reconcile.Book) (Reconciliation, bool) {
	defer g.SetBook(b)
	if g.book == nil {
		return Reconciliation{}, false
	}
	r := reconcile.Holdings(g.accountID, g.book, g.changes, b)
	switch {
	case r.Action == event.ReconciliationActionContinue:
		return Reconciliation{}, false
	case r.Action == event.ReconciliationActionHalt:
		// A Halt is suppressed only when the account is already Halted on
		// every one of its reasons AND every unexplained holding it carries
		// was already reported: nothing new. Otherwise it is journalled.
		if g.alreadyHaltedOn(r) && g.alreadyReported(r) {
			return Reconciliation{}, false
		}
	case g.classifiedStandsFor(b) || g.alreadyReported(r):
		return Reconciliation{}, false
	}
	// The book is reset as CheckBook returns, so what this result reports is
	// not recorded: the next check starts from b.
	return g.consequence(b.AnchorID, r), true
}

// classifiedStandsFor reports whether the latest reconciliation's verdict
// stands for b: it was classified against the book in force, b's holdings
// are exactly its positions, and both checks applied the same changes, so
// no recorded fill or corporate action after the book is dated, to the
// second, between the reconciliation's reading and b's (ADR 0019, amended
// 2026-10-21, point 6: a change dated between the two readings, whichever
// came first, ends its standing).
func (g *Gate) classifiedStandsFor(b reconcile.Book) bool {
	if g.classified == nil || !maps.Equal(g.classified, b.Positions) {
		return false
	}
	lo, hi := g.classifiedAsOf, b.AsOf
	if hi.Before(lo) {
		lo, hi = hi, lo
	}
	for _, c := range g.changes {
		t := c.At.Truncate(time.Second)
		if !t.Before(g.book.AsOf) && !t.Before(lo) && t.Before(hi) {
			return false
		}
	}
	return true
}

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
	// As for a fill: a reconciliation classified before this action did not
	// see it (ADR 0019, amended 2026-10-21, point 6; CheckBook).
	g.classified = nil
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
	// A reading taken before the latest classified one is older knowledge: it
	// does not replace that verdict, even after a recorded change cleared it
	// (ADR 0019, amended 2026-10-21, point 6).
	if g.book != nil && p.Readable(event.ReconciliationComponentPositions) &&
		!p.AsOf.Before(g.latestReadAsOf) {
		g.latestReadAsOf = p.AsOf
		g.classified = map[string]int64{}
		for _, position := range p.Positions {
			g.classified[position.InstrumentID] = position.Quantity
		}
		g.classifiedAsOf = p.AsOf
	}
	g.report(r, p.AsOf)
	return g.consequence(id, r), nil
}

// report records the residual of each instrument r finds unexplained, the
// differences journalled against the book in force, and forgets each
// instrument r finds explained, so a difference that recurs after it was
// resolved is a new difference (ADR 0019, amended 2026-10-21, point 6). r
// was read at asOf: a reading before the latest one about an instrument,
// whether that one reported or resolved it, says nothing new about it, so it
// neither forgets nor replaces what the later reading found. A resolved
// instrument keeps its reading time, without a residual, for that guard.
func (g *Gate) report(r event.ReconciliationResultPayload, asOf time.Time) {
	for _, c := range r.Components {
		switch {
		case c.InstrumentID == "":
		case c.Explained:
			if at, ok := g.reportedAsOf[c.InstrumentID]; ok && asOf.Before(at) {
				continue
			}
			delete(g.reported, c.InstrumentID)
			if g.reportedAsOf == nil {
				g.reported, g.reportedAsOf = map[string]int64{}, map[string]time.Time{}
			}
			g.reportedAsOf[c.InstrumentID] = asOf
		default:
			if at, ok := g.reportedAsOf[c.InstrumentID]; ok && asOf.Before(at) {
				continue
			}
			if g.reported == nil {
				g.reported, g.reportedAsOf = map[string]int64{}, map[string]time.Time{}
			}
			g.reported[c.InstrumentID] = c.Residual
			g.reportedAsOf[c.InstrumentID] = asOf
		}
	}
}

// alreadyHaltedOn reports whether the account is Halted and every reason r,
// an unverifiable result, gives was already given by a journalled Halt: the
// same unapplicable change, not a new one.
func (g *Gate) alreadyHaltedOn(r event.ReconciliationResultPayload) bool {
	if g.state != StateHalted || len(r.Unverifiable) == 0 {
		return false
	}
	for _, reason := range r.Unverifiable {
		if !g.haltedOn[reason] {
			return false
		}
	}
	return true
}

// alreadyReported reports whether every difference of r, an unexplained
// result (CheckBook journals a Halt before asking), was already reported
// with the same residual against the book in force; a close snapshot's check
// compares no cash.
func (g *Gate) alreadyReported(r event.ReconciliationResultPayload) bool {
	for _, c := range r.Components {
		if c.InstrumentID == "" || c.Explained {
			continue
		}
		if residual, ok := g.reported[c.InstrumentID]; !ok || residual != c.Residual {
			return false
		}
	}
	return true
}

// consequence journals r, the classification of input id, and moves the
// engine state as Reconcile states.
func (g *Gate) consequence(id string, r event.ReconciliationResultPayload) Reconciliation {
	out := Reconciliation{Result: r, ResultID: "reconciliation-result:" + g.accountID + ":" + id}
	var state *event.EngineStatePayload
	if r.Action == event.ReconciliationActionHalt {
		if g.haltedOn == nil {
			g.haltedOn = map[string]bool{}
		}
		for _, reason := range r.Unverifiable {
			g.haltedOn[reason] = true
		}
	}
	switch {
	case r.Action == event.ReconciliationActionHalt && g.state != StateHalted:
		state = &event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonReconciliationUnverifiable, Detail: "reconciliation " + r.ReconciliationID + " " + r.Detail}
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
			Detail:             "reconciliation " + r.ReconciliationID + " " + r.Detail,
			ScopeInstrumentIDs: slices.Clone(scope), ScopeCashComponents: slices.Clone(cash),
		}
		out.StateID = g.move(StateDegraded, id)
	}
	if state != nil {
		out.State, out.StateSchemaVersion = state, event.EngineStateScopedVersion
	}
	return out
}
