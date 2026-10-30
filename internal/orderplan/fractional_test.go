package orderplan_test

import (
	"reflect"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// TestFractionalIRAPlansExactTicks exercises ADR 0026 D7-F with synthetic
// prices: buys and sells round down to the declared increment, never a lot.
func TestFractionalIRAPlansExactTicks(t *testing.T) {
	in := iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.25}, event.TargetWeight{InstrumentID: "BBB", Weight: 0.01}), 100, map[string]float64{"AAA": 7, "BBB": 7, "CCC": 7}, event.AccountPosition{InstrumentID: "CCC", Quantity: 72500})
	in.QuantityScale, in.Snapshot.QuantityScale = event.FractionalQuantityScale, event.FractionalQuantityScale
	in.Decision.QuantityScale = 10000
	plan := planIRA(t, in)
	sameLegs(t, "buys", plan.Buys, leg{"AAA", event.OrderSideBuy, 35714}, leg{"BBB", event.OrderSideBuy, 1428})
	sameLegs(t, "sells", plan.Sells, leg{"CCC", event.OrderSideSell, 72500})
	for _, o := range append(plan.Buys, plan.Sells...) {
		if o.QuantityScale != event.FractionalQuantityScale {
			t.Fatalf("unscaled order %+v", o)
		}
	}
	in.FractionalIncrement = "0.25"
	in.WorkingSells = map[string]int64{"CCC": 35000}
	plan = planIRA(t, in)
	sameLegs(t, "quarter-share buys", plan.Buys, leg{"AAA", event.OrderSideBuy, 35000})
	sameLegs(t, "net sells", plan.Sells, leg{"CCC", event.OrderSideSell, 37500})
	for _, skip := range plan.Skipped {
		if err := skip.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].Reason != event.OrderSkippedBelowIncrement || plan.Skipped[0].RequestedValue != "1" {
		t.Fatalf("sub-increment request: %+v", plan.Skipped)
	}
}

func TestFractionalIRADecimalBoundaryAndBand(t *testing.T) {
	in := iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.29}), 100, map[string]float64{"AAA": 100})
	in.QuantityScale, in.Snapshot.QuantityScale = 10000, 10000
	in.Decision.QuantityScale = 10000
	plan := planIRA(t, in)
	sameLegs(t, "decimal boundary", plan.Buys, leg{"AAA", event.OrderSideBuy, 2900})
	in.Snapshot.Positions = []event.AccountPosition{{InstrumentID: "AAA", Quantity: 2900}}
	if p := planIRA(t, in); len(p.Sells)+len(p.Buys) != 0 {
		t.Fatalf("on target: %+v", p)
	}
	in.Snapshot.Positions[0].Quantity = 7250
	plan = planIRA(t, in)
	sameLegs(t, "fractional trim", plan.Sells, leg{"AAA", event.OrderSideSell, 4350})
	in.Snapshot.QuantityScale = 0
	if _, err := orderplan.PlanIRA(in); err == nil {
		t.Fatal("accepted mixed units")
	}
}

func TestFractionalIRABudgetUsesDecimalValueAndIncrement(t *testing.T) {
	in := iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.5}), 100, map[string]float64{"AAA": 10})
	in.QuantityScale, in.Snapshot.QuantityScale = 10000, 10000
	in.Decision.QuantityScale = 10000
	in.FractionalIncrement = "0.25"
	buys := planIRA(t, in).Buys
	if got := orderplan.BuyCost(buys[0], 1); got != 53.5 {
		t.Fatalf("cost %v, want 53.5", got)
	}
	kept, skipped, _ := orderplan.ScaleBuys(buys, 27.25, 1)
	sameLegs(t, "budget", kept, leg{"AAA", event.OrderSideBuy, 25000})
	if len(skipped) != 0 || orderplan.BuyCost(kept[0], 1) > 27.25 {
		t.Fatalf("budget result %+v %+v", kept, skipped)
	}
	kept, skipped, _ = orderplan.ScaleBuys(buys, 36.25, 10)
	sameLegs(t, "fee-corrected budget", kept, leg{"AAA", event.OrderSideBuy, 25000})
	if len(skipped) != 0 || orderplan.BuyCost(kept[0], 10) > 36.25 {
		t.Fatalf("fee-corrected budget result %+v %+v", kept, skipped)
	}
	kept, skipped, _ = orderplan.ScaleBuys(buys, 1.5, 1)
	if len(kept) != 0 || len(skipped) != 1 || skipped[0].RequestedValue == "0" {
		t.Fatalf("positive monetary request below increment %+v %+v", kept, skipped)
	}
	if err := skipped[0].Validate(); err != nil {
		t.Fatal(err)
	}
	kept, skipped, _ = orderplan.ScaleBuys(buys, 1, 1)
	for _, skip := range skipped {
		if err := skip.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if len(kept) != 0 || len(skipped) != 1 || skipped[0].Reason != event.OrderSkippedScaledBelowIncrement {
		t.Fatalf("below increment %+v %+v", kept, skipped)
	}
}

func TestFractionalIRARefusesInvalidRoundingInputs(t *testing.T) {
	base := iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.5}), 100, map[string]float64{"AAA": 10}, event.AccountPosition{InstrumentID: "AAA", Quantity: 10000})
	base.QuantityScale, base.Snapshot.QuantityScale = 10000, 10000
	base.Decision.QuantityScale = 10000
	for name, mutate := range map[string]func(*orderplan.IRAInput){
		"missing price": func(in *orderplan.IRAInput) { in.ReferencePrices = nil },
		"whole shares with increment": func(in *orderplan.IRAInput) {
			in.QuantityScale = 0
			in.Decision.QuantityScale = 0
			in.Snapshot.QuantityScale = 0
			in.FractionalIncrement = "0.25"
		},
		"invalid scale":                     func(in *orderplan.IRAInput) { in.QuantityScale = 1000 },
		"increment does not divide a share": func(in *orderplan.IRAInput) { in.FractionalIncrement = "0.3" },
		"over precision":                    func(in *orderplan.IRAInput) { in.FractionalIncrement = "0.00001" },
		"negative working sells":            func(in *orderplan.IRAInput) { in.WorkingSells = map[string]int64{"AAA": -1} },
		"overflow":                          func(in *orderplan.IRAInput) { in.ReferencePrices = map[string]float64{"AAA": 1e-300} },
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			mutate(&in)
			if _, err := orderplan.PlanIRA(in); err == nil {
				t.Fatal("accepted invalid sizing inputs")
			}
		})
	}
}

func TestFractionalIRADefaultIncrementIsDecisionEquivalent(t *testing.T) {
	in := iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.5}), 100, map[string]float64{"AAA": 7})
	in.QuantityScale, in.Snapshot.QuantityScale = 10000, 10000
	in.Decision.QuantityScale = 10000
	implicit := planIRA(t, in)
	in.FractionalIncrement = "0.0001"
	explicit := planIRA(t, in)
	if !reflect.DeepEqual(implicit, explicit) {
		t.Fatalf("default and explicit increment differ: %+v %+v", implicit, explicit)
	}
}

func TestFractionalIRAScalingNeverRoundsBeyondCash(t *testing.T) {
	for _, tt := range []struct {
		quantity            int64
		price, fees, budget float64
		want                int64
	}{
		{9531250, 10.5, 0.1, 2510.2929999999997, 2390659},
		{6071572541162903999, 100, 0, 7170629157645890, 717062915764589000},
	} {
		order := event.OrderPlannedPayload{AccountID: "ira", Tag: iraTag, ClientOrderID: "synthetic-buy", InstrumentID: "AAA", Side: event.OrderSideBuy, Phase: event.OrderSideBuy, OrderType: event.OrderTypeLimit, Quantity: tt.quantity, QuantityScale: 10000, FractionalIncrement: "0.0001", LimitPrice: tt.price, ReferencePrice: tt.price, NetLiquidation: 1e18, PriceBand: 0.05}
		kept, _, _ := orderplan.ScaleBuys([]event.OrderPlannedPayload{order}, tt.budget, tt.fees)
		if len(kept) != 1 || kept[0].Quantity != tt.want || orderplan.BuyCost(kept[0], tt.fees) > tt.budget {
			t.Fatalf("scaled q=%d at %v fees%v budget%.17g: %+v", tt.quantity, tt.price, tt.fees, tt.budget, kept)
		}
	}
}

func TestFractionalIRATinyHeldRemainderNamesTheIncrement(t *testing.T) {
	in := iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0}), 100, map[string]float64{"AAA": 7}, event.AccountPosition{InstrumentID: "AAA", Quantity: 1000})
	in.QuantityScale, in.Snapshot.QuantityScale = 10000, 10000
	in.Decision.QuantityScale = 10000
	in.FractionalIncrement = "0.25"
	plan := planIRA(t, in)
	if len(plan.Sells) != 0 || len(plan.Skipped) != 1 || plan.Skipped[0].Reason != event.OrderSkippedBelowIncrement {
		t.Fatalf("tiny holding attributed to working sells: %+v", plan)
	}
}

func TestFractionalIRARefusesWholeShareDecisionEvidence(t *testing.T) {
	in := iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.5}), 100, map[string]float64{"AAA": 7})
	in.QuantityScale, in.Snapshot.QuantityScale = 10000, 10000
	if _, err := orderplan.PlanIRA(in); err == nil {
		t.Fatal("fractional planner accepted whole-share decision evidence")
	}
}

func TestFractionalIRAScalingAlsoFitsRoundedMonetarySum(t *testing.T) {
	orders := []event.OrderPlannedPayload{{Quantity: 1000, QuantityScale: 10000, FractionalIncrement: "0.0001", LimitPrice: 1}, {Quantity: 2000, QuantityScale: 10000, FractionalIncrement: "0.0001", LimitPrice: 1}}
	kept, _, _ := orderplan.ScaleBuys(orders, 0.3, 0)
	sum := 0.0
	for i, b := range kept {
		sum += orderplan.BuyCost(b, 0)
		if b.Quantity > orders[i].Quantity {
			t.Fatal("scaling increased an order")
		}
	}
	if len(kept) != 2 || sum > 0.3 {
		t.Fatalf("basket %+v costs %.17g", kept, sum)
	}
}

func TestFractionalIRAScalingFailsClosedOnMixedOrInvalidUnits(t *testing.T) {
	good := event.OrderPlannedPayload{Quantity: 10000, QuantityScale: 10000, FractionalIncrement: "0.0001", LimitPrice: 10}
	for name, mutate := range map[string]func(*event.OrderPlannedPayload){
		"whole shares":           func(o *event.OrderPlannedPayload) { o.QuantityScale = 0 },
		"bad scale":              func(o *event.OrderPlannedPayload) { o.QuantityScale = 1000 },
		"negative quantity":      func(o *event.OrderPlannedPayload) { o.Quantity = -1 },
		"bad increment":          func(o *event.OrderPlannedPayload) { o.FractionalIncrement = "0.3" },
		"quantity off increment": func(o *event.OrderPlannedPayload) { o.FractionalIncrement = "0.25"; o.Quantity = 10001 },
		"missing price":          func(o *event.OrderPlannedPayload) { o.LimitPrice = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := good
			mutate(&bad)
			if kept, _, _ := orderplan.ScaleBuys([]event.OrderPlannedPayload{good, bad}, 5, 0); len(kept) != 0 {
				t.Fatalf("invalid basket released %+v", kept)
			}
		})
	}
	if kept, _, _ := orderplan.ScaleBuys([]event.OrderPlannedPayload{good}, 5, -1); len(kept) != 0 {
		t.Fatal("negative fee allowance accepted")
	}
}

func TestFractionalIRAScaleEdges(t *testing.T) {
	order := event.OrderPlannedPayload{Quantity: 10000, QuantityScale: 10000, FractionalIncrement: "0.0001", LimitPrice: 10}
	if kept, _, scale := orderplan.ScaleBuys([]event.OrderPlannedPayload{order}, 100, 0); len(kept) != 1 || kept[0].Quantity != 10000 || scale != 1 {
		t.Fatalf("ample cash %+v scale%v", kept, scale)
	}
	if kept, skipped, _ := orderplan.ScaleBuys([]event.OrderPlannedPayload{order}, 5, 10); len(kept) != 0 || len(skipped) != 1 {
		t.Fatalf("fee exceeds cash %+v %+v", kept, skipped)
	}
	order.Quantity, order.LimitPrice = 1000000000000000001, 1
	if kept, _, scale := orderplan.ScaleBuys([]event.OrderPlannedPayload{order}, 1e14, 0); len(kept) != 1 || kept[0].Quantity != 1000000000000000000 || !(scale < 1) {
		t.Fatalf("almost one scale %+v scale%.17g", kept, scale)
	}
	orders := make([]event.OrderPlannedPayload, 30)
	for i := range orders {
		orders[i] = order
		orders[i].Quantity = 1000
	}
	kept, _, _ := orderplan.ScaleBuys(orders, 3.000000000000001, 0)
	sum := 0.0
	for _, b := range kept {
		sum += orderplan.BuyCost(b, 0)
		if b.Quantity > 1000 {
			t.Fatal("scaling increased quantity")
		}
	}
	if len(kept) != 30 || sum > 3.000000000000001 {
		t.Fatalf("rounded basket count%d sum%.17g", len(kept), sum)
	}
}

func TestFractionalIRANoHoldingAndCommittedHolding(t *testing.T) {
	in := iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0}), 100, map[string]float64{"AAA": 7})
	in.QuantityScale, in.Snapshot.QuantityScale, in.Decision.QuantityScale = 10000, 10000, 10000
	if plan := planIRA(t, in); len(plan.Sells)+len(plan.Buys)+len(plan.Skipped) != 0 {
		t.Fatalf("empty position %+v", plan)
	}
	in.Snapshot.Positions = []event.AccountPosition{{InstrumentID: "AAA", Quantity: 10000}}
	in.WorkingSells = map[string]int64{"AAA": 10000}
	plan := planIRA(t, in)
	if len(plan.Sells) != 0 || len(plan.Skipped) != 1 || plan.Skipped[0].Reason != event.OrderSkippedHeldByWorkingSells {
		t.Fatalf("committed holding %+v", plan)
	}
}
