package orderplan_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

const iraTag = "targets-decided:ira:2025-03-04"

func momentumDecision(targets ...event.TargetWeight) event.TargetsDecidedPayload {
	return event.TargetsDecidedPayload{
		AccountID: "ira", StrategyID: event.TargetWeightStrategyMomentum,
		DecisionTime: session, SessionPeriodEnd: session, Outcome: event.TargetsOutcomeDecided, Targets: targets,
		Momentum: event.MomentumFigures{CloseSession: "2025-03-04", TrendInstrument: "SPY", TrendCloses: 300, TrendClose: 110, TrendMean: 100},
	}
}

func iraInput(d event.TargetsDecidedPayload, netLiq float64, prices map[string]float64, held ...event.AccountPosition) orderplan.IRAInput {
	return orderplan.IRAInput{
		Tag: iraTag, Decision: d,
		Snapshot:        event.AccountPositionsSnapshotPayload{AccountID: "ira", AsOf: session, NetLiquidation: netLiq, Currency: "USD", Positions: held},
		ReferencePrices: prices, PriceBand: 0.05, RebalanceBand: 0.25,
	}
}

func planIRA(t *testing.T, in orderplan.IRAInput) orderplan.IRAPlan {
	t.Helper()
	plan, err := orderplan.PlanIRA(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range append(append([]event.OrderPlannedPayload(nil), plan.Sells...), plan.Buys...) {
		if err := o.Validate(); err != nil {
			t.Fatalf("planned order is invalid: %v", err)
		}
	}
	return plan
}

type leg struct {
	instrument string
	side       string
	quantity   int64
}

func legs(orders []event.OrderPlannedPayload) []leg {
	out := make([]leg, len(orders))
	for i, o := range orders {
		out[i] = leg{o.InstrumentID, o.Side, o.Quantity}
	}
	return out
}

func sameLegs(t *testing.T, label string, got []event.OrderPlannedPayload, want ...leg) {
	t.Helper()
	g := legs(got)
	if len(g) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", label, g, want)
		}
	}
}

func TestIRABandAtExactlyAQuarterOfTheTargetDoesNotTrade(t *testing.T) {
	// V 10,000, price 10: 625 shares is 0.625 against a target of 0.5, off by
	// exactly 0.125 = 0.25 x 0.5, which is not strictly greater: no trade.
	d := momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.5})
	prices := map[string]float64{"AAA": 10}
	at := planIRA(t, iraInput(d, 10000, prices, event.AccountPosition{InstrumentID: "AAA", Quantity: 625}))
	sameLegs(t, "at the band", at.Sells)
	sameLegs(t, "at the band", at.Buys)
	// 626 shares is 0.626, past the band: sell back to 500 shares.
	past := planIRA(t, iraInput(d, 10000, prices, event.AccountPosition{InstrumentID: "AAA", Quantity: 626}))
	sameLegs(t, "past the band", past.Sells, leg{"AAA", event.OrderSideSell, 126})
	// 375 shares is 0.375, exactly at the band below: no trade; 374 buys 126.
	below := planIRA(t, iraInput(d, 10000, prices, event.AccountPosition{InstrumentID: "AAA", Quantity: 375}))
	sameLegs(t, "at the band below", below.Buys)
	under := planIRA(t, iraInput(d, 10000, prices, event.AccountPosition{InstrumentID: "AAA", Quantity: 374}))
	sameLegs(t, "past the band below", under.Buys, leg{"AAA", event.OrderSideBuy, 126})
}

func TestIRAEntriesExitsAndSellsFirstInInstrumentOrder(t *testing.T) {
	// Held: CCC (no target: exit), DDD (on target). Targeted: AAA (new), BBB
	// (new), DDD, and IEF. Prices are chosen so every quantity is exact.
	d := momentumDecision(
		event.TargetWeight{InstrumentID: "AAA", Weight: 0.25}, event.TargetWeight{InstrumentID: "BBB", Weight: 0.24},
		event.TargetWeight{InstrumentID: "DDD", Weight: 0.25}, event.TargetWeight{InstrumentID: "IEF", Weight: 0.24},
	)
	prices := map[string]float64{"AAA": 50, "BBB": 30, "CCC": 20, "DDD": 25, "IEF": 100}
	plan := planIRA(t, iraInput(d, 10000, prices,
		event.AccountPosition{InstrumentID: "CCC", Quantity: 40}, event.AccountPosition{InstrumentID: "DDD", Quantity: 100}))
	sameLegs(t, "sells", plan.Sells, leg{"CCC", event.OrderSideSell, 40})
	// AAA 0.25 x 10,000 / 50 = 50; BBB 2,400 / 30 = 80; IEF 2,400 / 100 = 24.
	sameLegs(t, "buys", plan.Buys, leg{"AAA", event.OrderSideBuy, 50}, leg{"BBB", event.OrderSideBuy, 80}, leg{"IEF", event.OrderSideBuy, 24})
	exit := plan.Sells[0]
	if exit.TargetWeight != 0 || exit.HeldQuantity != 40 || exit.LimitPrice != 19 || exit.Phase != event.OrderSideSell {
		t.Errorf("exit: %+v", exit)
	}
	if exit.ClientOrderID != orderplan.ClientOrderID(iraTag, "CCC", event.OrderSideSell) {
		t.Errorf("exit client order ID %s is not derived from tag, instrument and phase", exit.ClientOrderID)
	}
	if plan.Buys[0].LimitPrice != 52.5 || plan.Buys[0].ReferencePrice != 50 {
		t.Errorf("entry limit %v, want the band's edge 52.5", plan.Buys[0].LimitPrice)
	}
}

func TestIRAPlanWithNoSellsHasOnlyBuys(t *testing.T) {
	// An account holding only cash: every target is a buy, there is no sell
	// phase, so the buys are planned at once.
	d := momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.49}, event.TargetWeight{InstrumentID: "IEF", Weight: 0.49})
	plan := planIRA(t, iraInput(d, 1000, map[string]float64{"AAA": 7, "IEF": 98}))
	sameLegs(t, "sells", plan.Sells)
	// AAA: 490 at 7 is 70 shares; IEF: 490 at 98 is 5.
	sameLegs(t, "buys", plan.Buys, leg{"AAA", event.OrderSideBuy, 70}, leg{"IEF", event.OrderSideBuy, 5})
}

func TestIRANeverSellsBeyondTheHoldingNetOfWorkingSells(t *testing.T) {
	d := momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.1})
	in := iraInput(d, 10000, map[string]float64{"AAA": 10, "BBB": 10},
		event.AccountPosition{InstrumentID: "AAA", Quantity: 500}, event.AccountPosition{InstrumentID: "BBB", Quantity: 30})
	// 120 AAA are already in working sells and 30 BBB in a cancel-pending one,
	// which may still execute: AAA can sell 380 (not 400), BBB nothing.
	in.WorkingSells = map[string]int64{"AAA": 120, "BBB": 30}
	plan := planIRA(t, in)
	sameLegs(t, "sells", plan.Sells, leg{"AAA", event.OrderSideSell, 380})
}

func TestIRARoundsTowardZero(t *testing.T) {
	// 0.33 x 1000 / 7 = 47.14 -> 47; a held name 0.2 below its target by more
	// than the band: (0.5 x 1000 - 30 x 7) / 7 = 41.43 -> 41.
	d := momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.33}, event.TargetWeight{InstrumentID: "BBB", Weight: 0.5})
	plan := planIRA(t, iraInput(d, 1000, map[string]float64{"AAA": 7, "BBB": 7}, event.AccountPosition{InstrumentID: "BBB", Quantity: 30}))
	sameLegs(t, "buys", plan.Buys, leg{"AAA", event.OrderSideBuy, 47}, leg{"BBB", event.OrderSideBuy, 41})
	// A target worth less than one share orders nothing.
	tiny := planIRA(t, iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.001}), 1000, map[string]float64{"AAA": 7}))
	sameLegs(t, "tiny", tiny.Buys)
}

func TestIRAPlannerRefusesMalformedInput(t *testing.T) {
	good := iraInput(momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.5}), 1000, map[string]float64{"AAA": 10})
	for name, c := range map[string]struct {
		mutate func(*orderplan.IRAInput)
		want   string
	}{
		"invalid decision": {func(in *orderplan.IRAInput) { in.Decision.AccountID = "" }, "account"},
		"invalid snapshot": {func(in *orderplan.IRAInput) { in.Snapshot.Currency = "" }, "currency"},
		"untagged":         {func(in *orderplan.IRAInput) { in.Tag = "" }, "tagged"},
		"withheld": {func(in *orderplan.IRAInput) {
			in.Decision.Outcome, in.Decision.Targets = event.TargetsOutcomeWithheld, nil
			in.Decision.WithheldReason, in.Decision.Detail = event.WithheldMissingUniverse, "none"
		}, "not a momentum rebalance"},
		"other account": {func(in *orderplan.IRAInput) { in.Snapshot.AccountID = "joint" }, "cannot value"},
		"price band":    {func(in *orderplan.IRAInput) { in.PriceBand = 0 }, "band"},
		"rebalance":     {func(in *orderplan.IRAInput) { in.RebalanceBand = 1 }, "band"},
		"short": {func(in *orderplan.IRAInput) {
			in.Snapshot.Positions = []event.AccountPosition{{InstrumentID: "AAA", Quantity: -1}}
		}, "short"},
		"no price": {func(in *orderplan.IRAInput) { in.ReferencePrices = nil }, "reference price"},
		"huge": {func(in *orderplan.IRAInput) {
			in.ReferencePrices = map[string]float64{"AAA": 1e-300}
		}, "cannot be ordered"},
	} {
		t.Run(name, func(t *testing.T) {
			in := good
			c.mutate(&in)
			if _, err := orderplan.PlanIRA(in); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v does not mention %q", err, c.want)
			}
		})
	}
}

func TestBuysAreCostedAtTheBandEdgePlusFees(t *testing.T) {
	// $980 at a $98 close is 10 shares; at the 5% band's edge, $102.90, they
	// cost $1,029 before fees, and $1,030 with a $1 fee allowance.
	d := momentumDecision(event.TargetWeight{InstrumentID: "AAA", Weight: 0.98})
	plan := planIRA(t, iraInput(d, 1000, map[string]float64{"AAA": 98}))
	sameLegs(t, "buy", plan.Buys, leg{"AAA", event.OrderSideBuy, 10})
	if got := orderplan.BuyCost(plan.Buys[0], 0); got != 1029 {
		t.Errorf("worst-case cost = %v, want 1029", got)
	}
	if got := orderplan.BuyCost(plan.Buys[0], 1); got != 1030 {
		t.Errorf("worst-case cost with fees = %v, want 1030", got)
	}
	// With $1,000 of cash the $1,029 does not fit: scaled by 1000/1029 to
	// 9.7 -> 9 shares, $926.10.
	kept, scale := orderplan.ScaleBuys(plan.Buys, 1000, 0)
	sameLegs(t, "scaled", kept, leg{"AAA", event.OrderSideBuy, 9})
	if scale != 1000.0/1029 {
		t.Errorf("scale = %v", scale)
	}
}

func buy(id string, quantity int64, limit float64) event.OrderPlannedPayload {
	return event.OrderPlannedPayload{InstrumentID: id, Side: event.OrderSideBuy, Quantity: quantity, LimitPrice: limit}
}

func TestScaleBuys(t *testing.T) {
	buys := []event.OrderPlannedPayload{buy("AAA", 10, 10), buy("BBB", 3, 100), buy("CCC", 1, 50)}
	// Total 100 + 300 + 50 = 450.
	fit, scale := orderplan.ScaleBuys(buys, 450, 0)
	sameLegs(t, "exactly fits", fit, leg{"AAA", event.OrderSideBuy, 10}, leg{"BBB", event.OrderSideBuy, 3}, leg{"CCC", event.OrderSideBuy, 1})
	if scale != 1 {
		t.Errorf("scale = %v, want 1", scale)
	}
	// Half the budget: 5 AAA, 1.5 -> 1 BBB, 0.5 -> 0 CCC (dropped).
	half, _ := orderplan.ScaleBuys(buys, 225, 0)
	sameLegs(t, "half", half, leg{"AAA", event.OrderSideBuy, 5}, leg{"BBB", event.OrderSideBuy, 1})
	// With $10 fees each the total is 480; at a budget of 240 the ADR's
	// scale 0.5 gives 5 AAA (60) + 1 BBB (110) + 0 CCC = 170, which fits.
	fees, s := orderplan.ScaleBuys(buys, 240, 10)
	sameLegs(t, "fees", fees, leg{"AAA", event.OrderSideBuy, 5}, leg{"BBB", event.OrderSideBuy, 1})
	if s != 0.5 {
		t.Errorf("scale = %v, want 0.5", s)
	}
	// One buy of 10 at 10 with $50 fees costs 150; budget 100 gives the
	// ADR's scale 2/3 -> 6 shares, 110, over budget; the fee-aware scale
	// (100 - 50) / 100 = 0.5 -> 5 shares, exactly 100.
	tight, s2 := orderplan.ScaleBuys([]event.OrderPlannedPayload{buy("AAA", 10, 10)}, 100, 50)
	sameLegs(t, "fee-aware", tight, leg{"AAA", event.OrderSideBuy, 5})
	if s2 != 0.5 {
		t.Errorf("fee-aware scale = %v, want 0.5", s2)
	}
	// No cash, or a debit: nothing is bought.
	for _, budget := range []float64{0, -100} {
		none, s := orderplan.ScaleBuys(buys, budget, 1)
		if len(none) != 0 || s != 0 {
			t.Errorf("budget %v kept %v at scale %v", budget, legs(none), s)
		}
	}
}

func TestBudgetCarriedThroughReleasesAndPartialFills(t *testing.T) {
	b := orderplan.NewBudget(1000)
	b.AddProceeds(200)
	if b.Remaining() != 1200 {
		t.Fatalf("start + proceeds = %v", b.Remaining())
	}
	b.Release("tw-a", 525) // 5 at a 105 limit
	b.Release("tw-b", 300)
	if b.Remaining() != 375 {
		t.Fatalf("after two releases = %v, want 375", b.Remaining())
	}
	// A partial fill of a at 100 (2 shares, 200 + 1 commission) releases
	// nothing while the rest still works.
	if !b.Fill("tw-a", 201) {
		t.Fatal("tw-a was released")
	}
	if b.Remaining() != 375 {
		t.Errorf("a partial fill released cash while the rest works: %v", b.Remaining())
	}
	// Cancelling the rest releases the unspent 324.
	if !b.Close("tw-a") {
		t.Fatal("tw-a was released")
	}
	if b.Remaining() != 699 {
		t.Errorf("after the cancel = %v, want 1200 - 201 - 300 = 699", b.Remaining())
	}
	// A fill above the worst case (it cannot happen at a limit, but fees may
	// exceed the allowance) commits what was spent.
	b.Fill("tw-b", 310)
	if b.Remaining() != 689 {
		t.Errorf("an overspent buy commits its spend: %v", b.Remaining())
	}
	if b.Fill("tw-z", 1) || b.Close("tw-z") {
		t.Error("an unreleased buy cannot fill or close")
	}
}

// TestBudgetLosesCashANewerSnapshotCannotExplain encodes ADR 0026 point 2,
// "Budget" (line 104: buys are funded from cash on hand plus cash realised by
// the sells): cash the account no longer has comes out of what the buys may
// commit, alongside the proceeds and commitments (PR #358 review).
func TestBudgetLosesCashANewerSnapshotCannotExplain(t *testing.T) {
	b := orderplan.NewBudget(1000)
	b.AddProceeds(500)
	b.Remove(300)
	if got := b.Remaining(); got != 1200 {
		t.Fatalf("remaining %v, want 1200", got)
	}
	b.Release("tw-a", 200)
	if got := b.Remaining(); got != 1000 {
		t.Fatalf("remaining %v after a release, want 1000", got)
	}
}
