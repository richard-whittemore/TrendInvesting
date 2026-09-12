package fills

import (
	"errors"
	"fmt"
	"math"
)

// The two sides an order can rest on. A buy opens or extends a long position
// (an entry or an Add); a sell closes one (a Protective Stop or an
// Exit-Channel exit).
//
// This is deliberately the ORDER's side, not the position's direction —
// the opposite of event.FillPayload.Direction, which is the Campaign's own
// direction held constant across every fill of its life (see that field's
// doc comment). The fill model cares which way the order was pointing,
// because that is what decides which way the range test and the slippage
// run; the reducer cares which position the fill belongs to.
const (
	SideBuy  = "buy"
	SideSell = "sell"
)

// Range is what the fill model reads from one completed bar: the price the
// order was resting from, and the extremes the bar reached.
//
// # Reference, and why it is not simply the open
//
// For an order already resting when the bar opened, Reference IS the bar's
// open: if the level was already beyond the open, the order was triggered at
// the first instant of the bar and executed there (ADR 0005 rule 1 — "a gap
// through the level therefore fills at the open, never at the level"; Faith
// entered "on the open if the market gapped through" [T p.18]).
//
// For an order that came into being from a fill INSIDE this bar — a Unit's
// own Protective Stop, set the moment its entry or Add fill was accepted, or
// the next Add rung, measured from that same fill — the bar's open is the
// wrong reference, and using it would fabricate executions. Such an order did
// not exist at the open, so it cannot have executed there; the earliest
// instant it could have executed is the moment it was created, at the price
// its creating fill executed at. Reference is that price instead.
//
// The difference is not academic. A breakout bar whose open sits more than
// StopMultiple x N below its high sets a Unit-1 stop ABOVE the bar's own
// open. Referenced to the open, that stop would "gap through" and fill at the
// open — a price that occurred before the stop existed, and worse than any
// price the order could actually have got. Referenced to the entry fill, it
// fills at its level, which is what the bar's own range permits.
// TestBarCoveringBothEntryAndStopEntersThenStops is the fixture that fails if
// the two are ever conflated.
type Range struct {
	Reference float64
	High      float64
	Low       float64
}

// Execution is what one resting order did in one bar.
type Execution struct {
	// Filled reports whether the bar reached the order's level at all.
	Filled bool
	// Price is the executed price with slippage already applied against the
	// trader (ADR 0013). Meaningful only when Filled.
	Price float64
	// AtReference reports that the order was already beyond its level at the
	// reference price and so executed there rather than at its level — a gap
	// through. The per-bar protocol uses it to order fills within a bar: an
	// order that executed at the bar's open executed before anything that
	// happened later in the bar. See RunBar.
	AtReference bool
}

// Execute applies ADR 0005's rules 1 and 2 to one resting order.
//
// Rule 1, the coverage test, is ONE-SIDED per side:
//
//	buy  executes iff High >= level      sell executes iff Low <= level
//
// and the executed price before slippage is max(level, Reference) for a buy,
// min(level, Reference) for a sell.
//
// The one-sided form is deliberate and is the only reading that keeps ADR
// 0005's two sentences consistent with each other. "The first bar whose range
// covers its level, boundary inclusive" and "a gap through the level fills at
// the open" are not two independent rules: the second is the case where the
// bar was ALREADY past the level when it opened, which a two-sided
// `Low <= level <= High` test would reject outright. A buy-stop that the
// market gapped above and never traded back down to would then never fill at
// all — precisely the optimism (a breakout missed, so a loss avoided) that
// ADR 0005's three rules exist to prevent. Written one-sided, the test is the
// union of both cases, and max/min against Reference decides which of them
// happened.
//
// Rule 2, slippage, is applied against the trader on every fill: added to a
// buy, subtracted from a sell. A non-positive slippage is refused here as
// well as at the constructor (ADR 0013: "A backtest run with zero slippage is
// invalid by construction"), so there is no route to an unslipped fill.
//
// Every input is checked before it is used, and the function fails closed on
// anything it cannot price (.greptile/rules.md). A sell whose slippage would
// take the executed price to or below zero is one of those: no consumer can
// record a non-positive fill price (event.FillPayload.Validate), so it is an
// error rather than something to clamp.
func Execute(side string, level float64, r Range, slippage float64) (Execution, error) {
	if err := checkPositive("level", level); err != nil {
		return Execution{}, err
	}
	if err := checkPositive("reference", r.Reference); err != nil {
		return Execution{}, err
	}
	if err := checkPositive("high", r.High); err != nil {
		return Execution{}, err
	}
	if err := checkPositive("low", r.Low); err != nil {
		return Execution{}, err
	}
	if r.High < r.Low {
		return Execution{}, fmt.Errorf("fills: bar high %v is below its low %v", r.High, r.Low)
	}
	switch {
	case !isFinite(slippage):
		return Execution{}, errors.New("fills: slippage must be finite")
	case slippage <= 0:
		return Execution{}, fmt.Errorf("fills: slippage must be positive, got %v: a run with zero slippage is invalid by construction (ADR 0013)", slippage)
	}

	var reached bool
	var price float64
	switch side {
	case SideBuy:
		reached = r.High >= level
		price = math.Max(level, r.Reference) + slippage
	case SideSell:
		reached = r.Low <= level
		price = math.Min(level, r.Reference) - slippage
	default:
		return Execution{}, fmt.Errorf("fills: side %q is not a recognised order side", side)
	}
	if !reached {
		return Execution{}, nil
	}
	if price <= 0 {
		return Execution{}, fmt.Errorf("fills: a %s at level %v in a bar referenced at %v would execute at price %v once slippage %v is applied against the trader; a non-positive fill price is not recordable",
			side, level, r.Reference, price, slippage)
	}
	atReference := (side == SideBuy && r.Reference > level) || (side == SideSell && r.Reference < level)
	return Execution{Filled: true, Price: price, AtReference: atReference}, nil
}

// CommissionModel is ADR 0013's commission model: an Interactive-Brokers-style
// per-share schedule with a per-order floor and a per-order ceiling expressed
// as a fraction of the order's own trade value.
//
// It mirrors event.CommissionConfig field for field, and is deliberately its
// own type rather than an alias: internal/event owns the wire contract, this
// package owns the arithmetic that applies it, and neither should have to
// change because the other did. New converts one into the other, in the one
// place the two meet.
type CommissionModel struct {
	PerShare                    float64
	MinimumPerOrder             float64
	MaximumFractionOfTradeValue float64
}

// Charge returns the commission on one order of quantity executed at price.
//
// The order the two bounds are applied in is the whole of the model's
// behaviour, and it is: rate, then floor, then ceiling. The ceiling is a cap
// on the CHARGE, so it overrides the floor rather than the other way round —
// a 100-share order at 50 cents is a 50-dollar trade, and a 1 % cap means 50
// cents, not the 1.00 floor (which would be 2 % of the trade). Applying the
// floor last would quietly make small orders cost more than the published
// schedule says they do, which for a strategy whose own source warns that
// small accounts lose diversification [T p.15] is the wrong direction to be
// wrong in.
//
// Trade value includes the contract multiplier (quantity x price x
// dollarsPerPoint) so a futures contract's cap is computed on the notional
// the multiplier implies rather than on the quoted price (#10; #63 records
// that the multiplier belongs to the instrument rather than the strategy
// configuration).
func (m CommissionModel) Charge(quantity int64, price, dollarsPerPoint float64) (float64, error) {
	if quantity <= 0 {
		return 0, fmt.Errorf("fills: commission quantity must be a positive whole number, got %d", quantity)
	}
	if err := checkPositive("price", price); err != nil {
		return 0, err
	}
	if err := checkPositive("dollars per point", dollarsPerPoint); err != nil {
		return 0, err
	}
	switch {
	case !isFinite(m.PerShare):
		return 0, errors.New("fills: commission per share must be finite")
	case m.PerShare < 0:
		return 0, fmt.Errorf("fills: commission per share must not be negative, got %v", m.PerShare)
	}
	switch {
	case !isFinite(m.MinimumPerOrder):
		return 0, errors.New("fills: commission minimum per order must be finite")
	case m.MinimumPerOrder < 0:
		return 0, fmt.Errorf("fills: commission minimum per order must not be negative, got %v", m.MinimumPerOrder)
	}
	switch {
	case !isFinite(m.MaximumFractionOfTradeValue):
		return 0, errors.New("fills: commission maximum fraction of trade value must be finite")
	case m.MaximumFractionOfTradeValue <= 0 || m.MaximumFractionOfTradeValue > 1:
		return 0, fmt.Errorf("fills: commission maximum fraction of trade value must be greater than zero and at most one, got %v", m.MaximumFractionOfTradeValue)
	}

	charge := float64(quantity) * m.PerShare
	if charge < m.MinimumPerOrder {
		charge = m.MinimumPerOrder
	}
	if ceiling := float64(quantity) * price * dollarsPerPoint * m.MaximumFractionOfTradeValue; charge > ceiling {
		charge = ceiling
	}
	return charge, nil
}

func checkPositive(name string, v float64) error {
	switch {
	case !isFinite(v):
		return fmt.Errorf("fills: %s must be finite", name)
	case v <= 0:
		return fmt.Errorf("fills: %s must be positive, got %v", name, v)
	}
	return nil
}

// isFinite reports whether v is neither NaN nor infinite, mirroring
// internal/event's own guard: a NaN that passed validation would satisfy
// every ordered comparison silently and poison a fill price.
func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
