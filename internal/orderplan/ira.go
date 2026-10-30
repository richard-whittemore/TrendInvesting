package orderplan

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// IRAInput is what an IRA's rebalance plans from: the decision and the ID of
// the envelope that carried it, the account snapshot as of the previous close,
// each held or targeted instrument's raw previous close (ADR 0004), the
// configured price band and rebalance band, and the shares of each
// instrument already committed to working or pending sells, cancel-pending
// ones included, since they may still execute.
type IRAInput struct {
	Tag             string
	Decision        event.TargetsDecidedPayload
	Snapshot        event.AccountPositionsSnapshotPayload
	ReferencePrices map[string]float64
	PriceBand       float64
	RebalanceBand   float64
	WorkingSells    map[string]int64
	// QuantityScale is zero for D7-L or 10000 for D7-F ticks (ADR 0026).
	QuantityScale       int64
	FractionalIncrement string
}

// IRAPlan is a rebalance's two phases, each in ascending instrument ID. Buys
// are sized from the previous close and not yet budgeted: they are planned
// only once every sell is terminal, or at once when there are no sells
// (ADR 0026, point 2, "IRA execution"); ScaleBuys budgets them.
//
// Skipped records each trade the plan sized but cannot order, because
// rounding toward zero to the configured share increment leaves none, or a sell's holding is
// all in working sells: no planned trade disappears silently.
type IRAPlan struct {
	Sells   []event.OrderPlannedPayload
	Buys    []event.OrderPlannedPayload
	Skipped []event.OrderSkippedPayload
}

// PlanIRA plans an IRA's momentum rebalance (ADR 0026, point 2, "Momentum
// and bond book"; defensive_v2.py:383-405), valued at net liquidation V,
// holdings q and raw previous closes p:
//  1. the union of held and targeted instruments, in ascending instrument ID;
//  2. held with no target (or a target of 0): sell the whole holding;
//  3. not held, with a target T: buy T x V / p shares, rounded down to the configured increment;
//  4. held with a target: trade only if |q x p / V - T| > band x T, strictly,
//     then (T x V - q x p) / p shares, rounded toward zero to the increment;
//  5. no sell exceeds the holding minus the working and pending sells;
//  6. sells first, then buys.
//
// A trade that rounds to no share, or a sell with nothing left after the
// working sells, is recorded in Skipped with the shares the rule asked for.
//
// D7-F uses the configured fractional increment; D7-L rounds to whole shares.
// Whole shares rounded toward zero are D7-L's rounding; D7-L's substitution of
// a pick whose one share exceeds its target is the momentum rule's, not this.
// A held pick the decision records kept one lot short
// (event.MomentumStatusHeldOneLotShort) gets no order, no buy and no sell,
// and no skip: the decision itself records why (ADR 0026, point 2, "D7-L",
// amended 2026-10-18). A kept name the snapshot does not hold contradicts
// the decision and is an error.
// A decision that is not a decided momentum decision, a snapshot of another
// account, a short holding or an instrument without a positive reference
// price is a defect in the caller and an error.
func PlanIRA(in IRAInput) (IRAPlan, error) {
	d := in.Decision
	if err := d.Validate(); err != nil {
		return IRAPlan{}, fmt.Errorf("orderplan: %w", err)
	}
	if err := in.Snapshot.Validate(); err != nil {
		return IRAPlan{}, fmt.Errorf("orderplan: %w", err)
	}
	switch {
	case in.Tag == "":
		return IRAPlan{}, errors.New("orderplan: an order must be tagged with the decision it serves")
	case d.StrategyID != event.TargetWeightStrategyMomentum || d.Outcome != event.TargetsOutcomeDecided:
		return IRAPlan{}, fmt.Errorf("orderplan: a %s %s decision is not a momentum rebalance", d.Outcome, d.StrategyID)
	case in.Snapshot.AccountID != d.AccountID:
		return IRAPlan{}, fmt.Errorf("orderplan: snapshot of account %q cannot value a decision for %q", in.Snapshot.AccountID, d.AccountID)
	case !(in.PriceBand > 0 && in.PriceBand < 1) || !(in.RebalanceBand > 0 && in.RebalanceBand < 1):
		return IRAPlan{}, errors.New("orderplan: the price band and the rebalance band must lie strictly between 0 and 1")
	}
	if in.QuantityScale != 0 && in.QuantityScale != event.FractionalQuantityScale {
		return IRAPlan{}, errors.New("orderplan: unsupported quantity scale")
	}
	if d.QuantityScale != in.QuantityScale {
		return IRAPlan{}, errors.New("orderplan: decision and planner quantity scales differ")
	}
	if in.Snapshot.QuantityScale != in.QuantityScale {
		return IRAPlan{}, errors.New("orderplan: snapshot and planner quantity scales differ")
	}
	if in.QuantityScale == 0 && in.FractionalIncrement != "" {
		return IRAPlan{}, errors.New("orderplan: whole shares carry no fractional increment")
	}
	kept := map[string]bool{}
	for _, n := range d.Momentum.Names {
		if n.Status == event.MomentumStatusHeldOneLotShort {
			if in.Snapshot.Quantity(n.InstrumentID) < 1 {
				return IRAPlan{}, fmt.Errorf("orderplan: %s is kept one lot short, but the snapshot holds none of it", n.InstrumentID)
			}
			kept[n.InstrumentID] = true
		}
	}
	targets := make(map[string]float64, len(d.Targets))
	union := make([]string, 0, len(d.Targets)+len(in.Snapshot.Positions))
	for _, t := range d.Targets {
		targets[t.InstrumentID] = t.Weight
		union = append(union, t.InstrumentID)
	}
	for _, p := range in.Snapshot.Positions {
		if p.Quantity < 0 {
			return IRAPlan{}, fmt.Errorf("orderplan: account holds %d %s; the planner never plans against a short", p.Quantity, p.InstrumentID)
		}
		if _, ok := targets[p.InstrumentID]; !ok {
			union = append(union, p.InstrumentID)
		}
	}
	slices.Sort(union)
	if in.QuantityScale != 0 {
		return planFractionalIRA(in, targets, union)
	}

	v := in.Snapshot.NetLiquidation
	var plan IRAPlan
	for _, id := range union {
		if kept[id] {
			continue // D7-L's held-name exception: the holding is kept as it is
		}
		price := in.ReferencePrices[id]
		if !(price > 0) || math.IsInf(price, 0) {
			return IRAPlan{}, fmt.Errorf("orderplan: %s has no finite, positive reference price", id)
		}
		held, target := in.Snapshot.Quantity(id), targets[id]
		var shares float64
		switch {
		case target == 0 && held == 0:
			continue // a zero target not held: nothing to trade
		case target == 0:
			shares = -float64(held)
		case held == 0:
			shares = float64(target*v) / price
		default:
			value := float64(float64(held) * price)
			if !(math.Abs(value/v-target) > float64(in.RebalanceBand*target)) {
				continue
			}
			shares = (float64(target*v) - value) / price
		}
		delta := math.Trunc(shares)
		if math.Abs(delta) >= math.MaxInt64 {
			return IRAPlan{}, fmt.Errorf("orderplan: %v shares of %s cannot be ordered", delta, id)
		}
		quantity := int64(delta)
		side := event.OrderSideBuy
		if shares < 0 {
			side = event.OrderSideSell
			quantity = min(-quantity, held-in.WorkingSells[id])
		}
		if quantity <= 0 {
			reason := event.OrderSkippedBelowOneShare
			if side == event.OrderSideSell && delta != 0 {
				reason = event.OrderSkippedHeldByWorkingSells
			}
			plan.Skipped = append(plan.Skipped, event.OrderSkippedPayload{
				AccountID: d.AccountID, Tag: in.Tag, ClientOrderID: ClientOrderID(in.Tag, id, side),
				InstrumentID: id, Side: side, Phase: side, Reason: reason, Shares: math.Abs(shares),
				TargetWeight: target, NetLiquidation: v, HeldQuantity: held, ReferencePrice: price,
			})
			continue
		}
		order := event.OrderPlannedPayload{
			AccountID: d.AccountID, Tag: in.Tag, ClientOrderID: ClientOrderID(in.Tag, id, side),
			InstrumentID: id, Side: side, Phase: side, OrderType: event.OrderTypeLimit, Quantity: quantity,
			LimitPrice: LimitPrice(side, price, in.PriceBand), TargetWeight: target,
			NetLiquidation: v, HeldQuantity: held, ReferencePrice: price, PriceBand: in.PriceBand,
		}
		if side == event.OrderSideSell {
			plan.Sells = append(plan.Sells, order)
		} else {
			plan.Buys = append(plan.Buys, order)
		}
	}
	return plan, nil
}

// BuyCost is a buy's worst-case cost: quantity at the band's limit price,
// rounded up to cents for fractional orders, plus maximum fees and the order's
// explicit rounding slack (ADR 0026, "Per-execution rounding reserve").
// Whole-share orders retain their original float arithmetic. Invalid costs
// return +Inf so budget comparisons fail closed.
// Buys worth $980 at the previous close cost
// $1,029 at a 5% band before fees.
func BuyCost(o event.OrderPlannedPayload, maxFees float64) float64 {
	cost, err := event.BuyReservation(o.Quantity, o.QuantityScale, o.LimitPrice, maxFees, o.RoundingSlack)
	if err != nil {
		return math.Inf(1)
	}
	value, _ := cost.Float64()
	return value
}

// ScaleBuys fits buys to budget (ADR 0026, point 2, "Scaling"): when their
// worst-case total exceeds it, every buy's quantity is scaled by
// budget / total and rounded down to its increment again; a buy with no increment is
// recorded as skipped. It returns the buys kept, in their order, and the scale applied (1
// when they fit).
//
// The fees do not scale with the quantity, so with a nonzero fee allowance
// the ADR's budget / total can still leave the rounded total above the
// budget. Then the scale is (budget - n x fees) / (sum of quantity x limit)
// over the n buys, which always fits. With no budget, no buy is kept.
//
// This scaling is not in the backtest; the parity rerun measures it.
//
// A buy scaled to no share is returned in skipped, with the shares it was
// scaled to, so it is recorded rather than dropped silently.
// Fractional buys carry the configured RoundingSlack before this call; the
// entire original basket's slack is withheld before sizing its quantities.
func ScaleBuys(buys []event.OrderPlannedPayload, budget, maxFees float64) (kept []event.OrderPlannedPayload, skipped []event.OrderSkippedPayload, scale float64) {
	for _, b := range buys {
		if b.QuantityScale != 0 {
			return scaleFractionalBuys(buys, budget, maxFees)
		}
	}
	total, atLimit := 0.0, 0.0
	for _, b := range buys {
		total += BuyCost(b, maxFees)
		atLimit += event.QuantityValue(b.Quantity, b.QuantityScale, b.LimitPrice)
	}
	if total <= budget {
		return slices.Clone(buys), nil, 1
	}
	scale = max(budget/total, 0)
	kept, cost := scaled(buys, scale, maxFees)
	if cost > budget {
		scale = max((budget-float64(float64(len(buys))*maxFees))/atLimit, 0)
		kept, _ = scaled(buys, scale, maxFees)
	}
	for _, b := range buys {

		if shares := float64(b.Quantity) * scale; math.Floor(shares) < 1 {
			skipped = append(skipped, event.OrderSkippedPayload{
				AccountID: b.AccountID, Tag: b.Tag, ClientOrderID: b.ClientOrderID, InstrumentID: b.InstrumentID,
				Side: b.Side, Phase: b.Phase, Reason: event.OrderSkippedScaledBelowOneShare, Shares: shares, Scale: scale,
				TargetWeight: b.TargetWeight, NetLiquidation: b.NetLiquidation, HeldQuantity: b.HeldQuantity, ReferencePrice: b.ReferencePrice,
			})
		}
	}
	return kept, skipped, scale
}

// scaled returns buys with each quantity scaled by scale and rounded down,
// dropping those left with no share, and their worst-case total.
func scaled(buys []event.OrderPlannedPayload, scale, maxFees float64) (kept []event.OrderPlannedPayload, total float64) {
	for _, b := range buys {
		b.Quantity = int64(math.Floor(float64(b.Quantity) * scale))
		if b.Quantity > 0 {
			kept = append(kept, b)
			total += BuyCost(b, maxFees)
		}
	}
	return kept, total
}

// Budget is the cash an IRA's buy phase may still commit (ADR 0026, point 2,
// "Budget" and "Carried forward"): the starting cash under the D5 rule, plus
// the sell proceeds that rule counts, minus cash a newer snapshot shows gone,
// minus what each released buy commits.
// A working buy commits its worst-case cost, or what its fills have spent if
// that is more; once it is terminal it commits only what it spent, so a
// partial fill releases its unspent part only when the rest is cancelled.
type Budget struct {
	start    float64
	proceeds float64
	removed  float64
	buys     []*committedBuy // in release order, so the sum is deterministic
}

type committedBuy struct {
	clientOrderID string
	worstCost     float64
	spent         float64
	terminal      bool
}

// NewBudget returns a budget starting from cash.
func NewBudget(cash float64) *Budget { return &Budget{start: cash} }

// Remaining is the cash still uncommitted.
func (b *Budget) Remaining() float64 {
	remaining := b.start + b.proceeds - b.removed
	for _, c := range b.buys {
		committed := c.spent
		if !c.terminal {
			committed = max(c.worstCost, c.spent)
		}
		remaining -= committed
	}
	return remaining
}

// AddProceeds counts a sell's realised proceeds, net of its commission.
func (b *Budget) AddProceeds(amount float64) { b.proceeds += amount }

// Remove takes out cash the account no longer has: what a newer account
// snapshot shows missing beyond what the recorded fills explain (a fee or a
// transfer). The buys are never funded from cash that is gone.
func (b *Budget) Remove(amount float64) { b.removed += amount }

// Release commits a released buy's worst-case cost.
func (b *Budget) Release(clientOrderID string, worstCost float64) {
	b.buys = append(b.buys, &committedBuy{clientOrderID: clientOrderID, worstCost: worstCost})
}

// Fill records what one execution of a released buy spent, commission
// included. It reports false for a buy this budget never released.
func (b *Budget) Fill(clientOrderID string, spent float64) bool {
	c := b.find(clientOrderID)
	if c != nil {
		c.spent += spent
	}
	return c != nil
}

// Close marks a released buy terminal, so it commits only what it spent. It
// reports false for a buy this budget never released.
func (b *Budget) Close(clientOrderID string) bool {
	c := b.find(clientOrderID)
	if c != nil {
		c.terminal = true
	}
	return c != nil
}

func (b *Budget) find(clientOrderID string) *committedBuy {
	for _, c := range b.buys {
		if c.clientOrderID == clientOrderID {
			return c
		}
	}
	return nil
}

// planFractionalIRA applies ADR 0026 D7-F to exact decimal monetary inputs.
// Quantities stay integer ticks; only monetary band comparisons are floats.
func planFractionalIRA(in IRAInput, targets map[string]float64, union []string) (IRAPlan, error) {
	if in.FractionalIncrement == "" {
		in.FractionalIncrement = "0.0001"
	}
	increment, err := event.FractionalIncrementTicks(in.FractionalIncrement)
	if err != nil {
		return IRAPlan{}, fmt.Errorf("orderplan: %w", err)
	}
	var plan IRAPlan
	for _, id := range union {
		if in.WorkingSells[id] < 0 {
			return IRAPlan{}, fmt.Errorf("orderplan: working sells of %s must not be negative", id)
		}
		price := in.ReferencePrices[id]
		if !(price > 0) || math.IsInf(price, 0) {
			return IRAPlan{}, fmt.Errorf("orderplan: %s has no finite, positive reference price", id)
		}
		held, target := in.Snapshot.Quantity(id), targets[id]
		if target == 0 && held == 0 {
			continue
		}
		if target > 0 && held > 0 {
			value := event.QuantityValue(held, in.QuantityScale, price)
			if !(math.Abs(value/in.Snapshot.NetLiquidation-target) > float64(in.RebalanceBand*target)) {
				continue
			}
		}
		value := new(big.Rat).Mul(moneyRat(target), moneyRat(in.Snapshot.NetLiquidation))
		heldValue := new(big.Rat).Mul(new(big.Rat).SetFrac64(held, in.QuantityScale), moneyRat(price))
		value.Sub(value, heldValue)
		side := event.OrderSideBuy
		if value.Sign() < 0 {
			side = event.OrderSideSell
			value.Neg(value)
		}
		ticks := new(big.Rat).Quo(value, moneyRat(price))
		ticks.Mul(ticks, new(big.Rat).SetInt64(in.QuantityScale))
		quantity, ok := floorIncrement(ticks, increment)
		if !ok {
			return IRAPlan{}, fmt.Errorf("orderplan: quantity of %s cannot be ordered", id)
		}
		reason := event.OrderSkippedBelowIncrement
		if side == event.OrderSideSell {
			available := max(held-in.WorkingSells[id], 0)
			if quantity > 0 && available < increment && in.WorkingSells[id] > 0 {
				reason = event.OrderSkippedHeldByWorkingSells
			}
			quantity = min(quantity, available/increment*increment)
		}
		order := event.OrderPlannedPayload{
			AccountID: in.Decision.AccountID, Tag: in.Tag, ClientOrderID: ClientOrderID(in.Tag, id, side), InstrumentID: id,
			Side: side, Phase: side, OrderType: event.OrderTypeLimit, Quantity: quantity, QuantityScale: in.QuantityScale,
			FractionalIncrement: in.FractionalIncrement, LimitPrice: LimitPrice(side, price, in.PriceBand), TargetWeight: target,
			NetLiquidation: in.Snapshot.NetLiquidation, HeldQuantity: held, ReferencePrice: price, PriceBand: in.PriceBand,
		}
		if quantity == 0 {
			plan.Skipped = append(plan.Skipped, fractionalSkip(order, reason, value, 0))
			continue
		}
		if side == event.OrderSideSell {
			plan.Sells = append(plan.Sells, order)
		} else {
			plan.Buys = append(plan.Buys, order)
		}
	}
	return plan, nil
}

// moneyRat interprets a validated monetary float's shortest decimal exactly
// for ADR 0026 D7-F sizing; it never converts a quantity to floating point.
func moneyRat(value float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(value, 'g', -1, 64))
	return r
}

// floorIncrement rounds positive ticks down to the D7-F increment, refusing
// an int64 overflow before a capital quantity can wrap (ADR 0026).
func floorIncrement(ticks *big.Rat, increment int64) (int64, bool) {
	units := new(big.Rat).Quo(ticks, new(big.Rat).SetInt64(increment))
	n := new(big.Int).Quo(units.Num(), units.Denom())
	n.Mul(n, big.NewInt(increment))
	return n.Int64(), n.IsInt64()
}

// fractionalSkip records the monetary request that rounded below an
// increment, with the fixed-point holding that constrained it (ADR 0026).
func fractionalSkip(order event.OrderPlannedPayload, reason string, requested *big.Rat, scale float64) event.OrderSkippedPayload {
	return event.OrderSkippedPayload{
		AccountID: order.AccountID, Tag: order.Tag, ClientOrderID: order.ClientOrderID, InstrumentID: order.InstrumentID,
		Side: order.Side, Phase: order.Phase, Reason: reason, QuantityScale: order.QuantityScale, FractionalIncrement: order.FractionalIncrement,
		RequestedValue: finiteDecimal(requested), Scale: scale, TargetWeight: order.TargetWeight, NetLiquidation: order.NetLiquidation,
		HeldQuantity: order.HeldQuantity, ReferencePrice: order.ReferencePrice,
	}
}

// finiteDecimal writes exact requested monetary evidence for a skipped D7-F
// order; every input denominator is a power of ten (ADR 0026, ADR 0015).
func finiteDecimal(value *big.Rat) string {
	denominator := new(big.Int).Set(value.Denom())
	counts := [2]int{}
	for i, factor := range []int64{2, 5} {
		divisor := big.NewInt(factor)
		for new(big.Int).Mod(denominator, divisor).Sign() == 0 {
			denominator.Quo(denominator, divisor)
			counts[i]++
		}
	}
	text := value.FloatString(max(counts[0], counts[1]))
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return text
}

// scaleFractionalBuys applies ADR 0026's budget/total rule and fee correction
// as exact decimal ratios before rounding quantities down to their increments.
// Inputs must be validated fractional orders; mixed units fail closed.
func scaleFractionalBuys(buys []event.OrderPlannedPayload, budget, maxFees float64) ([]event.OrderPlannedPayload, []event.OrderSkippedPayload, float64) {
	if math.IsNaN(budget) || math.IsInf(budget, 0) || math.IsNaN(maxFees) || math.IsInf(maxFees, 0) || maxFees < 0 {
		return nil, nil, 0
	}
	atLimit, slack := new(big.Rat), new(big.Rat)
	increments := make([]int64, len(buys))
	for i, b := range buys {
		increment, err := event.FractionalIncrementTicks(b.FractionalIncrement)
		if b.QuantityScale != event.FractionalQuantityScale || b.Quantity <= 0 || !(b.LimitPrice > 0) || math.IsInf(b.LimitPrice, 0) || err != nil || b.Quantity%increment != 0 {
			return nil, nil, 0
		}
		increments[i] = increment
		if b.RoundingSlack != 0 && !event.ValidRoundingSlack(b.RoundingSlack) {
			return nil, nil, 0
		}
		slack.Add(slack, moneyRat(b.RoundingSlack))
		cost, err := event.BuyReservation(b.Quantity, b.QuantityScale, b.LimitPrice, maxFees, b.RoundingSlack)
		if err != nil {
			return nil, nil, 0
		}
		cost.Sub(cost, moneyRat(maxFees))
		cost.Sub(cost, moneyRat(b.RoundingSlack))
		atLimit.Add(atLimit, cost)
	}
	fees := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(len(buys))), moneyRat(maxFees))
	total := new(big.Rat).Add(atLimit, fees)
	ceiling := max(budget, 0)
	cash := moneyRat(ceiling)
	sizingCash := new(big.Rat).Sub(cash, slack)
	if sizingCash.Sign() < 0 {
		sizingCash.SetInt64(0)
	}
	ratio := new(big.Rat).Quo(sizingCash, total)
	if ratio.Cmp(new(big.Rat).SetInt64(1)) > 0 {
		ratio.SetInt64(1)
	}
	kept, cost, monetaryCost, err := fractionalOrdersAtScale(buys, increments, ratio, maxFees)
	if err != nil {
		return nil, nil, 0
	}
	if cost.Cmp(cash) > 0 {
		ratio = fractionalFeeScale(sizingCash, fees, atLimit)
		kept, cost, monetaryCost, err = fractionalOrdersAtScale(buys, increments, ratio, maxFees)
		if err != nil {
			return nil, nil, 0
		}
	}

	// ADR 0026, "Scaling": cent ceilings and the gate's sequential float
	// deductions must both fit. A bounded exact search crosses cent plateaus
	// without walking adjacent floats or individual quantity ticks. With int64
	// quantities, 128 bisections resolve even two distinct quantity breakpoints
	// (their denominators are at most MaxInt64).
	if cost.Cmp(cash) > 0 || monetaryCost > budget || !fractionalRunningCashFits(kept, budget, maxFees) {
		low, high := new(big.Rat), new(big.Rat).Set(ratio)
		kept = nil
		for range 128 {
			mid := new(big.Rat).Quo(new(big.Rat).Add(low, high), big.NewRat(2, 1))
			candidate, exact, monetary, err := fractionalOrdersAtScale(buys, increments, mid, maxFees)
			if err != nil {
				return nil, nil, 0
			}
			if exact.Cmp(cash) <= 0 && monetary <= budget && fractionalRunningCashFits(candidate, budget, maxFees) {
				low.Set(mid)
				kept = candidate
			} else {
				high.Set(mid)
			}
		}
		ratio = low
	}

	scale, _ := ratio.Float64()
	if ratio.Cmp(new(big.Rat).SetInt64(1)) < 0 && scale == 1 {
		scale = math.Nextafter(1, 0)
	}
	var skipped []event.OrderSkippedPayload
	for i, b := range buys {
		ticks := new(big.Rat).Mul(new(big.Rat).SetInt64(b.Quantity), ratio)
		if quantity, _ := floorIncrement(ticks, increments[i]); quantity == 0 {
			// RequestedValue is the monetary order before scaling; Scale states
			// the applied ratio. Neither stores fractional shares in a float.
			skipped = append(skipped, fractionalSkip(b, event.OrderSkippedScaledBelowIncrement, fractionalOrderValue(b), scale))
		}
	}
	return kept, skipped, scale
}

// fractionalOrderValue is quantity times limit price as an exact decimal
// monetary value under ADR 0026 D7-F.
func fractionalOrderValue(order event.OrderPlannedPayload) *big.Rat {
	return new(big.Rat).Mul(new(big.Rat).SetFrac64(order.Quantity, order.QuantityScale), moneyRat(order.LimitPrice))
}

// fractionalFeeScale reserves every original order's fee before sharing
// the remaining budget, as ADR 0026 point 2 "Scaling" requires.
func fractionalFeeScale(cash, fees, atLimit *big.Rat) *big.Rat {
	remaining := new(big.Rat).Sub(cash, fees)
	if remaining.Sign() < 0 {
		remaining.SetInt64(0)
	}
	remaining.Quo(remaining, atLimit)
	if remaining.Cmp(new(big.Rat).SetInt64(1)) > 0 {
		remaining.SetInt64(1)
	}
	return remaining
}

// fractionalOrdersAtScale rounds each exact scaled quantity down and
// values the kept basket in both the exact and account monetary domains
// using the conservative cent debit (ADR 0026, "Worst-case cost").
func fractionalOrdersAtScale(buys []event.OrderPlannedPayload, increments []int64, ratio *big.Rat, maxFees float64) ([]event.OrderPlannedPayload, *big.Rat, float64, error) {
	var kept []event.OrderPlannedPayload
	exactCost := new(big.Rat)
	monetaryCost := 0.0
	for i, b := range buys {
		ticks := new(big.Rat).Mul(new(big.Rat).SetInt64(b.Quantity), ratio)
		b.Quantity, _ = floorIncrement(ticks, increments[i])
		if b.Quantity > 0 {
			kept = append(kept, b)
			cost, err := event.BuyReservation(b.Quantity, b.QuantityScale, b.LimitPrice, maxFees, b.RoundingSlack)
			if err != nil {
				return nil, nil, 0, err
			}
			exactCost.Add(exactCost, cost)
			monetaryCost += BuyCost(b, maxFees)
		}
	}
	return kept, exactCost, monetaryCost, nil
}

// fractionalRunningCashFits mirrors the risk gate's release-order cash
// deductions (ADR 0026, point 2, "Scaling"). A summed float alone cannot
// prove that every individual order passes the insufficient-cash check.
func fractionalRunningCashFits(buys []event.OrderPlannedPayload, cash, fees float64) bool {
	for _, buy := range buys {
		cost := BuyCost(buy, fees)
		if cost > cash {
			return false
		}
		cash -= cost
	}
	return true
}
