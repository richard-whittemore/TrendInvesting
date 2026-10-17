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
// A snapshot shows the shares filled by when it was taken, and no others.
// Every fact of a fill carries the time it shows the shares filled by: a fill
// report its execution time, a broker listing and a holding fall the time of
// the snapshot that showed them, a lifecycle report that ends a sell the time
// it occurred. A snapshot taken at asOf is credited with its own facts and
// those of earlier snapshots, at or before asOf, and with the fill and
// lifecycle reports strictly before asOf, whenever they were recorded. IBKR
// stamps executions in whole seconds and a snapshot carries no broker time of
// its own, so a report in the same second as a snapshot may have happened
// after the snapshot read its positions; it is assumed to have (fail closed),
// and stays pending from that snapshot. Fill reports are pushed and
// snapshots polled, so a fill reported before a snapshot taken before it is
// still pending from that snapshot's view, and a fill reported after a
// snapshot taken after it is shares that snapshot's holding already showed,
// counted once. Account and broker snapshots are ordered only within their
// own stream, so a snapshot older than the latest holding seen may be
// recorded after it: it is credited by its own time like any other, and only
// its holding is not measured as a fall, the newer holding having been
// measured already. So an older snapshot never moves what the gate knows
// backwards, and no snapshot is credited with a fill report it cannot show
// nor with more filled than the gate counts filled now.

// endOfTime is later than any fact: what is known now.
var endOfTime = time.Unix(1<<62, 0)

// releasedOrder is a released order and the shares fill reports executed.
type releasedOrder struct {
	planned  event.OrderPlannedPayload
	reported int64
}

// shown is a fact that filled shares had been filled by at.
type shown struct {
	at     time.Time
	filled int64
}

// most returns the most shares snapshot facts at or before t show filled, 0
// with none.
func most(facts []shown, t time.Time) int64 {
	var out int64
	for _, f := range facts {
		if !f.at.After(t) {
			out = max(out, f.filled)
		}
	}
	return out
}

// trackedSell is one released sell and what its own recorded facts show.
type trackedSell struct {
	instrumentID string
	released     int64
	// fills are the executions fill reports show, each at its execution
	// time; listed are the shares broker listings showed filled, each at its
	// snapshot's time.
	fills, listed []shown
	// frozen is what was counted filled when a canceled or invalid lifecycle
	// report, which occurred at endedAt, ended the sell; frozenFills is how
	// many of fills it counted.
	frozen      int64
	frozenFills int
	endedAt     time.Time
	ended       bool
}

// endedBy reports whether a lifecycle report had ended the sell before t.
func (s *trackedSell) endedBy(t time.Time) bool { return s.ended && s.endedAt.Before(t) }

// knownBy is the most the sell's own facts credited to a snapshot taken at t
// show filled: fill reports and a lifecycle end before t, listings at or
// before t. A lifecycle end is credited with what it froze less the fill
// reports it counted that are stamped at or after t: a fill report stamped
// after the end is credited by its own time; a fall in the holding
// attributed to the sell, folded into what the end froze, is credited from
// the end's.
func (s *trackedSell) knownBy(t time.Time) int64 {
	var reported int64
	for _, f := range s.fills {
		if f.at.Before(t) {
			reported += f.filled
		}
	}
	known := max(reported, most(s.listed, t))
	if s.endedBy(t) {
		frozen := s.frozen
		for _, f := range s.fills[:s.frozenFills] {
			if !f.at.Before(t) {
				frozen -= f.filled
			}
		}
		known = max(known, frozen)
	}
	return known
}

// unfilledBy is the most of a floor the sell, not ended by t, can hold by
// the facts credited to a snapshot taken at t: its shares beyond what they
// show filled, or, once a lifecycle report has ended it, only what it is
// finally counted as having filled beyond them. So no snapshot is credited
// with more filled than the gate counts filled now, whatever order the end
// and the snapshot were recorded in.
func (s *trackedSell) unfilledBy(t time.Time) int64 {
	limit := s.released
	if s.ended {
		limit = min(limit, s.knownBy(endOfTime))
	}
	return limit - s.knownBy(t)
}

// sellBook is what snapshots show of one instrument's released sells
// together.
type sellBook struct {
	// floors are the fewest shares the holding's falls show the sells filled
	// together, each at the time of the snapshot that showed it.
	floors []shown
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

// shares returns what is known now of instrumentID's released sells
// together: the shares filled, and the shares still pending.
func (g *Gate) shares(instrumentID string) (filled, pending int64) {
	known, extra, open := g.spread(instrumentID, endOfTime, nil)
	return known + extra, open - extra
}

// filledBy returns the shares of instrumentID's released sells that the
// facts credited to a snapshot taken at t show filled: what it shows.
func (g *Gate) filledBy(instrumentID string, t time.Time) int64 {
	known, extra, _ := g.spread(instrumentID, t, nil)
	return known + extra
}

// spread returns, by the facts credited to a snapshot taken at t, the shares
// the sells' own facts show filled, the part of the floor beyond them that
// the open sells hold, and the open sells' unfilled shares. The floor beyond the sells' own
// facts is spread over the open sells oldest first, each up to its unfilled
// shares; part, when set, receives each open sell's part of it.
func (g *Gate) spread(instrumentID string, t time.Time, part func(*trackedSell, int64)) (known, extra, open int64) {
	sells := g.sellsOf(instrumentID)
	for _, s := range sells {
		known += s.knownBy(t)
		if !s.endedBy(t) {
			open += s.unfilledBy(t)
		}
	}
	left := min(max(most(g.book(instrumentID).floors, t)-known, 0), open)
	extra = left
	for _, s := range sells {
		if s.endedBy(t) || left == 0 {
			continue
		}
		take := min(left, s.unfilledBy(t))
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
// filled after it was taken, and every other working or pending sell the
// latest broker snapshot reports, when it read its open orders.
func (g *Gate) PendingSells(instrumentID string) int64 {
	return g.unshownSells(instrumentID, g.accountAsOf)
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
	return position.Quantity, g.unshownSells(instrumentID, s.AsOf), true
}

// unshownSells is the one rule PendingSells and the broker-holding check
// share: for a snapshot taken at asOf, the sells of instrumentID it cannot
// show, ours (pending, or filled after asOf) and unknown. A snapshot is never
// credited with more filled than the gate counts filled now; were it, what
// it shows of the sells' fills could not be read, and it is credited with
// none (fail closed: every share the sells filled or still pend counts as
// working), never with a count below zero.
func (g *Gate) unshownSells(instrumentID string, asOf time.Time) int64 {
	filled, pending := g.shares(instrumentID)
	shown := g.filledBy(instrumentID, asOf)
	if shown > filled {
		shown = 0
	}
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

// ObserveAccount records account snapshot p, the one PendingSells nets for:
// it shows the fills credited to its time. When it is not older than the
// latest holding seen, a fall in its holding that the fills by then do not
// explain is taken as filled by the pending sells, net first of every
// unknown sell the latest broker snapshot lists (which may be what sold),
// and its holding is the one later falls are measured from.
func (g *Gate) ObserveAccount(p event.AccountPositionsSnapshotPayload) {
	g.accountAsOf = p.AsOf
	if !g.notOlder(p.AsOf) {
		return
	}
	held := map[string]int64{}
	for _, position := range p.Positions {
		held[position.InstrumentID] = position.Quantity
	}
	for _, instrument := range g.sellInstruments() {
		if floor, ok := g.fallFloor(instrument, held[instrument]); ok {
			filled, pending := g.shares(instrument)
			g.takeFall(instrument, p.AsOf, floor-g.unknownWorking(instrument), filled+pending)
		}
	}
	g.seeHolding(p.AsOf, held)
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

// fallFloor returns the fewest shares instrumentID's sells had filled when a
// snapshot holding now was taken, by the fall from the latest snapshot that
// read the holding: what that snapshot showed filled, and the fall since.
// ok is false when no earlier snapshot read the holding.
func (g *Gate) fallFloor(instrumentID string, now int64) (floor int64, ok bool) {
	if g.holding == nil {
		return 0, false
	}
	return g.filledBy(instrumentID, g.holding.asOf) + g.holding.quantity[instrumentID] - now, true
}

// takeFall takes floor, seen by the snapshot taken at asOf, as filled by
// instrumentID's sells, so that no share they may have sold is sold again
// (ADR 0019, amended 2026-10-15, "never below it"), never past ceiling, the
// most the sells that may have sold can have filled. A floor the facts by
// asOf already show changes nothing.
func (g *Gate) takeFall(instrumentID string, asOf time.Time, floor, ceiling int64) {
	floor = min(floor, ceiling)
	if floor <= g.filledBy(instrumentID, asOf) {
		return
	}
	b := g.book(instrumentID)
	b.floors = append(b.floors, shown{at: asOf, filled: floor})
}

// seeHolding makes held, taken at asOf, the holding later falls are measured
// from.
func (g *Gate) seeHolding(asOf time.Time, held map[string]int64) {
	g.holding = &holdingSeen{asOf: asOf, quantity: held}
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
		s.fills = append(s.fills, shown{at: p.FilledAt, filled: p.Quantity})
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
		g.spread(s.instrumentID, endOfTime, func(t *trackedSell, take int64) {
			if t == s {
				part = take
			}
		})
		s.frozen, s.frozenFills, s.endedAt, s.ended = s.knownBy(endOfTime)+part, len(s.fills), p.OccurredAt, true
	}
	return nil
}

// resolve applies what the broker snapshot next, recorded after the latest
// one, shows of each released sell. A sell it lists working had filled all
// but its remaining quantity by its time. When it is not older than the
// latest holding seen, the sells it lists are shown exactly, so every floor
// keeps only what the open sells it does not list can have filled by then;
// and a fall in its holding that the fills by then, those it lists included,
// and an unknown sell's own fills do not explain is taken as filled by the
// pending sells it does not list. A sell it does not list stays pending
// otherwise. A snapshot that cannot read the open orders resolves nothing.
func (g *Gate) resolve(next event.BrokerSnapshotPayload) {
	if !next.Readable(event.BrokerComponentOpenOrders) {
		g.brokerSeen(next)
		return
	}
	listed := map[*trackedSell]bool{}
	for _, o := range next.OpenOrders {
		if s := g.sells[o.ClientOrderID]; s != nil && o.Side == event.OrderSideSell && o.InstrumentID == s.instrumentID {
			listed[s] = true
			s.listed = append(s.listed, shown{at: next.AsOf, filled: min(max(s.released-o.RemainingQuantity, 0), s.released)})
		}
	}
	if g.notOlder(next.AsOf) {
		for _, instrument := range g.sellInstruments() {
			ceiling, _, _ := g.spread(instrument, next.AsOf, nil)
			for _, s := range g.sellsOf(instrument) {
				if !s.endedBy(next.AsOf) && !listed[s] {
					ceiling += s.unfilledBy(next.AsOf)
				}
			}
			b := g.book(instrument)
			for i := range b.floors {
				b.floors[i].filled = min(b.floors[i].filled, ceiling)
			}
			if next.Readable(event.BrokerComponentPositions) {
				after, _ := next.Position(instrument)
				if floor, ok := g.fallFloor(instrument, after.Quantity); ok {
					g.takeFall(instrument, next.AsOf, floor-g.unknownFills(next, instrument), ceiling)
				}
			}
		}
	}
	g.brokerSeen(next)
}

// brokerSeen records broker snapshot next: when it is not older than the
// latest holding seen and read the holdings, its holding is the one later
// falls are measured from.
func (g *Gate) brokerSeen(next event.BrokerSnapshotPayload) {
	if !g.notOlder(next.AsOf) || !next.Readable(event.BrokerComponentPositions) {
		return
	}
	held := map[string]int64{}
	for _, p := range next.Positions {
		held[p.InstrumentID] = p.Quantity
	}
	g.seeHolding(next.AsOf, held)
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
