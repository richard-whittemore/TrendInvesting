package parity

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// The IRA execution checks (docs/research/parity-2026-10.md, "Remaining
// scope, plan, 2026-10-19").
const (
	CheckIRAMonths   = "ira-months"
	CheckIRAExport   = "ira-export"
	CheckIRAPlan     = "ira-plan"
	CheckIRAScale    = "ira-scale"
	CheckIRAOrders   = "ira-orders"
	CheckIRASequence = "ira-sequence"
)

// IRAOrderPrefix begins the tag of every order the IRA template places:
// "IRAORD1 <month> <phase>" (research/edge-search/momentum/parity_export.py).
const IRAOrderPrefix = "IRAORD1 "

// OrderFilled is QuantConnect's OrderStatus.Filled.
const OrderFilled = 3

// IRAPriceWait is the sessions the IRA template's buys wait for a bought
// security's first price (PRICE_WAIT in parity_export.py); a buy still
// without one after them is unplaced.
const IRAPriceWait = 5

// exportPrefixes begin the tag of an order that carries an export
// (parity_export.py): a carrier the template placed never fills.
var exportPrefixes = []string{"PARITY1 ", "GLIDE1 ", "LOTPARITY1 ", "IRAPARITY1 "}

// LEANDelistingTag is the tag LEAN gives the order by which it sells a
// holding itself when the stock is delisted.
const LEANDelistingTag = "Liquidate from delisting"

// IRAOrder is a planned order: shares and limit price.
type IRAOrder struct {
	Ticker string
	Shares int64
	Limit  float64
}

// IRASkip is a trade the planner sized but could not order.
type IRASkip struct {
	Ticker string
	Side   string
	Reason string
	Shares float64
}

// IRAKept is a buy kept by the scaling, with its scaled shares.
type IRAKept struct {
	Ticker string
	Shares int64
}

// IRAScaledSkip is a buy the scaling left with no share, and the shares it
// was scaled to.
type IRAScaledSkip struct {
	Ticker string
	Shares float64
}

// IRAMonth is one logged rebalance of a QuantConnect IRA run
// (research/edge-search/momentum/ira_parity_log.py): what the planner
// planned from (the targets, holdings, previous closes, net liquidation,
// bands and working sells at the rebalance) and what it planned; when and
// with what budget the buys were scaled, and what the scaling kept.
// Waited is the sessions the buys waited after the rebalance session:
// the delay (none with no sell), plus ForSells sessions while a sell was
// still working and ForPrice while a bought security had no price yet. A
// buy still without one after IRAPriceWait sessions is Unplaced.
type IRAMonth struct {
	Month, Decision, BuySession                  string
	Delay, Waited, ForSells, ForPrice            int
	NLV, Band, PriceBand, MaxFees, Budget, Scale float64
	Targets                                      map[string]float64
	Held, Working                                map[string]int64
	Prices                                       map[string]float64
	Unpriced, Unplaced                           []string
	Sells, Buys                                  []IRAOrder
	Skipped                                      []IRASkip
	Kept                                         []IRAKept
	ScaledSkipped                                []IRAScaledSkip
	// Placed names each order the run placed, by order ID: the orders
	// listing names a stock by its latest ticker, which a renamed stock
	// no longer shares with the backtest.
	Placed map[int]string
	// Refused lists each order QuantConnect refused to place.
	Refused []IRARefusal
	// Unfinished marks a month whose buys were never placed: a sell was
	// still working when the next rebalance came.
	Unfinished bool
	// Version is the export's version (IRAVersion, or an earlier one).
	Version int
	// Pending names the unpriced names that were targets, which the
	// template keeps waiting for a price and never sells; an unpriced name
	// not pending is a holding, sold by the month's unpriced sale. Exports
	// before version 3 do not record it.
	Pending []string
	// PendingIDs is each pending name's security ID, by which its buy
	// outside the planner is matched, whatever ticker the stock later
	// took. Exports before version 4 do not record it.
	PendingIDs map[string]string
}

// IRARefusal is an order QuantConnect refused, so it has no order ID of
// its own, and the reason it gave ("" when the export did not record one).
type IRARefusal struct {
	Ticker, Phase string
	ID            int
	Reason        string
}

// Order is one QuantConnect order of a run, as its orders listing gives it.
type Order struct {
	ID           int
	Ticker       string
	SecurityID   string
	Quantity     float64
	Status       int
	Time         string
	LastFillTime string
	// Terminal is when the order was filled, cancelled or refused; "" while
	// it was still working when the listing was taken.
	Terminal string
	Tag      string
	// FilledQuantity is the shares the order's events filled, and
	// FillsKnown whether the listing gave events, each with its fill
	// quantity, to count them from.
	FilledQuantity float64
	FillsKnown     bool
}

// The phases of an IRAORD1 tag: the rebalance's sells, its buys, and the
// sale of a holding with no reference price, which the planner never sees.
const (
	PhaseSell     = "sell"
	PhaseBuy      = "buy"
	PhaseUnpriced = "unpriced"
)

// QuantConnect's terminal order statuses besides OrderFilled, and
// OrderCancelPending, an order whose cancel is not yet confirmed.
const (
	OrderCanceled      = 5
	OrderInvalid       = 7
	OrderCancelPending = 8
)

// maxShares bounds an order's quantity: beyond it a float64 no longer
// holds every whole number, so no share count can be read from it.
const maxShares = 1 << 53

type iraRun struct {
	checks   map[string]*Check
	orders   map[string]map[string][]Order // month -> phase -> orders
	sells    []sellOrder                   // every sell of the run, whatever its tag
	sessions Sessions
	diffs    []Difference
	run      string
	// aside is each sell set aside, by order ID: the session it was.
	aside map[int]string
	// lastID and latest are the run's highest order ID and latest order
	// time.
	lastID int
	latest time.Time
	// firstPriced is the first month an export records a price for each
	// name.
	firstPriced map[string]string
	// named is each security ID's names in the exports: those the run's
	// IRAORD1 orders of it were placed under, by order ID.
	named map[string][]string
}

// sellOrder is a sell of the run with its times read: placed at at, and
// terminal at end unless it was still working when the listing was taken;
// set aside from aside on, when it was.
type sellOrder struct {
	Order
	at, end, aside  time.Time
	ended, setAside bool
}

// CompareIRA checks each logged rebalance of an IRA run against the Go
// planner given the run's own inputs: orderplan.PlanIRA's sells, buys and
// skips (instrument, side, shares, limit price, reason) exactly; then
// orderplan.ScaleBuys of Go's buys at the run's budget and fee allowance:
// the scale within WeightTolerance, the kept shares and the scaled-away
// buys exactly; the orders the run placed (tagged IRAOrderPrefix, each
// filled, in its phase's direction, in whole shares) against Go's sells and
// scaled buys exactly; and the sequence (ADR 0026 point 2, "IRA
// execution"): every sell placed at the rebalance and filled before any
// buy, no sell of the run, whatever its tag, still holding the buys when
// one was placed, and the buys on the session the export names. A sell
// that cannot complete, still working at 8:00 on the IRAPriceWait-th
// session at whose 8:00 it worked (its own day the first when it was
// placed before 8:00, as the template counts), is set aside from then on
// and holds no buy (ADR 0026, amendment of 2026-10-18, the owner's
// decision); never filled, it is reported as set-aside, not failed, when it
// is Go's planned sell in full, and an unpriced sale so set aside is
// reported too; any other fails ira-orders. The sessions
// between the rebalance and the buys are counted by sessions, never taken
// from the export: they must be the delay (none when no sell is placed or
// working), then each session a sell held the buys at 8:00, as the orders
// listing shows, then each session waited for a price. Those extra
// sessions are the sequence's measured difference; the rebalance and the
// buys must each be on a session. They are counted for every month, one
// that placed no order included. A wait for a price and a name with no
// reference price are listed as differences, which ADR 0026 point 8 lists
// only within their bounds (owner, 2026-10-18): a wait of at most
// IRAPriceWait sessions counted on the calendar, and a month before the
// name's first recorded price. Outside them each is unexplained.
//
// Every refusal and every unplaced buy is either listed under ADR 0026
// point 8's delisted cause or fails ira-orders: a refusal QuantConnect gave
// as non-tradable, or a buy unplaced after the IRAPriceWait sessions the
// export records and the calendar counts, of an order Go planned and the
// backtest did not fill.
// A placeholder order ID records only that QuantConnect refused, not why.
// Every order of the run is accounted for: an IRAORD1 order is compared;
// an export carrier is skipped when cancelled or refused with events that
// fill nothing, or, the run's last order, left cancel-pending by its end
// and filling nothing; LEAN's sale of a holding at its delisting, and the
// buy of a pending target of the last rebalance, matched by security ID,
// are listed as differences (point 8 lists the first only for a stock Go
// held, the second as the name's unpriced trade); any other order fails
// ira-orders.
//
// Every planned month must be exported once; every month the run logged
// is compared. A failed result is recorded, never rewritten (AGENTS.md
// rule 6).
func CompareIRA(run string, planned []string, months []IRAMonth, orders []Order, sessions Sessions) Report {
	r := iraRun{checks: map[string]*Check{}, orders: map[string]map[string][]Order{}, sessions: sessions, run: run}
	var order []*Check
	for _, c := range []struct{ name, threshold string }{
		{CheckIRAMonths, "every planned month exported once"},
		{CheckIRAExport, "well-formed"},
		{CheckIRAPlan, "exact"},
		{CheckIRAScale, fmt.Sprintf("shares exact; scale abs <= %g", WeightTolerance)},
		{CheckIRAOrders, "exact"},
		{CheckIRASequence, "sells filled before the buys, none working at a buy; the sessions waited counted by the calendar: the delay, then the sessions a sell worked, then any for a price"},
	} {
		r.checks[c.name] = &Check{Run: run, Name: c.name, Threshold: c.threshold}
		order = append(order, r.checks[c.name])
	}
	plannedMonths(r.checks[CheckIRAMonths], planned, monthsOf(months, func(m IRAMonth) string { return m.Month }))
	var outside []Order
	r.aside = map[int]string{}
	for _, o := range orders {
		r.lastID = max(r.lastID, o.ID)
		if at, err := time.Parse(time.RFC3339, o.Time); err == nil && at.After(r.latest) {
			r.latest = at
		}
		if o.Quantity < 0 {
			r.sells = append(r.sells, r.setAside(readSell(o)))
		}
		rest, ok := strings.CutPrefix(o.Tag, IRAOrderPrefix)
		if !ok {
			outside = append(outside, o)
			continue
		}
		month, phase, _ := strings.Cut(rest, " ")
		if phase != PhaseSell && phase != PhaseBuy && phase != PhaseUnpriced {
			r.checks[CheckIRAOrders].compare(false, 0, fmt.Sprintf("order %d tagged %q: no phase sell, buy or unpriced", o.ID, o.Tag))
			continue
		}
		if r.orders[month] == nil {
			r.orders[month] = map[string][]Order{}
		}
		r.orders[month][phase] = append(r.orders[month][phase], o)
	}
	r.firstPriced = map[string]string{}
	for _, m := range months {
		for n := range m.Prices {
			if f, ok := r.firstPriced[n]; !ok || m.Month < f {
				r.firstPriced[n] = m.Month
			}
		}
	}
	r.named = map[string][]string{}
	sids := map[int]string{}
	for _, o := range orders {
		sids[o.ID] = o.SecurityID
	}
	for _, m := range months {
		for id, n := range m.Placed {
			if sid := sids[id]; sid != "" {
				r.named[sid] = append(r.named[sid], n)
			}
		}
	}
	exported := map[string]bool{}
	for i := range months {
		exported[months[i].Month] = true
		r.month(&months[i])
	}
	for _, month := range sortedMonths(r.orders) {
		if !exported[month] {
			r.checks[CheckIRAOrders].compare(false, 0, fmt.Sprintf("%s: orders placed, no export", month))
		}
	}
	for _, o := range outside {
		r.outside(o, months)
	}
	out := Report{Differences: r.diffs}
	for _, c := range order {
		out.Checks = append(out.Checks, *c)
	}
	out.Checks = append(out.Checks, differencesCheck(run, r.diffs))
	return out
}

// outside accounts for an order not tagged IRAORD1 (see CompareIRA).
func (r *iraRun) outside(o Order, months []IRAMonth) {
	why := "not an order of the IRA plan"
	for _, p := range exportPrefixes {
		if strings.HasPrefix(o.Tag, p) {
			// A carrier is skipped only when it is shown never to have filled:
			// cancelled or refused, with events that fill nothing.
			ended := o.Status == OrderCanceled || o.Status == OrderInvalid
			if (ended || r.endCarrier(o)) && o.FillsKnown && o.FilledQuantity == 0 && o.LastFillTime == "" {
				return
			}
			why = "an export carrier not shown never to have filled"
		}
	}
	at, err := time.Parse(time.RFC3339, o.Time)
	day := at.In(nysecalendar.NewYork).Format(time.DateOnly)
	switch {
	case err != nil || o.Status != OrderFilled:
	case o.Tag == LEANDelistingTag && o.Quantity < 0:
		d := Difference{Run: r.run, Where: day + " " + o.Ticker, Kind: KindLiquidated, Size: fmt.Sprintf("order %d: %v shares", o.ID, o.Quantity)}
		if last := lastRebalance(months, day); last == nil || !goHeld(append([]string{o.Ticker, o.SecurityID}, r.named[o.SecurityID]...), last) {
			d.Kind, d.Size = KindUnexplained, d.Size+": liquidated, of a stock Go did not hold"
		}
		r.diffs = append(r.diffs, d)
		return
	case o.Tag == "" && o.Quantity > 0:
		if last := lastRebalance(months, day); last != nil {
			var n string
			if n, why = pendingBuy(o, last); why == "" {
				r.diffs = append(r.diffs, r.unpriced(last.Month, n, fmt.Sprintf("order %d: %v shares bought outside the planner", o.ID, o.Quantity)))
				return
			}
		}
	}
	r.checks[CheckIRAOrders].compare(false, 0, fmt.Sprintf("order %d (%s, %v shares, status %d) tagged %q: %s",
		o.ID, o.Ticker, o.Quantity, o.Status, o.Tag, why))
}

// lastRebalance is the last rebalance on or before day, nil when none.
func lastRebalance(months []IRAMonth, day string) *IRAMonth {
	var last *IRAMonth
	for i := range months {
		if months[i].Decision <= day && (last == nil || months[i].Decision > last.Decision) {
			last = &months[i]
		}
	}
	return last
}

// goHeld reports whether Go held the stock after the rebalance m, under
// any of its names (its ticker, its security ID, or a name the exports
// placed an order of it under): a holding at it, or a buy it kept.
func goHeld(names []string, m *IRAMonth) bool {
	for _, n := range names {
		if _, ok := m.Held[n]; ok {
			return true
		}
		for _, k := range m.Kept {
			if k.Ticker == n {
				return true
			}
		}
	}
	return false
}

// unpriced is the difference of a name with no reference price in month:
// listed only when the month comes before the first month any export of
// the run records a price for it (every IRAPARITY1 export read, its
// targets' and holdings' prices; a pending target has none), an instrument
// not yet listed (ADR 0026 point 8, owner, 2026-10-18). A name no export
// prices is unexplained. The exports cannot tell a stock not yet listed
// from one first seen in a gap in its data and priced later.
func (r *iraRun) unpriced(month, name, size string) Difference {
	d := Difference{Run: r.run, Where: month + " " + name, Kind: KindUnpriced, Size: size}
	if first, ok := r.firstPriced[name]; !ok || month >= first {
		d.Kind, d.Size = KindUnexplained, size+": unpriced, not before its first recorded price"
	}
	return d
}

// pendingBuy names the pending target of the month that an untagged buy is,
// or says why it is none. The template buys outside the planner only its
// pending targets (an unpriced holding it sells), and the orders listing
// names a stock by its latest ticker, so the buy is matched by security
// ID: the one a version 4 export records for each pending name, or the
// name itself when it is a security ID.
func pendingBuy(o Order, m *IRAMonth) (name, why string) {
	for _, n := range m.Pending {
		if id, ok := m.PendingIDs[n]; (ok && id == o.SecurityID) || n == o.SecurityID {
			return n, ""
		}
	}
	for _, n := range m.Unpriced {
		if n != o.Ticker && n != o.SecurityID {
			continue
		}
		switch {
		case m.Version < 3:
			return "", fmt.Sprintf("an export of version %d does not record which unpriced names were targets", m.Version)
		case !slices.Contains(m.Pending, n):
			return "", fmt.Sprintf("%s of %s was an unpriced holding, which the template sells, not a pending target", n, m.Month)
		case m.Version < 4:
			return "", fmt.Sprintf("an export of version %d does not record the security ID of %s, matched by ticker only", m.Version, n)
		}
		return "", fmt.Sprintf("security %s is not the %s pending in %s", o.SecurityID, n, m.Month)
	}
	return "", "not an order of the IRA plan"
}

// endCarrier reports whether o is a carrier the run's end left
// cancel-pending: the template's own housekeeping order, placed at the
// run's final step (the highest order ID, nothing placed after it), whose
// cancel the run ended before confirming. Unfilled, it is never filled.
func (r *iraRun) endCarrier(o Order) bool {
	at, err := time.Parse(time.RFC3339, o.Time)
	return o.Status == OrderCancelPending && o.ID == r.lastID && err == nil && !r.latest.After(at)
}

// setAside marks a sell that cannot complete: still working at 8:00 on the
// IRAPriceWait-th session it worked through, a market order with no price
// at any of those sessions' opens. From then on it holds no buy (ADR 0026,
// amendment of 2026-10-18, a sell that cannot complete). The sessions are
// counted as parity_export.py's _age_sells counts them: each session at
// whose 8:00 the sell was working, so the sell's own day is the first when
// it was placed before 8:00 that day. The calendar is asked only as far as
// that session; with none to count them, it is never set aside.
func (r *iraRun) setAside(s sellOrder) sellOrder {
	day := s.at.In(nysecalendar.NewYork)
	first := day
	if s.at.Before(at8(day.Format(time.DateOnly))) {
		first = day.AddDate(0, 0, -1)
	}
	fifth, ok := r.nthSession(first, IRAPriceWait)
	if !ok {
		return s
	}
	s.aside = at8(fifth)
	if s.working(s.aside) {
		s.setAside = true
		r.aside[s.ID] = fifth
	}
	return s
}

// maxSessionGap bounds the calendar days nthSession looks across for the
// next session: a calendar naming none for longer is taken as not covering
// the span. The longest closure in the backtests' years, after 2001-09-11,
// left seven days between sessions.
const maxSessionGap = 14

// nthSession is the n-th session after day, asked of the calendar one day
// at a time, so never past that session; false when the calendar does not
// cover it.
func (r *iraRun) nthSession(day time.Time, n int) (string, bool) {
	for gap := 1; gap <= maxSessionGap; gap++ {
		next := day.AddDate(0, 0, 1)
		got, err := r.between(day.Format(time.DateOnly), next.Format(time.DateOnly))
		switch {
		case err != nil:
			return "", false
		case len(got) > 0 && n == 1:
			return got[0], true
		case len(got) > 0:
			n, gap = n-1, 0
		}
		day = next
	}
	return "", false
}

// holds reports whether the sell held the buys at t: working, and not yet
// set aside.
func (s sellOrder) holds(t time.Time) bool {
	return s.working(t) && (!s.setAside || t.Before(s.aside))
}

func readSell(o Order) sellOrder {
	// A sell of no known time is taken as placed at the start, so that it
	// holds every buy until it is known to have ended.
	s := sellOrder{Order: o}
	if at, err := time.Parse(time.RFC3339, o.Time); err == nil {
		s.at = at
	}
	end := o.Terminal
	if end == "" && o.Status == OrderFilled {
		end = o.LastFillTime
	}
	if t, err := time.Parse(time.RFC3339, end); err == nil {
		s.end, s.ended = t, true
	}
	return s
}

// working reports whether the sell was working at t: placed by then and
// not yet terminal.
func (s sellOrder) working(t time.Time) bool {
	return !s.at.After(t) && (!s.ended || s.end.After(t))
}

// at8 is 8:00 New York time on day, when the template's rebalance and buy
// phase run.
func at8(day string) time.Time {
	d, _ := time.Parse(time.DateOnly, day)
	return time.Date(d.Year(), d.Month(), d.Day(), 8, 0, 0, 0, nysecalendar.NewYork)
}

// sellWorking reports whether any sell of the run held the buys at t.
func (r *iraRun) sellWorking(t time.Time) bool {
	for _, s := range r.sells {
		if s.holds(t) {
			return true
		}
	}
	return false
}

func sortedMonths(m map[string]map[string][]Order) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (r *iraRun) month(m *IRAMonth) {
	export := r.checks[CheckIRAExport]
	in, err := iraInput(m)
	var plan orderplan.IRAPlan
	if err == nil {
		plan, err = orderplan.PlanIRA(in)
	}
	export.compare(err == nil, 0, fmt.Sprintf("%s: %v", m.Month, err))
	if err != nil {
		return
	}
	c := r.checks[CheckIRAPlan]
	compareOrders(c, m.Month+" sell", planned(plan.Sells), m.Sells)
	compareOrders(c, m.Month+" buy", planned(plan.Buys), m.Buys)
	goSkips := map[string]IRASkip{}
	for _, s := range plan.Skipped {
		goSkips[s.InstrumentID+" "+s.Side] = IRASkip{Ticker: s.InstrumentID, Side: s.Side, Reason: s.Reason, Shares: s.Shares}
	}
	qcSkips := map[string]IRASkip{}
	for _, s := range m.Skipped {
		qcSkips[s.Ticker+" "+s.Side] = s
	}
	for _, k := range unionKeys(goSkips, qcSkips) {
		g, inGo := goSkips[k]
		q, inQC := qcSkips[k]
		c.compare(inGo && inQC && g == q, 0, fmt.Sprintf("%s skip %s: Go %+v (set %v), QuantConnect %+v (set %v)", m.Month, k, g, inGo, q, inQC))
	}

	goBuys := map[string]int64{}
	if m.Unfinished {
		// Never scaled: the buys Go planned are what was never placed.
		for _, b := range plan.Buys {
			goBuys[b.InstrumentID] = b.Quantity
		}
	} else {
		goBuys = r.scale(m, plan)
	}

	names := map[string]bool{}
	for n := range m.Prices {
		names[n] = true
	}
	for _, n := range m.Unpriced {
		names[n] = true
	}
	placed := r.orders[m.Month]
	goSells := map[string]int64{}
	for _, o := range plan.Sells {
		goSells[o.InstrumentID] = -o.Quantity
	}
	o := r.checks[CheckIRAOrders]
	goBy := map[string]map[string]int64{PhaseSell: goSells, PhaseBuy: goBuys}
	qcBy := map[string]map[string]int64{}
	why, listed := map[string]string{}, map[string]string{}
	// A sell set aside, never filled, is no sell made: it is reported, as
	// the owner decided (ADR 0026, amendment of 2026-10-18), and excuses
	// Go's planned sell only when it is that sell, of its shares and with
	// none of them filled; any other fails.
	aside := map[string]string{}
	for _, phase := range []string{PhaseSell, PhaseBuy} {
		var a map[string]setAsideSell
		qcBy[phase], a = r.orderShares(o, m.Month, phase, placed[phase], names, m.Placed)
		for _, n := range unionKeys(a, nil) {
			g, inGo := goBy[phase][n]
			_, filled := qcBy[phase][n]
			if inGo && !filled && g == a[n].shares {
				aside[n+" "+phase], listed[n+" "+phase] = a[n].size, a[n].size
				continue
			}
			o.compare(false, 0, fmt.Sprintf("%s %s %s: set aside, %d shares, against Go's %d (set %v) with %d filled: %s",
				m.Month, phase, n, a[n].shares, g, inGo, qcBy[phase][n], a[n].size))
		}
	}
	// excuses reports whether a recorded detection names an order Go
	// planned and the backtest has none of: only such an order can be a
	// delisted departure.
	excuses := func(phase, name string) bool {
		_, planned := goBy[phase][name]
		_, filled := qcBy[phase][name]
		return planned && !filled
	}
	for _, f := range m.Refused {
		k := f.Ticker + " " + f.Phase
		if strings.Contains(f.Reason, "non-tradable") && excuses(f.Phase, f.Ticker) {
			listed[k] = "refused by QuantConnect: " + f.Reason
			continue
		}
		reason := f.Reason
		if reason == "" {
			reason = "no reason recorded"
		}
		if f.ID < 0 {
			reason += fmt.Sprintf(" (placeholder order ID %d)", f.ID)
		}
		why[k] = "refused by QuantConnect: " + reason
		o.compare(false, 0, fmt.Sprintf("%s %s %s: refused by QuantConnect: %s; only a non-tradable refusal of a planned order the backtest did not fill is listed",
			m.Month, f.Phase, f.Ticker, reason))
	}
	// The sessions waited are counted on the calendar for every month,
	// whether it placed an order or not: an unplaced buy is listed only on
	// the five sessions so counted.
	waited, uncounted := r.calendar(m, len(plan.Sells) > 0)
	for _, n := range m.Unplaced {
		if uncounted == "" && m.ForPrice == IRAPriceWait && excuses(PhaseBuy, n) {
			listed[n+" "+PhaseBuy] = "no price within five sessions (unplaced)"
			continue
		}
		why := ""
		if uncounted != "" {
			why = "; " + uncounted
		}
		o.compare(false, 0, fmt.Sprintf("%s buy %s: unplaced, with %d sessions waited for a price (listed only after %d counted on the calendar) or no planned buy left unfilled%s",
			m.Month, n, m.ForPrice, IRAPriceWait, why))
	}
	for n := range goBuys {
		if m.Unfinished {
			why[n+" "+PhaseBuy] = "never placed: a sell was still working at the next rebalance"
		}
	}
	for _, phase := range []string{PhaseSell, PhaseBuy} {
		for _, n := range compareShares(o, m.Month+" "+phase+" orders", goBy[phase], qcBy[phase], why, listed, phase) {
			kind := KindDelisted
			if _, ok := aside[n+" "+phase]; ok {
				kind = KindSetAside
			}
			r.diffs = append(r.diffs, Difference{Run: r.run, Where: m.Month + " " + phase + " " + n, Kind: kind,
				Size: listed[n+" "+phase]})
		}
	}
	unpriced, pending := map[string]bool{}, map[string]bool{}
	for _, n := range m.Unpriced {
		unpriced[n] = true
		r.diffs = append(r.diffs, r.unpriced(m.Month, n, "no reference price: traded outside the planner"))
	}
	for _, n := range m.Pending {
		pending[n] = true
	}
	sales, salesAside := r.orderShares(o, m.Month, PhaseUnpriced, placed[PhaseUnpriced], names, m.Placed)
	sold := map[string]bool{}
	for n := range sales {
		sold[n] = true
	}
	for n := range salesAside {
		sold[n] = true
	}
	for _, n := range unionKeys(sold, nil) {
		what := ""
		switch {
		case !unpriced[n]:
			what = "not a name without a reference price"
		case m.Version < 3:
			what = fmt.Sprintf("an export of version %d does not record which unpriced names were targets", m.Version)
		case pending[n]:
			what = "a pending target, which the template keeps"
		}
		o.compare(what == "", 0, fmt.Sprintf("%s unpriced sale of %s: %s", m.Month, n, what))
		// A sale set aside, never filled, is reported (ADR 0026, amendment
		// of 2026-10-18): the holding is still held.
		if a, ok := salesAside[n]; ok && what == "" {
			r.diffs = append(r.diffs, Difference{Run: r.run, Where: m.Month + " " + PhaseUnpriced + " " + n, Kind: KindSetAside, Size: a.size})
		}
	}
	if m.ForPrice > 0 {
		// Listed only within the five sessions, as the calendar counts them.
		d := Difference{Run: r.run, Where: m.Month, Kind: KindPriceWait, Size: fmt.Sprintf("%d sessions", m.ForPrice)}
		if m.ForPrice > IRAPriceWait || uncounted != "" {
			d.Kind = KindUnexplained
			d.Size = fmt.Sprintf("price-wait of %d sessions, outside its bound of %d counted on the calendar", m.ForPrice, IRAPriceWait)
		}
		r.diffs = append(r.diffs, d)
	}
	r.sequence(m, placed, waited, uncounted)
}

// scale checks ScaleBuys against the month's scaling and returns Go's kept
// buys.
func (r *iraRun) scale(m *IRAMonth, plan orderplan.IRAPlan) map[string]int64 {
	kept, scaledAway, scale := orderplan.ScaleBuys(plan.Buys, m.Budget, m.MaxFees)
	s := r.checks[CheckIRAScale]
	d := math.Abs(scale - m.Scale)
	s.compare(d <= WeightTolerance, d, fmt.Sprintf("%s: Go scale %v, QuantConnect %v", m.Month, scale, m.Scale))
	goKept := map[string]int64{}
	for _, k := range kept {
		goKept[k.InstrumentID] = k.Quantity
	}
	qcKept := map[string]int64{}
	for _, k := range m.Kept {
		qcKept[k.Ticker] = k.Shares
	}
	compareShares(s, m.Month+" kept", goKept, qcKept, nil, nil, "")
	goAway, qcAway := map[string]float64{}, map[string]float64{}
	for _, a := range scaledAway {
		goAway[a.InstrumentID] = a.Shares
	}
	for _, a := range m.ScaledSkipped {
		qcAway[a.Ticker] = a.Shares
	}
	for _, n := range union(goAway, qcAway) {
		g, inGo := goAway[n]
		q, inQC := qcAway[n]
		s.compare(inGo && inQC && g == q, math.Abs(g-q), fmt.Sprintf("%s scaled away %s: Go %v (set %v), QuantConnect %v (set %v)", m.Month, n, g, inGo, q, inQC))
	}
	return goKept
}

// sequence checks the order of the month's phases (ADR 0026, point 2, "IRA
// execution"), then reports the month's calendar count: the sessions
// waited past the delay, or why the export's counters are not the
// calendar's.
func (r *iraRun) sequence(m *IRAMonth, placed map[string][]Order, waited int, uncounted string) {
	c := r.checks[CheckIRASequence]
	fail := func(format string, args ...any) {
		c.compare(false, 0, m.Month+": "+fmt.Sprintf(format, args...))
	}
	if m.Unfinished {
		fail("the buys were never placed: a sell was still working at the next rebalance")
		return
	}
	var lastFill time.Time
	for _, o := range placed[PhaseSell] {
		at, err := time.Parse(time.RFC3339, o.Time)
		fill, ferr := time.Parse(time.RFC3339, o.LastFillTime)
		switch {
		case err != nil || at.Format(time.DateOnly) != m.Decision:
			fail("sell %d placed %q, not on the rebalance session %s", o.ID, o.Time, m.Decision)
			return
		case r.aside[o.ID] != "":
			// Set aside, it holds no buy once it is (sellWorking), filled
			// later or never.
		case o.Status != OrderFilled || ferr != nil:
			fail("sell %d not filled (status %d)", o.ID, o.Status)
			return
		case fill.After(lastFill):
			lastFill = fill
		}
	}
	for _, o := range placed[PhaseBuy] {
		at, err := time.Parse(time.RFC3339, o.Time)
		switch {
		case err != nil || at.Format(time.DateOnly) != m.BuySession:
			fail("buy %d placed %q, not on the logged buy session %s", o.ID, o.Time, m.BuySession)
			return
		case !at.After(lastFill):
			fail("buy %d placed %s, before the last sell filled at %s", o.ID, o.Time, lastFill.Format(time.RFC3339))
			return
		}
		for _, s := range r.sells {
			if s.ID < o.ID && s.holds(at) {
				fail("buy %d placed %s while sell %d (%s) was still working", o.ID, o.Time, s.ID, s.Ticker)
				return
			}
		}
	}
	if uncounted != "" {
		fail("%s", uncounted)
		return
	}
	c.compare(true, float64(waited), "")
}

// calendar counts the sessions from the rebalance through the buys on the
// calendar, never taking them from the export (see CompareIRA): the
// rebalance and the buys each on a session, then the delay, the sessions a
// sell was working at 8:00 and those waited for a price. It returns the
// sessions waited past the delay, or why the export's counters are not the
// calendar's.
func (r *iraRun) calendar(m *IRAMonth, plannedSells bool) (waited int, uncounted string) {
	// The span from the day before the rebalance: its first session must
	// be the rebalance, its last the buys.
	decided, _ := time.Parse(time.DateOnly, m.Decision)
	span, err := r.between(decided.AddDate(0, 0, -1).Format(time.DateOnly), m.BuySession)
	switch {
	case err != nil:
		return 0, err.Error()
	case len(span) == 0 || span[0] != m.Decision:
		return 0, fmt.Sprintf("the rebalance's day %s is not a trading session", m.Decision)
	case span[len(span)-1] != m.BuySession:
		return 0, fmt.Sprintf("the buys' day %s is not a trading session", m.BuySession)
	}
	sessions := span[1:]
	if len(sessions) != m.Waited {
		return 0, fmt.Sprintf("the export records %d sessions waited; the calendar has %d after %s through %s", m.Waited, len(sessions),
			m.Decision, m.BuySession)
	}
	required := 0
	if plannedSells || r.sellWorking(at8(m.Decision)) {
		required = m.Delay
	}
	// Each counter is bounded by the sessions counted before any sum, so no
	// sum of decoded counters can overflow.
	if required > m.Waited || m.ForSells > m.Waited-required || m.ForPrice != m.Waited-required-m.ForSells {
		return 0, fmt.Sprintf("the buys waited %d sessions: not the delay %d plus %d for sells and %d for a price", m.Waited, required,
			m.ForSells, m.ForPrice)
	}
	// The sessions waited after the delay, the rebalance's own first when
	// there is none: a sell working at 8:00 makes it a session waited for
	// sells, and any other is one waited for a price.
	days := append([]string{m.Decision}, sessions...)
	forSells := 0
	for _, d := range days[required:m.Waited] {
		if r.sellWorking(at8(d)) {
			forSells++
		}
	}
	if forSells != m.ForSells {
		return 0, fmt.Sprintf("the export records %d sessions waiting for a sell; the orders show a sell working at 8:00 on %d", m.ForSells, forSells)
	}
	return m.Waited - required, ""
}

func (r *iraRun) between(after, through string) ([]string, error) {
	if r.sessions == nil {
		return nil, fmt.Errorf("%w: (%s, %s]", ErrNoSessions, after, through)
	}
	return r.sessions(after, through)
}

func planned(orders []event.OrderPlannedPayload) map[string]IRAOrder {
	out := map[string]IRAOrder{}
	for _, o := range orders {
		out[o.InstrumentID] = IRAOrder{Ticker: o.InstrumentID, Shares: o.Quantity, Limit: o.LimitPrice}
	}
	return out
}

func compareOrders(c *Check, where string, goOrders map[string]IRAOrder, qc []IRAOrder) {
	qcOrders := map[string]IRAOrder{}
	for _, o := range qc {
		qcOrders[o.Ticker] = o
	}
	for _, k := range unionKeys(goOrders, qcOrders) {
		g, inGo := goOrders[k]
		q, inQC := qcOrders[k]
		c.compare(inGo && inQC && g == q, 0, fmt.Sprintf("%s %s: Go %+v (set %v), QuantConnect %+v (set %v)", where, k, g, inGo, q, inQC))
	}
}

// compareShares compares share counts by name; why says, by "name phase",
// why QuantConnect has none of a name. A name Go has and QuantConnect has
// none of is no failure when listed gives the detection the export
// records for it (ADR 0026 point 8, a stock delisted or acquired at the
// rebalance); those names are returned, to be listed as differences.
func compareShares(c *Check, where string, goShares, qcShares map[string]int64, why, listed map[string]string, phase string) []string {
	var departures []string
	for _, k := range unionKeys(goShares, qcShares) {
		g, inGo := goShares[k]
		q, inQC := qcShares[k]
		if _, ok := listed[k+" "+phase]; ok && inGo && !inQC {
			c.compare(true, 0, "")
			departures = append(departures, k)
			continue
		}
		what := fmt.Sprintf("%s %s: Go %d (set %v), QuantConnect %d (set %v)", where, k, g, inGo, q, inQC)
		if reason, ok := why[k+" "+phase]; ok && !inQC {
			what += ": " + reason
		}
		c.compare(inGo && inQC && g == q, 0, what)
	}
	return departures
}

// setAsideSell is the sells of one name and phase set aside, never filled:
// their shares, summed, and each order's report.
type setAsideSell struct {
	shares int64
	size   string
}

// orderShares sums the placed orders of a phase by the name the export
// uses: the one it records for the order's ID, else the security ID when
// the export names one, else the ticker. Only a filled order in its
// phase's direction (a buy positive, a sell or unpriced sale negative) of
// whole shares is summed; any other fails c and is left out, so that no
// order of the wrong kind can make up a total. A sell set aside and never
// filled (setAside) must be of that direction and whole shares too; it is
// returned apart, summed by name, so that its shares are compared with
// Go's plan before it may excuse it.
func (r *iraRun) orderShares(c *Check, month, phase string, orders []Order, names map[string]bool, byID map[int]string) (out map[string]int64, aside map[string]setAsideSell) {
	out, aside = map[string]int64{}, map[string]setAsideSell{}
	for _, o := range orders {
		name, ok := byID[o.ID]
		switch {
		case ok:
		case names[o.SecurityID]:
			name = o.SecurityID
		default:
			name = o.Ticker
		}
		setAside := r.aside[o.ID] != "" && o.Status != OrderFilled && o.FillsKnown && o.FilledQuantity == 0
		bad := ""
		switch {
		case o.Status != OrderFilled && !setAside:
			bad = fmt.Sprintf("status %d, not filled", o.Status)
		case phase == PhaseBuy && !(o.Quantity > 0), phase != PhaseBuy && !(o.Quantity < 0):
			bad = fmt.Sprintf("quantity %v against the %s's direction", o.Quantity, phase)
		case math.Abs(o.Quantity) > maxShares || o.Quantity != math.Trunc(o.Quantity):
			bad = fmt.Sprintf("quantity %v is not a whole number of shares", o.Quantity)
		}
		switch {
		case bad != "":
			c.compare(false, 0, fmt.Sprintf("%s %s order %d (%s): %s", month, phase, o.ID, name, bad))
		case setAside:
			a := aside[name]
			if a.size != "" {
				a.size += "; "
			}
			a.shares += int64(o.Quantity)
			a.size += fmt.Sprintf("order %d: %v shares never sold, set aside on %s with no price for %d sessions",
				o.ID, -o.Quantity, r.aside[o.ID], IRAPriceWait)
			aside[name] = a
		default:
			out[name] += int64(o.Quantity)
		}
	}
	return out, aside
}

func unionKeys[V any](a, b map[string]V) []string {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// iraInput is the planner's input for the month: a decided momentum
// decision of the run's targets, each the momentum sleeve's alone, and the
// account at the rebalance, as of the session before it.
func iraInput(m *IRAMonth) (orderplan.IRAInput, error) {
	day, err := time.Parse(time.DateOnly, m.Decision)
	if err != nil {
		return orderplan.IRAInput{}, fmt.Errorf("decision date %q", m.Decision)
	}
	buy, err := time.Parse(time.DateOnly, m.BuySession)
	switch {
	case err != nil || buy.Before(day):
		return orderplan.IRAInput{}, fmt.Errorf("buy session %q", m.BuySession)
	case day.Format("2006-01") != m.Month:
		return orderplan.IRAInput{}, fmt.Errorf("decision %s is not in month %s", m.Decision, m.Month)
	case m.Delay < 0 || m.Waited < 0 || m.ForSells < 0 || m.ForPrice < 0:
		return orderplan.IRAInput{}, fmt.Errorf("delay %d, waited %d (%d, %d)", m.Delay, m.Waited, m.ForSells, m.ForPrice)
	}
	var targets []event.TargetWeight
	for _, s := range sortedKeys(m.Targets) {
		w := m.Targets[s]
		targets = append(targets, event.TargetWeight{InstrumentID: s, Weight: w,
			Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: w}}})
	}
	held := make([]string, 0, len(m.Held))
	for s := range m.Held {
		held = append(held, s)
	}
	sort.Strings(held)
	positions := make([]event.AccountPosition, 0, len(held))
	for _, s := range held {
		positions = append(positions, event.AccountPosition{InstrumentID: s, Quantity: m.Held[s]})
	}
	d := event.TargetsDecidedPayload{
		AccountID: "ira", StrategyID: event.TargetWeightStrategyMomentum, DecisionTime: day, SessionPeriodEnd: day,
		Outcome: event.TargetsOutcomeDecided, Targets: targets,
		Momentum: event.MomentumFigures{CloseSession: day.AddDate(0, 0, -1).Format(time.DateOnly), TrendInstrument: "SPY"},
		Glide:    event.GlideFigures{RebalanceSession: m.Decision, AgeMonths: 1},
	}
	return orderplan.IRAInput{
		Tag: "parity:" + m.Month, Decision: d,
		Snapshot:        event.AccountPositionsSnapshotPayload{AccountID: "ira", AsOf: day, NetLiquidation: m.NLV, Currency: "USD", Positions: positions},
		ReferencePrices: m.Prices, PriceBand: m.PriceBand, RebalanceBand: m.Band, WorkingSells: m.Working,
	}, nil
}

type iraPayload struct {
	V             *int                `json:"v"`
	Month         *string             `json:"month"`
	Decision      *string             `json:"decision"`
	NLV           *float64            `json:"nlv"`
	Targets       map[string]*float64 `json:"targets"`
	Held          map[string]*int64   `json:"held"`
	Prices        map[string]*float64 `json:"prices"`
	Working       map[string]*int64   `json:"working"`
	Unpriced      []string            `json:"unpriced"`
	Sells         [][]json.RawMessage `json:"sells"`
	Buys          [][]json.RawMessage `json:"buys"`
	Skipped       [][]json.RawMessage `json:"skipped"`
	Waited        *int                `json:"waited"`
	BuySession    *string             `json:"buy_session"`
	Delay         *int                `json:"delay"`
	Band          *float64            `json:"band"`
	PriceBand     *float64            `json:"price_band"`
	MaxFees       *float64            `json:"max_fees"`
	Budget        *float64            `json:"budget"`
	Scale         *float64            `json:"scale"`
	Kept          [][]json.RawMessage `json:"kept"`
	ScaledSkipped [][]json.RawMessage `json:"scaled_skipped"`
	ForSells      *int                `json:"for_sells"`
	ForPrice      *int                `json:"for_price"`
	Unplaced      []string            `json:"unplaced"`
	Placed        [][]json.RawMessage `json:"placed"`
	Refused       [][]json.RawMessage `json:"refused"`
	Unfinished    *bool               `json:"unfinished"`
	Pending       []string            `json:"pending"`
	PendingIDs    map[string]string   `json:"pending_ids"`
}

// IRAVersion is the export version the IRA template writes. Version 1, the
// first runs', had no version field, recorded a refused order among the
// placed ones under QuantConnect's placeholder ID, and had neither refused
// nor unfinished; version 2 had no pending; version 3 no pending_ids.
const IRAVersion = 4

// DecodeIRA decodes one IRAPARITY1 payload of any version from 1 through
// IRAVersion, refusing an unknown field or version, a required field
// missing or null, a null map value, a row of the wrong shape, a placed or
// refused order of an unknown phase, a pending name that is not unpriced,
// pending security IDs that are not one non-empty ID for each pending
// name, and a row repeated: a name twice in a list, an order ID placed
// twice.
func DecodeIRA(raw []byte) (IRAMonth, error) {
	var p iraPayload
	if err := strictDecode(raw, &p); err != nil {
		return IRAMonth{}, err
	}
	if p.Month == nil || *p.Month == "" {
		return IRAMonth{}, errors.New("parity: IRA payload has no month")
	}
	version := 1
	if p.V != nil {
		version = *p.V
	}
	switch {
	case version < 1 || version > IRAVersion:
		return IRAMonth{}, fmt.Errorf("parity: %s: unknown IRA export version %d", *p.Month, version)
	case version == 1 && (p.Refused != nil || p.Unfinished != nil):
		return IRAMonth{}, fmt.Errorf("parity: %s: refused or unfinished in a version 1 export", *p.Month)
	case version >= 2 && p.Refused == nil:
		return IRAMonth{}, fmt.Errorf("parity: %s: refused missing or null", *p.Month)
	case version >= 2 && p.Unfinished == nil:
		return IRAMonth{}, fmt.Errorf("parity: %s: unfinished missing or null", *p.Month)
	case version < 3 && p.Pending != nil:
		return IRAMonth{}, fmt.Errorf("parity: %s: pending in a version %d export", *p.Month, version)
	case version >= 3 && p.Pending == nil:
		return IRAMonth{}, fmt.Errorf("parity: %s: pending missing or null", *p.Month)
	case version < 4 && p.PendingIDs != nil:
		return IRAMonth{}, fmt.Errorf("parity: %s: pending_ids in a version %d export", *p.Month, version)
	case version >= 4 && p.PendingIDs == nil:
		return IRAMonth{}, fmt.Errorf("parity: %s: pending_ids missing or null", *p.Month)
	}
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"decision", p.Decision != nil}, {"nlv", p.NLV != nil}, {"targets", p.Targets != nil}, {"held", p.Held != nil},
		{"prices", p.Prices != nil}, {"working", p.Working != nil}, {"unpriced", p.Unpriced != nil},
		{"sells", p.Sells != nil}, {"buys", p.Buys != nil}, {"skipped", p.Skipped != nil}, {"waited", p.Waited != nil},
		{"buy_session", p.BuySession != nil}, {"delay", p.Delay != nil}, {"band", p.Band != nil},
		{"price_band", p.PriceBand != nil}, {"max_fees", p.MaxFees != nil}, {"budget", p.Budget != nil},
		{"scale", p.Scale != nil}, {"kept", p.Kept != nil}, {"scaled_skipped", p.ScaledSkipped != nil},
		{"for_sells", p.ForSells != nil}, {"for_price", p.ForPrice != nil}, {"unplaced", p.Unplaced != nil},
		{"placed", p.Placed != nil},
	} {
		if !f.present {
			return IRAMonth{}, fmt.Errorf("parity: %s: %s missing or null", *p.Month, f.name)
		}
	}
	m := IRAMonth{Month: *p.Month, Decision: *p.Decision, BuySession: *p.BuySession, Delay: *p.Delay, Waited: *p.Waited,
		NLV: *p.NLV, Band: *p.Band, PriceBand: *p.PriceBand, MaxFees: *p.MaxFees, Budget: *p.Budget, Scale: *p.Scale,
		Unpriced: p.Unpriced, ForSells: *p.ForSells, ForPrice: *p.ForPrice, Unplaced: p.Unplaced,
		Placed: map[int]string{}, Unfinished: p.Unfinished != nil && *p.Unfinished, Version: version, Pending: p.Pending,
		PendingIDs: p.PendingIDs}
	var err error
	if m.Targets, err = required(m.Month, "targets", p.Targets); err != nil {
		return IRAMonth{}, err
	}
	if m.Held, err = required(m.Month, "held", p.Held); err != nil {
		return IRAMonth{}, err
	}
	if m.Prices, err = required(m.Month, "prices", p.Prices); err != nil {
		return IRAMonth{}, err
	}
	if m.Working, err = required(m.Month, "working", p.Working); err != nil {
		return IRAMonth{}, err
	}
	for _, list := range []struct {
		name  string
		names []string
	}{{"unpriced", m.Unpriced}, {"unplaced", m.Unplaced}, {"pending", m.Pending}} {
		seen := map[string]bool{}
		for _, n := range list.names {
			if seen[n] {
				return IRAMonth{}, fmt.Errorf("parity: %s %s: %s twice", m.Month, list.name, n)
			}
			seen[n] = true
		}
	}
	unpriced := map[string]bool{}
	for _, n := range m.Unpriced {
		unpriced[n] = true
	}
	for _, n := range m.Pending {
		if !unpriced[n] {
			return IRAMonth{}, fmt.Errorf("parity: %s pending: %s is not unpriced", m.Month, n)
		}
		if version >= 4 && m.PendingIDs[n] == "" {
			return IRAMonth{}, fmt.Errorf("parity: %s pending_ids: no security ID for %s", m.Month, n)
		}
	}
	if len(m.PendingIDs) > len(m.Pending) {
		return IRAMonth{}, fmt.Errorf("parity: %s pending_ids: a name not pending", m.Month)
	}
	for _, rows := range []struct {
		name string
		rows [][]json.RawMessage
		add  func([]json.RawMessage) (key string, err error)
	}{
		{"sells", p.Sells, func(r []json.RawMessage) (string, error) { return appendOrder(&m.Sells, r) }},
		{"buys", p.Buys, func(r []json.RawMessage) (string, error) { return appendOrder(&m.Buys, r) }},
		{"skipped", p.Skipped, func(r []json.RawMessage) (string, error) {
			var s IRASkip
			if err := fields(r, &s.Ticker, &s.Side, &s.Reason, &s.Shares); err != nil {
				return "", err
			}
			m.Skipped = append(m.Skipped, s)
			return s.Ticker + " " + s.Side, nil
		}},
		{"kept", p.Kept, func(r []json.RawMessage) (string, error) {
			var k IRAKept
			if err := fields(r, &k.Ticker, &k.Shares); err != nil {
				return "", err
			}
			m.Kept = append(m.Kept, k)
			return k.Ticker, nil
		}},
		{"placed", p.Placed, func(r []json.RawMessage) (string, error) {
			var name, phase string
			var id int
			if err := fields(r, &name, &phase, &id); err != nil {
				return "", err
			}
			if phase != PhaseSell && phase != PhaseBuy && phase != PhaseUnpriced {
				return "", fmt.Errorf("order %d of unknown phase %q", id, phase)
			}
			if id <= 0 {
				// Version 1 recorded a refusal here, under QuantConnect's
				// placeholder ID, which two refusals share.
				if version != 1 {
					return "", fmt.Errorf("placed order ID %d", id)
				}
				m.Refused = append(m.Refused, IRARefusal{Ticker: name, Phase: phase, ID: id})
				return "", nil
			}
			if _, dup := m.Placed[id]; dup {
				return "", fmt.Errorf("order %d placed twice", id)
			}
			m.Placed[id] = name
			return "", nil
		}},
		{"refused", p.Refused, func(r []json.RawMessage) (string, error) {
			var f IRARefusal
			if err := fields(r, &f.Ticker, &f.Phase, &f.Reason); err != nil {
				return "", err
			}
			if f.Phase != PhaseSell && f.Phase != PhaseBuy && f.Phase != PhaseUnpriced {
				return "", fmt.Errorf("refusal of %s of unknown phase %q", f.Ticker, f.Phase)
			}
			m.Refused = append(m.Refused, f)
			return "", nil
		}},
		{"scaled_skipped", p.ScaledSkipped, func(r []json.RawMessage) (string, error) {
			var s IRAScaledSkip
			if err := fields(r, &s.Ticker, &s.Shares); err != nil {
				return "", err
			}
			m.ScaledSkipped = append(m.ScaledSkipped, s)
			return s.Ticker, nil
		}},
	} {
		seen := map[string]bool{}
		for i, row := range rows.rows {
			key, err := rows.add(row)
			if err == nil && key != "" && seen[key] {
				err = fmt.Errorf("%s twice", key)
			}
			if err != nil {
				return IRAMonth{}, fmt.Errorf("parity: %s %s row %d: %w", m.Month, rows.name, i+1, err)
			}
			seen[key] = true
		}
	}
	return m, nil
}

// appendOrder decodes a planned order and returns its ticker, which a list
// of orders names once.
func appendOrder(dst *[]IRAOrder, row []json.RawMessage) (string, error) {
	var o IRAOrder
	if err := fields(row, &o.Ticker, &o.Shares, &o.Limit); err != nil {
		return "", err
	}
	*dst = append(*dst, o)
	return o.Ticker, nil
}

// fields decodes a row of exactly len(dst) values, none null, the first a
// non-empty ticker.
func fields(row []json.RawMessage, dst ...any) error {
	if len(row) != len(dst) {
		return fmt.Errorf("%d fields, want %d", len(row), len(dst))
	}
	for i, v := range row {
		if string(v) == "null" {
			return fmt.Errorf("field %d is null", i+1)
		}
		if err := json.Unmarshal(v, dst[i]); err != nil {
			return err
		}
	}
	if s, ok := dst[0].(*string); !ok || *s == "" {
		return errors.New("no ticker")
	}
	return nil
}

type orderPayload struct {
	ID     int `json:"id"`
	Symbol struct {
		Value string `json:"value"`
		ID    string `json:"id"`
	} `json:"symbol"`
	Quantity     float64 `json:"quantity"`
	Status       int     `json:"status"`
	Time         string  `json:"time"`
	LastFillTime *string `json:"lastFillTime"`
	Tag          string  `json:"tag"`
	Events       []struct {
		Time         *float64 `json:"time"`
		Status       string   `json:"status"`
		FillQuantity *float64 `json:"fillQuantity"`
	} `json:"events"`
}

// maxEventTime bounds an event's time, in seconds of the Unix epoch: the
// end of the year 9999, well inside what a time in microseconds can hold.
const maxEventTime = 253402300800

// DecodeOrders decodes a run's orders listing, QuantConnect's order objects,
// keeping what the IRA comparison reads. An order's Terminal time is its
// last filled, canceled or invalid event, to the microsecond (the listing's
// fractional seconds, so that a sell is never taken as ended before it
// did), else, for a filled order, its last fill. A terminal event with no
// time, or one not after the epoch and before the year 10000, is refused:
// it would end a sell before every buy. An order's FilledQuantity is the
// shares its events filled, known only when it has events and each gives
// its fill quantity.
func DecodeOrders(raw []byte) ([]Order, error) {
	var ps []orderPayload
	if err := json.Unmarshal(raw, &ps); err != nil {
		return nil, fmt.Errorf("parity: orders: %w", err)
	}
	out := make([]Order, len(ps))
	for i, p := range ps {
		out[i] = Order{ID: p.ID, Ticker: p.Symbol.Value, SecurityID: p.Symbol.ID, Quantity: p.Quantity, Status: p.Status,
			Time: p.Time, Tag: p.Tag, FillsKnown: len(p.Events) > 0}
		if p.LastFillTime != nil {
			out[i].LastFillTime = *p.LastFillTime
		}
		for _, e := range p.Events {
			if e.FillQuantity == nil {
				out[i].FillsKnown = false
			} else {
				out[i].FilledQuantity += math.Abs(*e.FillQuantity)
			}
			if e.Status == "filled" || e.Status == "canceled" || e.Status == "invalid" {
				if e.Time == nil || !(*e.Time > 0 && *e.Time < maxEventTime) {
					return nil, fmt.Errorf("parity: orders: order %d: a %s event with no usable time", p.ID, e.Status)
				}
				out[i].Terminal = time.UnixMicro(int64(math.Round(*e.Time * 1e6))).UTC().Format(time.RFC3339Nano)
			}
		}
		if out[i].Terminal == "" && p.Status == OrderFilled {
			out[i].Terminal = out[i].LastFillTime
		}
	}
	return out, nil
}
