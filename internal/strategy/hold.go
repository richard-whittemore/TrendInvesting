package strategy

import (
	"fmt"
	"slices"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds ADR 0020's reservation at proposal, as amended 2026-09-24
// (CONTEXT.md: "Hold"). Every entry or Add proposal places one hold the
// moment it is emitted. The hold reserves the Unit's worst-case cost against
// spendable cash, and one Unit of headroom under every ADR 0008 cap the Unit
// counts towards. It stands until its proposal fills, expires or is
// cancelled, and every later proposal is checked against what it leaves:
//
//	available = basis - fill debits - holds          (cashAtPreviousClose)
//	post-trade exposure = committed + reserved + 1   (capExceeded)
//
// A fill releases its hold and is debited at its actual cost (debitFill), so
// the two are never counted together. A snapshot never releases a hold: it
// states the account's cash, which unfilled orders have not reduced (ADR 0020,
// as amended).

// hold is one standing reservation. It carries what capExceeded needs to
// count its Unit (the instrument and the classification the Unit would join)
// and what cashAtPreviousClose needs to deduct (its cost). It holds no
// reference types, so Reducer.begin's slice copy isolates it completely.
type hold struct {
	proposalID     string
	instrumentID   string
	classification classification
	cost           float64
}

// placeHold records the hold of a proposal just emitted. A second hold for
// one proposal would reserve its Unit twice, so it fails closed.
func (r *transition) placeHold(proposalID, instrumentID string, c classification, cost float64) error {
	if slices.ContainsFunc(r.holds, func(h hold) bool { return h.proposalID == proposalID }) {
		return fmt.Errorf("strategy: instrument %q: proposal %q already has a hold standing; a proposal reserves its Unit once (ADR 0020)", instrumentID, proposalID)
	}
	r.holds = append(r.holds, hold{proposalID: proposalID, instrumentID: instrumentID, classification: c, cost: cost})
	return nil
}

// releaseHold drops the hold of a proposal that has filled, expired or been
// cancelled (ADR 0020, as amended: "What releases a hold"). Every
// outstanding entry and Add proposal has exactly one hold, so a proposal
// with none means the ledger and the proposals disagree, and the run stops
// rather than trade against a ledger it cannot account for.
func (r *transition) releaseHold(proposalID string) error {
	i := slices.IndexFunc(r.holds, func(h hold) bool { return h.proposalID == proposalID })
	if i < 0 {
		return fmt.Errorf("strategy: proposal %q has no hold standing to release; the cash and cap reservations no longer match the outstanding proposals (ADR 0020)", proposalID)
	}
	r.holds = slices.Delete(r.holds, i, i+1)
	return nil
}

// holdTotal is the sum of every standing hold, in placement order so the sum
// is the same on every replay.
func (r *Reducer) holdTotal() float64 {
	total := 0.0
	for _, h := range r.holds {
		total += h.cost
	}
	return total
}

// reservedUnits counts the standing holds whose Unit matches: one Unit per
// hold, since every proposal is for exactly one Unit.
func (r *transition) reservedUnits(matches func(hold) bool) int {
	n := 0
	for _, h := range r.holds {
		if matches(h) {
			n++
		}
	}
	return n
}

// buyHold is the hold a buy of quantity at level places, and the price cap
// its proposal carries (ADR 0005 and ADR 0020, as amended 2026-09-24).
//
//   - Under event.OrderTypeStopLimit, the Baseline, the cap is level +
//     GapBufferN x n and the hold is the worst case a fill can cost:
//     quantity x (cap + SlippageN x n) x dollars per point, plus ADR 0013's
//     commission at that price (sizing.WorstCaseBuyCost).
//   - Under event.OrderTypeStopMarket, the declared Variant "uncapped", there
//     is no cap, and the hold is the Unit's cost at its level, quantity x
//     level x dollars per point: the affordability check this reducer made
//     before the amendment, kept for the head-to-head comparison ADR 0012
//     requires.
//
// n is the N the Unit's slippage is measured in: the decision N for an entry,
// the Campaign's frozen N for an Add (ADR 0006). ok is false when the hold or
// the cap leaves the float64 range; the caller skips the Unit as one whose
// cost no cash could fund, as unitCost's callers do.
func (r *transition) buyHold(quantity int64, level, n float64) (cost, priceCap float64, ok bool) {
	if r.buyOrderType == event.OrderTypeStopMarket {
		cost, ok = unitCost(quantity, level, r.dollarsPerPoint)
		return cost, 0, ok
	}
	priceCap, ok = sizing.PriceCap(level, r.gapBufferN, n)
	if !ok {
		return 0, 0, false
	}
	cost, ok = sizing.WorstCaseBuyCost(quantity, priceCap, r.slippageN, n, r.dollarsPerPoint, r.commission)
	return cost, priceCap, ok
}

// buyHoldDetail states the operands of a buy's hold for a decline's Detail,
// in the same terms buyHold computed it from.
func (r *transition) buyHoldDetail(quantity int64, level, n, priceCap float64) string {
	if r.buyOrderType == event.OrderTypeStopMarket {
		return fmt.Sprintf("%d shares x level %v x %v dollars per point", quantity, level, r.dollarsPerPoint)
	}
	return fmt.Sprintf("%d shares x (price cap %v, level %v + %v x n %v, plus slippage %v x n) x %v dollars per point, plus commission at that price",
		quantity, priceCap, level, r.gapBufferN, n, r.slippageN, r.dollarsPerPoint)
}

// pendingProposalID is the id of instrument state's outstanding entry
// proposal, or "" when it has none.
func pendingProposalID(state *instrumentState) string {
	if state.pendingProposal == nil {
		return ""
	}
	return state.pendingProposal.proposalID
}

// pendingAddProposalID is the id of instrument state's outstanding Add
// proposal, or "" when it has none.
func pendingAddProposalID(state *instrumentState) string {
	if state.pendingAddProposal == nil {
		return ""
	}
	return state.pendingAddProposal.proposalID
}
