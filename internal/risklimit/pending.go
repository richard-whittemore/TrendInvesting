package risklimit

import (
	"fmt"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// A released sell is pending from its release until recorded broker facts
// resolve it, and every sizing that nets working sells nets it (ADR 0019,
// amended 2026-10-15, "Sized from the broker, net of every working sell",
// cancel-pending included; ADR 0026, points 2.5 and 4):
//
//	pending = released - filled, or 0 once a lifecycle report ends it
//
// Filled is the most that any recorded fact shows executed: the fill
// reports; a broker snapshot that lists the sell working at a remaining
// quantity, which has filled the rest; and a fall in the holding that the
// fills already known do not explain, seen by an account or a broker
// snapshot against the snapshot that read the holding before it, which is
// taken as filled by the pending sells, oldest first. Only a canceled or
// invalid lifecycle report ends what has not filled. A snapshot that no
// longer lists a sell never shows it cancelled: it may not have reached
// IBKR yet, and ADR 0019 treats an unexplained cancellation as a
// discrepancy, never an inference. A cancel-pending sell may still fill, so
// it stays pending. So pending is never below zero and never above the
// release.
//
// Every snapshot's holding shows the shares filled when it was taken
// (atAccount, atBroker); a fill learned later that a snapshot's own holding
// fall already showed is the same fill, so it is counted once.

// releasedOrder is a released order and the shares fill reports executed.
type releasedOrder struct {
	planned  event.OrderPlannedPayload
	reported int64
}

// trackedSell is one released sell and what recorded broker facts show of it.
type trackedSell struct {
	instrumentID string
	released     int64
	// reported is what fill reports executed, and inferred what snapshots
	// show filled; ended is set by a canceled or invalid lifecycle report.
	reported, inferred int64
	ended              bool
	// atAccount, atBroker and atHolding are filled as of the latest account
	// snapshot, broker snapshot and snapshot of either kind that read the
	// holding: a share filled since is not in that snapshot's holding.
	atAccount, atBroker, atHolding int64
}

func (s *trackedSell) filled() int64 { return max(s.reported, s.inferred) }

func (s *trackedSell) pending() int64 {
	if s.ended {
		return 0
	}
	return s.released - s.filled()
}

// notShown is the shares of the sell a snapshot taken when it had filled
// shown cannot show: those still pending and those filled since.
func (s *trackedSell) notShown(shown int64) int64 { return s.pending() + s.filled() - shown }

// infer records that snapshots show filled shares executed. A listing
// replaces an earlier inference, so the views of earlier snapshots are kept
// within what is now known filled.
func (s *trackedSell) infer(filled int64) {
	s.inferred = min(max(filled, 0), s.released)
	f := s.filled()
	s.atAccount, s.atBroker, s.atHolding = min(s.atAccount, f), min(s.atBroker, f), min(s.atHolding, f)
}

// holdingSeen is the latest snapshot, account or broker, that read the
// holdings: when it was taken and what it held.
type holdingSeen struct {
	asOf     time.Time
	quantity map[string]int64
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

// ObserveAccount records account snapshot p: a fall in its holding that the
// fills already known do not explain is taken as filled by the pending sells,
// net first of every unknown sell the latest broker snapshot lists (which may
// be what sold), and its holding then shows every share known filled.
func (g *Gate) ObserveAccount(p event.AccountPositionsSnapshotPayload) {
	held := map[string]int64{}
	for _, position := range p.Positions {
		held[position.InstrumentID] = position.Quantity
	}
	if g.holding == nil || !p.AsOf.Before(g.holding.asOf) {
		for _, instrument := range g.sellInstruments() {
			fall := g.knownFall(instrument, held[instrument])
			if b := g.broker; b != nil && b.Readable(event.BrokerComponentOpenOrders) {
				for _, o := range b.OpenOrders {
					if _, ours := g.sells[o.ClientOrderID]; !ours && o.InstrumentID == instrument && o.Side == event.OrderSideSell {
						fall -= o.RemainingQuantity
					}
				}
			}
			g.takeFall(instrument, fall, nil)
		}
		g.seeHolding(p.AsOf, held)
	}
	for _, s := range g.sells {
		s.atAccount = s.filled()
	}
}

// sellInstruments returns the instruments of the released sells, in release
// order.
func (g *Gate) sellInstruments() []string {
	var out []string
	for _, id := range g.sellOrder {
		if i := g.sells[id].instrumentID; !slices.Contains(out, i) {
			out = append(out, i)
		}
	}
	return out
}

// knownFall returns how far instrumentID's holding fell from the latest
// snapshot that read it to now, beyond the fills learned since; 0 when no
// earlier snapshot read the holding.
func (g *Gate) knownFall(instrumentID string, now int64) int64 {
	if g.holding == nil {
		return 0
	}
	fall := g.holding.quantity[instrumentID] - now
	for _, id := range g.sellOrder {
		if s := g.sells[id]; s.instrumentID == instrumentID {
			fall -= s.filled() - s.atHolding
		}
	}
	return fall
}

// takeFall takes fall as filled by instrumentID's pending sells, oldest
// first, skipping those skip names (a listing already shows them), so that
// no share they may have sold is sold again (ADR 0019, amended 2026-10-15,
// "never below it").
func (g *Gate) takeFall(instrumentID string, fall int64, skip map[string]bool) {
	for _, id := range g.sellOrder {
		s := g.sells[id]
		if fall <= 0 {
			return
		}
		if s.instrumentID != instrumentID || skip[id] || s.pending() == 0 {
			continue
		}
		f := min(s.pending(), fall)
		fall -= f
		s.infer(s.filled() + f)
	}
}

// seeHolding makes held, taken at asOf, the holding later falls are measured
// from.
func (g *Gate) seeHolding(asOf time.Time, held map[string]int64) {
	g.holding = &holdingSeen{asOf: asOf, quantity: held}
	for _, s := range g.sells {
		s.atHolding = s.filled()
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
		s.reported = o.reported
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
		s.ended = true
	}
	return nil
}

// resolve applies what the broker snapshot next, recorded after the latest
// one, shows of each released sell. A sell it lists working has filled all
// but its remaining quantity. A fall in its holding that the fills known,
// those it lists included, and an unknown sell's own fills do not explain is
// taken as filled by the pending sells it does not list, oldest first. A
// sell it does not list stays pending otherwise. A snapshot that cannot read
// the open orders resolves nothing.
func (g *Gate) resolve(next event.BrokerSnapshotPayload) {
	if !next.Readable(event.BrokerComponentOpenOrders) {
		g.brokerSeen(next)
		return
	}
	listed := map[string]bool{}
	for _, o := range next.OpenOrders {
		if s := g.sells[o.ClientOrderID]; s != nil && o.Side == event.OrderSideSell && o.InstrumentID == s.instrumentID {
			listed[o.ClientOrderID] = true
			s.infer(s.released - o.RemainingQuantity)
		}
	}
	if next.Readable(event.BrokerComponentPositions) && (g.holding == nil || !next.AsOf.Before(g.holding.asOf)) {
		for _, instrument := range g.sellInstruments() {
			after, _ := next.Position(instrument)
			g.takeFall(instrument, g.knownFall(instrument, after.Quantity)-g.unknownFills(next, instrument), listed)
		}
	}
	g.brokerSeen(next)
}

// brokerSeen records that broker snapshot next shows every share known
// filled, and, when it read the holdings and is not older than the latest
// that did, makes its holding the one later falls are measured from.
func (g *Gate) brokerSeen(next event.BrokerSnapshotPayload) {
	if next.Readable(event.BrokerComponentPositions) && (g.holding == nil || !next.AsOf.Before(g.holding.asOf)) {
		held := map[string]int64{}
		for _, p := range next.Positions {
			held[p.InstrumentID] = p.Quantity
		}
		g.seeHolding(next.AsOf, held)
	}
	for _, s := range g.sells {
		s.atBroker = s.filled()
	}
}

// unknownFills returns the shares of instrumentID that unknown sells listed
// in both the latest broker snapshot and next sold between them, by the fall
// in their remaining quantity.
func (g *Gate) unknownFills(next event.BrokerSnapshotPayload, instrumentID string) int64 {
	prev := g.broker
	if prev == nil || !prev.Readable(event.BrokerComponentOpenOrders) {
		return 0
	}
	remaining := map[string]int64{}
	for _, o := range prev.OpenOrders {
		if _, ours := g.sells[o.ClientOrderID]; !ours && o.InstrumentID == instrumentID && o.Side == event.OrderSideSell {
			remaining[o.BrokerOrderID] = o.RemainingQuantity
		}
	}
	var sold int64
	for _, o := range next.OpenOrders {
		if was, ok := remaining[o.BrokerOrderID]; ok && o.InstrumentID == instrumentID && o.Side == event.OrderSideSell && was > o.RemainingQuantity {
			sold += was - o.RemainingQuantity
		}
	}
	return sold
}
