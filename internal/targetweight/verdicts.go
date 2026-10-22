package targetweight

import (
	"fmt"
	"slices"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// BoostVerdicts is Boost's count of the venue's rejections in a row (ADR
// 0026, point 2, "Boost", amended 2026-10-18): the orders decided sessions
// released that the venue rejected in a row, whatever their targets, since
// the last one it accepted, each counted as the venue's first word on it is
// read, however late. At event.BoostVenueRejectionLimit no session buys
// until the owner starts a fresh run, which starts a new count, and the
// count stays there, never above it. The engine's handler counts with it,
// and so does the reporter, from a decision's recorded count and the
// venue's words journalled after it, so the two cannot disagree. It is
// pure: it holds no clock and reads no journal.
type BoostVerdicts struct {
	// rejections is the count (BoostFigures.VenueRejections).
	rejections int
	// inARow names the orders rejections counts, in the order read: at the
	// stop, the two that stopped buys (BoostFigures.RejectedInARow).
	inARow []event.BoostRejectedOrder
	// undecided are the orders decided sessions released that the venue has
	// not yet ruled on, in release order, so a late verdict on any of them
	// is counted (BoostFigures.UndecidedOrders). An order leaves it at the
	// venue's first word on it, when the owner settles it as never sent
	// (ClearNeverSent), or when it is retired as never sent (Retire); a
	// dry-run's orders, which no venue receives, never
	// join it.
	undecided []undecidedOrder
	// olderThanRejection counts the first of undecided, in release order,
	// that were released before the latest released of the orders inARow
	// names: an acceptance of one of them is older than the run of
	// rejections and does not end it (BoostFigures.UndecidedBeforeRejection).
	olderThanRejection int
	// whileStopped counts every rejection read since the stop
	// (BoostFigures.RejectionsWhileStopped).
	whileStopped int
}

// Verdict is what the venue's word on an order did to the count.
type Verdict int

const (
	// VerdictNone: not the first word on an order awaiting it, so nothing
	// changed.
	VerdictNone Verdict = iota
	// VerdictAccepted: the venue accepted the order; before the stop, the
	// count went back to none unless the order is older than the latest
	// rejection counted.
	VerdictAccepted
	// VerdictRejected: one more rejection in a row; Stopped reports whether
	// it was the one that stopped buys.
	VerdictRejected
	// VerdictRejectedWhileStopped: a rejection read once buys were stopped,
	// counted among the rejections while stopped.
	VerdictRejectedWhileStopped
)

// BoostLifecycleWord says whether a venue report of status is the venue's
// word on an order, and whether that word is a rejection: invalid is a
// rejection, any report but submitted shows the venue accepted the order,
// and submitted is not enough, as IBKR's PreSubmitted maps to it and can
// precede a rejection (ADR 0026, point 2, "Boost", amended 2026-10-18). An
// execution of the order, which the gate's fill rule accepts, is an
// acceptance too.
func BoostLifecycleWord(status string) (rejected, decides bool) {
	switch status {
	case event.OrderLifecycleStatusInvalid:
		return true, true
	case event.OrderLifecycleStatusSubmitted:
		return false, false
	default:
		return false, true
	}
}

// Recover takes the count a decision records as the count in force, with
// the orders it lists as awaiting the venue's word, so a count read from a
// decision goes on as the full run's does (ADR 0026, point 5: recovered,
// never recomputed). The watched order's venue order ID is the one recorded
// with it.
func (v *BoostVerdicts) Recover(b event.BoostFigures) {
	*v = BoostVerdicts{
		rejections: b.VenueRejections, inARow: slices.Clone(b.RejectedInARow),
		olderThanRejection: b.UndecidedBeforeRejection, whileStopped: b.RejectionsWhileStopped,
	}
	for _, r := range b.UndecidedOrders {
		o := undecidedOrder{id: r.ClientOrderID, venueID: r.VenueOrderID}
		if !b.Watched.IsZero() && r.ClientOrderID == b.Watched.ClientOrderID {
			o.venueID = b.Watched.VenueOrderID
		}
		v.undecided = append(v.undecided, o)
	}
}

// Release adds order id, which a decided session released to a venue, to
// the orders awaiting the venue's word, last in release order; an order
// already awaiting it stays where it is.
func (v *BoostVerdicts) Release(id string) {
	if slices.ContainsFunc(v.undecided, func(o undecidedOrder) bool { return o.id == id }) {
		return
	}
	v.undecided = append(v.undecided, undecidedOrder{id: id})
}

// LearnVenueID records venueID as the venue order ID of order id while it
// awaits the venue's word, once a venue report names it, so a rejection
// names the order as the venue does.
func (v *BoostVerdicts) LearnVenueID(id, venueID string) {
	for i := range v.undecided {
		if r := &v.undecided[i]; r.id == id && r.venueID == "" {
			r.venueID = venueID
		}
	}
}

// Observe reads the venue's first word on order id, a rejection or an
// acceptance, when a decided session released it and the venue had not yet
// ruled on it; any later word, and a word on any other order, changes
// nothing. Before the stop an acceptance of an order released after every
// rejection counted sets the count back to none; an acceptance of an older
// one, read late, does not, since the rejections came after it in release
// order, so a re-planned order rejected again is always the second in a
// row. A rejection counts one more, however late it is read. Once buys are
// stopped the count stays there and each rejection read is counted as one
// while stopped (ADR 0026, point 2, "Boost", amended 2026-10-18).
func (v *BoostVerdicts) Observe(id string, rejected bool) Verdict {
	_, verdict := v.observe(id, rejected)
	return verdict
}

// observe is Observe, also returning the order as it awaited the word.
func (v *BoostVerdicts) observe(id string, rejected bool) (undecidedOrder, Verdict) {
	i := slices.IndexFunc(v.undecided, func(o undecidedOrder) bool { return o.id == id })
	if i < 0 {
		return undecidedOrder{}, VerdictNone
	}
	o := v.undecided[i]
	older := v.leave(i)
	stopped := v.Stopped()
	if !rejected {
		if !stopped && !older {
			v.rejections, v.inARow, v.olderThanRejection = 0, nil, 0
		}
		return o, VerdictAccepted
	}
	if stopped {
		v.whileStopped++
		return o, VerdictRejectedWhileStopped
	}
	v.rejections++
	v.inARow = append(v.inARow, event.BoostRejectedOrder{ClientOrderID: id, VenueOrderID: o.venueID})
	if !older {
		// The latest released rejection counted: every order still
		// undecided before it in release order is older.
		v.olderThanRejection = i
	}
	return o, VerdictRejected
}

// leave takes the i-th undecided order off the list and reports whether it
// was released before the latest released rejection counted
// (olderThanRejection), which then counts one fewer.
func (v *BoostVerdicts) leave(i int) bool {
	v.undecided = slices.Delete(v.undecided, i, i+1)
	older := i < v.olderThanRejection
	if older {
		v.olderThanRejection--
	}
	return older
}

// ClearNeverSent applies the owner's settlement p of Boost orders the
// adapter never sent (operator.rebalance.cleared, journalled as input
// inputID): no venue received them, so none will ever rule on them, and
// they leave the orders awaiting the venue's word, counted neither accepted
// nor rejected (ADR 0026, point 2, "Boost", amended 2026-10-18). Each must
// be an order decision p.DecisionID released, of instrument instrumentID on
// the side sideOf names, still awaiting the venue's word and never named by
// the venue, else the whole settlement is refused and nothing is cleared, so
// a clearance never means more than the owner said (fail closed), and an
// order named twice is refused too: an order
// the venue has named, even only as submitted, was sent.
func (v *BoostVerdicts) ClearNeverSent(inputID string, p event.RebalanceClearedPayload, instrumentID string, sideOf func(id string) string) error {
	for n, id := range p.ClientOrderIDs {
		if slices.Contains(p.ClientOrderIDs[:n], id) {
			return fmt.Errorf("targetweight: clearance %s names order %s twice; a settlement names each order once", inputID, id)
		}
		i := slices.IndexFunc(v.undecided, func(o undecidedOrder) bool { return o.id == id })
		switch {
		case i < 0:
			return fmt.Errorf("targetweight: clearance %s names order %s, which is not an order this run released that the venue has not ruled on; Boost clears only an order the adapter never sent", inputID, id)
		case v.undecided[i].venueID != "":
			return fmt.Errorf("targetweight: clearance %s names order %s, which the venue has already named as order %s; it was sent, so it is not an order the adapter never sent, and Boost clears only an order the adapter never sent", inputID, id, v.undecided[i].venueID)
		}
		if orderplan.ClientOrderID(p.DecisionID, instrumentID, sideOf(id)) != id {
			return fmt.Errorf("targetweight: clearance %s names order %s, which decision %s did not release", inputID, id, p.DecisionID)
		}
	}
	for _, id := range p.ClientOrderIDs {
		v.leave(slices.IndexFunc(v.undecided, func(o undecidedOrder) bool { return o.id == id }))
	}
	return nil
}

// Holds reports whether order id is among the orders awaiting the venue's
// word, so its retirement is this count's to read (Retire).
func (v *BoostVerdicts) Holds(id string) bool {
	return slices.ContainsFunc(v.undecided, func(o undecidedOrder) bool { return o.id == id })
}

// Retire takes order id off the orders awaiting the venue's word when it is
// retired as never sent (execution.order.retired, journalled as input
// inputID): its session ended before the adapter could send it, or the owner
// retired one the adapter can never send, so no venue will ever rule on it,
// and it is counted neither accepted nor rejected (ADR 0026, point 2,
// "Boost", amended 2026-10-22). It applies only to an order the count holds
// (Holds), which must never have been named by the venue, else it is
// refused and nothing changes (fail closed): an order the venue has named,
// even only as submitted, was sent. An order the count does not hold, the
// sale back to 98% among them, is retired by the caller's own rule.
func (v *BoostVerdicts) Retire(inputID, id string) error {
	i, err := v.retirable(inputID, id)
	if err != nil {
		return err
	}
	v.leave(i)
	return nil
}

// retirable is Retire's rule, changing nothing: the index of order id among
// the orders awaiting the venue's word, or the refusal.
func (v *BoostVerdicts) retirable(inputID, id string) (int, error) {
	i := slices.IndexFunc(v.undecided, func(o undecidedOrder) bool { return o.id == id })
	switch {
	case i < 0:
		return 0, fmt.Errorf("targetweight: retirement %s names order %s, which is not among the orders awaiting the venue's word", inputID, id)
	case v.undecided[i].venueID != "":
		return 0, fmt.Errorf("targetweight: retirement %s names order %s, which the venue has already named as order %s; it was sent, and only an order no venue received is retired", inputID, id, v.undecided[i].venueID)
	}
	return i, nil
}

// RetirementState is what a reader of Boost's orders holds of those a
// retirement as never sent (execution.order.retired) may name, for
// RetirementVerdict: the run's account, whether it is a dry-run, each
// order's first release, the orders the owner's clearance the engine
// applied settled (Cleared), those whose retirement it applied (Retired),
// those a venue named in a report, an execution or a broker listing the
// engine read (Named), and the Boost count (Verdicts). A nil func holds no
// order, and a nil Verdicts holds none awaiting the venue's word.
type RetirementState struct {
	AccountID string
	DryRun    bool
	Released  func(id string) (event.OrderReleasedPayload, bool)
	Cleared   func(id string) bool
	Retired   func(id string) bool
	Named     func(id string) bool
	Verdicts  *BoostVerdicts
}

// has reports whether set holds id; a nil set holds nothing.
func has(set func(string) bool, id string) bool { return set != nil && set(id) }

// RetirementVerdict is the one rule by which Boost applies retirement p of
// an order the adapter never sent, journalled as input inputID (the owner's
// decision of 2026-10-22; ADR 0026, point 2, "Boost", amended 2026-10-22):
// the engine (applyRetired), the reporter and the restart reading each call
// it, so none re-implements it (AGENTS.md). It must be for the run's
// account, in a run that sends orders (a dry-run, or an order a dry-run
// released, has nothing to retire), of an order released as p describes it
// (event's Describes), neither cleared nor retired before. An order the
// Boost count holds as awaiting the venue's word must pass that count's
// rule (BoostVerdicts.Retire) and is reported held, for the caller to take
// off the count; any other, the sale back to 98% among them, may never have
// been named by a venue. Anything else is refused, so a retirement never
// ends an order that may be working (fail closed, AGENTS.md). It is pure:
// it changes nothing.
func RetirementVerdict(s RetirementState, inputID string, p event.OrderRetiredPayload) (held bool, err error) {
	id := p.ClientOrderID
	dryRun := fmt.Errorf("targetweight: retirement %s names order %s in a dry-run, which sends no order and ends each it releases with its own synthetic cancel, so it has nothing to retire: find what fed it before going on", inputID, id)
	if s.DryRun {
		return false, dryRun
	}
	if p.AccountID != s.AccountID {
		return false, fmt.Errorf("targetweight: retirement %s is for account %q; this run decides %q", inputID, p.AccountID, s.AccountID)
	}
	var o event.OrderReleasedPayload
	ok := false
	if s.Released != nil {
		o, ok = s.Released(id)
	}
	switch {
	case !ok:
		return false, fmt.Errorf("targetweight: retirement %s names order %s, which this run did not release", inputID, id)
	case o.Mode == event.TradingModeDryRun:
		return false, dryRun
	case has(s.Cleared, id):
		return false, fmt.Errorf("targetweight: retirement %s names order %s, which the owner cleared as never sent already", inputID, id)
	case has(s.Retired, id):
		return false, fmt.Errorf("targetweight: retirement %s names order %s, which is retired already", inputID, id)
	case !p.Describes(o):
		return false, fmt.Errorf("targetweight: retirement %s retires %s %s %d %s at %v; order %s was released as %s %s %d %s at %v",
			inputID, p.AccountID, p.Side, p.Quantity, p.InstrumentID, p.LimitPrice, id, o.AccountID, o.Side, o.Quantity, o.InstrumentID, o.LimitPrice)
	}
	if s.Verdicts != nil && s.Verdicts.Holds(id) {
		if _, err := s.Verdicts.retirable(inputID, id); err != nil {
			return false, err
		}
		return true, nil
	}
	if has(s.Named, id) {
		return false, fmt.Errorf("targetweight: retirement %s names order %s, which a venue has named or reported executed; it was sent, and only an order no venue received is retired", inputID, id)
	}
	return false, nil
}

// Rejections is the count in force.
func (v *BoostVerdicts) Rejections() int { return v.rejections }

// Stopped reports whether the count stops Boost's buys.
func (v *BoostVerdicts) Stopped() bool { return v.rejections >= maxVenueRejections }

// InARow names the orders the count counts, in the order read.
func (v *BoostVerdicts) InARow() []event.BoostRejectedOrder { return slices.Clone(v.inARow) }

// WhileStopped counts the rejections read since buys were stopped.
func (v *BoostVerdicts) WhileStopped() int { return v.whileStopped }
