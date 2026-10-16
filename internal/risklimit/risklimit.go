// Package risklimit is the hard-limit gate outside the strategy (ADR 0026,
// point 3): every strategy.order.planned gets risk.order.released or
// risk.order.refused, and a breach moves the account to Degraded or Halted
// through strategy.engine.state. It holds the account's engine state and its
// scope, the kill switch, the latest broker snapshot and the margin-cushion
// alarm, and decides when ADR 0019's one sale back to 98% goes (ADR 0026,
// point 4). It is pure: everything it checks arrives as an argument.
package risklimit

import (
	"errors"
	"fmt"
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

// The account kinds. ADR 0019's one sale back to 98% and the margin-cushion
// alarm belong to a margin account only; an IRA never borrows (plan.md
// section 5).
const (
	KindMargin = "margin"
	KindIRA    = "ira"
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

// Gate checks one account's planned orders against its limits and holds the
// account's engine state between decisions.
type Gate struct {
	accountID string
	kind      string
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
}

// NewGate returns a Normal gate for the margin account accountID under
// limits and mode.
func NewGate(accountID string, limits event.MarginLimits, mode string) (*Gate, error) {
	return NewAccountGate(KindMargin, accountID, limits, mode)
}

// NewAccountGate returns a Normal gate for an account of kind (KindMargin or
// KindIRA) under limits and mode.
func NewAccountGate(kind, accountID string, limits event.MarginLimits, mode string) (*Gate, error) {
	if kind != KindMargin && kind != KindIRA {
		return nil, fmt.Errorf("risklimit: account kind %q is not margin or ira", kind)
	}
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
		accountID: accountID, kind: kind, limits: limits, mode: mode, decided: make(map[string]bool),
		state: StateNormal, alarmLevel: event.MarginAlarmClear,
		released: make(map[string]bool), saleTags: make(map[string]bool),
		attempted: make(map[string]bool), pending: make(map[string]bool),
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

// Check decides every order of one decision time's plan, in plan order. An
// order that fails its own payload validation, or belongs to another account,
// is a defect in the caller and returns an error with no outcome and no state
// change; an untagged order is a refusal, not an error, and halts.
//
// The checks, in order (ADR 0026, points 3 and 4):
//   - a sale back to 98% that this gate issued (Sale) goes in every state
//     and through the kill switch, once, if it is still within the broker's
//     holding net of every working sell; one it never issued halts;
//   - Halted refuses every other order;
//   - a tag naming no recorded decision refuses and halts;
//   - the kill switch refuses every other order and changes no state;
//   - Degraded refuses every buy and every order on an instrument its scope
//     names, and a sell elsewhere with no broker snapshot to size it from;
//   - a sell beyond the latest broker snapshot's holding net of every
//     working or pending sell is a breach;
//   - then, unless the order is risk-reducing, the order count (buys only,
//     when the plan's other orders are over it), the mark, the price band and
//     the order size; and the pre-trade exposure limit.
//
// A breach refuses the order and degrades the account, naming the
// instrument.
//
// A risk-reducing order (ADR 0026, point 4) is a sell within the latest
// broker snapshot's holding net of every working or pending sell, which moves
// exposure toward 98% or lower. It is exempt from the order count, size and
// price band, and is not counted against the count.
//
// Exposure is the order's own instrument's holding x the reference price over
// net liquidation, before and after the order: the joint account holds SPY
// only. Only an order that increases it is checked against MaxExposure, so a
// sale is never refused by that check even when the account stays above it.
func (g *Gate) Check(plan []Order) ([]Outcome, error) {
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
	counted := 0
	for _, o := range plan {
		if !g.riskReducing(o.Planned) {
			counted++
		}
	}
	outcomes := make([]Outcome, 0, len(plan))
	for _, o := range plan {
		outcomes = append(outcomes, g.check(o, len(plan), counted))
	}
	return outcomes, nil
}

func (g *Gate) check(o Order, planSize, counted int) Outcome {
	p := o.Planned
	f := g.figures(o, planSize)
	if p.Sale != (event.SaleFigures{}) {
		return g.checkSale(o, f)
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
		return Outcome{Refused: g.refusal(o, f, event.RefusedEngineDegraded, fmt.Sprintf("the account is Degraded (scope %v): no buy, and the scope's instruments are frozen", g.scope))}
	case g.state == StateDegraded && !f.BrokerSet:
		return Outcome{Refused: g.refusal(o, f, event.RefusedBrokerHolding, "the account is Degraded and no readable broker snapshot holds the quantity to size this sell from")}
	}
	if reason, detail := g.breach(o, f, counted); reason != "" {
		out := Outcome{Refused: g.refusal(o, f, reason, detail)}
		g.degrade(&out, p.InstrumentID, fmt.Sprintf("order %s refused: %s: %s", p.ClientOrderID, reason, detail), p.ClientOrderID)
		return out
	}
	return g.release(o, f)
}

// release releases o with figures f.
func (g *Gate) release(o Order, f event.RiskFigures) Outcome {
	p := o.Planned
	g.released[p.ClientOrderID] = true
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
	}, event.EngineStateSchemaVersion, cause)
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
// counted is the plan's orders that are not risk-reducing.
func (g *Gate) breach(o Order, f event.RiskFigures, counted int) (reason, detail string) {
	p := o.Planned
	held := f.BrokerHolding - f.WorkingSells
	switch {
	case p.Side == event.OrderSideSell && f.BrokerSet && p.Quantity > held:
		return event.RefusedBrokerHolding, fmt.Sprintf("sell of %d exceeds the broker's holding %d net of %d working sells (snapshot %s)", p.Quantity, f.BrokerHolding, f.WorkingSells, g.brokerID)
	case f.RiskReducing:
		return "", ""
	case counted > g.limits.MaxOrdersPerDecision && p.Side == event.OrderSideBuy:
		return event.RefusedOrderCount, fmt.Sprintf("%d orders that are not risk-reducing planned at one decision time; the limit is %d, so the plan's buys are refused", counted, g.limits.MaxOrdersPerDecision)
	case !o.MarkSet:
		return event.RefusedMissingMark, fmt.Sprintf("no mark recorded for %s at or after the session's close", p.InstrumentID)
	case f.MarkDeviation > g.limits.PriceBand:
		return event.RefusedPriceBand, fmt.Sprintf("reference price %v is %v from mark %v; the band is %v", p.ReferencePrice, f.MarkDeviation, o.Mark, g.limits.PriceBand)
	case f.OrderFraction > g.limits.MaxOrderFraction:
		return event.RefusedOrderSize, fmt.Sprintf("order is %v of net liquidation; the limit is %v", f.OrderFraction, g.limits.MaxOrderFraction)
	case math.Abs(f.ExposureAfter) > math.Abs(f.ExposureBefore) && f.ExposureAfter > g.limits.MaxExposure:
		return event.RefusedMarginExposure, fmt.Sprintf("exposure after the order %v exceeds %v (before %v)", f.ExposureAfter, g.limits.MaxExposure, f.ExposureBefore)
	}
	return "", ""
}

// figures are the values o is checked against, with the latest broker
// snapshot's holding and working sells when one is readable.
func (g *Gate) figures(o Order, planSize int) event.RiskFigures {
	p := o.Planned
	after := p.HeldQuantity + p.Quantity
	if p.Side == event.OrderSideSell {
		after = p.HeldQuantity - p.Quantity
	}
	f := event.RiskFigures{
		NetLiquidation:   p.NetLiquidation,
		ReferencePrice:   p.ReferencePrice,
		ExposureBefore:   float64(p.HeldQuantity) * p.ReferencePrice / p.NetLiquidation,
		ExposureAfter:    math.Abs(float64(after) * p.ReferencePrice / p.NetLiquidation),
		OrderFraction:    float64(p.Quantity) * p.ReferencePrice / p.NetLiquidation,
		OrdersInDecision: planSize,
	}
	if o.MarkSet {
		f.Mark, f.MarkSet = o.Mark, true
		f.MarkDeviation = math.Abs(p.ReferencePrice-o.Mark) / o.Mark
	}
	if holding, working, ok := g.brokerHolding(p.InstrumentID); ok {
		f.BrokerSet, f.BrokerHolding, f.WorkingSells = true, holding, working
		f.RiskReducing = g.riskReducing(p)
	}
	return f
}

func (g *Gate) refusal(o Order, f event.RiskFigures, reason, detail string) *event.OrderRefusedPayload {
	p := o.Planned
	return &event.OrderRefusedPayload{
		AccountID: g.accountID, PlanID: o.PlanID, Tag: p.Tag, ClientOrderID: p.ClientOrderID,
		InstrumentID: p.InstrumentID, Side: p.Side, Quantity: p.Quantity, LimitPrice: p.LimitPrice,
		Reason: reason, Detail: detail, Figures: f,
	}
}
