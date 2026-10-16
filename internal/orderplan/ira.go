package orderplan

import (
	"errors"
	"fmt"
	"math"
	"slices"

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
}

// IRAPlan is a rebalance's two phases, each in ascending instrument ID. Buys
// are sized from the previous close and not yet budgeted: they are planned
// only once every sell is terminal, or at once when there are no sells
// (ADR 0026, point 2, "IRA execution"); ScaleBuys budgets them.
//
// Skipped records each trade the plan sized but cannot order, because
// rounding toward zero to whole shares leaves none, or a sell's holding is
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
//  3. not held, with a target T: buy T x V / p whole shares, rounded toward zero;
//  4. held with a target: trade only if |q x p / V - T| > band x T, strictly,
//     then (T x V - q x p) / p shares, rounded toward zero;
//  5. no sell exceeds the holding minus the working and pending sells;
//  6. sells first, then buys.
//
// A trade that rounds to no share, or a sell with nothing left after the
// working sells, is recorded in Skipped with the shares the rule asked for.
//
// Whole shares rounded toward zero are D7-L's rounding; D7-L's substitution of
// a pick whose one share exceeds its target is the momentum rule's, not this.
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

	v := in.Snapshot.NetLiquidation
	var plan IRAPlan
	for _, id := range union {
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

// BuyCost is a buy's worst-case cost: its quantity at its limit price, the
// band's edge, plus the configured maximum commission and fees (ADR 0026,
// point 2, "Worst-case cost"). Buys worth $980 at the previous close cost
// $1,029 at a 5% band before fees.
func BuyCost(o event.OrderPlannedPayload, maxFees float64) float64 {
	return float64(float64(o.Quantity)*o.LimitPrice) + maxFees
}

// ScaleBuys fits buys to budget (ADR 0026, point 2, "Scaling"): when their
// worst-case total exceeds it, every buy's quantity is scaled by
// budget / total and rounded down again, and a buy that rounds to no share is
// dropped. It returns the buys kept, in their order, and the scale applied (1
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
func ScaleBuys(buys []event.OrderPlannedPayload, budget, maxFees float64) (kept []event.OrderPlannedPayload, skipped []event.OrderSkippedPayload, scale float64) {
	total, atLimit := 0.0, 0.0
	for _, b := range buys {
		total += BuyCost(b, maxFees)
		atLimit += float64(float64(b.Quantity) * b.LimitPrice)
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
// the sell proceeds that rule counts, minus what each released buy commits.
// A working buy commits its worst-case cost, or what its fills have spent if
// that is more; once it is terminal it commits only what it spent, so a
// partial fill releases its unspent part only when the rest is cancelled.
type Budget struct {
	start    float64
	proceeds float64
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
	remaining := b.start + b.proceeds
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
