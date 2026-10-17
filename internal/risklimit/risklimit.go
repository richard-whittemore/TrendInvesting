// Package risklimit is the hard-limit gate outside the strategy (ADR 0026,
// point 3): every strategy.order.planned gets risk.order.released or
// risk.order.refused, and a breach moves the account to Degraded or Halted
// through strategy.engine.state. It holds the account's engine state and its
// scope, the kill switch, the latest broker snapshot and the margin-cushion
// alarm, and decides when ADR 0019's one sale back to 98% goes (ADR 0026,
// point 4). It checks the joint margin account's limits and an IRA's. It is
// pure: everything it checks arrives as an argument.
package risklimit

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// The gate's states (ADR 0019, amended 2026-09-24; ADR 0026, point 4).
// Leaving Degraded or Halted is never automatic.
const (
	StateNormal   = "normal"
	StateDegraded = "degraded"
	StateHalted   = "halted"
)

// Order is one planned order with the ID of the envelope that carried it and
// the latest recorded mark for its instrument (MarkSet false when none).
type Order struct {
	PlanID  string
	Planned event.OrderPlannedPayload
	Mark    float64
	MarkSet bool
}

// Outcome is the gate's answer for one order: exactly one of Released and
// Refused, and State when the order moved the account's engine state, at
// StateSchemaVersion, to be journalled under StateID.
type Outcome struct {
	Released           *event.OrderReleasedPayload
	Refused            *event.OrderRefusedPayload
	State              *event.EngineStatePayload
	StateSchemaVersion uint32
	StateID            string
}

// Book is the account as the plan is checked against it: the broker-reported
// holdings and each holding's reference price (the raw previous close, ADR
// 0026 point 2, "Valuation basis"), the shares already in working or pending
// sells, cancel-pending ones included, the orders already planned at this
// decision time in an earlier phase and how many of those are not
// risk-reducing, and, for an IRA, the cash its buys may
// still commit under D5, net of the buys already committed.
type Book struct {
	Holdings      map[string]int64
	Prices        map[string]float64
	WorkingSells  map[string]int64
	PriorOrders   int
	PriorCounted  int
	CashAvailable float64
	// At is when the check's trigger was journalled: the session's close, or
	// the broker snapshot that fired a sale. A margin account's sells are
	// never checked against a broker snapshot older than the configured
	// MaxSnapshotAge at that time.
	At time.Time
}

// Gate checks one account's planned orders against its limits and holds the
// account's engine state between decisions. ADR 0019's one sale back to 98%
// and the margin-cushion alarm belong to a margin account only; an IRA never
// borrows (plan.md section 5).
type Gate struct {
	accountID string
	ira       bool
	maxFees   float64
	limits    event.MarginLimits
	mode      string
	decided   map[string]bool
	state     string
	scope     []string
	// incident is the engine-state record that took the account out of
	// Normal; it tags the incident's one sale back to 98%.
	incident string

	killEngaged bool
	killID      string

	broker   *event.BrokerSnapshotPayload
	brokerID string
	// firstBroker is when the run's first broker snapshot was taken.
	firstBroker time.Time
	// maxSnapshotAge is the oldest the latest broker snapshot may be at a
	// trigger to size the sale back to 98%; urgentRetry is set while the
	// urgent alarm's sale is withheld from a snapshot that could not size it.
	maxSnapshotAge time.Duration
	urgentRetry    bool

	alarmLevel     string
	alarmID        string
	lastReading    time.Time
	lastReadingSet bool

	// released holds every client order ID the journal released, so a
	// working broker order is recognised by it. saleTags holds the tags a
	// sale back to 98% may carry; attempted the sale client IDs planned or
	// withheld for a person, never planned again; pending those planned and
	// not yet checked.
	released  map[string]bool
	saleTags  map[string]bool
	attempted map[string]bool
	pending   map[string]bool
	// sells holds every sell the gate released, by client order ID, and
	// sellOrder their IDs in release order; orders holds every released
	// order, so a fill or lifecycle report is checked against the order it
	// names, and fills every execution reported, by fill ID.
	sells     map[string]*trackedSell
	sellOrder []string
	// holding is the latest snapshot that read the holdings, which a later
	// snapshot's fall in the holding is measured from.
	holding *holdingSeen
	orders  map[string]*releasedOrder
	fills   map[string]event.TargetWeightFillPayload
}

// NewGate returns a Normal gate for the joint margin account accountID under
// limits and mode. Its limits set the margin-cushion alarm's thresholds (ADR
// 0026, point 6), and maxSnapshotAge, the configured execution
// MaxSnapshotAge, is the oldest the latest broker snapshot may be at a
// trigger for the sale back to 98% to be sized from it (ADR 0019).
func NewGate(accountID string, limits event.MarginLimits, mode string, maxSnapshotAge time.Duration) (*Gate, error) {
	if !limits.HasCushion() {
		return nil, errors.New("risklimit: a margin account's limits set the excess liquidity thresholds")
	}
	if maxSnapshotAge <= 0 {
		return nil, errors.New("risklimit: a margin account's sale needs a positive maximum broker snapshot age")
	}
	g, err := newGate(accountID, limits, mode)
	if err != nil {
		return nil, err
	}
	g.maxSnapshotAge = maxSnapshotAge
	return g, nil
}

// NewIRAGate returns a Normal gate for an IRA: its exposure limit is at most
// 1, since an IRA never uses margin, its limits set no margin cushion, and
// every buy's worst-case cost, its quantity at its limit plus maxFees, must
// fit the cash available (ADR 0026, point 3).
func NewIRAGate(accountID string, limits event.MarginLimits, mode string, maxFees float64) (*Gate, error) {
	if limits.MaxExposure > 1 {
		return nil, fmt.Errorf("risklimit: an IRA's max exposure %v exceeds 1: it never uses margin", limits.MaxExposure)
	}
	if limits.HasCushion() {
		return nil, errors.New("risklimit: an IRA's limits set no excess liquidity thresholds: it has no margin cushion")
	}
	if !(maxFees >= 0) || math.IsInf(maxFees, 0) {
		return nil, errors.New("risklimit: the fee allowance must be finite and not negative")
	}
	g, err := newGate(accountID, limits, mode)
	if err != nil {
		return nil, err
	}
	g.ira, g.maxFees = true, maxFees
	return g, nil
}

func newGate(accountID string, limits event.MarginLimits, mode string) (*Gate, error) {
	if accountID == "" {
		return nil, errors.New("risklimit: account id is required")
	}
	if err := limits.Validate(); err != nil {
		return nil, fmt.Errorf("risklimit: %w", err)
	}
	if !event.ValidTradingMode(mode) {
		return nil, fmt.Errorf("risklimit: mode %q is not dry-run, paper or live", mode)
	}
	return &Gate{
		accountID: accountID, limits: limits, mode: mode, decided: make(map[string]bool),
		state: StateNormal, alarmLevel: event.MarginAlarmClear,
		released: make(map[string]bool), saleTags: make(map[string]bool),
		attempted: make(map[string]bool), pending: make(map[string]bool),
		sells: make(map[string]*trackedSell), orders: make(map[string]*releasedOrder),
		fills: make(map[string]event.TargetWeightFillPayload),
	}, nil
}

// RecordDecision records the ID of a journalled strategy.targets.decided
// envelope, which an order's tag must name to be released.
func (g *Gate) RecordDecision(id string) { g.decided[id] = true }

// State returns the gate's engine state.
func (g *Gate) State() string { return g.state }

// Incident returns the ID of the engine-state record that took the account
// out of Normal, or "" while Normal.
func (g *Gate) Incident() string { return g.incident }

// RestoreState applies one strategy.engine.state record an earlier run of
// this account journalled under envelope ID id, at schemaVersion, before any
// order is checked: "a crash or restart while Degraded or Halted resumes in
// that same state and scope, read back from the run's durable record" (ADR
// 0019, amended 2026-09-24, "The state survives a restart"; ADR 0026, point
// 5). Leaving either state is never automatic, so a record never loosens the
// gate: a Halted gate refuses a Degraded record, and a Degraded record widens
// the scope to the union. A scope naming orders or cash components is
// refused, since this gate holds instruments only and cannot honour it. The
// first record out of Normal is the incident, whose one sale back to 98% it
// tags: moving from Degraded to Halted does not start another (ADR 0019,
// amendment of 2026-10-17).
func (g *Gate) RestoreState(id string, p event.EngineStatePayload, schemaVersion uint32) error {
	if err := p.ValidateSchema(schemaVersion); err != nil {
		return fmt.Errorf("risklimit: recorded %w", err)
	}
	if id == "" {
		return errors.New("risklimit: a recorded engine state needs its envelope ID")
	}
	if len(p.ScopeOrderIDs) > 0 || len(p.ScopeCashComponents) > 0 {
		return errors.New("risklimit: a recorded scope naming orders or cash components is not one this gate can hold")
	}
	if p.State != event.EngineStateHalted && g.state == StateHalted {
		return errors.New("risklimit: a recorded Degraded state cannot follow Halted: leaving Halted is never automatic")
	}
	if g.state == StateNormal {
		g.incident = id
		g.saleTags[id] = true
	}
	if p.State == event.EngineStateHalted {
		g.state, g.scope = StateHalted, nil
		return nil
	}
	scope := slices.Clone(g.scope)
	for _, instrument := range p.ScopeInstrumentIDs {
		if !slices.Contains(scope, instrument) {
			scope = append(scope, instrument)
		}
	}
	slices.Sort(scope)
	g.state, g.scope = StateDegraded, scope
	return nil
}

// Check decides every order of one decision time's plan, or of one phase of
// it, in plan order, against book. An order that fails its own payload
// validation, or belongs to another account, is a defect in the caller and
// returns an error with no outcome and no state change; an untagged order is
// a refusal, not an error, and halts.
//
// The checks, in order (ADR 0026, points 3 and 4):
//   - a sale back to 98% that this gate issued (Sale) goes in every state
//     and through the kill switch, once, if it is still within the broker's
//     holding net of every working sell; one it never issued halts;
//   - Halted refuses every other order;
//   - a tag naming no recorded decision refuses and halts;
//   - the kill switch refuses every other order and changes no state;
//   - in a margin account, a sell checked against a latest broker snapshot
//     older at book.At than MaxSnapshotAge, or one reporting a net
//     liquidation or mark that is not strictly positive, is refused and
//     changes no state (unsizable);
//   - Degraded refuses every buy and every order on an instrument its scope
//     names, and a sell elsewhere with no broker snapshot to size it from
//     (ADR 0019, amended 2026-09-24: sells are "sized from" the broker
//     quantity);
//   - a sell beyond the latest broker snapshot's holding net of every
//     working or pending sell is a breach, refused and not resized: the
//     planner, not the gate, chooses quantities (ADR 0019, amendment of
//     2026-10-17), and so is a sell beyond the book's
//     holding net of its working and pending sells;
//   - the order count (buys only, when the decision time's orders that are
//     not risk-reducing are over it); then, unless the order is
//     risk-reducing, the mark, the price band and the order size; then for
//     an exposure-increasing order a holding with no reference price, the
//     exposure limit, and for an IRA's buy the cash available.
//
// A breach refuses the order and degrades the account, naming the
// instrument.
//
// A risk-reducing order (ADR 0026, point 4) is "a sell that moves the
// account's exposure toward 98% or lower, sized within the broker-reported
// holding": a sell within the latest broker snapshot's holding net of every
// working or pending sell, or, with no readable broker snapshot, a sell no
// larger than the account snapshot's holding the planner sized it from,
// which reports no working orders. It is exempt from the order count, the
// order size and the price band (point 3), and so from the mark, the band's
// only input; one late mark cannot refuse Boost's exit. It is not counted
// toward the count (ADR 0026, amendment of 2026-10-17: risk-reducing orders
// are not counted toward N).
//
// Exposure is account-wide: the sum of every holding x its reference price
// (the order's own at its reference price) over net liquidation, before and
// after the order (ADR 0026, point 3). Only an order that increases it is
// checked against MaxExposure, so a sale is never refused by that check even
// when the account stays above it. A released order moves the book the next
// order is checked against: its shares, valued at its reference price, and an
// IRA buy's worst-case cost out of the cash available. A released sell lowers
// the holding once; it is not also added to the working sells, which would
// count it twice against what the next sell may take.
func (g *Gate) Check(book Book, plan []Order) ([]Outcome, error) {
	for i, o := range plan {
		probe := o.Planned
		if probe.Tag == "" {
			probe.Tag = "untagged"
		}
		if err := probe.Validate(); err != nil {
			return nil, fmt.Errorf("risklimit: order %d: %w", i+1, err)
		}
		if o.Planned.AccountID != g.accountID {
			return nil, fmt.Errorf("risklimit: order %d belongs to account %q, not %q", i+1, o.Planned.AccountID, g.accountID)
		}
		if o.PlanID == "" {
			return nil, fmt.Errorf("risklimit: order %d has no plan ID", i+1)
		}
		if o.MarkSet && (!(o.Mark > 0) || math.IsInf(o.Mark, 0)) {
			return nil, fmt.Errorf("risklimit: order %d: a recorded mark must be finite and positive", i+1)
		}
	}
	if g.maxSnapshotAge > 0 && book.At.IsZero() {
		return nil, errors.New("risklimit: a margin account's check needs the time of its trigger, to bound the broker snapshot's age")
	}
	running := &book
	running.Holdings = maps.Clone(book.Holdings)
	running.Prices = maps.Clone(book.Prices)
	running.WorkingSells = maps.Clone(book.WorkingSells)
	if running.Holdings == nil {
		running.Holdings = map[string]int64{}
	}
	if running.Prices == nil {
		running.Prices = map[string]float64{}
	}
	if running.WorkingSells == nil {
		running.WorkingSells = map[string]int64{}
	}
	planSize := book.PriorOrders + len(plan)
	counted := book.PriorCounted
	for _, o := range plan {
		if !g.riskReducing(o.Planned) {
			counted++
		}
	}
	outcomes := make([]Outcome, 0, len(plan))
	for _, o := range plan {
		out := g.check(o, planSize, counted, running)
		if out.Released != nil {
			p := o.Planned
			running.Prices[p.InstrumentID] = p.ReferencePrice
			if p.Side == event.OrderSideSell {
				// The lower holding is what the next sell may take from,
				// net of the book's working sells: the released sell is
				// counted once, not again as a working sell (ADR 0026,
				// point 2.5).
				running.Holdings[p.InstrumentID] -= p.Quantity
			} else {
				running.Holdings[p.InstrumentID] += p.Quantity
				running.CashAvailable -= out.Released.Figures.BuyCost
			}
		}
		outcomes = append(outcomes, out)
	}
	return outcomes, nil
}

// check decides one order of a plan of planSize orders at its decision time,
// counted of which are not risk-reducing, against book.
func (g *Gate) check(o Order, planSize, counted int, book *Book) Outcome {
	p := o.Planned
	f, unvalued := g.figures(o, planSize, book)
	if p.Sale != (event.SaleFigures{}) {
		return g.checkSale(o, f)
	}
	if reason, detail := g.unsizable(p, book.At); reason != "" && g.state != StateHalted && g.decided[p.Tag] && !g.killEngaged {
		return Outcome{Refused: g.refusal(o, f, reason, detail)}
	}
	switch {
	case g.state == StateHalted:
		return Outcome{Refused: g.refusal(o, f, event.RefusedEngineHalted, "the account is Halted: no order is released until the owner clears it")}
	case !g.decided[p.Tag]:
		out := Outcome{Refused: g.refusal(o, f, event.RefusedUndecidedOrder, fmt.Sprintf("tag %q names no journalled strategy.targets.decided in this run", p.Tag))}
		g.halt(&out, fmt.Sprintf("order %s refused: %s; a defect in the order path, so its scope cannot be bounded", p.ClientOrderID, event.RefusedUndecidedOrder), p.ClientOrderID)
		return out
	case g.killEngaged:
		return Outcome{Refused: g.refusal(o, f, event.RefusedKillSwitch, fmt.Sprintf("the kill switch is engaged (%s): every order is refused except the margin account's one sale back to 98%%", g.killID))}
	case g.state == StateDegraded && (p.Side == event.OrderSideBuy || slices.Contains(g.scope, p.InstrumentID)):
		// ADR 0019, amended 2026-09-24: "Nothing that opens or adds to a
		// position is allowed while not Normal", and named instruments are
		// frozen; risk-reducing sells continue elsewhere (ADR 0026, point 4).
		return Outcome{Refused: g.refusal(o, f, event.RefusedEngineDegraded, fmt.Sprintf("the account is Degraded (scope %v): only risk-reducing sells go, and the scope's instruments are frozen", g.scope))}
	case g.state == StateDegraded && !f.BrokerSet:
		return Outcome{Refused: g.refusal(o, f, event.RefusedBrokerHolding, "the account is Degraded and no readable broker snapshot holds the quantity to size this sell from")}
	}
	if reason, detail := g.breach(o, f, counted, book, unvalued); reason != "" {
		out := Outcome{Refused: g.refusal(o, f, reason, detail)}
		g.degrade(&out, p.InstrumentID, fmt.Sprintf("order %s refused: %s: %s", p.ClientOrderID, reason, detail), p.ClientOrderID)
		return out
	}
	return g.release(o, f)
}

// unsizable returns why a margin account's sell p cannot be checked against
// the latest broker snapshot at at, the time its strategy.order.planned is
// journalled, or "" when it can or no readable broker snapshot is recorded:
// the snapshot is older than MaxSnapshotAge (ADR 0019: "a stale matching
// snapshot is not success"), checked first, or its net liquidation or the
// instrument's mark is not strictly positive (ADR 0019: never short).
// Neither changes the engine state.
func (g *Gate) unsizable(p event.OrderPlannedPayload, at time.Time) (reason, detail string) {
	s := g.broker
	if p.Side != event.OrderSideSell || g.maxSnapshotAge <= 0 || s == nil ||
		!s.Readable(event.BrokerComponentPositions) || !s.Readable(event.BrokerComponentOpenOrders) {
		// No snapshot, or one that cannot be read, is the existing case: the
		// sell is sized as it would be without one.
		return "", ""
	}
	if age := at.Sub(s.AsOf); age > g.maxSnapshotAge {
		return event.RefusedStaleBrokerSnapshot, fmt.Sprintf("broker snapshot %s is %v old at the trigger; the most a sell is checked against is %v", g.brokerID, age, g.maxSnapshotAge)
	}
	if s.Readable(event.BrokerComponentNetLiquidation) && !(s.NetLiquidation > 0) {
		return event.RefusedNonPositiveBrokerValues, fmt.Sprintf("broker snapshot %s reports net liquidation %v", g.brokerID, s.NetLiquidation)
	}
	if position, held := s.Position(p.InstrumentID); held && position.MarkSet && !(position.Mark > 0) {
		return event.RefusedNonPositiveBrokerValues, fmt.Sprintf("broker snapshot %s reports a mark of %v for %s", g.brokerID, position.Mark, p.InstrumentID)
	}
	return "", ""
}

// release releases o with figures f.
func (g *Gate) release(o Order, f event.RiskFigures) Outcome {
	p := o.Planned
	g.released[p.ClientOrderID] = true
	g.orders[p.ClientOrderID] = &releasedOrder{planned: p}
	if p.Side == event.OrderSideSell {
		g.sells[p.ClientOrderID] = &trackedSell{instrumentID: p.InstrumentID, released: p.Quantity}
		g.sellOrder = append(g.sellOrder, p.ClientOrderID)
	}
	return Outcome{Released: &event.OrderReleasedPayload{
		AccountID: g.accountID, PlanID: o.PlanID, Tag: p.Tag, ClientOrderID: p.ClientOrderID,
		InstrumentID: p.InstrumentID, Side: p.Side, Quantity: p.Quantity, LimitPrice: p.LimitPrice,
		Mode: g.mode, Send: g.mode != event.TradingModeDryRun, Figures: f,
	}}
}

// halt moves the account to Halted, which names no scope, recording the
// state change on out under an ID derived from the causing order.
func (g *Gate) halt(out *Outcome, detail, cause string) {
	g.enter(out, StateHalted, &event.EngineStatePayload{
		State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: detail,
	}, event.EngineStateScopedVersion, cause)
	g.scope = nil
}

// degrade adds instrumentID to a Degraded account's scope, recording the
// state change on out. instrumentID is never already named: a Degraded
// account refuses every order on a named instrument before any limit is
// checked, and a duplicate would fail the state payload's validation.
func (g *Gate) degrade(out *Outcome, instrumentID, detail, cause string) {
	scope := append(slices.Clone(g.scope), instrumentID)
	slices.Sort(scope)
	g.scope = scope
	g.enter(out, StateDegraded, &event.EngineStatePayload{
		State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: detail,
		ScopeInstrumentIDs: slices.Clone(scope),
	}, event.EngineStateScopedVersion, cause)
}

// enter records a state change on out. The first change out of Normal is the
// incident: its ID tags the incident's one sale back to 98% (ADR 0026,
// point 2, "Order identity").
func (g *Gate) enter(out *Outcome, state string, payload *event.EngineStatePayload, version uint32, cause string) {
	id := "engine-state:" + g.accountID + ":" + cause
	if g.state == StateNormal {
		g.incident = id
		g.saleTags[id] = true
	}
	g.state = state
	out.State, out.StateSchemaVersion, out.StateID = payload, version, id
}

// breach returns the first limit o breaches and its detail, or "" when none.
// counted is the decision time's orders that are not risk-reducing.
func (g *Gate) breach(o Order, f event.RiskFigures, counted int, book *Book, unvalued string) (reason, detail string) {
	p := o.Planned
	increases := f.ExposureAfter > f.ExposureBefore
	brokerHeld := f.BrokerHolding - f.WorkingSells
	available := book.Holdings[p.InstrumentID] - book.WorkingSells[p.InstrumentID]
	switch {
	case p.Side == event.OrderSideSell && f.BrokerSet && p.Quantity > brokerHeld:
		return event.RefusedBrokerHolding, fmt.Sprintf("sell of %d exceeds the broker's holding %d net of %d working sells (snapshot %s)", p.Quantity, f.BrokerHolding, f.WorkingSells, g.brokerID)
	case p.Side == event.OrderSideSell && p.Quantity > available:
		return event.RefusedSellExceedsHolding, fmt.Sprintf("sell of %d %s exceeds the %d held net of working and pending sells", p.Quantity, p.InstrumentID, available)
	case counted > g.limits.MaxOrdersPerDecision && p.Side == event.OrderSideBuy:
		return event.RefusedOrderCount, fmt.Sprintf("%d orders that are not risk-reducing planned at one decision time; the limit is %d, so the plan's buys are refused", counted, g.limits.MaxOrdersPerDecision)
	case g.riskReducing(p):
		// Exempt from the mark, band and size (ADR 0026, point 3); a sell
		// never breaches the exposure limit, which checks only an increase.
		return "", ""
	case !o.MarkSet:
		return event.RefusedMissingMark, fmt.Sprintf("no mark recorded for %s at or after the session's close", p.InstrumentID)
	case f.MarkDeviation > g.limits.PriceBand:
		return event.RefusedPriceBand, fmt.Sprintf("reference price %v is %v from mark %v; the band is %v", p.ReferencePrice, f.MarkDeviation, o.Mark, g.limits.PriceBand)
	case f.OrderFraction > g.limits.MaxOrderFraction:
		return event.RefusedOrderSize, fmt.Sprintf("order is %v of net liquidation; the limit is %v", f.OrderFraction, g.limits.MaxOrderFraction)
	case increases && unvalued != "":
		return event.RefusedUnvaluedHolding, fmt.Sprintf("holding %s has no reference price, so the account's exposure cannot be valued", unvalued)
	case increases && f.ExposureAfter > g.limits.MaxExposure && g.ira:
		return event.RefusedIRAExposure, fmt.Sprintf("exposure after the order %v exceeds %v (before %v)", f.ExposureAfter, g.limits.MaxExposure, f.ExposureBefore)
	case increases && f.ExposureAfter > g.limits.MaxExposure:
		return event.RefusedMarginExposure, fmt.Sprintf("exposure after the order %v exceeds %v (before %v)", f.ExposureAfter, g.limits.MaxExposure, f.ExposureBefore)
	case f.CashChecked && f.BuyCost > f.CashAvailable:
		return event.RefusedInsufficientCash, fmt.Sprintf("worst-case cost %v exceeds the %v of cash available; an IRA never borrows", f.BuyCost, f.CashAvailable)
	}
	return "", ""
}

// figures values o against book: the account's exposure before and after it,
// its share of net liquidation, its mark deviation, for an IRA's buy its
// worst-case cost and the cash available, and the latest broker snapshot's
// holding and working sells when one is readable. It also returns the first
// holding, by instrument, that has no reference price, or "" when every one
// has.
func (g *Gate) figures(o Order, planSize int, book *Book) (f event.RiskFigures, unvalued string) {
	p := o.Planned
	held := book.Holdings[p.InstrumentID]
	after := held + p.Quantity
	if p.Side == event.OrderSideSell {
		after = held - p.Quantity
	}
	others := 0.0
	for _, id := range slices.Sorted(maps.Keys(book.Holdings)) {
		price, ok := book.Prices[id]
		switch {
		case id == p.InstrumentID || book.Holdings[id] == 0:
		case !ok:
			if unvalued == "" {
				unvalued = id
			}
		default:
			others += math.Abs(float64(float64(book.Holdings[id]) * price))
		}
	}
	own := func(q int64) float64 { return math.Abs(float64(float64(q) * p.ReferencePrice)) }
	f = event.RiskFigures{
		NetLiquidation:   p.NetLiquidation,
		ReferencePrice:   p.ReferencePrice,
		ExposureBefore:   (others + own(held)) / p.NetLiquidation,
		ExposureAfter:    (others + own(after)) / p.NetLiquidation,
		OrderFraction:    float64(p.Quantity) * p.ReferencePrice / p.NetLiquidation,
		OrdersInDecision: planSize,
	}
	if o.MarkSet {
		f.Mark, f.MarkSet = o.Mark, true
		f.MarkDeviation = math.Abs(p.ReferencePrice-o.Mark) / o.Mark
	}
	if g.ira && p.Side == event.OrderSideBuy {
		f.CashChecked = true
		f.BuyCost = float64(float64(p.Quantity)*p.LimitPrice) + g.maxFees
		f.CashAvailable = book.CashAvailable
	}
	if holding, working, ok := g.brokerHolding(p.InstrumentID); ok {
		f.BrokerSet, f.BrokerHolding, f.WorkingSells = true, holding, working
	}
	// Recorded whenever the exemption applies, with or without a broker
	// snapshot, so the journal alone explains a sell whose mark, band and
	// size were not checked (ADR 0026, points 3 and 4).
	f.RiskReducing = g.riskReducing(p)
	return f, unvalued
}

func (g *Gate) refusal(o Order, f event.RiskFigures, reason, detail string) *event.OrderRefusedPayload {
	p := o.Planned
	return &event.OrderRefusedPayload{
		AccountID: g.accountID, PlanID: o.PlanID, Tag: p.Tag, ClientOrderID: p.ClientOrderID,
		InstrumentID: p.InstrumentID, Side: p.Side, Quantity: p.Quantity, LimitPrice: p.LimitPrice,
		Reason: reason, Detail: detail, Figures: f,
	}
}
