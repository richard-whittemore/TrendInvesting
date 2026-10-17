package risklimit

import (
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// A released sell is pending from its release until recorded broker facts
// resolve it, and every sizing that nets working sells nets it (ADR 0019,
// amended 2026-10-15, "Sized from the broker, net of every working sell",
// cancel-pending included; ADR 0026, points 2.5 and 4):
//
//	pending = released - filled - cancelled
//
// Filled is what fill reports executed, or more when a broker snapshot shows
// more: a sell it lists working at a remaining quantity has filled the rest
// of what was not cancelled, and a sell it no longer lists has filled what
// the fall in its holding shows. Cancelled is the remainder a canceled or
// invalid lifecycle report ends, or the rest of a sell a broker snapshot no
// longer lists. A cancel-pending sell may still fill, so it stays pending in
// full. An account or broker snapshot never clears a sell by itself: one
// that cannot read the open orders, or a dropped sell with no earlier
// holding to compare, resolves nothing, and the sell stays pending until a
// report does. So pending is never below zero and never above the release.

// releasedOrder is a released order and the shares fill reports executed.
type releasedOrder struct {
	planned  event.OrderPlannedPayload
	reported int64
}

// trackedSell is one released sell and what recorded broker facts show of it.
type trackedSell struct {
	instrumentID string
	released     int64
	// filled is the shares known executed and cancelled the remainder known
	// never to execute.
	filled, cancelled int64
	// atAccount and atBroker are filled as of the latest account and broker
	// snapshot: a share filled since is not in that snapshot's holding.
	atAccount, atBroker int64
	// watched is whether the latest broker snapshot listed the sell working,
	// or it was released since: only then does a snapshot that drops it,
	// compared with that one's holding, show what it filled.
	watched bool
}

func (s *trackedSell) pending() int64 { return s.released - s.filled - s.cancelled }

// notShown is the shares of the sell a snapshot taken when it had filled
// shown cannot show: those still pending and those filled since.
func (s *trackedSell) notShown(shown int64) int64 { return s.pending() + s.filled - shown }

// fill records that known shares are executed. A share filled after a
// cancellation was counted comes back from cancelled, so filled and
// cancelled together never pass the release.
func (s *trackedSell) fill(known int64) {
	s.filled = max(s.filled, known)
	s.cancelled = min(s.cancelled, s.released-s.filled)
}

// PendingSells returns the shares of instrumentID already being sold that
// the latest account snapshot cannot show, for a planner sizing a new sell
// from that snapshot: each released sell's pending shares and those it
// filled since that snapshot, and every other working or pending sell the
// latest broker snapshot reports, when it read its open orders.
func (g *Gate) PendingSells(instrumentID string) int64 {
	return g.unshownSells(instrumentID, func(s *trackedSell) int64 { return s.atAccount })
}

// brokerHolding returns the latest broker snapshot's holding of instrumentID
// and the shares already being sold that it cannot show, by the same rule as
// PendingSells, when the snapshot read both positions and open orders.
func (g *Gate) brokerHolding(instrumentID string) (holding, working int64, ok bool) {
	s := g.broker
	if s == nil || !s.Readable(event.BrokerComponentPositions) || !s.Readable(event.BrokerComponentOpenOrders) {
		return 0, 0, false
	}
	position, _ := s.Position(instrumentID)
	return position.Quantity, g.unshownSells(instrumentID, func(t *trackedSell) int64 { return t.atBroker }), true
}

// unshownSells is the one rule PendingSells and the broker-holding check
// share: for a snapshot whose view of each released sell's fills is shown,
// the sells of instrumentID it cannot show, ours and unknown.
func (g *Gate) unshownSells(instrumentID string, shown func(*trackedSell) int64) int64 {
	var total int64
	for _, id := range g.sellOrder {
		if s := g.sells[id]; s.instrumentID == instrumentID {
			total += s.notShown(shown(s))
		}
	}
	if b := g.broker; b != nil && b.Readable(event.BrokerComponentOpenOrders) {
		for _, o := range b.OpenOrders {
			if _, ours := g.sells[o.ClientOrderID]; !ours && o.InstrumentID == instrumentID && o.Side == event.OrderSideSell {
				total += o.RemainingQuantity
			}
		}
	}
	return total
}

// ObserveAccount records that an account snapshot was taken: its holding
// shows every share filled so far, and nothing still pending.
func (g *Gate) ObserveAccount() {
	for _, s := range g.sells {
		s.atAccount = s.filled
	}
}

// reportedOrder returns the released order a venue report names, checking
// that the report describes it.
func (g *Gate) reportedOrder(account, client, instrument, side string) (*releasedOrder, error) {
	o, ok := g.orders[client]
	switch {
	case account != g.accountID:
		return nil, fmt.Errorf("risklimit: report for account %q; this gate is %q", account, g.accountID)
	case !ok:
		return nil, fmt.Errorf("risklimit: report names order %s, which the gate never released", client)
	case o.planned.InstrumentID != instrument || o.planned.Side != side:
		return nil, fmt.Errorf("risklimit: report describes %s %s, but order %s is %s %s", side, instrument, client, o.planned.Side, o.planned.InstrumentID)
	}
	return o, nil
}

// ObserveFill applies a recorded execution of a released order. A
// redelivery of an execution already applied changes nothing.
func (g *Gate) ObserveFill(p event.TargetWeightFillPayload) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("risklimit: %w", err)
	}
	if seen, ok := g.fills[p.FillID]; ok {
		if seen.SameExecution(p) {
			return nil
		}
		return fmt.Errorf("risklimit: fill id %s is reused for a different execution", p.FillID)
	}
	o, err := g.reportedOrder(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side)
	if err != nil {
		return err
	}
	if o.reported+p.Quantity > o.planned.Quantity {
		return fmt.Errorf("risklimit: fill %s takes order %s past its %d shares", p.FillID, p.ClientOrderID, o.planned.Quantity)
	}
	o.reported += p.Quantity
	g.fills[p.FillID] = p
	if s := g.sells[p.ClientOrderID]; s != nil {
		s.fill(o.reported)
	}
	return nil
}

// ObserveLifecycle applies a recorded lifecycle report on a released order:
// canceled or invalid ends a sell's remainder; submitted and cancel-pending
// leave it pending.
func (g *Gate) ObserveLifecycle(p event.LimitOrderLifecyclePayload) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("risklimit: %w", err)
	}
	o, err := g.reportedOrder(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side)
	if err != nil {
		return err
	}
	if p.Quantity != o.planned.Quantity || p.LimitPrice != o.planned.LimitPrice {
		return fmt.Errorf("risklimit: a lifecycle report on order %s reports %d at %v; the order is %d at %v", p.ClientOrderID, p.Quantity, p.LimitPrice, o.planned.Quantity, o.planned.LimitPrice)
	}
	if s := g.sells[p.ClientOrderID]; s != nil && p.Terminal() {
		s.cancelled = s.released - s.filled
	}
	return nil
}

// resolve applies what the broker snapshot next, recorded after the latest
// one, shows of each released sell. A sell it lists working is pending at
// its remaining quantity. A watched sell it no longer lists is resolved
// when both snapshots read the holding: the fall in the holding that fills
// already shown do not explain is taken as filled, oldest sell first, so
// that no share it may have sold is sold again (ADR 0019, amended
// 2026-10-15, "never below it"), and the rest is cancelled. Otherwise the
// sell stays pending.
func (g *Gate) resolve(next event.BrokerSnapshotPayload) {
	readOrders := next.Readable(event.BrokerComponentOpenOrders)
	listed := map[string]bool{}
	if readOrders {
		for _, o := range next.OpenOrders {
			if s := g.sells[o.ClientOrderID]; s != nil && o.Side == event.OrderSideSell && o.InstrumentID == s.instrumentID {
				listed[o.ClientOrderID] = true
				s.fill(s.released - s.cancelled - o.RemainingQuantity)
			}
		}
	}
	unexplained := map[string]int64{}
	for _, id := range g.sellOrder {
		s := g.sells[id]
		if !readOrders || !s.watched || listed[id] || s.pending() == 0 {
			continue
		}
		left, ok := unexplained[s.instrumentID]
		if !ok {
			if left, ok = g.unexplainedFall(next, s.instrumentID); !ok {
				continue
			}
		}
		f := min(s.pending(), left)
		unexplained[s.instrumentID] = left - f
		s.fill(s.filled + f)
		s.cancelled = s.released - s.filled
	}
	for _, id := range g.sellOrder {
		s := g.sells[id]
		s.watched = listed[id] && s.pending() > 0
		s.atBroker = s.filled
	}
}

// unexplainedFall returns how far instrumentID's holding fell from the
// latest broker snapshot to next beyond what the fills shown since explain:
// those of released sells, and an unknown sell listed in both whose
// remaining quantity fell. It reports false when either snapshot could not
// read the holding.
func (g *Gate) unexplainedFall(next event.BrokerSnapshotPayload, instrumentID string) (int64, bool) {
	prev := g.broker
	if prev == nil || !prev.Readable(event.BrokerComponentPositions) || !next.Readable(event.BrokerComponentPositions) {
		return 0, false
	}
	before, _ := prev.Position(instrumentID)
	after, _ := next.Position(instrumentID)
	fall := before.Quantity - after.Quantity
	for _, id := range g.sellOrder {
		if s := g.sells[id]; s.instrumentID == instrumentID {
			fall -= s.filled - s.atBroker
		}
	}
	remaining := map[string]int64{}
	for _, o := range prev.OpenOrders {
		if _, ours := g.sells[o.ClientOrderID]; !ours && o.InstrumentID == instrumentID && o.Side == event.OrderSideSell {
			remaining[o.BrokerOrderID] = o.RemainingQuantity
		}
	}
	for _, o := range next.OpenOrders {
		if was, ok := remaining[o.BrokerOrderID]; ok && o.InstrumentID == instrumentID && o.Side == event.OrderSideSell && was > o.RemainingQuantity {
			fall -= was - o.RemainingQuantity
		}
	}
	return max(fall, 0), true
}
