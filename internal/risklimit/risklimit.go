// Package risklimit is the hard-limit gate outside the strategy (ADR 0026,
// point 3): every strategy.order.planned gets risk.order.released or
// risk.order.refused, and a breach moves the account to Degraded or Halted
// through strategy.engine.state. Only the joint margin account's limits are
// checked here. It is pure: everything it checks arrives as an argument.
package risklimit

import (
	"errors"
	"fmt"
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

// Gate checks one account's planned orders against its limits and holds the
// account's engine state between decisions.
type Gate struct {
	accountID string
	limits    event.MarginLimits
	mode      string
	decided   map[string]bool
	state     string
	scope     []string
}

// NewGate returns a Normal gate for accountID under limits and mode.
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

// Check decides every order of one decision time's plan, in plan order. An
// order that fails its own payload validation, or belongs to another account,
// is a defect in the caller and returns an error with no outcome and no state
// change; an untagged order is a refusal, not an error, and halts.
//
// The checks, in order: Halted refuses every order; a tag naming no recorded
// decision refuses and halts; Degraded refuses every buy, every sell that is
// not risk-reducing, and every order on an instrument its scope names; then
// the order count (buys only, when the plan's orders that are not
// risk-reducing are over it), and, unless the order is risk-reducing, the
// mark, the price band and the order size; then the pre-trade exposure
// limit. The first breach refuses the order and degrades the account, naming
// the instrument.
//
// A risk-reducing order (ADR 0026, point 4) is "a sell that moves the
// account's exposure toward 98% or lower, sized within the broker-reported
// holding": here, a sell no larger than the holding the planner sized it
// from. It is exempt from the order count, the order size and the price band
// (point 3), and so from the mark, the band's only input; one late mark
// cannot refuse Boost's exit. The holding is the account snapshot's, which
// reports no working orders, so no working sell is netted from it.
//
// The one sale back to 98% that ADR 0019 allows a margin account while Halted
// (amended 2026-10-15) or Degraded on the instrument to sell (amended
// 2026-10-16) is not issued here: it is sized from IBKR's holding, net
// liquidation and open orders read together, and no such sale goes when
// those cannot be read. This gate reads no open orders, so a Halted account,
// and a Degraded one's named instruments, release nothing.
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
		if !riskReducing(o.Planned) {
			counted++
		}
	}
	outcomes := make([]Outcome, 0, len(plan))
	for _, o := range plan {
		outcomes = append(outcomes, g.check(o, len(plan), counted))
	}
	return outcomes, nil
}

// riskReducing reports whether p is risk-reducing (ADR 0026, point 4): a sell
// within the holding it was sized from. No buy is, even within a rebalance
// that also sells.
func riskReducing(p event.OrderPlannedPayload) bool {
	return p.Side == event.OrderSideSell && p.Quantity <= p.HeldQuantity
}

// check decides one order of a plan of planSize orders, counted of which are
// not risk-reducing.
func (g *Gate) check(o Order, planSize, counted int) Outcome {
	p := o.Planned
	f := figures(o, planSize)
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
	case g.state == StateDegraded && (!riskReducing(p) || slices.Contains(g.scope, p.InstrumentID)):
		// ADR 0019, amended 2026-09-24: "Nothing that opens or adds to a
		// position is allowed while not Normal", and named instruments are
		// frozen; risk-reducing sells continue elsewhere (ADR 0026, point 4).
		return Outcome{Refused: g.refusal(o, f, event.RefusedEngineDegraded, fmt.Sprintf("the account is Degraded (scope %v): only risk-reducing sells go, and the scope's instruments are frozen", g.scope))}
	}
	if reason, detail := g.breach(o, f, counted); reason != "" {
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
// counted is the plan's orders that are not risk-reducing.
func (g *Gate) breach(o Order, f event.RiskFigures, counted int) (reason, detail string) {
	p := o.Planned
	switch {
	case counted > g.limits.MaxOrdersPerDecision && p.Side == event.OrderSideBuy:
		return event.RefusedOrderCount, fmt.Sprintf("%d orders that are not risk-reducing planned at one decision time; the limit is %d, so the plan's buys are refused", counted, g.limits.MaxOrdersPerDecision)
	case riskReducing(p):
		// Exempt from the mark, band and size (ADR 0026, point 3); a sell
		// never breaches the exposure limit, which checks only an increase.
		return "", ""
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

func figures(o Order, planSize int) event.RiskFigures {
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
