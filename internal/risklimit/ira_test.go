package risklimit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

func iraLimits() event.MarginLimits {
	return event.MarginLimits{MaxExposure: 1, MaxOrdersPerDecision: 10, MaxOrderFraction: 0.7, PriceBand: 0.05}
}

func iraGate(t *testing.T, maxFees float64) *risklimit.Gate {
	t.Helper()
	g, err := risklimit.NewIRAGate("joint", iraLimits(), event.TradingModeDryRun, maxFees, 30*time.Minute)
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
	if book.At.IsZero() {
		book.At = now(g)
	}
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
	// sold, 71 may not. A released sell lowers what the next may take.
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

// TestEachReleasedSellReducesWhatMaySellOnlyOnce encodes ADR 0026 point 2.5
// (docs/adr/0026-...md line 95: "A sell never exceeds the broker-reported
// holding minus every working or pending sell") for two sells in one plan: 100
// held, 40 then 50 sold leaves 10 that may still be sold, so both go, and an
// 11th share would not. A released sell counts once against the holding, not
// once as a lower holding and again as a working sell.
func TestEachReleasedSellReducesWhatMaySellOnlyOnce(t *testing.T) {
	book := func() risklimit.Book {
		return risklimit.Book{Holdings: map[string]int64{"AAA": 100}, Prices: map[string]float64{"AAA": 1}}
	}
	sell := func(name string, quantity int64) risklimit.Order {
		o := iraOrder("AAA", event.OrderSideSell, quantity)
		o.PlanID, o.Planned.ClientOrderID = "order-planned:"+name, "tw-"+name
		return o
	}
	for _, g := range []*risklimit.Gate{iraGate(t, 0), gate(t, limits(), event.TradingModeDryRun)} {
		out := checkBook(t, g, book(), sell("first", 40), sell("second", 50), sell("third", 10))
		released(t, out[0])
		released(t, out[1])
		released(t, out[2])
		over := checkBook(t, g, book(), sell("first", 40), sell("second", 50), sell("third", 11))
		refusedFor(t, over[2], event.RefusedSellExceedsHolding)
	}
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
	// Ten orders allowed: nine sells that are not risk-reducing already
	// planned in the first phase, so two buys in the second make eleven
	// counted and are refused.
	out := checkBook(t, iraGate(t, 0), risklimit.Book{PriorOrders: 9, PriorCounted: 9, CashAvailable: 1e6},
		iraOrder("AAA", event.OrderSideBuy, 1), iraOrder("BBB", event.OrderSideBuy, 1))
	refusedFor(t, out[0], event.RefusedOrderCount)
	if out[0].Refused.Figures.OrdersInDecision != 11 {
		t.Errorf("figures: %+v", out[0].Refused.Figures)
	}
	// Nine risk-reducing sells are exempt from the count (ADR 0026, point
	// 3), so the same buys are released; the figures still name all eleven.
	out = checkBook(t, iraGate(t, 0), risklimit.Book{PriorOrders: 9, CashAvailable: 1e6},
		iraOrder("AAA", event.OrderSideBuy, 1), iraOrder("BBB", event.OrderSideBuy, 1))
	for _, o := range out {
		released(t, o)
		if o.Released.Figures.OrdersInDecision != 11 {
			t.Errorf("figures: %+v", o.Released.Figures)
		}
	}
}

func TestIRAGateRefusesBadConstruction(t *testing.T) {
	margin := iraLimits()
	margin.MaxExposure = 1.5
	if _, err := risklimit.NewIRAGate("ira", margin, event.TradingModeDryRun, 0, 30*time.Minute); err == nil {
		t.Error("an IRA gate above 100% accepted")
	}
	if _, err := risklimit.NewIRAGate("ira", iraLimits(), event.TradingModeDryRun, -1, 30*time.Minute); err == nil {
		t.Error("a negative fee allowance accepted")
	}
	if _, err := risklimit.NewIRAGate("", iraLimits(), event.TradingModeDryRun, 0, 30*time.Minute); err == nil {
		t.Error("a missing account accepted")
	}
}

// TestAnIRASellIsNeverCheckedAgainstAStaleSnapshot: MaxSnapshotAge is
// required for every account, since an IRA's broker-holding check reads the
// broker snapshot too.
func TestAnIRASellIsNeverCheckedAgainstAStaleSnapshot(t *testing.T) {
	if _, err := risklimit.NewIRAGate("ira", iraLimits(), event.TradingModeDryRun, 0, 0); err == nil {
		t.Error("an IRA gate without a maximum broker snapshot age")
	}
	g := iraGate(t, 0)
	b := broker(100)
	b.Positions[0].InstrumentID = "AAA"
	observe(t, g, "broker:1", b)
	sell := iraOrder("AAA", event.OrderSideSell, 10)
	book := risklimit.Book{Holdings: map[string]int64{"AAA": 100}, Prices: map[string]float64{"AAA": 1}, At: at.Add(31 * time.Minute)}
	refusedFor(t, checkBook(t, g, book, sell)[0], event.RefusedStaleBrokerSnapshot)
}

// TestAnIRAGateNamesNoSaleTag: an IRA plans no sale back to 98%, so no
// sale's tag is in force, whatever its state (ADR 0019).
func TestAnIRAGateNamesNoSaleTag(t *testing.T) {
	g := iraGate(t, 0)
	if err := g.RestoreIncidentState("incident-1", event.EngineStateHalted, nil, nil); err != nil {
		t.Fatal(err)
	}
	if tag := g.SaleTag(event.SaleTriggerMarginUrgent, "SPY"); tag != "" {
		t.Errorf("an IRA gate names sale tag %q, want none", tag)
	}
}

// ADR 0026, amendment of 2026-10-29: a missing IRA buy mark
// withholds that pick alone, without spending its cash or degrading.
func TestIRAMissingMarkWithholdsOnlyThatBuy(t *testing.T) {
	g := iraGate(t, 0)
	missing := iraOrder("AAA", event.OrderSideBuy, 100)
	missing.MarkSet = false
	out := checkBook(t, g, risklimit.Book{CashAvailable: 200}, missing, iraOrder("BBB", event.OrderSideBuy, 100))
	if out[0].Refused == nil || out[0].Refused.Reason != event.RefusedMissingMark || out[0].State != nil {
		t.Fatalf("missing mark outcome: %+v; want refusal without state change", out[0])
	}
	released(t, out[1])
	if out[1].Released.Figures.CashAvailable != 200 || g.State() != risklimit.StateNormal {
		t.Fatalf("cash/state changed on withheld buy: %+v, %s", out[1].Released, g.State())
	}
}

func TestIRAMissingMarkDoesNotMaskAnotherBreach(t *testing.T) {
	g := iraGate(t, 0)
	missing := iraOrder("AAA", event.OrderSideBuy, 100)
	missing.MarkSet = false
	out := checkBook(t, g, risklimit.Book{CashAvailable: 200}, missing, iraOrder("BBB", event.OrderSideBuy, 300), iraOrder("CCC", event.OrderSideBuy, 1))
	if out[1].Refused == nil || out[1].Refused.Reason != event.RefusedInsufficientCash || out[1].State == nil {
		t.Fatalf("second breach must degrade on insufficient cash: %+v", out[1])
	}
	if out[2].Refused == nil || out[2].Refused.Reason != event.RefusedEngineDegraded {
		t.Fatalf("later buy: %+v", out[2])
	}
}

// ADR 0026, amendment of 2026-10-29: only a missing mark is exempt
// from degradation; another breach on that same buy still degrades.
func TestIRAMissingMarkDoesNotMaskSameBuyBreach(t *testing.T) {
	for _, tc := range []struct {
		reason   string
		quantity int64
		book     risklimit.Book
	}{
		{event.RefusedInsufficientCash, 5000, risklimit.Book{CashAvailable: 200}},
		{event.RefusedOrderSize, 7001, risklimit.Book{CashAvailable: 10000}},
		{event.RefusedUnvaluedHolding, 100, risklimit.Book{CashAvailable: 10000, Holdings: map[string]int64{"ZZZ": 1}}},
		{event.RefusedIRAExposure, 5000, risklimit.Book{CashAvailable: 10000, Holdings: map[string]int64{"ZZZ": 6000}, Prices: map[string]float64{"ZZZ": 1}}},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			buy := iraOrder("AAA", event.OrderSideBuy, tc.quantity)
			marked := checkBook(t, iraGate(t, 0), tc.book, buy)[0]
			refusedFor(t, marked, tc.reason)

			g := iraGate(t, 0)
			buy.MarkSet, buy.Mark = false, 0
			out := checkBook(t, g, tc.book, buy, iraOrder("BBB", event.OrderSideBuy, 100))
			refusedFor(t, out[0], event.RefusedMissingMark)
			if out[0].Refused.Figures.MarkSet || out[0].Refused.Figures.Mark != 0 {
				t.Errorf("refusal invented a mark: %+v", out[0].Refused.Figures)
			}
			if out[0].State == nil {
				t.Errorf("missing mark masked %s: no Degraded transition", tc.reason)
			} else {
				state := out[0].State
				if err := state.ValidateSchema(out[0].StateSchemaVersion); err != nil {
					t.Fatalf("invalid engine state: %v", err)
				}
				if state.State != event.EngineStateDegraded || state.Reason != event.EngineStateReasonRiskLimitBreached ||
					len(state.ScopeInstrumentIDs) != 1 || state.ScopeInstrumentIDs[0] != "AAA" || state.Detail != marked.State.Detail {
					t.Errorf("state %+v, want the marked buy's breach: %+v", state, marked.State)
				}
			}
			if g.State() != risklimit.StateDegraded {
				t.Errorf("gate state = %s, want Degraded", g.State())
			}
			refusedFor(t, out[1], event.RefusedEngineDegraded)
		})
	}
}
