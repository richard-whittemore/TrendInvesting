package targetweight

import (
	"fmt"
	"maps"
	"slices"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// applyRecorded reads one record of a momentum run's restart prelude (see
// restart.go), in journal order, through the same order book the live run
// keeps, without planning or emitting anything: what the earlier run planned
// and released is in the records themselves (ADR 0026, point 5).
func (r *momentumRun) applyRecorded(in event.Envelope) error {
	h := r.h
	switch in.Type {
	case event.TargetsDecidedEventType:
		p, err := decodeAccount(h, in, event.TargetsDecidedSchemaVersion, func(p event.TargetsDecidedPayload) string { return p.AccountID })
		if err != nil {
			return err
		}
		if p.StrategyID != event.TargetWeightStrategyMomentum {
			return fmt.Errorf("targetweight: recovered decision %s is not a momentum decision", in.ID)
		}
		r.lastDecision, r.quietUntil = in.ID, p.SessionPeriodEnd
		if p.Outcome == event.TargetsOutcomeDecided {
			h.gate.RecordDecision(in.ID)
			r.book, r.bookID = &p, in.ID
			r.rebalance = &rebalance{decisionID: in.ID, session: p.SessionPeriodEnd, sold: map[string]int64{}, recovered: true, decision: p}
		}
		return nil
	case event.OrderPlannedEventType:
		p, err := decodeAccount(h, in, event.OrderPlannedSchemaVersion, func(p event.OrderPlannedPayload) string { return p.AccountID })
		if err != nil {
			return err
		}
		reb := r.rebalance
		if reb == nil || p.Tag != reb.decisionID {
			return fmt.Errorf("targetweight: recorded order %s serves %s, which is not the rebalance recovered", p.ClientOrderID, p.Tag)
		}
		if _, seen := r.recorded[p.ClientOrderID]; seen {
			return fmt.Errorf("targetweight: order %s is recorded as planned twice", p.ClientOrderID)
		}
		if r.recorded == nil {
			r.recorded = map[string]event.OrderPlannedPayload{}
		}
		r.recorded[p.ClientOrderID] = p
		if p.Phase == event.OrderSideSell {
			reb.recordedSells = append(reb.recordedSells, p)
		} else {
			reb.buysPlanned = true
		}
		return nil
	case event.OrderReleasedEventType:
		p, err := decodeAccount(h, in, event.OrderReleasedSchemaVersion, func(p event.OrderReleasedPayload) string { return p.AccountID })
		if err != nil {
			return err
		}
		plan, ok := r.recorded[p.ClientOrderID]
		switch {
		case !ok:
			return fmt.Errorf("targetweight: recorded release %s names order %s, which no recorded plan names", in.ID, p.ClientOrderID)
		case plan.Tag != p.Tag || plan.InstrumentID != p.InstrumentID || plan.Side != p.Side || plan.Quantity != p.Quantity || plan.LimitPrice != p.LimitPrice:
			return fmt.Errorf("targetweight: recorded release %s does not describe the order planned as %s", in.ID, p.ClientOrderID)
		case r.orders[p.ClientOrderID] != nil:
			return fmt.Errorf("targetweight: order %s is recorded as released twice", p.ClientOrderID)
		}
		r.orders[p.ClientOrderID] = &trackedOrder{planned: plan, fills: map[string]event.TargetWeightFillPayload{}}
		reb := r.rebalance
		if plan.Side == event.OrderSideSell {
			reb.sells = append(reb.sells, plan.ClientOrderID)
		} else {
			cost := orderplan.BuyCost(plan, r.exec.MaxFeesPerOrder)
			reb.budgetDo(func(b *orderplan.Budget) { b.Release(plan.ClientOrderID, cost) })
		}
		return nil
	case event.OrderRefusedEventType:
		// A refused order was never sent, so it leaves nothing to recover.
		_, err := decodeAccount(h, in, event.OrderRefusedSchemaVersion, func(p event.OrderRefusedPayload) string { return p.AccountID })
		return err
	case event.EngineStateEventType:
		return h.restoreEngineState(in)
	case event.OrderLifecycleEventType:
		_, err := r.applyLifecycle(in)
		return err
	case event.FillEventType:
		_, err := r.applyFill(in)
		return err
	case event.RestartRecoveredEventType:
		p, err := decodeAccount(h, in, event.RestartRecoveredSchemaVersion, func(p event.RestartRecoveredPayload) string { return p.AccountID })
		if err != nil {
			return err
		}
		if p.Rebalance.Held {
			if r.rebalance == nil || r.rebalance.decisionID != p.Rebalance.DecisionID {
				return fmt.Errorf("targetweight: recorded restart %s held %s, which is not the rebalance recovered", in.ID, p.Rebalance.DecisionID)
			}
			r.rebalance.held = true
		}
		return nil
	case event.RebalanceClearedEventType:
		_, err := r.applyCleared(in)
		return err
	}
	return fmt.Errorf("targetweight: %s %s is not a record a momentum restart reads", in.Type, in.ID)
}

// recovered fills in what the prelude recovered and, when a rebalance is in
// progress, holds it: nothing more is sent for its decision time until the
// owner clears it (ADR 0026, point 5; plan.md section 5).
func (r *momentumRun) recovered(p *event.RestartRecoveredPayload) {
	p.LastDecisionID, p.QuietUntil = r.lastDecision, r.quietUntil
	if r.book != nil {
		p.BookDecisionID, p.Book, p.Glide = r.bookID, r.book.Targets, r.book.Glide
	}
	reb := r.rebalance
	if reb == nil {
		return
	}
	var working []string
	for _, id := range slices.Sorted(maps.Keys(r.orders)) {
		if !r.orders[id].terminal {
			working = append(working, id)
		}
	}
	phase := event.RebalanceComplete
	switch {
	case reb.abandoned:
		phase = event.RebalanceAbandoned
	case !r.sellsTerminal():
		phase = event.RebalanceSellsWorking
	case !reb.buysPlanned && len(reb.sells) > 0:
		phase = event.RebalanceBuysPending
	case len(working) > 0:
		phase = event.RebalanceBuysWorking
	}
	reb.held = phase == event.RebalanceSellsWorking || phase == event.RebalanceBuysPending || phase == event.RebalanceBuysWorking
	p.Rebalance = event.RecoveredRebalance{DecisionID: reb.decisionID, Phase: phase, Held: reb.held, Working: working}
}

// recoverBasis gives a recovered rebalance back what its buy phase is
// planned from, at the close of its decision session in the replayed
// history: the account snapshot and the raw closes the earlier run planned
// from. Replanning from them must give exactly the sells the journal
// records; if not, the history is not the one the decision was made on, and
// the run stops rather than plan buys from it. The budget is then the D5 cash
// of that snapshot with every recorded release and fill applied in order.
func (r *momentumRun) recoverBasis() error {
	reb, h := r.rebalance, r.h
	if r.snapshot == nil || r.snapshot.AsOf.Before(reb.session) {
		return fmt.Errorf("targetweight: the replayed history has no account snapshot as of %s, the session %s was decided on", reb.session.Format(idTime), reb.decisionID)
	}
	plan, err := orderplan.PlanIRA(orderplan.IRAInput{
		Tag: reb.decisionID, Decision: reb.decision, Snapshot: r.snapshot.AccountPositionsSnapshotPayload,
		ReferencePrices: r.raw, PriceBand: h.cfg.Limits.PriceBand, RebalanceBand: r.cfg.RebalanceBand,
	})
	if err != nil {
		return fmt.Errorf("targetweight: the replayed history cannot value %s: %w", reb.decisionID, err)
	}
	if !slices.Equal(plan.Sells, reb.recordedSells) {
		return fmt.Errorf("targetweight: the replayed history does not give the sells recorded for %s", reb.decisionID)
	}
	reb.snapshot, reb.prices = *r.snapshot, maps.Clone(r.raw)
	reb.buys, reb.sellsPlanned = plan.Buys, len(plan.Sells)
	reb.budget = orderplan.NewBudget(r.cash(*r.snapshot))
	for _, f := range reb.ledger {
		f(reb.budget)
	}
	reb.ledger = nil
	return nil
}

// budgetDo applies f to the rebalance's budget, or keeps it, in order, until
// a recovered rebalance has its budget back.
func (reb *rebalance) budgetDo(f func(*orderplan.Budget)) {
	if reb.budget != nil {
		f(reb.budget)
		return
	}
	reb.ledger = append(reb.ledger, f)
}

// applyCleared reads the owner's clearing of a held rebalance. Resume lets the
// decision time continue: its buy phase is planned now, caused by the
// clearing, when every sell is already terminal, or else once the last one
// is. Abandon sends nothing more for it. A clearing that names no held
// rebalance is refused. In the restart prelude it only replays the clearing:
// the buys it caused are in the records that follow.
func (r *momentumRun) applyCleared(in event.Envelope) ([]event.Envelope, error) {
	p, err := decodeAccount(r.h, in, event.RebalanceClearedSchemaVersion, func(p event.RebalanceClearedPayload) string { return p.AccountID })
	if err != nil {
		return nil, err
	}
	reb := r.rebalance
	if reb == nil || !reb.held || reb.decisionID != p.DecisionID {
		return nil, fmt.Errorf("targetweight: clearing %s names %s, which is not a held rebalance", in.ID, p.DecisionID)
	}
	pending := len(reb.sells) > 0 && r.sellsTerminal() && !reb.buysPlanned
	if p.Action == event.RebalanceAbandon {
		reb.held, reb.abandoned = false, true
		return nil, nil
	}
	if r.h.prelude > 0 {
		reb.held = false
		reb.buysPlanned = reb.buysPlanned || pending
		return nil, nil
	}
	if pending && reb.budget == nil {
		return nil, fmt.Errorf("targetweight: %s cannot resume: the replayed history never reached its decision session", reb.decisionID)
	}
	reb.held = false
	if !pending {
		return nil, nil
	}
	b := &emissions{h: r.h, in: in, at: in.EventTime}
	r.planBuys(b)
	return b.result()
}
