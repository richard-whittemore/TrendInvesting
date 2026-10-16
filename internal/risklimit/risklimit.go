// Package risklimit is the hard-limit gate outside the strategy (ADR 0026,
// point 3): every strategy.order.planned gets risk.order.released or
// risk.order.refused, and a breach moves the account to Degraded or Halted
// through strategy.engine.state. It checks the joint margin account's limits
// and an IRA's. It is pure: everything it checks arrives as an argument.
package risklimit

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

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
// StateSchemaVersion.
type Outcome struct {
	Released           *event.OrderReleasedPayload
	Refused            *event.OrderRefusedPayload
	State              *event.EngineStatePayload
	StateSchemaVersion uint32
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
}

// Gate checks one account's planned orders against its limits and holds the
// account's engine state between decisions.
type Gate struct {
	accountID string
	limits    event.MarginLimits
	mode      string
	ira       bool
	maxFees   float64
	decided   map[string]bool
	state     string
	scope     []string
}

// NewIRAGate returns a Normal gate for an IRA: its exposure limit is at most
// 1, since an IRA never uses margin, and every buy's worst-case cost, its
// quantity at its limit plus maxFees, must fit the cash available (ADR 0026,
// point 3).
func NewIRAGate(accountID string, limits event.MarginLimits, mode string, maxFees float64) (*Gate, error) {
	if limits.MaxExposure > 1 {
		return nil, fmt.Errorf("risklimit: an IRA's max exposure %v exceeds 1: it never uses margin", limits.MaxExposure)
	}
	if !(maxFees >= 0) || math.IsInf(maxFees, 0) {
		return nil, errors.New("risklimit: the fee allowance must be finite and not negative")
	}
	g, err := NewGate(accountID, limits, mode)
	if err != nil {
		return nil, err
	}
	g.ira, g.maxFees = true, maxFees
	return g, nil
}

// NewGate returns a Normal gate for the joint margin account accountID under
// limits and mode.
func NewGate(accountID string, limits event.MarginLimits, mode string) (*Gate, error) {
	if accountID == "" {
		return nil, errors.New("risklimit: account id is required")
	}
	if err := limits.Validate(); err != nil {
		return nil, fmt.Errorf("risklimit: %w", err)
	}
	if !event.ValidTradingMode(mode) {
		return nil, fmt.Errorf("risklimit: mode %q is not dry-run, paper or live", mode)
	}
	return &Gate{accountID: accountID, limits: limits, mode: mode, decided: make(map[string]bool), state: StateNormal}, nil
}

// RecordDecision records the ID of a journalled strategy.targets.decided
// envelope, which an order's tag must name to be released.
func (g *Gate) RecordDecision(id string) { g.decided[id] = true }

// State returns the gate's engine state.
func (g *Gate) State() string { return g.state }

// RestoreState applies one strategy.engine.state record an earlier run of
// this account journalled, at schemaVersion, before any order is checked: "a
// crash or restart while Degraded or Halted resumes in that same state and
// scope, read back from the run's durable record" (ADR 0019, amended
// 2026-09-24, "The state survives a restart"; ADR 0026, point 5). Leaving
// either state is never automatic, so a record never loosens the gate: a
// Halted gate refuses a Degraded record, and a Degraded record widens the
// scope to the union. A scope naming orders or cash components is refused,
// since this gate holds instruments only and cannot honour it.
func (g *Gate) RestoreState(p event.EngineStatePayload, schemaVersion uint32) error {
	if err := p.ValidateSchema(schemaVersion); err != nil {
		return fmt.Errorf("risklimit: recorded %w", err)
	}
	if len(p.ScopeOrderIDs) > 0 || len(p.ScopeCashComponents) > 0 {
		return errors.New("risklimit: a recorded scope naming orders or cash components is not one this gate can hold")
	}
	if p.State == event.EngineStateHalted {
		g.state, g.scope = StateHalted, nil
		return nil
	}
	if g.state == StateHalted {
		return errors.New("risklimit: a recorded Degraded state cannot follow Halted: leaving Halted is never automatic")
	}
	scope := slices.Clone(g.scope)
	for _, id := range p.ScopeInstrumentIDs {
		if !slices.Contains(scope, id) {
			scope = append(scope, id)
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
// The checks, in order: Halted refuses every order; a tag naming no recorded
// decision refuses and halts; Degraded refuses every buy, every sell that is
// not risk-reducing, and every order on an instrument its scope names; then a
// sell beyond the holding net of working and pending sells, the order count
// (buys only, when the decision time's orders that are not risk-reducing are
// over it), and, unless the order is risk-reducing, the mark, the price band
// and the order size; then for an exposure-increasing order a holding with no
// reference price, the exposure limit, and for an IRA's buy the cash
// available. The first breach refuses the order and degrades the account,
// naming the instrument.
//
// A risk-reducing order (ADR 0026, point 4) is "a sell that moves the
// account's exposure toward 98% or lower, sized within the broker-reported
// holding": here, a sell no larger than the holding the planner sized it
// from. It is exempt from the order count, the order size and the price band
// (point 3), and so from the mark, the band's only input; one late mark
// cannot refuse Boost's exit. It is still refused when it exceeds the book's
// holding net of working and pending sells.
//
// The one sale back to 98% that ADR 0019 allows a margin account while Halted
// (amended 2026-10-15) or Degraded on the instrument to sell (amended
// 2026-10-16) is not issued here: it is sized from IBKR's holding, net
// liquidation and open orders read together, and no such sale goes when
// those cannot be read. This gate reads no open orders, so a Halted account,
// and a Degraded one's named instruments, release nothing.
//
// Exposure is account-wide: the sum of every holding x its reference price
// (the order's own at its reference price) over net liquidation, before and
// after the order (ADR 0026, point 3). Only an order that increases it is
// checked against MaxExposure, so a sale is never refused by that check even
// when the account stays above it. A released order moves the book the next
// order is checked against: its shares, its working sell, and an IRA buy's
// worst-case cost out of the cash available, valued at its reference price.
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
		if !RiskReducing(o.Planned) {
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
				running.Holdings[p.InstrumentID] -= p.Quantity
				running.WorkingSells[p.InstrumentID] += p.Quantity
			} else {
				running.Holdings[p.InstrumentID] += p.Quantity
				running.CashAvailable -= out.Released.Figures.BuyCost
			}
		}
		outcomes = append(outcomes, out)
	}
	return outcomes, nil
}

// RiskReducing reports whether p is risk-reducing (ADR 0026, point 4): a sell
// within the holding it was sized from. No buy is, even within a rebalance
// that also sells.
func RiskReducing(p event.OrderPlannedPayload) bool {
	return p.Side == event.OrderSideSell && p.Quantity <= p.HeldQuantity
}

// check decides one order of a plan of planSize orders at its decision time,
// counted of which are not risk-reducing, against book.
func (g *Gate) check(o Order, planSize, counted int, book *Book) Outcome {
	p := o.Planned
	f, unvalued := g.figures(o, planSize, book)
	switch {
	case g.state == StateHalted:
		return Outcome{Refused: g.refusal(o, f, event.RefusedEngineHalted, "the account is Halted: no order is released until the owner clears it")}
	case !g.decided[p.Tag]:
		out := Outcome{Refused: g.refusal(o, f, event.RefusedUndecidedOrder, fmt.Sprintf("tag %q names no journalled strategy.targets.decided in this run", p.Tag))}
		g.state = StateHalted
		g.scope = nil
		out.State = &event.EngineStatePayload{
			State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached,
			Detail: fmt.Sprintf("order %s refused: %s; a defect in the order path, so its scope cannot be bounded", p.ClientOrderID, event.RefusedUndecidedOrder),
		}
		out.StateSchemaVersion = event.EngineStateScopedVersion
		return out
	case g.state == StateDegraded && (!RiskReducing(p) || slices.Contains(g.scope, p.InstrumentID)):
		// ADR 0019, amended 2026-09-24: "Nothing that opens or adds to a
		// position is allowed while not Normal", and named instruments are
		// frozen; risk-reducing sells continue elsewhere (ADR 0026, point 4).
		return Outcome{Refused: g.refusal(o, f, event.RefusedEngineDegraded, fmt.Sprintf("the account is Degraded (scope %v): only risk-reducing sells go, and the scope's instruments are frozen", g.scope))}
	}
	if reason, detail := g.breach(o, f, counted, book, unvalued); reason != "" {
		out := Outcome{Refused: g.refusal(o, f, reason, detail)}
		if !slices.Contains(g.scope, p.InstrumentID) {
			g.state = StateDegraded
			g.scope = append(g.scope, p.InstrumentID)
			slices.Sort(g.scope)
			out.State = &event.EngineStatePayload{
				State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached,
				Detail:             fmt.Sprintf("order %s refused: %s: %s", p.ClientOrderID, reason, detail),
				ScopeInstrumentIDs: slices.Clone(g.scope),
			}
			out.StateSchemaVersion = event.EngineStateScopedVersion
		}
		return out
	}
	return Outcome{Released: &event.OrderReleasedPayload{
		AccountID: g.accountID, PlanID: o.PlanID, Tag: p.Tag, ClientOrderID: p.ClientOrderID,
		InstrumentID: p.InstrumentID, Side: p.Side, Quantity: p.Quantity, LimitPrice: p.LimitPrice,
		Mode: g.mode, Send: g.mode != event.TradingModeDryRun, Figures: f,
	}}
}

// breach returns the first limit o breaches and its detail, or "" when none.
// counted is the decision time's orders that are not risk-reducing.
func (g *Gate) breach(o Order, f event.RiskFigures, counted int, book *Book, unvalued string) (reason, detail string) {
	p := o.Planned
	increases := f.ExposureAfter > f.ExposureBefore
	available := book.Holdings[p.InstrumentID] - book.WorkingSells[p.InstrumentID]
	switch {
	case p.Side == event.OrderSideSell && p.Quantity > available:
		return event.RefusedSellExceedsHolding, fmt.Sprintf("sell of %d %s exceeds the %d held net of working and pending sells", p.Quantity, p.InstrumentID, available)
	case counted > g.limits.MaxOrdersPerDecision && p.Side == event.OrderSideBuy:
		return event.RefusedOrderCount, fmt.Sprintf("%d orders that are not risk-reducing planned at one decision time; the limit is %d, so the plan's buys are refused", counted, g.limits.MaxOrdersPerDecision)
	case RiskReducing(p):
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
// its share of net liquidation, its mark deviation and, for an IRA's buy, its
// worst-case cost and the cash available. It also returns the first holding,
// by instrument, that has no reference price, or "" when every one has.
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
