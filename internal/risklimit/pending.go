package risklimit

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// The pending rule (ADR 0019, amended 2026-10-15, "Sized from the broker, net
// of every working sell", cancel-pending included; ADR 0026, points 2.5 and
// 4) is one bound, worked out afresh from the facts recorded, in whatever
// order they were recorded. For an instrument and a view V, a snapshot that
// read the holding:
//
//	unshown(V) = max(held(V) - remains, 0)
//	remains    = max(0, the most over references P of held(P) - toGo(P))
//
// unshown(V) is the shares already being sold that V cannot show: those sold
// since V was taken, and those that may still sell. PendingSells is unshown
// at the latest account snapshot; the broker-holding check and the sale back
// to 98% net unshown at the latest broker snapshot. remains is the most the
// facts show is sure to be held once every sell still working has filled. A
// reference is a snapshot that read the holding: an account snapshot, or a
// broker snapshot that read both its positions and its open orders. toGo(P)
// is the most that can still sell after P was taken:
//
//   - each released sell's release, less what it is known to have filled by
//     P, or nothing once a canceled or invalid lifecycle report has ended it
//     in a second before P's;
//   - each unknown sell's remaining in the latest broker snapshot credited to
//     P that read the open orders, or nothing when that snapshot no longer
//     lists it. A reference that is credited with no listing of an unknown
//     sell that a later snapshot lists cannot bound what it sold, and is none;
//     so is one after whose credited listing the remaining ever rises from
//     one listing to the next (the owner raised its size, keeping its order
//     ID), as what it sold since is then unknown.
//
// A sell is known to have filled by P the most of its fill reports stamped
// before P's second and, for each listing of it credited to P, its release
// less the remaining listed with the fills reported in the seconds between
// the listing's and P's. A snapshot is credited with its own listing and with
// the facts stamped before its second. With no reference at all, remains is
// nothing: a long holding is never sold below nothing. The view now, with no
// holding, is credited with every fact: unshown(now) is toGo(now), what may
// still sell.
//
// Its invariants:
//
//   - Never below the truth (fail closed): toGo(P) is never below what can
//     truly still sell after P, so remains is never above what will truly be
//     held, and a sell sized from V's holding net of unshown(V) never sells a
//     share that a working sell has sold since V or may still sell.
//   - Monotone in facts: a fill report, a lifecycle end or a snapshot only
//     adds to what is known, so never raises unshown at a view; only a
//     release, or a listing of an unknown sell where it was not listed or
//     above its listing before, shows more being sold.
//   - Same-second snapshots share nothing: the order of reads and fills
//     within a second cannot be known, so each snapshot is credited with its
//     own listing and the facts of earlier seconds only, and its holding
//     bounds remains alone, never with another's.
//   - A broker snapshot that cannot read the open orders is neither a
//     reference nor a source of unknown sells: the latest that read them is.
//   - Unknown sells are counted once, by their remaining at each reference,
//     never by putting a fall in the holding down to anyone: every share sold
//     between two references is in their holdings' difference, whoever sold
//     it, so a hand sell's fill is never taken as ours, nor ours as theirs.
//   - An ended sell frees at most what it finally left unfilled: nothing at a
//     reference its end may follow, and at one after its end its fills are
//     all in that reference's holding, so it can never be counted as having
//     filled more of a fall than it did.
//   - Same-second fills post-date the snapshot: a fill stamped in a
//     snapshot's second is never credited as shown by it, nor added to a
//     listing of that second, which may already hold it.
//
// A snapshot that no longer lists a sell of ours never shows it ended: it may
// not have reached IBKR yet, and ADR 0019 treats an unexplained cancellation
// as a discrepancy, never an inference. A cancel-pending sell may still fill,
// so it stays pending. Snapshot and fill times are whole seconds (event
// validation, ADR 0015); a lifecycle report is taken at its second.
//
// A reference older than one at which nothing released before it, and no
// unknown sell, can still sell is never the better bound: what was sold
// between them is in the newer holding. So remains stops at the latest such
// reference, which changes nothing for facts that are true and complete and
// only lowers remains, failing closed, for any that are not, such as a hand
// sell no snapshot has listed.

// endOfTime is later than any fact: what is known now.
var endOfTime = time.Unix(1<<62, 0)

// view is where facts are credited from: a snapshot, taken at at, with its
// identity by (its place among the snapshots recorded, from 1), or now.
type view struct {
	at time.Time
	by int
}

// now is the view of what is known now, later than any fact.
var now = view{at: endOfTime}

// credits reports whether a fact stamped at, shown by the snapshot by (0 for
// a report), is credited to v: one of an earlier second, or v's own.
func (v view) credits(at time.Time, by int) bool {
	return at.Before(v.at) || by != 0 && by == v.by
}

// stamped is a quantity a fact shows at a time: a fill report's execution, or
// a listing's remaining in the snapshot by.
type stamped struct {
	at       time.Time
	by       int
	quantity int64
}

// releasedOrder is a released order, the shares fill reports executed, and
// those reports, in the order applied.
type releasedOrder struct {
	planned  event.OrderPlannedPayload
	reported int64
	fills    []event.TargetWeightFillPayload
}

// ledgerSell is a released sell and the facts recorded of it.
type ledgerSell struct {
	instrumentID string
	released     int64
	// since is the number of snapshots recorded before its release.
	since int
	// fills are its fill reports; listings its remaining in each broker
	// snapshot that read the open orders and listed it, in no order the
	// rule reads (filledBy).
	fills, listings []stamped
	// ended is set by a canceled or invalid lifecycle report, end the
	// earliest such report's second.
	ended bool
	end   time.Time
	// endStatus is that report's status, canceled or invalid.
	endStatus string
	// venueID is the sell's venue order ID, as a lifecycle report on it, a
	// listing echoing its client order ID, or a restored sell's decision,
	// recorded it: a listing under it with no client order ID is the sell's
	// own (learnVenue).
	venueID string
}

// reference is a snapshot that read the holding, and what it held.
type reference struct {
	view
	held map[string]int64
}

// sellLedger is every recorded fact the pending rule reads.
type sellLedger struct {
	// sells are the released sells by client order ID, and by instrument in
	// release order.
	sells        map[string]*ledgerSell
	byInstrument map[string][]*ledgerSell
	// snapshots counts the snapshots recorded; held is what each that read
	// the holding held, by identity.
	snapshots int
	held      map[int]map[string]int64
	// refs are the references, by time and then identity; openOrders the
	// broker snapshots that read the open orders, in time order.
	refs       []reference
	openOrders []view
	// unknown holds each unknown sell's listings, by instrument and broker
	// order ID, in time order.
	unknown map[string]map[string]*unknownSell
}

// unknownSell is a sell the broker lists that the gate did not release: the
// client order ID it was listed with, and its listings.
type unknownSell struct {
	client   string
	listings []stamped
}

func newSellLedger() sellLedger {
	return sellLedger{
		sells: map[string]*ledgerSell{}, byInstrument: map[string][]*ledgerSell{},
		held: map[int]map[string]int64{}, unknown: map[string]map[string]*unknownSell{},
	}
}

// release records a released sell.
func (l *sellLedger) release(client, instrumentID string, quantity int64) {
	s := &ledgerSell{instrumentID: instrumentID, released: quantity, since: l.snapshots}
	l.sells[client] = s
	l.byInstrument[instrumentID] = append(l.byInstrument[instrumentID], s)
}

// restore records a sell an earlier run released, before every snapshot
// this run records, so no reference of this run is taken before it: first in
// release order. A broker snapshot recorded before the restore may already
// list it, as an unknown sell, and IBKR does not always echo its client
// order ID, so the restored sell is counted once (the invariant "Unknown
// sells are counted once" above; ADR 0026, point 5, amended 2026-10-18):
//
//   - a listing under its client order ID and instrument, as listOpenOrders
//     matches a released sell's, is taken onto it;
//   - failing that, a listing with no client order ID under the venue order
//     ID venueID its decision recorded is taken onto it, and so is every
//     later one (listOpenOrders).
//
// Any other unechoed listing is counted beside it until its identity is
// shown by client order ID or venue order ID: a brief over-count, which
// sells less (fail closed), ending at the first listing that echoes the
// client order ID under the listing's venue order ID, or the first
// lifecycle report naming that venue order ID; every later listing under it
// with none echoed is then the sell's own (learnVenue; ADR 0026, point 5,
// amended 2026-10-18).
func (l *sellLedger) restore(client, instrumentID string, quantity int64, venueID string) *ledgerSell {
	s := &ledgerSell{instrumentID: instrumentID, released: quantity, venueID: venueID}
	byID := l.unknown[instrumentID]
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		if u := byID[id]; u.client == client || venueID != "" && u.client == "" && id == venueID {
			s.listings = append(s.listings, u.listings...)
			delete(byID, id)
		}
	}
	l.sells[client] = s
	l.byInstrument[instrumentID] = append([]*ledgerSell{s}, l.byInstrument[instrumentID]...)
	return s
}

// snapshot records a snapshot taken at asOf that read held (nil when it read
// no holding), a reference when it is one, and returns its view.
func (l *sellLedger) snapshot(asOf time.Time, held map[string]int64, isReference bool) view {
	l.snapshots++
	v := view{at: asOf, by: l.snapshots}
	if held != nil {
		l.held[v.by] = held
	}
	if isReference {
		at, _ := slices.BinarySearchFunc(l.refs, v, func(r reference, v view) int {
			if c := r.at.Compare(v.at); c != 0 {
				return c
			}
			return r.by - v.by
		})
		l.refs = slices.Insert(l.refs, at, reference{view: v, held: held})
	}
	return v
}

// listOpenOrders records the open orders a broker snapshot v read: each sell
// of ours it lists, by its client order ID or, with none echoed, by its
// venue order ID (learnVenue), and each sell it lists that the gate did not
// release.
func (l *sellLedger) listOpenOrders(v view, orders []event.BrokerOrder) {
	l.openOrders = append(l.openOrders, v)
	for _, o := range orders {
		if o.Side != event.OrderSideSell {
			continue
		}
		listing := stamped{at: v.at, by: v.by, quantity: o.RemainingQuantity}
		if s := l.sells[o.ClientOrderID]; s != nil && s.instrumentID == o.InstrumentID {
			s.listings = append(s.listings, listing)
			// A listing echoing the client order ID shows the sell's venue
			// order ID too, so a later listing under it with none echoed is
			// the sell's own (learnVenue).
			l.learnVenue(s, o.BrokerOrderID)
			continue
		}
		if s := l.byVenue(o); s != nil {
			s.listings = append(s.listings, listing)
			continue
		}
		byID := l.unknown[o.InstrumentID]
		if byID == nil {
			byID = map[string]*unknownSell{}
			l.unknown[o.InstrumentID] = byID
		}
		u := byID[o.BrokerOrderID]
		if u == nil {
			u = &unknownSell{}
			byID[o.BrokerOrderID] = u
		}
		u.client = o.ClientOrderID
		u.listings = append(u.listings, listing)
	}
}

// refuseSharedVenue refuses broker snapshot id when it lists a sell of ours
// under its client order ID with a broker order ID another sell of ours
// carries as its venue order ID: a venue order ID names one order, and the
// listing would give a second sell the ID (learnVenue). One snapshot never
// lists two orders under one broker order ID: its validation requires them
// non-empty and strictly ascending.
func (l *sellLedger) refuseSharedVenue(id string, orders []event.BrokerOrder) error {
	for _, o := range orders {
		if s := l.sells[o.ClientOrderID]; o.Side == event.OrderSideSell && s != nil && s.instrumentID == o.InstrumentID {
			if other := l.venueHolder(o.BrokerOrderID, o.ClientOrderID); other != "" {
				return fmt.Errorf("risklimit: broker snapshot %s lists order %s under venue order %s, which sell %s already carries; a venue order ID names one order", id, o.ClientOrderID, o.BrokerOrderID, other)
			}
		}
	}
	return nil
}

// learnVenue records venueID, the venue order ID a lifecycle report or a
// listing echoing s's client order ID names, as the released sell s's, when
// none is recorded: IBKR does not always echo
// a client order ID, so a listing under that ID with none, read before the
// report or after it, is s's own and counted once (the invariant "Unknown
// sells are counted once" above).
func (l *sellLedger) learnVenue(s *ledgerSell, venueID string) {
	if venueID == "" || s.venueID != "" {
		return
	}
	s.venueID = venueID
	if u := l.unknown[s.instrumentID][venueID]; u != nil && u.client == "" {
		s.listings = append(s.listings, u.listings...)
		delete(l.unknown[s.instrumentID], venueID)
	}
}

// byVenue returns the released sell whose venue order ID is o's, when IBKR
// lists o with no client order ID (learnVenue, restore), and none when two
// sells carry that venue order ID: the listing is then counted beside them,
// an over-count, which sells less (fail closed, AGENTS.md).
func (l *sellLedger) byVenue(o event.BrokerOrder) *ledgerSell {
	if o.ClientOrderID != "" || o.BrokerOrderID == "" {
		return nil
	}
	var match *ledgerSell
	for _, s := range l.byInstrument[o.InstrumentID] {
		if s.venueID != o.BrokerOrderID {
			continue
		}
		if match != nil {
			// Two sells under one venue order ID cannot say whose the
			// listing is: it is counted beside them, an over-count, which
			// sells less (fail closed, AGENTS.md).
			return nil
		}
		match = s
	}
	return match
}

// venueTaken reports whether a sell of ours already carries venueID as its
// venue order ID.
func (l *sellLedger) venueTaken(venueID string) bool {
	return l.venueHolder(venueID, "") != ""
}

// venueHolder returns the client order ID of a sell of ours other than
// except that carries venueID as its venue order ID, or none; a synthetic
// report names no venue order ID, and nothing carries none. With more than
// one holder it names the least client order ID, so the answer does not
// depend on map order.
func (l *sellLedger) venueHolder(venueID, except string) string {
	if venueID == "" {
		return ""
	}
	var holder string
	for client, s := range l.sells {
		if s.venueID == venueID && client != except && (holder == "" || client < holder) {
			holder = client
		}
	}
	return holder
}

// unshown is the pending rule: the shares of instrumentID already being sold
// that the view v, now or a snapshot that read the holding, cannot show. A
// view of no snapshot is now.
func (l *sellLedger) unshown(instrumentID string, v view) int64 {
	if v.by == 0 {
		toGo, _, _ := l.toGo(instrumentID, now)
		return toGo
	}
	return max(subtractPending(l.held[v.by][instrumentID], l.remains(instrumentID)), 0)
}

// remains is the most of instrumentID the facts show is sure to be held once
// every sell still working has filled, never below nothing.
func (l *sellLedger) remains(instrumentID string) int64 {
	var best int64
	var stop time.Time
	for _, p := range slices.Backward(l.refs) {
		if p.at.Before(stop) {
			break
		}
		toGo, quiet, ok := l.toGo(instrumentID, p.view)
		if !ok {
			continue
		}
		best = max(best, subtractPending(p.held[instrumentID], toGo))
		if quiet && stop.IsZero() {
			stop = p.at
		}
	}
	return best
}

// toGo returns the most of instrumentID that can still sell after v was
// taken, and ok false when v cannot bound an unknown sell. quiet reports
// that nothing released before v was recorded, and no unknown sell, can.
func (l *sellLedger) toGo(instrumentID string, v view) (toGo int64, quiet, ok bool) {
	quiet = true
	for _, s := range l.byInstrument[instrumentID] {
		if s.endedBefore(v) {
			continue
		}
		n := s.released - s.filledBy(v)
		toGo = addPending(toGo, n)
		quiet = quiet && (n == 0 || s.since >= v.by)
	}
	var latest view
	for _, b := range slices.Backward(l.openOrders) {
		if v.credits(b.at, b.by) {
			latest = b
			break
		}
	}
	for _, u := range l.unknown[instrumentID] {
		listings := u.listings
		// Broker snapshots follow one another, so the listings credited to v
		// come first.
		credited := len(listings)
		if i := slices.IndexFunc(listings, func(x stamped) bool { return !v.credits(x.at, x.by) }); i >= 0 {
			credited = i
		}
		switch {
		case credited > 0 && listings[credited-1].by == latest.by:
			if raised(listings[credited-1:]) {
				return 0, false, false
			}
			toGo = addPending(toGo, listings[credited-1].quantity)
			quiet = false
		case credited < len(listings):
			// Listed after v but not in the latest listing credited to v.
			return 0, false, false
		}
	}
	return toGo, quiet, true
}

// addPending adds nonnegative outstanding quantities, saturating the bound
// when it cannot be represented (ADR 0026 D7-F). Over-counting sells fails
// closed; a wrapped negative count could release shares already committed.
func addPending(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// subtractPending subtracts a nonnegative commitment from a signed holding,
// saturating below the integer range (ADR 0026 D7-F). A short holding can
// never wrap into a positive holding that permits another sell.
func subtractPending(held, pending int64) int64 {
	if held < math.MinInt64+pending {
		return math.MinInt64
	}
	return held - pending
}

// raised reports whether an unknown sell's remaining rises from one of its
// listings to the next: its size was raised, keeping its order ID.
func raised(listings []stamped) bool {
	for i := 1; i < len(listings); i++ {
		if listings[i].quantity > listings[i-1].quantity {
			return true
		}
	}
	return false
}

// endedBefore reports whether a lifecycle report has ended the sell in a
// second before v's. A fill report stamped after the end's second shows the
// end no earlier than that fill's.
func (s *ledgerSell) endedBefore(v view) bool {
	if !s.ended {
		return false
	}
	for _, f := range s.fills {
		if !f.at.Before(v.at) {
			return false
		}
	}
	return s.end.Before(v.at)
}

// filledBy is the most the sell's facts credited to v show it filled, never
// past its release: its fills reported before v's second, or a listing
// credited to v with the fills reported in the seconds after it.
func (s *ledgerSell) filledBy(v view) int64 {
	reported := func(after time.Time) int64 {
		var n int64
		for _, f := range s.fills {
			if f.at.After(after) && f.at.Before(v.at) {
				n = addPending(n, f.quantity)
			}
		}
		return n
	}
	best := reported(time.Time{})
	for _, x := range s.listings {
		if v.credits(x.at, x.by) {
			best = max(best, addPending(s.released-min(x.quantity, s.released), reported(x.at)))
		}
	}
	return min(best, s.released)
}

// PendingSells returns the shares of instrumentID already being sold that
// the latest account snapshot cannot show, for a planner sizing a new sell
// from that snapshot (unshown): its released sells' and the unknown sells'
// shares sold since it was taken or still to sell.
func (g *Gate) PendingSells(instrumentID string) int64 {
	return g.ledger.unshown(instrumentID, g.account)
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
	return position.Quantity, g.ledger.unshown(instrumentID, g.brokerView), true
}

// ObserveAccount records account snapshot p, the one PendingSells nets for,
// and a reference. Its quantity units must match the account (ADR 0026 D7-F).
func (g *Gate) ObserveAccount(p event.AccountPositionsSnapshotPayload) error {
	if err := g.checkQuantityScale(p.QuantityScale); err != nil {
		return err
	}
	held := map[string]int64{}
	for _, position := range p.Positions {
		held[position.InstrumentID] = position.Quantity
	}
	g.account = g.ledger.snapshot(p.AsOf, held, true)
	return nil
}

// recordBroker records broker snapshot next, recorded after the latest one: its
// holding when it read the positions, its listings when it read the open
// orders, and a reference when it read both.
func (g *Gate) recordBroker(next event.BrokerSnapshotPayload) {
	var held map[string]int64
	if next.Readable(event.BrokerComponentPositions) {
		held = map[string]int64{}
		for _, p := range next.Positions {
			held[p.InstrumentID] = p.Quantity
		}
	}
	orders := next.Readable(event.BrokerComponentOpenOrders)
	g.brokerView = g.ledger.snapshot(next.AsOf, held, held != nil && orders)
	if orders {
		g.ledger.listOpenOrders(g.brokerView, next.OpenOrders)
	}
}

// reportedOrder returns the released order a venue report names, checking
// that the report describes it (checkReport).
func (g *Gate) reportedOrder(account, client, instrument, side string) (*releasedOrder, error) {
	o := g.orders[client]
	if err := checkReport(account, client, instrument, side, g.accountID, o.fillOrder()); err != nil {
		return nil, err
	}
	return o, nil
}

// checkReport checks that a venue report for account names a released order
// of the gate's account, order (nil when none was released), and describes
// its instrument and side.
func checkReport(account, client, instrument, side, gateAccount string, order *FillOrder) error {
	switch {
	case account != gateAccount:
		return fmt.Errorf("risklimit: report for account %q; this gate is %q", account, gateAccount)
	case order == nil:
		return fmt.Errorf("risklimit: report names order %s, which the gate never released", client)
	case order.InstrumentID != instrument || order.Side != side:
		return fmt.Errorf("risklimit: report describes %s %s, but order %s is %s %s", side, instrument, client, order.Side, order.InstrumentID)
	}
	return nil
}

// FillOrder is a released order as the fill rule reads it: its account,
// client order ID, instrument and side, the shares released, and the shares
// the fills applied to it so far executed.
type FillOrder struct {
	AccountID, ClientOrderID, InstrumentID, Side string
	Quantity, Reported                           int64
	QuantityScale                                int64
}

// fillOrder is the order as the fill rule reads it; nil for none.
func (o *releasedOrder) fillOrder() *FillOrder {
	if o == nil {
		return nil
	}
	return &FillOrder{AccountID: o.planned.AccountID, ClientOrderID: o.planned.ClientOrderID, InstrumentID: o.planned.InstrumentID,
		Side: o.planned.Side, Quantity: o.planned.Quantity, Reported: o.reported, QuantityScale: o.planned.QuantityScale}
}

// CheckFill is the rule a recorded execution of a released order is applied
// by: the one ObserveFill applies, and the one the reporter reads a Boost
// run's fills by, so the two cannot disagree about which fill counts. It
// needs no limits: a fill is checked against the order it names, nothing
// else. p must validate; a fill whose ID was already applied (applied, by
// fill ID) is a redelivery, which changes nothing, when it is the same
// execution, and refused when it is another (ADR 0019, "Explained means": a
// fill already applied can never be added again to erase a difference);
// otherwise it must be for the gate's account (account), name a released
// order (order, nil when none was), describe its instrument and side, and
// not take it past the shares released. A whole or part fill counts
// whatever the lifecycle reports before it said, a late fill after a
// cancellation included (ADR 0022).
func CheckFill(p event.TargetWeightFillPayload, account string, order *FillOrder, applied map[string]event.TargetWeightFillPayload) (redelivery bool, err error) {
	if err := p.Validate(); err != nil {
		return false, fmt.Errorf("risklimit: %w", err)
	}
	if seen, ok := applied[p.FillID]; ok {
		if seen.SameExecution(p) {
			return true, nil
		}
		return false, fmt.Errorf("risklimit: fill id %s is reused for a different execution", p.FillID)
	}
	if err := checkReport(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, account, order); err != nil {
		return false, err
	}
	if p.QuantityScale != order.QuantityScale {
		return false, fmt.Errorf("risklimit: fill quantity scale %d differs from order scale %d", p.QuantityScale, order.QuantityScale)
	}
	if p.Quantity > order.Quantity-order.Reported {
		return false, fmt.Errorf("risklimit: fill %s takes order %s past its %s shares", p.FillID, p.ClientOrderID, event.FormatQuantity(order.Quantity, order.QuantityScale))
	}
	return false, nil
}

// ObserveFill applies a recorded execution of a released order, which is
// also a change a reconciliation's book is bridged by, when the fill rule
// (CheckFill) accepts it. A redelivery of an execution already applied
// changes nothing: a fill already applied can never be added again to erase
// a difference (ADR 0019, "Explained means").
func (g *Gate) ObserveFill(p event.TargetWeightFillPayload) error {
	if err := g.checkQuantityScale(p.QuantityScale); err != nil {
		return err
	}
	o := g.orders[p.ClientOrderID]
	switch redelivery, err := CheckFill(p, g.accountID, o.fillOrder(), g.fills); {
	case err != nil:
		return err
	case redelivery:
		return nil
	}
	o.reported += p.Quantity
	o.fills = append(o.fills, p)
	g.fills[p.FillID] = p
	g.changes = append(g.changes, reconcile.FillChange(p))
	// A reconciliation classified before this fill did not see it, so its
	// verdict no longer stands for the next snapshot (ADR 0019, amended
	// 2026-10-21, point 6; CheckBook).
	g.classified = nil
	if s := g.ledger.sells[p.ClientOrderID]; s != nil {
		s.fills = append(s.fills, stamped{at: p.FilledAt, quantity: p.Quantity})
	}
	return nil
}

// Filled returns the shares the venue's fill reports executed of the
// released order clientOrderID, each execution counted once, and 0 for an
// order the gate never released.
func (g *Gate) Filled(clientOrderID string) int64 {
	if o, ok := g.orders[clientOrderID]; ok {
		return o.reported
	}
	return 0
}

// Fills returns the fill reports applied to the released order
// clientOrderID, each execution once, in the order applied, and none for an
// order the gate never released.
func (g *Gate) Fills(clientOrderID string) []event.TargetWeightFillPayload {
	if o, ok := g.orders[clientOrderID]; ok {
		return slices.Clone(o.fills)
	}
	return nil
}

// RestoreReleased restores an order an earlier run released and a run
// restarted from its recorded decision still watches (ADR 0026, point 5),
// with the executions its fill reports stated before the restart, when the
// venue had ended it the time of that end, and its venue order ID when the
// decision recorded one (empty otherwise), by which IBKR's listing of a
// restored sell with no client order ID echoed is matched (restore). The venue's later reports on
// it are read against it: an execution restored, sent again, is a
// redelivery that changes nothing (ObserveFill; ADR 0019, "Explained
// means"). A restored sell is still being sold, so it is netted as any
// released sell (ADR 0019, amended 2026-10-15, "net of every working
// sell"): released before every snapshot of this run, filled by its
// executions as stamped, and ended from its end. The order must be this
// gate's account's and not already known, its executions its own, with
// distinct fill ids and fewer shares than released; on a refusal nothing is
// restored.
func (g *Gate) RestoreReleased(o event.OrderReleasedPayload, fills []event.TargetWeightFillPayload, ended time.Time, venueOrderID string) error {
	return g.RestoreRecovery(Recovery{Released: []RestoredOrder{{Order: o, Fills: fills, Ended: ended, VenueOrderID: venueOrderID}}})
}

// ObserveLifecycle applies a recorded lifecycle report on a released order:
// canceled or invalid ends a sell's remainder, from the earliest such
// report's second; submitted and cancel-pending leave it pending. The venue
// order ID it names is the sell's, so IBKR's listing of the sell with no
// client order ID echoed is counted as the sell's own (learnVenue). The
// report's limit is the order's when within event.LimitPriceTolerance of it
// (event.SameLimitPrice), the rule the engine and the reporter read a report
// by.
func (g *Gate) ObserveLifecycle(p event.LimitOrderLifecyclePayload) error {
	if err := g.checkQuantityScale(p.QuantityScale); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("risklimit: %w", err)
	}
	o, err := g.reportedOrder(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side)
	if err != nil {
		return err
	}
	if p.Quantity != o.planned.Quantity || !event.SameLimitPrice(p.LimitPrice, o.planned.LimitPrice) {
		return fmt.Errorf("risklimit: a lifecycle report on order %s reports %d at %v; the order is %d at %v", p.ClientOrderID, p.Quantity, p.LimitPrice, o.planned.Quantity, o.planned.LimitPrice)
	}
	s := g.ledger.sells[p.ClientOrderID]
	if other := g.ledger.venueHolder(p.OrderID, p.ClientOrderID); s != nil && other != "" {
		return fmt.Errorf("risklimit: lifecycle report on order %s names venue order %s, which sell %s already carries; a venue order ID names one order", p.ClientOrderID, p.OrderID, other)
	}
	if s != nil {
		g.ledger.learnVenue(s, p.OrderID)
	}
	if s != nil && p.Terminal() {
		if end := p.OccurredAt.Truncate(time.Second); !s.ended || end.Before(s.end) {
			s.ended, s.end, s.endStatus = true, end, p.Status
		}
	}
	return nil
}
