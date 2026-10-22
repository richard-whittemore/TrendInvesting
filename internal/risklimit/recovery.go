package risklimit

import (
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// RestoredOrder is an order an earlier run released that a recovered
// decision restores as released (RestoreReleased): the order, the
// executions its fill reports stated before the restart, when the venue
// had ended it the time of that end (zero otherwise), and its venue order
// ID when the decision recorded one (empty otherwise).
type RestoredOrder struct {
	Order        event.OrderReleasedPayload
	Fills        []event.TargetWeightFillPayload
	Ended        time.Time
	VenueOrderID string
}

// RestoredSaleAttempt is an incident's sale attempt as RestoreSaleAttempt
// restores it: whether an earlier run attempted it, and how many of its
// attempts earlier runs retired as never sent.
type RestoredSaleAttempt struct {
	IncidentID string
	Attempted  bool
	Retired    int
}

// RetiredSaleAttempt is an attempt of a sale back to 98% retired as never
// sent, as RestoreRetiredSale restores it: the attempt numbered Attempt (the
// first is 1) of the sale for Tag and InstrumentID.
type RetiredSaleAttempt struct {
	Tag          string
	InstrumentID string
	Attempt      int
}

// RestoredOwnSale is the run's own live sale back to 98% as RestoreOwnSale
// restores it: the order released, with its executions and venue order ID
// and no end, what fired it, and the attempt it is, the first being 1
// (0 for a decision recorded at version 7, which states none).
type RestoredOwnSale struct {
	RestoredOrder
	Trigger string
	Attempt int
}

// Recovery is everything a recovered decision restores into the gate, in
// the order restored: the orders released before the sale (Released), the
// incident's sale attempt, the sale attempts retired as never sent, the
// run's own live sale, and then the working orders (Working). Release order
// is the order of restoration, so a list restored latest first is given
// latest first.
type Recovery struct {
	Released     []RestoredOrder
	SaleAttempt  *RestoredSaleAttempt
	RetiredSales []RetiredSaleAttempt
	OwnSale      *RestoredOwnSale
	Working      []RestoredOrder
}

// RestoreRecovery restores r, all of it or none of it: every part is
// checked, against the gate as it stands and every part restored before it,
// before anything is restored, so a refusal of any part leaves the gate as
// it was and a recovered decision is never half applied (ADR 0026, point 5:
// recovered, never recomputed; fail closed, AGENTS.md). Each part is
// checked by the rule its own restore states: RestoreReleased,
// RestoreSaleAttempt, RestoreRetiredSale and RestoreOwnSale.
func (g *Gate) RestoreRecovery(r Recovery) error {
	if err := g.checkRecovery(r); err != nil {
		return err
	}
	// Every part was checked above against the gate and the parts before
	// it, so each applies.
	for _, o := range r.Released {
		g.applyReleased(o)
	}
	if a := r.SaleAttempt; a != nil {
		g.attempts[a.IncidentID] = &saleAttempt{earlier: a.Attempted, retired: a.Retired, instruments: make(map[string]bool)}
		g.restored = a.IncidentID
	}
	for _, s := range r.RetiredSales {
		key := saleKey(s.Tag, s.InstrumentID)
		g.retiredSales[key] = max(g.retiredSales[key], s.Attempt-g.restoredRetired(s.Tag))
	}
	if s := r.OwnSale; s != nil {
		g.applyReleased(s.RestoredOrder)
		g.orders[s.Order.ClientOrderID].planned.Sale = event.SaleFigures{Kind: event.SaleKindIncident, Trigger: s.Trigger}
		g.recordAttempt(s.Order.Tag, s.Order.InstrumentID)
	}
	for _, o := range r.Working {
		g.applyReleased(o)
	}
	return nil
}

// recoveryStage is what a Recovery's parts checked so far would add to the
// gate: the client order IDs, the sells' venue order IDs and the fill IDs
// restored, the incident's sale attempt, and the retired sale attempts, by
// saleKey.
type recoveryStage struct {
	orders, venues, fills map[string]bool
	attempt               *RestoredSaleAttempt
	retiredSales          map[string]int
}

// restoredRetired is restoredRetired as the gate would hold it with the
// stage applied.
func (st *recoveryStage) restoredRetired(g *Gate, tag string) int {
	if a := st.attempt; a != nil && a.IncidentID == tag {
		return a.Retired
	}
	return g.restoredRetired(tag)
}

// retired is how many of the sale's attempts for tag and instrumentID the
// gate, with the stage applied, holds as retired in this run's records.
func (st *recoveryStage) retired(g *Gate, tag, instrumentID string) int {
	key := saleKey(tag, instrumentID)
	if n, ok := st.retiredSales[key]; ok {
		return n
	}
	return g.retiredSales[key]
}

// checkRecovery checks every part of r in the order RestoreRecovery
// restores them, each against the gate with the parts before it staged.
func (g *Gate) checkRecovery(r Recovery) error {
	st := &recoveryStage{orders: map[string]bool{}, venues: map[string]bool{}, fills: map[string]bool{}, retiredSales: map[string]int{}}
	for _, o := range r.Released {
		if err := g.checkReleased(o, st); err != nil {
			return err
		}
	}
	if a := r.SaleAttempt; a != nil {
		if err := g.checkSaleAttempt(*a); err != nil {
			return err
		}
		st.attempt = a
	}
	for _, s := range r.RetiredSales {
		if s.Tag == "" || s.InstrumentID == "" || s.Attempt < 1 {
			return fmt.Errorf("risklimit: a restored retired sale names its tag, its instrument and its attempt, the first being 1; got %q, %q, %d", s.Tag, s.InstrumentID, s.Attempt)
		}
		st.retiredSales[saleKey(s.Tag, s.InstrumentID)] = max(st.retired(g, s.Tag, s.InstrumentID), s.Attempt-st.restoredRetired(g, s.Tag))
	}
	if s := r.OwnSale; s != nil {
		if err := g.checkOwnSale(*s, st); err != nil {
			return err
		}
	}
	for _, o := range r.Working {
		if err := g.checkReleased(o, st); err != nil {
			return err
		}
	}
	return nil
}

// checkSaleAttempt is RestoreSaleAttempt's check.
func (g *Gate) checkSaleAttempt(a RestoredSaleAttempt) error {
	switch {
	case a.IncidentID == "":
		return errors.New("risklimit: a restored sale attempt must name its incident")
	case a.Retired < 0:
		return fmt.Errorf("risklimit: incident %q's restored retired sale attempts %d are fewer than none", a.IncidentID, a.Retired)
	}
	if _, ok := g.attempts[a.IncidentID]; ok {
		return fmt.Errorf("risklimit: incident %q's sale attempt is restored once, before its sale is attempted", a.IncidentID)
	}
	return nil
}

// checkOwnSale is RestoreOwnSale's check, against the gate with st applied.
func (g *Gate) checkOwnSale(s RestoredOwnSale, st *recoveryStage) error {
	o := s.Order
	restored, earlier, attempted := g.restored, false, false
	if a := g.attempts[o.Tag]; a != nil {
		earlier, attempted = a.earlier, a.instruments[o.InstrumentID]
	}
	known := g.attempts[o.Tag] != nil
	if a := st.attempt; a != nil {
		restored = a.IncidentID
		if a.IncidentID == o.Tag {
			known, earlier, attempted = true, a.Attempted, false
		}
	}
	switch {
	case o.Tag == "" || o.Tag != restored || !known || earlier:
		return fmt.Errorf("risklimit: restored sale %s is under %q, not the incident whose attempt was restored as not spent by an earlier run", o.ClientOrderID, o.Tag)
	case attempted:
		return fmt.Errorf("risklimit: incident %q's sale of %s is attempted already; its order %s is restored once", o.Tag, o.InstrumentID, o.ClientOrderID)
	case o.Side != event.OrderSideSell:
		return fmt.Errorf("risklimit: restored sale %s is a %s; a sale back to 98%% is a sell", o.ClientOrderID, o.Side)
	case s.Trigger != event.SaleTriggerExit && s.Trigger != event.SaleTriggerMarginUrgent:
		return fmt.Errorf("risklimit: restored sale %s was fired by %q, not exit or margin-urgent", o.ClientOrderID, s.Trigger)
	}
	next := st.restoredRetired(g, o.Tag) + st.retired(g, o.Tag, o.InstrumentID)
	if want := orderplan.SaleAttemptClientOrderID(o.Tag, o.InstrumentID, next); o.ClientOrderID != want {
		return fmt.Errorf("risklimit: restored sale %s is not the next attempt of incident %q's sale, whose client order ID is %s", o.ClientOrderID, o.Tag, want)
	}
	if s.Attempt != 0 && s.Attempt != next+1 {
		return fmt.Errorf("risklimit: restored sale %s is recorded as attempt %d of incident %q's sale; the next attempt is %d", o.ClientOrderID, s.Attempt, o.Tag, next+1)
	}
	return g.checkReleased(s.RestoredOrder, st)
}

// checkReleased is RestoreReleased's check of r, against the gate with st
// applied; it stages r on success.
func (g *Gate) checkReleased(r RestoredOrder, st *recoveryStage) error {
	o := r.Order
	switch known := g.orders[o.ClientOrderID] != nil || st.orders[o.ClientOrderID]; {
	case o.AccountID != g.accountID:
		return fmt.Errorf("risklimit: restored order %s is for account %q; this gate is %q", o.ClientOrderID, o.AccountID, g.accountID)
	case known || o.ClientOrderID == "":
		return fmt.Errorf("risklimit: order %q is already known, or unnamed, and cannot be restored", o.ClientOrderID)
	case o.Quantity <= 0:
		return fmt.Errorf("risklimit: restored order %s releases %d shares", o.ClientOrderID, o.Quantity)
	case r.VenueOrderID != "" && o.Side == event.OrderSideSell && (g.ledger.venueTaken(r.VenueOrderID) || st.venues[r.VenueOrderID]):
		return fmt.Errorf("risklimit: restored order %s names venue order %s, which another sell carries; a venue order ID names one order", o.ClientOrderID, r.VenueOrderID)
	}
	var filled int64
	seen := map[string]bool{}
	for _, f := range r.Fills {
		switch err := f.Validate(); {
		case err != nil:
			return fmt.Errorf("risklimit: restored order %s: %w", o.ClientOrderID, err)
		case f.AccountID != o.AccountID || f.ClientOrderID != o.ClientOrderID || f.InstrumentID != o.InstrumentID || f.Side != o.Side:
			return fmt.Errorf("risklimit: restored order %s is %s %s; fill %s describes %s %s of order %s", o.ClientOrderID, o.Side, o.InstrumentID, f.FillID, f.Side, f.InstrumentID, f.ClientOrderID)
		case seen[f.FillID]:
			return fmt.Errorf("risklimit: restored order %s states fill id %s twice", o.ClientOrderID, f.FillID)
		}
		if _, applied := g.fills[f.FillID]; applied || st.fills[f.FillID] {
			return fmt.Errorf("risklimit: restored fill id %s is already applied to another order", f.FillID)
		}
		// Checked against the shares released before it is added, so the
		// sum stays below them and can never wrap (fail closed, AGENTS.md).
		if f.Quantity >= o.Quantity-filled {
			return fmt.Errorf("risklimit: restored order %s has %d shares filled before fill %s of %d; a watched order has fewer filled than its %d released", o.ClientOrderID, filled, f.FillID, f.Quantity, o.Quantity)
		}
		seen[f.FillID] = true
		filled += f.Quantity
	}
	st.orders[o.ClientOrderID] = true
	if r.VenueOrderID != "" && o.Side == event.OrderSideSell {
		st.venues[r.VenueOrderID] = true
	}
	for id := range seen {
		st.fills[id] = true
	}
	return nil
}

// applyReleased restores r, which checkReleased accepted, as released.
func (g *Gate) applyReleased(r RestoredOrder) {
	o := r.Order
	g.released[o.ClientOrderID] = true
	g.orders[o.ClientOrderID] = &releasedOrder{
		planned: event.OrderPlannedPayload{
			AccountID: o.AccountID, Tag: o.Tag, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID,
			Side: o.Side, Quantity: o.Quantity, LimitPrice: o.LimitPrice,
		},
	}
	if o.Side == event.OrderSideSell {
		s := g.ledger.restore(o.ClientOrderID, o.InstrumentID, o.Quantity, r.VenueOrderID)
		if !r.Ended.IsZero() {
			s.ended, s.end = true, r.Ended.Truncate(time.Second)
		}
	}
	for _, f := range r.Fills {
		// Checked against the order and its shares, so it applies.
		_ = g.ObserveFill(f)
	}
}
