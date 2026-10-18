package orderplan_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

var session = time.Date(2025, time.March, 4, 21, 0, 0, 0, time.UTC)

func decision(target, inForce float64, inForceSet bool) event.TargetsDecidedPayload {
	return event.TargetsDecidedPayload{
		AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost,
		DecisionTime: session, SessionPeriodEnd: session, Outcome: event.TargetsOutcomeDecided,
		Targets: []event.TargetWeight{{InstrumentID: "SPY", Weight: target}},
		Boost:   event.BoostFigures{InstrumentID: "SPY", Close: 400, TargetInForce: inForce, TargetInForceSet: inForceSet},
	}
}

func input(d event.TargetsDecidedPayload, netLiq float64, held int64, price float64) orderplan.BoostInput {
	snapshot := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: session, NetLiquidation: netLiq, Currency: "USD"}
	if held != 0 {
		snapshot.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: held}}
	}
	return orderplan.BoostInput{Tag: "targets-decided:joint:x", Decision: d, Snapshot: snapshot, ReferencePrice: price, PriceBand: 0.05}
}

func plan(t *testing.T, in orderplan.BoostInput) (event.OrderPlannedPayload, bool) {
	t.Helper()
	order, ok, err := orderplan.PlanBoost(in)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		if err := order.Validate(); err != nil {
			t.Fatalf("planned order is invalid: %v", err)
		}
	}
	return order, ok
}

func TestFirstDecisionBuysToNinetyEightPercentFromCash(t *testing.T) {
	// 0.98 x 4000 / 400 = 9.8 shares, rounded toward zero to 9.
	order, ok := plan(t, input(decision(0.98, 0, false), 4000, 0, 400))
	if !ok {
		t.Fatal("the first decision always orders to its target (template.py, last_want = None)")
	}
	if order.Side != event.OrderSideBuy || order.Quantity != 9 || order.Phase != event.OrderSideBuy {
		t.Errorf("got %s %d (phase %s), want buy 9", order.Side, order.Quantity, order.Phase)
	}
	if order.LimitPrice != 420 {
		t.Errorf("buy limit = %v, want the band's upper edge 420", order.LimitPrice)
	}
	if order.Tag != "targets-decided:joint:x" || order.AccountID != "joint" || order.InstrumentID != "SPY" {
		t.Errorf("identity: %+v", order)
	}
	if order.NetLiquidation != 4000 || order.HeldQuantity != 0 || order.ReferencePrice != 400 || order.TargetWeight != 0.98 {
		t.Errorf("figures: %+v", order)
	}
}

func TestBoostEntryRoundsTowardZero(t *testing.T) {
	// 1.47 x 4000 / 400 - 9 = 5.7 shares: 5.
	order, ok := plan(t, input(decision(1.47, 0.98, true), 4000, 9, 400))
	if !ok || order.Side != event.OrderSideBuy || order.Quantity != 5 {
		t.Fatalf("got ok=%v %s %d, want buy 5", ok, order.Side, order.Quantity)
	}
}

func TestBoostExitRoundsTowardZero(t *testing.T) {
	// 0.98 x 4000 / 400 - 14 = -4.2 shares: sell 4, keeping the account at or
	// above 98% rather than below it.
	order, ok := plan(t, input(decision(0.98, 1.47, true), 4000, 14, 400))
	if !ok || order.Side != event.OrderSideSell || order.Quantity != 4 || order.Phase != event.OrderSideSell {
		t.Fatalf("got ok=%v %s %d, want sell 4", ok, order.Side, order.Quantity)
	}
	if order.LimitPrice != 380 {
		t.Errorf("sell limit = %v, want the band's lower edge 380", order.LimitPrice)
	}
}

func TestNoOrderWithoutAStateChange(t *testing.T) {
	// Drift is not traded (template.py:97-100): 98% in force, 98% decided.
	if _, ok := plan(t, input(decision(0.98, 0.98, true), 4000, 5, 400)); ok {
		t.Error("an unchanged target must plan no order, whatever the holding")
	}
}

// TestARefusedOrderIsReplannedAtTheNextDecision: after the last order was
// refused and the engine stayed Normal (the kill switch, a stale broker
// snapshot), the next decision plans the difference to the target in force
// although the target has not changed (ADR 0026, point 2, "Boost", amended
// 2026-10-18; ADR 0019 reading 10).
func TestARefusedOrderIsReplannedAtTheNextDecision(t *testing.T) {
	in := input(decision(0.98, 0.98, true), 4000, 0, 400)
	in.Replan = true
	order, ok := plan(t, in)
	if !ok || order.Side != event.OrderSideBuy || order.Quantity != 9 {
		t.Fatalf("got ok=%v %s %d, want the refused entry re-planned as buy 9", ok, order.Side, order.Quantity)
	}
	// The exit to 98% was refused with 14 held: sell 4 to 98%.
	in = input(decision(0.98, 0.98, true), 4000, 14, 400)
	in.Replan = true
	if order, ok = plan(t, in); !ok || order.Side != event.OrderSideSell || order.Quantity != 4 {
		t.Fatalf("got ok=%v %s %d, want the refused exit re-planned as sell 4", ok, order.Side, order.Quantity)
	}
	// Already at the target: a re-plan that rounds to zero shares plans nothing.
	in = input(decision(0.98, 0.98, true), 4000, 9, 400)
	in.Replan = true
	if _, ok = plan(t, in); ok {
		t.Error("9.8 - 9 rounds to 0: no order, re-plan or not")
	}
}

func TestNoOrderWhenTheRoundedQuantityIsZero(t *testing.T) {
	if _, ok := plan(t, input(decision(0.98, 0, false), 4000, 9, 400)); ok {
		t.Error("9.8 - 9 rounds to 0: no order")
	}
}

func TestLimitPricesStayInsideTheBandOnCents(t *testing.T) {
	in := input(decision(0.98, 0, false), 100000, 0, 401.37)
	order, _ := plan(t, in)
	// 401.37 x 1.05 = 421.4385: rounded down to a cent, inside the band.
	if order.LimitPrice != 421.43 {
		t.Errorf("buy limit = %v, want 421.43", order.LimitPrice)
	}
	in = input(decision(0.98, 1.47, true), 100000, 400, 401.37)
	order, _ = plan(t, in)
	// 401.37 x 0.95 = 381.3015: rounded up to a cent, inside the band.
	if order.LimitPrice != 381.31 {
		t.Errorf("sell limit = %v, want 381.31", order.LimitPrice)
	}
}

func TestClientOrderIDIsDeterministicPerTagInstrumentAndPhase(t *testing.T) {
	a := orderplan.ClientOrderID("t1", "SPY", event.OrderSideBuy)
	if a != orderplan.ClientOrderID("t1", "SPY", event.OrderSideBuy) {
		t.Error("the same tag, instrument and phase must give the same ID")
	}
	for _, other := range []string{
		orderplan.ClientOrderID("t2", "SPY", event.OrderSideBuy),
		orderplan.ClientOrderID("t1", "QQQ", event.OrderSideBuy),
		orderplan.ClientOrderID("t1", "SPY", event.OrderSideSell),
	} {
		if other == a {
			t.Error("a different tag, instrument or phase must give a different ID")
		}
	}
	if !strings.HasPrefix(a, "tw-") || len(a) != 27 {
		t.Errorf("ClientOrderID = %q, want tw- and 24 hex digits", a)
	}
	order, _ := plan(t, input(decision(0.98, 0, false), 4000, 0, 400))
	if order.ClientOrderID != orderplan.ClientOrderID("targets-decided:joint:x", "SPY", event.OrderSideBuy) {
		t.Errorf("planned order's client ID %q is not derived from its tag", order.ClientOrderID)
	}
}

func TestPlanBoostRefusesWhatItCannotPlan(t *testing.T) {
	withheld := decision(0.98, 0, false)
	withheld.Outcome = event.TargetsOutcomeWithheld
	withheld.Targets = nil
	withheld.WithheldReason = event.WithheldMissingBars
	withheld.Detail = "no bar"
	twoTargets := decision(0.98, 0, false)
	twoTargets.Targets = []event.TargetWeight{{InstrumentID: "IEF", Weight: 0.5}, {InstrumentID: "SPY", Weight: 0.5}}
	otherInstrument := decision(0.98, 0, false)
	otherInstrument.Targets[0].InstrumentID = "QQQ"
	cases := map[string]orderplan.BoostInput{
		"withheld":         input(withheld, 4000, 0, 400),
		"two targets":      input(twoTargets, 4000, 0, 400),
		"other instrument": input(otherInstrument, 4000, 0, 400),
		"no tag": func() orderplan.BoostInput {
			in := input(decision(0.98, 0, false), 4000, 0, 400)
			in.Tag = ""
			return in
		}(),
		"short holding": input(decision(0.98, 0, false), 4000, -3, 400),
		"no price":      input(decision(0.98, 0, false), 4000, 0, 0),
		"no band": func() orderplan.BoostInput {
			in := input(decision(0.98, 0, false), 4000, 0, 400)
			in.PriceBand = 0
			return in
		}(),
		"other account": func() orderplan.BoostInput {
			in := input(decision(0.98, 0, false), 4000, 0, 400)
			in.Snapshot.AccountID = "ira"
			return in
		}(),
		"invalid snapshot":   input(decision(0.98, 0, false), 0, 0, 400),
		"invalid decision":   input(event.TargetsDecidedPayload{}, 4000, 0, 400),
		"order beyond int64": input(decision(0.98, 0, false), 1e300, 0, 400),
	}
	for name, in := range cases {
		if _, _, err := orderplan.PlanBoost(in); err == nil {
			t.Errorf("%s: PlanBoost accepted it", name)
		}
	}
}

// TestAnExitIsNetOfSellsAlreadyWorking: Boost's exit and the sells already
// working never together sell below the target (ADR 0019, amended
// 2026-10-15, "never below it"), and a working sell never turns the exit
// into a buy. ADR 0019's worked example: 147 shares, net liquidation $8,530,
// a $90 close; the exit to 98% sells 54.
func TestAnExitIsNetOfSellsAlreadyWorking(t *testing.T) {
	in := input(decision(0.98, 1.47, true), 8530, 147, 90)
	order, ok := plan(t, in)
	if !ok || order.Side != event.OrderSideSell || order.Quantity != 54 {
		t.Fatalf("exit without working sells: %+v", order)
	}
	in.WorkingSells = 20
	if order, ok = plan(t, in); !ok || order.Quantity != 34 || order.HeldQuantity != 147 {
		t.Fatalf("exit net of 20 working: %+v", order)
	}
	in.WorkingSells = 54
	if order, ok = plan(t, in); ok {
		t.Fatalf("an urgent sale of 54 already working leaves nothing to sell: %+v", order)
	}
	in.WorkingSells = 90
	if order, ok = plan(t, in); ok {
		t.Fatalf("a working sell never turns the exit into a buy: %+v", order)
	}
	entry := input(decision(1.47, 0.98, true), 8530, 93, 90)
	entry.WorkingSells = 10
	if order, ok = plan(t, entry); !ok || order.Side != event.OrderSideBuy {
		t.Fatalf("a buy is planned as before: %+v", order)
	}
	in.WorkingSells = -1
	if _, _, err := orderplan.PlanBoost(in); err == nil {
		t.Error("negative working sells are a defect")
	}
}
