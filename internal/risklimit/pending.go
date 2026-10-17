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
// What is known of each sell's fills is the most that its own facts show:
// its fill reports, and a broker snapshot that lists it working at a
// remaining quantity, which has filled the rest. A fall in an instrument's
// holding that the fills already known do not explain, seen by an account or
// a broker snapshot against the snapshot that read the holding before it, is
// taken as filled by the instrument's pending sells. It is a floor on what
// they filled together, not an addition to any one sell's reports: a fill
// report on any sell of the instrument counts toward it, so the same shares
// are never counted filled twice. A listing in a snapshot not older than the
// latest holding seen shows its sells exactly, so the floor then holds only
// what the sells it does not list can have filled. Only a canceled or invalid
// lifecycle report ends what has not filled. A snapshot that no longer lists
// a sell never shows it cancelled: it may not have reached IBKR yet, and ADR
// 0019 treats an unexplained cancellation as a discrepancy, never an
// inference. A cancel-pending sell may still fill, so it stays pending. So
// pending is never below zero and never above the release.
//
// Every snapshot's holding shows the shares filled when it was taken, and
// the gate records, per instrument, how many it knew filled when it applied
// the latest account snapshot, broker snapshot and snapshot of either kind
// that read the holding. A fill learned later that a snapshot's own holding
// fall already showed is the same fill, so it is counted once. A snapshot
// older than the latest holding seen, applied after it because account and
// broker snapshots are ordered only within their own stream, shows nothing
// newer: it advances none of these, so a fill a newer snapshot showed is
// never taken as already in an older snapshot's holding.

// releasedOrder is a released order and the shares fill reports executed.
type releasedOrder struct {
	planned  event.OrderPlannedPayload
	reported int64
}

// trackedSell is one released sell and what its own recorded facts show.
type trackedSell struct {
	instrumentID string
	released     int64
	// reported is what fill reports executed, listed the most a broker
	// listing showed filled, and frozen what was counted filled when a
	// canceled or invalid lifecycle report ended it.
	reported, listed, frozen int64
	ended                    bool
}

// known is the most the sell's own facts show filled.
func (s *trackedSell) known() int64 { return max(s.reported, s.listed, s.frozen) }

// sellBook is what snapshots show of one instrument's released sells
// together.
type sellBook struct {
	// floor is the fewest shares the holding's falls show the sells filled.
	floor int64
	// atAccount, atBroker and atHolding are the shares known filled when the
	// latest account snapshot, broker snapshot and snapshot of either kind
	// that read the holding were applied: a share filled since is not in
	// that snapshot's holding.
	atAccount, atBroker, atHolding int64
}

// holdingSeen is the latest snapshot, account or broker, that read the
// holdings: when it was taken and what it held.
type holdingSeen struct {
	asOf     time.Time
	quantity map[string]int64
}

// sellsOf returns instrumentID's released sells, in release order.
func (g *Gate) sellsOf(instrumentID string) []*trackedSell {
	var out []*trackedSell
	for _, id := range g.sellOrder {
		if s := g.sells[id]; s.instrumentID == instrumentID {
			out = append(out, s)
		}
	}
	return out
}

// book returns instrumentID's sell book, creating it empty.
func (g *Gate) book(instrumentID string) *sellBook {
	b, ok := g.books[instrumentID]
	if !ok {
		b = &sellBook{}
		g.books[instrumentID] = b
	}
	return b
}

// shares returns what is known of instrumentID's released sells together:
// the shares filled, the most of the floor the open sells can hold, and the
// shares still pending. extra, the floor beyond what the sells' own facts
// show, is spread over the open sells oldest first, each up to its unfilled
// shares (spread, which also reports each sell's part).
func (g *Gate) shares(instrumentID string) (filled, pending int64) {
	known, extra, open := g.spread(instrumentID, nil)
	return known + extra, open - extra
}

// spread returns the shares the sells' own facts show filled, the part of
// the floor beyond them that the open sells hold, and the open sells'
// unfilled shares; part, when set, receives each open sell's part of it.
func (g *Gate) spread(instrumentID string, part func(*trackedSell, int64)) (known, extra, open int64) {
	sells := g.sellsOf(instrumentID)
	for _, s := range sells {
		known += s.known()
		if !s.ended {
			open += s.released - s.known()
		}
	}
	left := min(max(g.book(instrumentID).floor-known, 0), open)
	extra = left
	for _, s := range sells {
		if s.ended || left == 0 {
			continue
		}
		take := min(left, s.released-s.known())
		left -= take
		if part != nil {
			part(s, take)
		}
	}
	return known, extra, open
}

// PendingSells returns the shares of instrumentID already being sold that
// the latest account snapshot cannot show, for a planner sizing a new sell
// from that snapshot: its released sells' pending shares and those they
// filled since that snapshot, and every other working or pending sell the
// latest broker snapshot reports, when it read its open orders.
func (g *Gate) PendingSells(instrumentID string) int64 {
	return g.unshownSells(instrumentID, g.book(instrumentID).atAccount)
}

// brokerHolding returns the latest broker snapshot's holding of instrumentID
// and the shares already being sold that it cannot show, by the same rule as
// PendingSells, when the snapshot read both positions and open orders. It is
// what the broker-holding check and the sale back to 98% net.
func (g *Gate) brokerHolding(instrumentID string) (holding, working int64, ok bool) {
	s := g.broker
	if s == nil || !s.Readable(event.BrokerComponentPositions) || !s.Readable(event.BrokerComponentOpenOrders) {
		return 0, 0, false
	}
	position, _ := s.Position(instrumentID)
	return position.Quantity, g.unshownSells(instrumentID, g.book(instrumentID).atBroker), true
}

// unshownSells is the one rule PendingSells and the broker-holding check
// share: for a snapshot applied when shown shares of instrumentID's released
// sells were known filled, the sells of instrumentID it cannot show, ours
// (pending, or filled since) and unknown.
func (g *Gate) unshownSells(instrumentID string, shown int64) int64 {
	filled, pending := g.shares(instrumentID)
	return pending + filled - shown + g.unknownWorking(instrumentID)
}

// unknownWorking returns the remaining shares of every working sell of
// instrumentID the latest broker snapshot lists that the gate did not
// release, when it read its open orders.
func (g *Gate) unknownWorking(instrumentID string) int64 {
	var total int64
	if b := g.broker; b != nil && b.Readable(event.BrokerComponentOpenOrders) {
		for _, o := range b.OpenOrders {
			if _, ours := g.sells[o.ClientOrderID]; !ours && o.InstrumentID == instrumentID && o.Side == event.OrderSideSell {
				total += o.RemainingQuantity
			}
		}
	}
	return total
}

// notOlder reports whether a snapshot taken at asOf is not older than the
// latest snapshot that read the holding.
func (g *Gate) notOlder(asOf time.Time) bool {
	return g.holding == nil || !asOf.Before(g.holding.asOf)
}

// ObserveAccount records account snapshot p. When it is not older than the
// latest holding seen, a fall in its holding that the fills already known do
// not explain is taken as filled by the pending sells, net first of every
// unknown sell the latest broker snapshot lists (which may be what sold), and
// its holding then shows every share known filled. An older one changes
// nothing.
func (g *Gate) ObserveAccount(p event.AccountPositionsSnapshotPayload) {
	if !g.notOlder(p.AsOf) {
		return
	}
	held := map[string]int64{}
	for _, position := range p.Positions {
		held[position.InstrumentID] = position.Quantity
	}
	for _, instrument := range g.sellInstruments() {
		filled, pending := g.shares(instrument)
		g.takeFall(instrument, g.knownFall(instrument, held[instrument])-g.unknownWorking(instrument), filled+pending)
	}
	g.seeHolding(p.AsOf, held)
	for _, instrument := range g.sellInstruments() {
		filled, _ := g.shares(instrument)
		g.book(instrument).atAccount = filled
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
	filled, _ := g.shares(instrumentID)
	return g.holding.quantity[instrumentID] - now - (filled - g.book(instrumentID).atHolding)
}

// takeFall takes fall as filled by instrumentID's pending sells, so that no
// share they may have sold is sold again (ADR 0019, amended 2026-10-15,
// "never below it"): it raises the floor, never past ceiling, the most the
// sells that may have sold can have filled.
func (g *Gate) takeFall(instrumentID string, fall, ceiling int64) {
	if fall <= 0 {
		return
	}
	filled, _ := g.shares(instrumentID)
	b := g.book(instrumentID)
	b.floor = max(b.floor, min(filled+fall, ceiling))
}

// seeHolding makes held, taken at asOf, the holding later falls are measured
// from.
func (g *Gate) seeHolding(asOf time.Time, held map[string]int64) {
	g.holding = &holdingSeen{asOf: asOf, quantity: held}
	for _, instrument := range g.sellInstruments() {
		filled, _ := g.shares(instrument)
		g.book(instrument).atHolding = filled
	}
}

// settle keeps each snapshot's view of instrumentID within what is now known
// filled, after a listing has lowered the floor.
func (g *Gate) settle(instrumentID string) {
	filled, _ := g.shares(instrumentID)
	b := g.book(instrumentID)
	b.atAccount, b.atBroker, b.atHolding = min(b.atAccount, filled), min(b.atBroker, filled), min(b.atHolding, filled)
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
	if s := g.sells[p.ClientOrderID]; s != nil && p.Terminal() && !s.ended {
		// What the sell is counted as having filled stays counted, so the
		// floor's part it held never moves onto a later sell.
		part := int64(0)
		g.spread(s.instrumentID, func(t *trackedSell, take int64) {
			if t == s {
				part = take
			}
		})
		s.frozen, s.ended = s.known()+part, true
	}
	return nil
}

// resolve applies what the broker snapshot next, recorded after the latest
// one, shows of each released sell. A sell it lists working has filled all
// but its remaining quantity. When it is not older than the latest holding
// seen, the sells it lists are shown exactly, so the floor keeps only what
// the open sells it does not list can have filled; and a fall in its holding
// that the fills known, those it lists included, and an unknown sell's own
// fills do not explain is taken as filled by the pending sells it does not
// list. A sell it does not list stays pending otherwise. A snapshot that
// cannot read the open orders resolves nothing.
func (g *Gate) resolve(next event.BrokerSnapshotPayload) {
	if !next.Readable(event.BrokerComponentOpenOrders) {
		g.brokerSeen(next)
		return
	}
	listed := map[*trackedSell]bool{}
	for _, o := range next.OpenOrders {
		if s := g.sells[o.ClientOrderID]; s != nil && o.Side == event.OrderSideSell && o.InstrumentID == s.instrumentID {
			listed[s] = true
			s.listed = max(s.listed, min(max(s.released-o.RemainingQuantity, 0), s.released))
		}
	}
	if g.notOlder(next.AsOf) {
		for _, instrument := range g.sellInstruments() {
			ceiling, _, _ := g.spread(instrument, nil)
			for _, s := range g.sellsOf(instrument) {
				if !s.ended && !listed[s] {
					ceiling += s.released - s.known()
				}
			}
			b := g.book(instrument)
			b.floor = min(b.floor, ceiling)
			g.settle(instrument)
			if next.Readable(event.BrokerComponentPositions) {
				after, _ := next.Position(instrument)
				g.takeFall(instrument, g.knownFall(instrument, after.Quantity)-g.unknownFills(next, instrument), ceiling)
			}
		}
	}
	g.brokerSeen(next)
}

// brokerSeen records broker snapshot next. When it is not older than the
// latest holding seen, it shows every share known filled, and, when it read
// the holdings, its holding is the one later falls are measured from. An
// older one shows nothing newer, so it advances neither.
func (g *Gate) brokerSeen(next event.BrokerSnapshotPayload) {
	if !g.notOlder(next.AsOf) {
		return
	}
	if next.Readable(event.BrokerComponentPositions) {
		held := map[string]int64{}
		for _, p := range next.Positions {
			held[p.InstrumentID] = p.Quantity
		}
		g.seeHolding(next.AsOf, held)
	}
	for _, instrument := range g.sellInstruments() {
		filled, _ := g.shares(instrument)
		g.book(instrument).atBroker = filled
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
