package risklimit_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

const tag = "targets-decided:joint:2025-03-04"

func limits() event.MarginLimits {
	return event.MarginLimits{MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 0.5, PriceBand: 0.05}
}

func gate(t *testing.T, l event.MarginLimits, mode string) *risklimit.Gate {
	t.Helper()
	g, err := risklimit.NewGate("joint", l, mode)
	if err != nil {
		t.Fatal(err)
	}
	g.RecordDecision(tag)
	return g
}

// order is a planned order on SPY at price 1 against net liquidation 10,000,
// so exposures are exact: held/10,000.
func order(side string, quantity, held int64) risklimit.Order {
	return risklimit.Order{
		PlanID: "order-planned:" + side,
		Planned: event.OrderPlannedPayload{
			AccountID: "joint", Tag: tag, ClientOrderID: "tw-" + side, InstrumentID: "SPY", Side: side, Phase: side,
			OrderType: event.OrderTypeLimit, Quantity: quantity, LimitPrice: 1, TargetWeight: 1,
			NetLiquidation: 10000, HeldQuantity: held, ReferencePrice: 1, PriceBand: 0.05,
		},
		Mark: 1, MarkSet: true,
	}
}

// bookOf is the book the orders were planned against: each order's held
// quantity of its instrument at its reference price, and nothing else held.
func bookOf(plan ...risklimit.Order) risklimit.Book {
	book := risklimit.Book{Holdings: map[string]int64{}, Prices: map[string]float64{}}
	for _, o := range plan {
		if o.Planned.HeldQuantity != 0 {
			book.Holdings[o.Planned.InstrumentID] = o.Planned.HeldQuantity
			book.Prices[o.Planned.InstrumentID] = o.Planned.ReferencePrice
		}
	}
	return book
}

func check(t *testing.T, g *risklimit.Gate, plan ...risklimit.Order) []risklimit.Outcome {
	t.Helper()
	outcomes, err := g.Check(bookOf(plan...), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != len(plan) {
		t.Fatalf("%d outcomes for %d orders", len(outcomes), len(plan))
	}
	for _, o := range outcomes {
		if o.Released != nil {
			if err := o.Released.Validate(); err != nil {
				t.Fatalf("invalid release: %v", err)
			}
		}
		if o.Refused != nil {
			if err := o.Refused.Validate(); err != nil {
				t.Fatalf("invalid refusal: %v", err)
			}
		}
		if o.State != nil {
			if err := o.State.ValidateSchema(o.StateSchemaVersion); err != nil {
				t.Fatalf("invalid engine state: %v", err)
			}
		}
	}
	return outcomes
}

func refusedFor(t *testing.T, o risklimit.Outcome, reason string) {
	t.Helper()
	if o.Refused == nil {
		t.Fatalf("released, want refused for %s: %+v", reason, o.Released)
	}
	if o.Refused.Reason != reason {
		t.Fatalf("refused for %s (%s), want %s", o.Refused.Reason, o.Refused.Detail, reason)
	}
}

func released(t *testing.T, o risklimit.Outcome) {
	t.Helper()
	if o.Released == nil {
		t.Fatalf("refused for %s: %s", o.Refused.Reason, o.Refused.Detail)
	}
}

func TestMarginExposureAtBelowAndAbove(t *testing.T) {
	// From 140% held: a buy to 149.99%, to 150% exactly, and to 150.01%.
	released(t, check(t, gate(t, limits(), event.TradingModeDryRun), order(event.OrderSideBuy, 999, 14000))[0])
	released(t, check(t, gate(t, limits(), event.TradingModeDryRun), order(event.OrderSideBuy, 1000, 14000))[0])
	g := gate(t, limits(), event.TradingModeDryRun)
	o := check(t, g, order(event.OrderSideBuy, 1001, 14000))[0]
	refusedFor(t, o, event.RefusedMarginExposure)
	if o.Refused.Figures.ExposureAfter != 1.5001 || o.Refused.Figures.ExposureBefore != 1.4 {
		t.Errorf("figures: %+v", o.Refused.Figures)
	}
	if o.State == nil || o.State.State != event.EngineStateDegraded || o.State.Reason != event.EngineStateReasonRiskLimitBreached {
		t.Fatalf("a margin breach degrades the account: %+v", o.State)
	}
	if o.StateSchemaVersion != event.EngineStateScopedVersion || len(o.State.ScopeInstrumentIDs) != 1 || o.State.ScopeInstrumentIDs[0] != "SPY" {
		t.Errorf("degraded scope: %+v at schema %d", o.State, o.StateSchemaVersion)
	}
	if g.State() != risklimit.StateDegraded {
		t.Errorf("gate state %s, want degraded", g.State())
	}
}

func TestASellIsNeverRefusedByTheExposureCheck(t *testing.T) {
	// 160% to 155%: still above 150%, but the sale reduces exposure (ADR 0026, point 3).
	o := check(t, gate(t, limits(), event.TradingModeDryRun), order(event.OrderSideSell, 500, 16000))[0]
	released(t, o)
	if o.Released.Figures.ExposureBefore != 1.6 || o.Released.Figures.ExposureAfter != 1.55 {
		t.Errorf("figures: %+v", o.Released.Figures)
	}
}

func TestMaxOrderSizeAtBelowAndAbove(t *testing.T) {
	released(t, check(t, gate(t, limits(), event.TradingModeDryRun), order(event.OrderSideBuy, 4999, 0))[0])
	released(t, check(t, gate(t, limits(), event.TradingModeDryRun), order(event.OrderSideBuy, 5000, 0))[0])
	refusedFor(t, check(t, gate(t, limits(), event.TradingModeDryRun), order(event.OrderSideBuy, 5001, 0))[0], event.RefusedOrderSize)
	refusedFor(t, check(t, gate(t, limits(), event.TradingModeDryRun), order(event.OrderSideSell, 5001, 9800))[0], event.RefusedOrderSize)
}

func TestMaxOrderCountAtBelowAndAbove(t *testing.T) {
	l := limits()
	l.MaxOrdersPerDecision = 2
	buy := order(event.OrderSideBuy, 10, 100)
	sell := order(event.OrderSideSell, 10, 100)
	sell.Planned.InstrumentID = "IEF"
	sell.Planned.ClientOrderID = "tw-ief"
	released(t, check(t, gate(t, l, event.TradingModeDryRun), buy)[0])
	for _, o := range check(t, gate(t, l, event.TradingModeDryRun), sell, buy) {
		released(t, o)
	}
	l.MaxOrdersPerDecision = 1
	outcomes := check(t, gate(t, l, event.TradingModeDryRun), sell, buy)
	// Over the count, the plan's buys are refused; its sells are checked as usual.
	released(t, outcomes[0])
	refusedFor(t, outcomes[1], event.RefusedOrderCount)
	if outcomes[1].Refused.Figures.OrdersInDecision != 2 {
		t.Errorf("figures: %+v", outcomes[1].Refused.Figures)
	}
}

func TestPriceBandAtBelowAndAbove(t *testing.T) {
	for _, tc := range []struct {
		mark    float64
		refused bool
	}{{1, false}, {0.95, true}, {1.06, true}, {1.04, false}} {
		o := order(event.OrderSideBuy, 10, 0)
		o.Mark = tc.mark
		got := check(t, gate(t, limits(), event.TradingModeDryRun), o)[0]
		if tc.refused {
			refusedFor(t, got, event.RefusedPriceBand)
		} else {
			released(t, got)
		}
	}
	// Exactly at the band: reference 105 against mark 100 is 5%.
	o := order(event.OrderSideBuy, 10, 0)
	o.Planned.ReferencePrice, o.Planned.LimitPrice, o.Planned.NetLiquidation, o.Mark = 105, 110.25, 1e6, 100
	released(t, check(t, gate(t, limits(), event.TradingModeDryRun), o)[0])
}

func TestMissingMarkIsRefused(t *testing.T) {
	o := order(event.OrderSideBuy, 10, 0)
	o.Mark, o.MarkSet = 0, false
	refusedFor(t, check(t, gate(t, limits(), event.TradingModeDryRun), o)[0], event.RefusedMissingMark)
}

func TestAnUntaggedOrderIsRefusedAndHalts(t *testing.T) {
	var g *risklimit.Gate
	for _, badTag := range []string{"targets-decided:joint:never", ""} {
		g = gate(t, limits(), event.TradingModeDryRun)
		o := order(event.OrderSideSell, 10, 100)
		o.Planned.Tag = badTag
		got := check(t, g, o)[0]
		refusedFor(t, got, event.RefusedUndecidedOrder)
		if got.State == nil || got.State.State != event.EngineStateHalted || got.StateSchemaVersion != event.EngineStateSchemaVersion {
			t.Fatalf("an order with no journalled decision halts: %+v", got.State)
		}
	}
	if g.State() != risklimit.StateHalted {
		t.Fatalf("gate state %s, want halted", g.State())
	}
	got := check(t, g, order(event.OrderSideSell, 10, 100))[0]
	refusedFor(t, got, event.RefusedEngineHalted)
	if got.State != nil {
		t.Error("a halted gate does not announce the state again")
	}
}

func TestDegradedAllowsNoBuyAndFreezesItsScope(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	check(t, g, order(event.OrderSideBuy, 5001, 0))
	refusedFor(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0], event.RefusedEngineDegraded)
	refusedFor(t, check(t, g, order(event.OrderSideSell, 1, 10))[0], event.RefusedEngineDegraded)
	other := order(event.OrderSideSell, 1, 10)
	other.Planned.InstrumentID = "IEF"
	released(t, check(t, g, other)[0])
	otherBuy := order(event.OrderSideBuy, 1, 10)
	otherBuy.Planned.InstrumentID = "IEF"
	refusedFor(t, check(t, g, otherBuy)[0], event.RefusedEngineDegraded)
}

func TestASecondBreachWidensTheDegradedScope(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	check(t, g, order(event.OrderSideBuy, 5001, 0))
	o := order(event.OrderSideSell, 10, 100)
	o.Planned.InstrumentID = "IEF"
	o.MarkSet, o.Mark = false, 0
	got := check(t, g, o)[0]
	refusedFor(t, got, event.RefusedMissingMark)
	if got.State == nil || len(got.State.ScopeInstrumentIDs) != 2 {
		t.Fatalf("scope should name IEF and SPY: %+v", got.State)
	}
	again := check(t, g, o)[0]
	refusedFor(t, again, event.RefusedEngineDegraded)
	if again.State != nil {
		t.Error("a frozen instrument's refusal does not restate the scope")
	}
}

func TestDryRunReleasesWithoutSending(t *testing.T) {
	o := check(t, gate(t, limits(), event.TradingModeDryRun), order(event.OrderSideBuy, 10, 0))[0]
	released(t, o)
	if o.Released.Send || o.Released.Mode != event.TradingModeDryRun {
		t.Errorf("dry-run release: send %v mode %s", o.Released.Send, o.Released.Mode)
	}
	o = check(t, gate(t, limits(), event.TradingModePaper), order(event.OrderSideBuy, 10, 0))[0]
	if !o.Released.Send {
		t.Error("a paper release is marked to send")
	}
	if o.Released.PlanID != "order-planned:buy" || o.Released.Tag != tag || o.Released.ClientOrderID != "tw-buy" {
		t.Errorf("identity: %+v", o.Released)
	}
}

func TestGateRefusesBadConstructionAndInput(t *testing.T) {
	if _, err := risklimit.NewGate("joint", event.MarginLimits{}, event.TradingModeDryRun); err == nil {
		t.Error("invalid limits accepted")
	}
	if _, err := risklimit.NewGate("joint", limits(), "yolo"); err == nil {
		t.Error("invalid mode accepted")
	}
	if _, err := risklimit.NewGate("", limits(), event.TradingModeDryRun); err == nil {
		t.Error("missing account accepted")
	}
	g := gate(t, limits(), event.TradingModeDryRun)
	bad := order(event.OrderSideBuy, 0, 0)
	if _, err := g.Check(risklimit.Book{}, []risklimit.Order{bad}); err == nil {
		t.Error("an invalid planned order must be an error, not a refusal")
	}
	wrongAccount := order(event.OrderSideBuy, 1, 0)
	wrongAccount.Planned.AccountID = "ira"
	if _, err := g.Check(risklimit.Book{}, []risklimit.Order{wrongAccount}); err == nil {
		t.Error("another account's order must be an error")
	}
	noPlan := order(event.OrderSideBuy, 1, 0)
	noPlan.PlanID = ""
	if _, err := g.Check(risklimit.Book{}, []risklimit.Order{noPlan}); err == nil {
		t.Error("an order without its plan ID must be an error")
	}
	badMark := order(event.OrderSideBuy, 1, 0)
	badMark.Mark = -1
	if _, err := g.Check(risklimit.Book{}, []risklimit.Order{badMark}); err == nil {
		t.Error("a negative recorded mark must be an error")
	}
	if g.State() != risklimit.StateNormal {
		t.Errorf("errors change no state: %s", g.State())
	}
}
