package risklimit_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

func iraLimits() event.MarginLimits {
	return event.MarginLimits{MaxExposure: 1, MaxOrdersPerDecision: 10, MaxOrderFraction: 0.7, PriceBand: 0.05}
}

func iraGate(t *testing.T, maxFees float64) *risklimit.Gate {
	t.Helper()
	g, err := risklimit.NewIRAGate("joint", iraLimits(), event.TradingModeDryRun, maxFees)
	if err != nil {
		t.Fatal(err)
	}
	g.RecordDecision(tag)
	return g
}

// iraOrder is order on instrument id, priced 1 with a limit of 1, against net
// liquidation 10,000.
func iraOrder(id, side string, quantity int64) risklimit.Order {
	o := order(side, quantity, 0)
	o.PlanID = "order-planned:" + id + side
	o.Planned.InstrumentID, o.Planned.ClientOrderID = id, "tw-"+id+"-"+side
	return o
}

func checkBook(t *testing.T, g *risklimit.Gate, book risklimit.Book, plan ...risklimit.Order) []risklimit.Outcome {
	t.Helper()
	outcomes, err := g.Check(book, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outcomes {
		if o.Released != nil {
			if err := o.Released.Validate(); err != nil {
				t.Fatalf("invalid release: %v", err)
			}
		} else if err := o.Refused.Validate(); err != nil {
			t.Fatalf("invalid refusal: %v", err)
		}
	}
	return outcomes
}

// TestIRAExposureIsAccountWideAtBelowAndAbove holds 4,000 of IEF and buys
// SPY: the limit is checked on the sum, not on SPY alone.
func TestIRAExposureIsAccountWideAtBelowAndAbove(t *testing.T) {
	book := func() risklimit.Book {
		return risklimit.Book{Holdings: map[string]int64{"IEF": 4000}, Prices: map[string]float64{"IEF": 1}, CashAvailable: 1e6}
	}
	// To 99.99%, 100% exactly, and 100.01%.
	released(t, checkBook(t, iraGate(t, 0), book(), iraOrder("SPY", event.OrderSideBuy, 5999))[0])
	at := checkBook(t, iraGate(t, 0), book(), iraOrder("SPY", event.OrderSideBuy, 6000))[0]
	released(t, at)
	if at.Released.Figures.ExposureBefore != 0.4 || at.Released.Figures.ExposureAfter != 1 {
		t.Errorf("figures: %+v", at.Released.Figures)
	}
	above := checkBook(t, iraGate(t, 0), book(), iraOrder("SPY", event.OrderSideBuy, 6001))[0]
	refusedFor(t, above, event.RefusedIRAExposure)
	if above.State == nil || above.State.State != event.EngineStateDegraded || above.State.ScopeInstrumentIDs[0] != "SPY" {
		t.Errorf("an IRA exposure breach degrades the account: %+v", above.State)
	}
	// A sell never trips it, even from above 100%.
	over := risklimit.Book{Holdings: map[string]int64{"IEF": 4000, "SPY": 7000}, Prices: map[string]float64{"IEF": 1, "SPY": 1}}
	released(t, checkBook(t, iraGate(t, 0), over, iraOrder("SPY", event.OrderSideSell, 500))[0])
}

func TestMarginExposureIsAccountWide(t *testing.T) {
	// Holding 6,000 of IEF and 8,000 of SPY (140%), a 1,001-share SPY buy
	// leaves 150.01%: refused on the sum, though SPY alone is 90%.
	g := gate(t, limits(), event.TradingModeDryRun)
	book := risklimit.Book{Holdings: map[string]int64{"IEF": 6000, "SPY": 8000}, Prices: map[string]float64{"IEF": 1, "SPY": 1}}
	refusedFor(t, checkBook(t, g, book, iraOrder("SPY", event.OrderSideBuy, 1001))[0], event.RefusedMarginExposure)
}

func TestIRABuyCostAtBelowAndAboveTheCashAvailable(t *testing.T) {
	// A buy of 100 at a limit of 1 with $2 of fees costs 102.
	for _, c := range []struct {
		cash    float64
		refused bool
	}{{103, false}, {102, false}, {101.99, true}} {
		o := checkBook(t, iraGate(t, 2), risklimit.Book{CashAvailable: c.cash}, iraOrder("AAA", event.OrderSideBuy, 100))[0]
		if c.refused {
			refusedFor(t, o, event.RefusedInsufficientCash)
			continue
		}
		released(t, o)
		f := o.Released.Figures
		if !f.CashChecked || f.BuyCost != 102 || f.CashAvailable != c.cash {
			t.Errorf("cash %v: figures %+v", c.cash, f)
		}
	}
}

func TestIRANeverBuysOnMargin(t *testing.T) {
	// A debit balance is a margin loan: no buy is released against it.
	refusedFor(t, checkBook(t, iraGate(t, 0), risklimit.Book{CashAvailable: -1}, iraOrder("AAA", event.OrderSideBuy, 1))[0], event.RefusedInsufficientCash)
	// A sell is never cash-checked.
	o := checkBook(t, iraGate(t, 0), risklimit.Book{Holdings: map[string]int64{"AAA": 5}, Prices: map[string]float64{"AAA": 1}, CashAvailable: -1},
		iraOrder("AAA", event.OrderSideSell, 5))[0]
	released(t, o)
	if o.Released.Figures.CashChecked {
		t.Errorf("a sell carries no cash check: %+v", o.Released.Figures)
	}
}

func TestReleasedBuysCarryTheBudgetToTheNext(t *testing.T) {
	// Cash 250: the first buy of 100 commits 100, the second 100 leaves 50,
	// the third does not fit; a refused buy commits nothing, but the gate is
	// then Degraded, so the fourth is refused too.
	g := iraGate(t, 0)
	out := checkBook(t, g, risklimit.Book{CashAvailable: 250},
		iraOrder("AAA", event.OrderSideBuy, 100), iraOrder("BBB", event.OrderSideBuy, 100),
		iraOrder("CCC", event.OrderSideBuy, 100), iraOrder("DDD", event.OrderSideBuy, 10))
	released(t, out[0])
	released(t, out[1])
	if out[1].Released.Figures.CashAvailable != 150 {
		t.Errorf("second buy checked against %v, want 150", out[1].Released.Figures.CashAvailable)
	}
	refusedFor(t, out[2], event.RefusedInsufficientCash)
	refusedFor(t, out[3], event.RefusedEngineDegraded)
}

func TestASellBeyondTheHoldingNetOfWorkingSellsIsRefused(t *testing.T) {
	// 100 held, 30 in a cancel-pending sell that may still execute: 70 may be
	// sold, 71 may not. A released sell adds to the working sells for the next.
	book := func() risklimit.Book {
		return risklimit.Book{Holdings: map[string]int64{"AAA": 100}, Prices: map[string]float64{"AAA": 1}, WorkingSells: map[string]int64{"AAA": 30}}
	}
	released(t, checkBook(t, iraGate(t, 0), book(), iraOrder("AAA", event.OrderSideSell, 70))[0])
	refusedFor(t, checkBook(t, iraGate(t, 0), book(), iraOrder("AAA", event.OrderSideSell, 71))[0], event.RefusedSellExceedsHolding)
	second := iraOrder("AAA", event.OrderSideSell, 1)
	second.PlanID, second.Planned.ClientOrderID = "order-planned:second", "tw-second"
	out := checkBook(t, iraGate(t, 0), book(), iraOrder("AAA", event.OrderSideSell, 70), second)
	released(t, out[0])
	refusedFor(t, out[1], event.RefusedSellExceedsHolding)
	// Nothing held at all: the margin gate refuses a sale too.
	refusedFor(t, checkBook(t, gate(t, limits(), event.TradingModeDryRun), risklimit.Book{}, iraOrder("AAA", event.OrderSideSell, 1))[0], event.RefusedSellExceedsHolding)
}

func TestAnUnvaluedHoldingRefusesOnlyIncreasingOrders(t *testing.T) {
	book := risklimit.Book{Holdings: map[string]int64{"AAA": 10, "ZZZ": 5}, Prices: map[string]float64{"AAA": 1}, CashAvailable: 1e6}
	o := checkBook(t, iraGate(t, 0), book, iraOrder("BBB", event.OrderSideBuy, 10))[0]
	refusedFor(t, o, event.RefusedUnvaluedHolding)
	if !strings.Contains(o.Refused.Detail, "ZZZ") {
		t.Errorf("detail names the holding: %s", o.Refused.Detail)
	}
	released(t, checkBook(t, iraGate(t, 0), book, iraOrder("AAA", event.OrderSideSell, 10))[0])
}

func TestPriorOrdersCountTowardTheDecisionTime(t *testing.T) {
	// Ten orders allowed: nine sells already planned in the first phase, so
	// two buys in the second make eleven and are refused.
	out := checkBook(t, iraGate(t, 0), risklimit.Book{PriorOrders: 9, CashAvailable: 1e6},
		iraOrder("AAA", event.OrderSideBuy, 1), iraOrder("BBB", event.OrderSideBuy, 1))
	refusedFor(t, out[0], event.RefusedOrderCount)
	if out[0].Refused.Figures.OrdersInDecision != 11 {
		t.Errorf("figures: %+v", out[0].Refused.Figures)
	}
}

func TestIRAGateRefusesBadConstruction(t *testing.T) {
	margin := iraLimits()
	margin.MaxExposure = 1.5
	if _, err := risklimit.NewIRAGate("ira", margin, event.TradingModeDryRun, 0); err == nil {
		t.Error("an IRA gate above 100% accepted")
	}
	if _, err := risklimit.NewIRAGate("ira", iraLimits(), event.TradingModeDryRun, -1); err == nil {
		t.Error("a negative fee allowance accepted")
	}
	if _, err := risklimit.NewIRAGate("", iraLimits(), event.TradingModeDryRun, 0); err == nil {
		t.Error("a missing account accepted")
	}
}
