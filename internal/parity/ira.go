package parity

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
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
// buy still without one is Unplaced.
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
	Tag          string
}

type iraRun struct {
	checks map[string]*Check
	orders map[string]map[string][]Order // month -> phase -> orders
}

// CompareIRA checks each logged rebalance of an IRA run against the Go
// planner given the run's own inputs: orderplan.PlanIRA's sells, buys and
// skips (instrument, side, shares, limit price, reason) exactly; then
// orderplan.ScaleBuys of Go's buys at the run's budget and fee allowance:
// the scale within WeightTolerance, the kept shares and the scaled-away
// buys exactly; the orders the run placed (tagged IRAOrderPrefix) against
// Go's sells and scaled buys exactly; and the sequence (ADR 0026 point 2,
// "IRA execution"): every sell placed at the rebalance and filled before
// any buy is placed, the buys on the session the export names, which is
// Delay sessions after the rebalance (none when there is no sell) plus the
// sessions it records waiting for a working sell or a price. Those extra
// sessions are the sequence's measured difference.
//
// Every planned month must be exported once; every month the run logged
// is compared.
func CompareIRA(run string, planned []string, months []IRAMonth, orders []Order) Report {
	r := iraRun{checks: map[string]*Check{}, orders: map[string]map[string][]Order{}}
	var order []*Check
	for _, c := range []struct{ name, threshold string }{
		{CheckIRAMonths, "every planned month exported once"},
		{CheckIRAExport, "well-formed"},
		{CheckIRAPlan, "exact"},
		{CheckIRAScale, fmt.Sprintf("shares exact; scale abs <= %g", WeightTolerance)},
		{CheckIRAOrders, "exact"},
		{CheckIRASequence, "sells filled before the buys; buys on the logged session, the delay plus the recorded waits after"},
	} {
		r.checks[c.name] = &Check{Run: run, Name: c.name, Threshold: c.threshold}
		order = append(order, r.checks[c.name])
	}
	plannedMonths(r.checks[CheckIRAMonths], planned, monthsOf(months, func(m IRAMonth) string { return m.Month }))
	for _, o := range orders {
		rest, ok := strings.CutPrefix(o.Tag, IRAOrderPrefix)
		month, phase, _ := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		if r.orders[month] == nil {
			r.orders[month] = map[string][]Order{}
		}
		r.orders[month][phase] = append(r.orders[month][phase], o)
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
	var out Report
	for _, c := range order {
		out.Checks = append(out.Checks, *c)
	}
	return out
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
	compareShares(s, m.Month+" kept", goKept, qcKept)
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

	names := map[string]bool{}
	for n := range m.Prices {
		names[n] = true
	}
	placed := r.orders[m.Month]
	goSells := map[string]int64{}
	for _, o := range plan.Sells {
		goSells[o.InstrumentID] = -o.Quantity
	}
	o := r.checks[CheckIRAOrders]
	compareShares(o, m.Month+" sell orders", goSells, orderShares(placed["sell"], names, m.Placed))
	compareShares(o, m.Month+" buy orders", goKept, orderShares(placed["buy"], names, m.Placed))
	r.sequence(m, len(plan.Sells) > 0, placed)
}

// sequence checks the order of the month's phases (ADR 0026, point 2, "IRA
// execution").
func (r *iraRun) sequence(m *IRAMonth, sells bool, placed map[string][]Order) {
	c := r.checks[CheckIRASequence]
	fail := func(format string, args ...any) {
		c.compare(false, 0, m.Month+": "+fmt.Sprintf(format, args...))
	}
	var lastFill time.Time
	for _, o := range placed["sell"] {
		at, err := time.Parse(time.RFC3339, o.Time)
		fill, ferr := time.Parse(time.RFC3339, o.LastFillTime)
		switch {
		case err != nil || at.Format(time.DateOnly) != m.Decision:
			fail("sell %d placed %q, not on the rebalance session %s", o.ID, o.Time, m.Decision)
			return
		case o.Status != OrderFilled || ferr != nil:
			fail("sell %d not filled (status %d)", o.ID, o.Status)
			return
		case fill.After(lastFill):
			lastFill = fill
		}
	}
	for _, o := range placed["buy"] {
		at, err := time.Parse(time.RFC3339, o.Time)
		switch {
		case err != nil || at.Format(time.DateOnly) != m.BuySession:
			fail("buy %d placed %q, not on the logged buy session %s", o.ID, o.Time, m.BuySession)
			return
		case !at.After(lastFill):
			fail("buy %d placed %s, before the last sell filled at %s", o.ID, o.Time, lastFill.Format(time.RFC3339))
			return
		}
	}
	required := 0
	if sells {
		required = m.Delay
	}
	if m.Waited != required+m.ForSells+m.ForPrice {
		fail("the buys waited %d sessions: not the delay %d plus %d for sells and %d for a price", m.Waited, required,
			m.ForSells, m.ForPrice)
		return
	}
	c.compare(true, float64(m.Waited-required), "")
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

func compareShares(c *Check, where string, goShares, qcShares map[string]int64) {
	for _, k := range unionKeys(goShares, qcShares) {
		g, inGo := goShares[k]
		q, inQC := qcShares[k]
		c.compare(inGo && inQC && g == q, 0, fmt.Sprintf("%s %s: Go %d (set %v), QuantConnect %d (set %v)", where, k, g, inGo, q, inQC))
	}
}

// orderShares sums the placed orders' shares by the name the export uses:
// the one it records for the order's ID, else the security ID when the
// export names one, else the ticker. An order of a fraction of a share is
// reported as a shares count no plan can match.
func orderShares(orders []Order, names map[string]bool, byID map[int]string) map[string]int64 {
	out := map[string]int64{}
	for _, o := range orders {
		name, ok := byID[o.ID]
		switch {
		case ok:
		case names[o.SecurityID]:
			name = o.SecurityID
		default:
			name = o.Ticker
		}
		q := int64(o.Quantity)
		if float64(q) != o.Quantity {
			q = math.MinInt64
		}
		out[name] += q
	}
	return out
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
}

// DecodeIRA decodes one IRAPARITY1 payload, refusing an unknown field, a
// required field missing or null, a null map value, and an order, skip or
// kept row of the wrong shape.
func DecodeIRA(raw []byte) (IRAMonth, error) {
	var p iraPayload
	if err := strictDecode(raw, &p); err != nil {
		return IRAMonth{}, err
	}
	if p.Month == nil || *p.Month == "" {
		return IRAMonth{}, errors.New("parity: IRA payload has no month")
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
		Placed: map[int]string{}}
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
	for _, rows := range []struct {
		name string
		rows [][]json.RawMessage
		add  func([]json.RawMessage) error
	}{
		{"sells", p.Sells, func(r []json.RawMessage) error { return appendOrder(&m.Sells, r) }},
		{"buys", p.Buys, func(r []json.RawMessage) error { return appendOrder(&m.Buys, r) }},
		{"skipped", p.Skipped, func(r []json.RawMessage) error {
			var s IRASkip
			if err := fields(r, &s.Ticker, &s.Side, &s.Reason, &s.Shares); err != nil {
				return err
			}
			m.Skipped = append(m.Skipped, s)
			return nil
		}},
		{"kept", p.Kept, func(r []json.RawMessage) error {
			var k IRAKept
			if err := fields(r, &k.Ticker, &k.Shares); err != nil {
				return err
			}
			m.Kept = append(m.Kept, k)
			return nil
		}},
		{"placed", p.Placed, func(r []json.RawMessage) error {
			var name, phase string
			var id int
			if err := fields(r, &name, &phase, &id); err != nil {
				return err
			}
			if _, dup := m.Placed[id]; dup {
				return fmt.Errorf("order %d placed twice", id)
			}
			m.Placed[id] = name
			return nil
		}},
		{"scaled_skipped", p.ScaledSkipped, func(r []json.RawMessage) error {
			var s IRAScaledSkip
			if err := fields(r, &s.Ticker, &s.Shares); err != nil {
				return err
			}
			m.ScaledSkipped = append(m.ScaledSkipped, s)
			return nil
		}},
	} {
		for i, row := range rows.rows {
			if err := rows.add(row); err != nil {
				return IRAMonth{}, fmt.Errorf("parity: %s %s row %d: %w", m.Month, rows.name, i+1, err)
			}
		}
	}
	return m, nil
}

func appendOrder(dst *[]IRAOrder, row []json.RawMessage) error {
	var o IRAOrder
	if err := fields(row, &o.Ticker, &o.Shares, &o.Limit); err != nil {
		return err
	}
	*dst = append(*dst, o)
	return nil
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
}

// DecodeOrders decodes a run's orders listing, QuantConnect's order objects,
// keeping what the IRA comparison reads.
func DecodeOrders(raw []byte) ([]Order, error) {
	var ps []orderPayload
	if err := json.Unmarshal(raw, &ps); err != nil {
		return nil, fmt.Errorf("parity: orders: %w", err)
	}
	out := make([]Order, len(ps))
	for i, p := range ps {
		out[i] = Order{ID: p.ID, Ticker: p.Symbol.Value, SecurityID: p.Symbol.ID, Quantity: p.Quantity, Status: p.Status,
			Time: p.Time, Tag: p.Tag}
		if p.LastFillTime != nil {
			out[i].LastFillTime = *p.LastFillTime
		}
	}
	return out, nil
}
