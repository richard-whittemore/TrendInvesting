package orderplan_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

func TestFractionalScalingRejectsInvalidRoundingSlack(t *testing.T) {
	for _, slack := range []float64{-0.01, 0.0099, 1.0001, math.NaN(), math.Inf(1), math.Inf(-1)} {
		o := slackBuy(t, 10000, 10, 0.10)
		o.RoundingSlack = slack
		kept, skipped, scale := orderplan.ScaleBuys([]event.OrderPlannedPayload{o}, 100, 0)
		if kept != nil || skipped != nil || scale != 0 {
			t.Errorf("slack %v: got %+v, %+v, %v; want fail-closed nil, nil, 0", slack, kept, skipped, scale)
		}
	}
}

// ADR 0026: cash below the order's reserve cannot fund even one increment.
func TestFractionalScalingCashBelowRoundingSlack(t *testing.T) {
	o := slackBuy(t, 10000, 10, 0.10)
	kept, skipped, scale := orderplan.ScaleBuys([]event.OrderPlannedPayload{o}, 0.09, 0)
	if len(kept) != 0 || scale != 0 || len(skipped) != 1 {
		t.Fatalf("got %+v, %+v, %v; want no buys, one skip, zero scale", kept, skipped, scale)
	}
	if skipped[0].Reason != event.OrderSkippedScaledBelowIncrement || skipped[0].RequestedValue != "10" || skipped[0].Scale != 0 {
		t.Fatalf("lost skipped request evidence: %+v", skipped[0])
	}
}

func slackBuy(t *testing.T, quantity int64, price, slack float64) event.OrderPlannedPayload {
	t.Helper()
	o := event.OrderPlannedPayload{Quantity: quantity, QuantityScale: 10000, FractionalIncrement: "0.0001", LimitPrice: price, Side: event.OrderSideBuy}
	raw, err := json.Marshal(map[string]float64{"rounding_slack": slack})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	return o
}

// ADR 0026: synthetic four-execution example for the per-execution cent
// rounding allowance, independently checked against the posted cash book.
func TestRoundingSlackCoversFourPartialExecutions(t *testing.T) {
	o := slackBuy(t, 72500, 10.10, 0.10)
	reserved := orderplan.BuyCost(o, 0)
	if reserved != 73.33 {
		t.Errorf("reservation with slack = %.2f; want 73.33", reserved)
	}
	var posted int64
	spent := 0.0
	for range 4 {
		f := event.TargetWeightFillPayload{Quantity: 18125, QuantityScale: 10000, Price: 10.10, Side: event.OrderSideBuy}
		posted -= reconcile.FillChange(f).Cash
		cost, err := event.BuyCost(f.Quantity, f.QuantityScale, f.Price, 0)
		if err != nil {
			t.Fatal(err)
		}
		debit, _ := cost.Float64()
		spent += debit
	}
	if posted != 7324 || spent > reserved || float64(posted)/100 > reserved {
		t.Fatalf("four executions: posted %d cents, budget debit %.2f, reservation %.2f", posted, spent, reserved)
	}
}

// maxSplitDebit exhaustively maximizes the debit over ALL integer-tick
// partitions into up to fills positive fills. These synthetic prices are exact
// cents; integer ceilings provide an oracle independent of event.BuyCost.
func maxSplitDebit(quantity, priceCents int64, fills int) int64 {
	previous := make([]int64, quantity+1)
	for q := int64(1); q <= quantity; q++ {
		previous[q] = -1
	}
	best := int64(0)
	for range fills {
		next := make([]int64, quantity+1)
		for q := int64(0); q <= quantity; q++ {
			next[q] = -1
			for fill := int64(1); fill <= q; fill++ {
				if previous[q-fill] >= 0 {
					next[q] = max(next[q], previous[q-fill]+(fill*priceCents+9999)/10000)
				}
			}
		}
		best = max(best, next[quantity])
		previous = next
	}
	return best
}

func TestRoundingSlackBasketCoversEverySplitUpToCap(t *testing.T) {
	for _, fills := range []int{1, 4, 10, 100} {
		slack := float64(fills) / 100
		orders := []event.OrderPlannedPayload{slackBuy(t, 40, 1010, slack), slackBuy(t, 39, 1010, slack)}
		cashCents := int64(377)
		kept, _, _ := orderplan.ScaleBuys(orders, float64(cashCents)/100, 0)
		if len(kept) != 2 {
			t.Fatalf("fills %d: kept %+v", fills, kept)
		}
		remaining, worst := float64(cashCents)/100, int64(0)
		for _, o := range kept {
			cost := orderplan.BuyCost(o, 0)
			if cost > remaining {
				t.Fatalf("fills %d: reservation %.17g exceeds cash %.17g", fills, cost, remaining)
			}
			remaining -= cost
			worst += maxSplitDebit(o.Quantity, 101000, fills)
		}
		if worst > cashCents || math.IsNaN(remaining) {
			t.Errorf("fills %d: some partial-fill split spends %d cents against %d cash", fills, worst, cashCents)
		}
	}
}
