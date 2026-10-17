package risklimit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

var at = time.Date(2025, time.March, 5, 15, 0, 0, 0, time.UTC)

// broker is a broker snapshot of the order fixture's account: SPY held at a
// mark of 1 against net liquidation 10,000, so exposures read as held/10,000,
// with Excess Liquidity at 60% of net liquidation.
func broker(held int64, orders ...event.BrokerOrder) event.BrokerSnapshotPayload {
	s := event.BrokerSnapshotPayload{AccountID: "joint", AsOf: at, MarketOpen: true, NetLiquidation: 10000, ExcessLiquidity: 6000, OpenOrders: orders}
	if held != 0 {
		s.Positions = []event.BrokerPosition{{InstrumentID: "SPY", Quantity: held, Mark: 1, MarkSet: true}}
	}
	return s
}

// boosted is ADR 0019's 2026-10-15 worked example: 147 shares, $4,700
// borrowed, a mark of $90.
func boosted(orders ...event.BrokerOrder) event.BrokerSnapshotPayload {
	return event.BrokerSnapshotPayload{
		AccountID: "joint", AsOf: at, MarketOpen: true, NetLiquidation: 8530, ExcessLiquidity: 5222.5,
		Positions:  []event.BrokerPosition{{InstrumentID: "SPY", Quantity: 147, Mark: 90, MarkSet: true}},
		OpenOrders: orders,
	}
}

func observe(t *testing.T, g *risklimit.Gate, id string, s event.BrokerSnapshotPayload) risklimit.Observation {
	t.Helper()
	obs, err := g.ObserveBroker(id, s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Alarm != nil {
		if err := obs.Alarm.Validate(); err != nil {
			t.Fatalf("invalid alarm: %v", err)
		}
	}
	return obs
}

func kill(t *testing.T, g *risklimit.Gate, id string, engaged bool) {
	t.Helper()
	if err := g.SetKillSwitch(id, event.KillSwitchPayload{AccountID: "joint", AsOf: at, Engaged: engaged, Operator: "owner", Detail: "test"}); err != nil {
		t.Fatal(err)
	}
}

func requestSale(t *testing.T, g *risklimit.Gate, trigger string) (orderplan.Sale, bool) {
	t.Helper()
	s, ok, err := g.Sale(trigger, "SPY", 0.98, 0.05)
	if err != nil {
		t.Fatal(err)
	}
	return s, ok
}

// saleOrder wraps a planned sale for the gate.
func saleOrder(p *event.OrderPlannedPayload) risklimit.Order {
	return risklimit.Order{PlanID: "order-planned:" + p.ClientOrderID, Planned: *p}
}

// degrade breaches the order size on SPY, so the account is Degraded naming SPY.
func degrade(t *testing.T, g *risklimit.Gate) risklimit.Outcome {
	t.Helper()
	o := check(t, g, order(event.OrderSideBuy, 5001, 0))[0]
	if o.State == nil || o.State.State != event.EngineStateDegraded {
		t.Fatalf("setup: not degraded: %+v", o)
	}
	return o
}

// halt refuses an untagged order, so the account is Halted.
func halt(t *testing.T, g *risklimit.Gate) risklimit.Outcome {
	t.Helper()
	o := order(event.OrderSideSell, 1, 10)
	o.Planned.Tag, o.Planned.ClientOrderID = "", "tw-untagged"
	out := check(t, g, o)[0]
	if out.State == nil || out.State.State != event.EngineStateHalted {
		t.Fatalf("setup: not halted: %+v", out)
	}
	return out
}

func TestAStateChangeIsNamedForTheOrderThatCausedIt(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	if o := degrade(t, g); o.StateID != "engine-state:joint:tw-buy" {
		t.Errorf("state id %q", o.StateID)
	}
	if o := halt(t, g); o.StateID != "engine-state:joint:tw-untagged" {
		t.Errorf("state id %q", o.StateID)
	}
	if g.Incident() != "engine-state:joint:tw-buy" {
		t.Errorf("the incident is the record that left Normal, not a later one: %q", g.Incident())
	}
}

func TestRiskReducingSellsAreExemptFromCountSizeAndBand(t *testing.T) {
	l := limits()
	l.MaxOrdersPerDecision = 1
	g := gate(t, l, event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(16000))
	// 6,000 of SPY at 60% of V: over the size limit; its mark is missing and
	// the plan is over the count. Within the broker's holding, so it goes.
	sell := order(event.OrderSideSell, 6000, 16000)
	sell.Mark, sell.MarkSet = 0, false
	other := order(event.OrderSideSell, 10, 100)
	other.Planned.InstrumentID, other.Planned.ClientOrderID = "IEF", "tw-ief"
	out := check(t, g, sell, other)
	released(t, out[0])
	f := out[0].Released.Figures
	if !f.RiskReducing || !f.BrokerSet || f.BrokerHolding != 16000 || f.WorkingSells != 0 {
		t.Errorf("figures: %+v", f)
	}
	// IEF is not held at the broker, so that sell is not risk-reducing; it is
	// a sell beyond the holding.
	refusedFor(t, out[1], event.RefusedBrokerHolding)

	// Exactly the holding net of a working sell is within it; one more is not.
	g = gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(5000, event.BrokerOrder{
		BrokerOrderID: "ib-1", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderCancelPending, RemainingQuantity: 1000,
	}))
	released(t, check(t, g, order(event.OrderSideSell, 4000, 5000))[0])
	over := check(t, g, order(event.OrderSideSell, 4001, 5000))[0]
	refusedFor(t, over, event.RefusedBrokerHolding)
	if over.State == nil || over.State.State != event.EngineStateDegraded {
		t.Errorf("a sell beyond the broker's holding is a breach: %+v", over.State)
	}

	// No buy is risk-reducing: the band still applies.
	g = gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	buy := order(event.OrderSideBuy, 10, 100)
	buy.Mark = 2
	refusedFor(t, check(t, g, buy)[0], event.RefusedPriceBand)
}

// TestWithoutABrokerSnapshotASellIsExemptWithinTheAccountHolding: with no
// readable broker snapshot, a Normal account's sell is risk-reducing within
// the account snapshot's holding it was sized from (ADR 0026, point 4), and
// its figures record no broker snapshot.
func TestWithoutABrokerSnapshotASellIsExemptWithinTheAccountHolding(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	o := check(t, g, order(event.OrderSideSell, 5001, 9800))[0]
	released(t, o)
	if o.Released.Figures.RiskReducing || o.Released.Figures.BrokerSet {
		t.Errorf("figures: %+v", o.Released.Figures)
	}
	// Beyond the holding it is not exempt, and the book's holding refuses it
	// before the size is reached.
	refusedFor(t, check(t, g, order(event.OrderSideSell, 5001, 5000))[0], event.RefusedSellExceedsHolding)
}

func TestDegradedSellsElsewhereAreSizedFromTheBroker(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	degrade(t, g)
	ief := func(quantity int64) risklimit.Order {
		o := order(event.OrderSideSell, quantity, 300)
		o.Planned.InstrumentID, o.Planned.ClientOrderID = "IEF", "tw-ief"
		return o
	}
	// No broker snapshot: nothing to size the sale from.
	refusedFor(t, check(t, g, ief(100))[0], event.RefusedBrokerHolding)
	s := broker(200)
	s.Positions = append([]event.BrokerPosition{{InstrumentID: "IEF", Quantity: 250, Mark: 1, MarkSet: true}}, s.Positions...)
	observe(t, g, "broker:1", s)
	got := check(t, g, ief(250))[0]
	released(t, got)
	if !got.Released.Figures.RiskReducing || got.Released.Figures.BrokerHolding != 250 {
		t.Errorf("figures: %+v", got.Released.Figures)
	}
	// The planner believed 300; the broker holds 250. The broker governs.
	refusedFor(t, check(t, g, ief(251))[0], event.RefusedBrokerHolding)
	// SPY stays frozen and no buy goes anywhere.
	refusedFor(t, check(t, g, order(event.OrderSideSell, 1, 200))[0], event.RefusedEngineDegraded)
	buy := ief(1)
	buy.Planned.Side, buy.Planned.Phase, buy.Planned.LimitPrice = event.OrderSideBuy, event.OrderSideBuy, 1.05
	refusedFor(t, check(t, g, buy)[0], event.RefusedEngineDegraded)
}

func TestKillSwitchRefusesEveryRoutineOrderAndLeavesTheState(t *testing.T) {
	for kind, g := range map[string]*risklimit.Gate{"margin": gate(t, limits(), event.TradingModeDryRun), "ira": iraGate(t, 0)} {
		observe(t, g, "broker:1", broker(9800))
		kill(t, g, "kill:1", true)
		for _, o := range []risklimit.Order{order(event.OrderSideBuy, 1, 9800), order(event.OrderSideSell, 1, 9800)} {
			got := check(t, g, o)[0]
			refusedFor(t, got, event.RefusedKillSwitch)
			if got.State != nil || g.State() != risklimit.StateNormal {
				t.Errorf("%s: the kill switch changes no state: %+v", kind, got.State)
			}
		}
		kill(t, g, "kill:2", false)
		released(t, check(t, g, order(event.OrderSideSell, 1, 9800))[0])
	}
}

func TestKillSwitchRefusesARoutineIRASellAndPassesTheSaleBackTo98(t *testing.T) {
	ira := iraGate(t, 0)
	observe(t, ira, "broker:1", boosted())
	kill(t, ira, "kill:1", true)
	refusedFor(t, check(t, ira, order(event.OrderSideSell, 10, 147))[0], event.RefusedKillSwitch)
	if _, ok := requestSale(t, ira, event.SaleTriggerExit); ok {
		t.Error("ADR 0019's sale is for a margin account, never an IRA")
	}

	margin := gate(t, limits(), event.TradingModeDryRun)
	observe(t, margin, "broker:1", boosted())
	kill(t, margin, "kill:1", true)
	refusedFor(t, check(t, margin, order(event.OrderSideSell, 54, 147))[0], event.RefusedKillSwitch)
	s, ok := requestSale(t, margin, event.SaleTriggerExit)
	if !ok || s.Order == nil {
		t.Fatalf("the sale back to 98%% goes through the kill switch: %+v %v", s, ok)
	}
	if s.Order.Tag != "kill:1" || s.Order.Quantity != 54 || s.Order.Sale.Kind != event.SaleKindIncident {
		t.Errorf("sale: %+v", s.Order)
	}
	got := check(t, margin, saleOrder(s.Order))[0]
	released(t, got)
	if !got.Released.Figures.RiskReducing || margin.State() != risklimit.StateNormal {
		t.Errorf("figures %+v state %s", got.Released.Figures, margin.State())
	}
	// Once per engagement: a repeated engagement is the same one.
	kill(t, margin, "kill:2", true)
	if _, ok := requestSale(t, margin, event.SaleTriggerExit); ok {
		t.Error("attempted once per engagement")
	}
}

func TestDegradedOnSPYWhileBoostedStillSellsBackTo98(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	degraded := degrade(t, g)
	observe(t, g, "broker:1", boosted())
	// Boost's exit fires; the planned exit on the named instrument is frozen.
	refusedFor(t, check(t, g, order(event.OrderSideSell, 54, 147))[0], event.RefusedEngineDegraded)
	s, ok := requestSale(t, g, event.SaleTriggerExit)
	if !ok || s.Order == nil {
		t.Fatalf("ADR 0019's 2026-10-16 sale: %+v %v", s, ok)
	}
	if s.Order.Tag != degraded.StateID || s.Order.ClientOrderID != orderplan.SaleClientOrderID(degraded.StateID, "SPY") {
		t.Errorf("tagged with the incident: %+v", s.Order)
	}
	if s.Order.Quantity != 54 || s.Order.ReferencePrice != 90 || s.Order.NetLiquidation != 8530 {
		t.Errorf("sized from the broker snapshot: %+v", s.Order)
	}
	got := check(t, g, saleOrder(s.Order))[0]
	released(t, got)
	if g.State() != risklimit.StateDegraded {
		t.Error("the account stays Degraded until recovery")
	}
	// Every other order on SPY stays frozen, and there is no second sale.
	refusedFor(t, check(t, g, order(event.OrderSideSell, 1, 93))[0], event.RefusedEngineDegraded)
	if _, ok := requestSale(t, g, event.SaleTriggerMarginUrgent); ok {
		t.Error("one attempt per incident, whatever the trigger")
	}
	refusedFor(t, check(t, g, saleOrder(s.Order))[0], event.RefusedSaleAttempted)
}

func TestNoIncidentSaleForAnUnnamedInstrumentOrInNormal(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", boosted())
	if _, ok := requestSale(t, g, event.SaleTriggerExit); ok {
		t.Error("in Normal the exit is an ordinary order, not a sale")
	}
	l := limits()
	g = gate(t, l, event.TradingModeDryRun)
	ief := order(event.OrderSideBuy, 5001, 0)
	ief.Planned.InstrumentID, ief.Planned.ClientOrderID = "IEF", "tw-ief"
	check(t, g, ief)
	observe(t, g, "broker:1", boosted())
	if _, ok := requestSale(t, g, event.SaleTriggerExit); ok {
		t.Error("Degraded on IEF: SPY's exit is an ordinary risk-reducing sell")
	}
}

func TestHaltedSendsOnlyTheSaleOncePerIncident(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	halted := halt(t, g)
	observe(t, g, "broker:1", boosted())
	refusedFor(t, check(t, g, order(event.OrderSideSell, 54, 147))[0], event.RefusedEngineHalted)
	s, ok := requestSale(t, g, event.SaleTriggerExit)
	if !ok || s.Order == nil || s.Order.Tag != halted.StateID {
		t.Fatalf("Halted sale: %+v %v", s, ok)
	}
	released(t, check(t, g, saleOrder(s.Order))[0])
	if _, ok := requestSale(t, g, event.SaleTriggerExit); ok {
		t.Error("attempted once only")
	}
	if g.State() != risklimit.StateHalted {
		t.Error("the account stays Halted until recovery")
	}

	ira := iraGate(t, 0)
	halt(t, ira)
	observe(t, ira, "broker:1", boosted())
	if _, ok := requestSale(t, ira, event.SaleTriggerExit); ok {
		t.Error("not for IRAs")
	}
}

func TestHaltedSaleChecks(t *testing.T) {
	ours := event.BrokerOrder{BrokerOrderID: "ib-1", ClientOrderID: "tw-ours", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: 20}
	t.Run("net of a working sell", func(t *testing.T) {
		g := gate(t, limits(), event.TradingModeDryRun)
		mine := order(event.OrderSideSell, 20, 147)
		mine.Planned.ClientOrderID = "tw-ours"
		released(t, check(t, g, mine)[0])
		halt(t, g)
		observe(t, g, "broker:1", boosted(ours))
		s, _ := requestSale(t, g, event.SaleTriggerExit)
		if s.Order == nil || s.Order.Quantity != 34 || s.Order.Sale.WorkingSells != 20 {
			t.Fatalf("sale: %+v", s)
		}
		got := check(t, g, saleOrder(s.Order))[0]
		released(t, got)
		if got.Released.Figures.WorkingSells != 20 {
			t.Errorf("figures: %+v", got.Released.Figures)
		}
	})
	t.Run("an unknown working sell", func(t *testing.T) {
		g := gate(t, limits(), event.TradingModeDryRun)
		halt(t, g)
		observe(t, g, "broker:1", boosted(ours))
		s, ok := requestSale(t, g, event.SaleTriggerExit)
		if !ok || s.Withheld == nil || s.Withheld.Reason != event.SaleWithheldUnknownWorkingSell {
			t.Fatalf("withheld: %+v", s)
		}
		// The alert has told the owner to act by hand: never attempted again.
		observe(t, g, "broker:2", func() event.BrokerSnapshotPayload { b := boosted(); b.AsOf = at.Add(time.Minute); return b }())
		if _, ok := requestSale(t, g, event.SaleTriggerExit); ok {
			t.Error("a withheld sale that asked for a person is not retried")
		}
	})
	t.Run("no readable broker values", func(t *testing.T) {
		g := gate(t, limits(), event.TradingModeDryRun)
		halt(t, g)
		s, ok := requestSale(t, g, event.SaleTriggerExit)
		if !ok || s.Withheld == nil || s.Withheld.Reason != event.SaleWithheldUnreadable {
			t.Fatalf("withheld: %+v", s)
		}
		if _, ok := requestSale(t, g, event.SaleTriggerExit); ok {
			t.Error("not retried")
		}
	})
	t.Run("nothing to sell is not an attempt", func(t *testing.T) {
		g := gate(t, limits(), event.TradingModeDryRun)
		halt(t, g)
		low := boosted()
		low.Positions[0].Quantity = 90
		observe(t, g, "broker:1", low)
		s, _ := requestSale(t, g, event.SaleTriggerExit)
		if s.Withheld == nil || s.Withheld.Reason != event.SaleWithheldNothingToSell {
			t.Fatalf("withheld: %+v", s)
		}
		later := boosted()
		later.AsOf = at.Add(time.Minute)
		observe(t, g, "broker:2", later)
		if s, ok := requestSale(t, g, event.SaleTriggerMarginUrgent); !ok || s.Order == nil {
			t.Fatalf("a later trigger in the incident may still sell: %+v", s)
		}
	})
	t.Run("a forged sale halts", func(t *testing.T) {
		g := gate(t, limits(), event.TradingModeDryRun)
		observe(t, g, "broker:1", boosted())
		forged, err := orderplan.PlanSale(orderplan.SaleInput{
			AccountID: "joint", Tag: "engine-state:joint:never", InstrumentID: "SPY", Kind: event.SaleKindIncident,
			Trigger: event.SaleTriggerExit, Target: 0.98, PriceBand: 0.05, SnapshotID: "broker:1", Snapshot: ptr(boosted()),
		})
		if err != nil {
			t.Fatal(err)
		}
		got := check(t, g, saleOrder(forged.Order))[0]
		refusedFor(t, got, event.RefusedUndecidedOrder)
		if g.State() != risklimit.StateHalted {
			t.Error("a sale no incident issued is a defect in the order path")
		}
	})
	t.Run("a sale beyond a later snapshot's holding", func(t *testing.T) {
		g := gate(t, limits(), event.TradingModeDryRun)
		halt(t, g)
		observe(t, g, "broker:1", boosted())
		s, _ := requestSale(t, g, event.SaleTriggerExit)
		smaller := boosted()
		smaller.AsOf, smaller.Positions[0].Quantity = at.Add(time.Minute), 40
		observe(t, g, "broker:2", smaller)
		refusedFor(t, check(t, g, saleOrder(s.Order))[0], event.RefusedBrokerHolding)
	})
}

func ptr[T any](v T) *T { return &v }

func TestMarginAlarmLevels(t *testing.T) {
	for _, tc := range []struct {
		excess float64
		level  string
	}{{4000, event.MarginAlarmClear}, {3999, event.MarginAlarmWarning}, {2000, event.MarginAlarmWarning}, {1999, event.MarginAlarmUrgent}, {-500, event.MarginAlarmUrgent}} {
		g := gate(t, limits(), event.TradingModeDryRun)
		s := broker(14700)
		s.ExcessLiquidity = tc.excess
		obs := observe(t, g, "broker:1", s)
		if tc.level == event.MarginAlarmClear {
			if obs.Alarm != nil {
				t.Errorf("excess %v: a clear first reading changes nothing: %+v", tc.excess, obs.Alarm)
			}
			continue
		}
		if obs.Alarm == nil || obs.Alarm.Level != tc.level || obs.Alarm.Cause != event.MarginAlarmCauseRatio {
			t.Errorf("excess %v: alarm %+v, want %s", tc.excess, obs.Alarm, tc.level)
			continue
		}
		if obs.AlarmID != "margin-alarm:joint:broker:1" || obs.Urgent != (tc.level == event.MarginAlarmUrgent) {
			t.Errorf("excess %v: id %q urgent %v", tc.excess, obs.AlarmID, obs.Urgent)
		}
		if obs.Alarm.Warning != 0.4 || obs.Alarm.Urgent != 0.2 || obs.Alarm.StaleMinutes != 30 {
			t.Errorf("thresholds: %+v", obs.Alarm)
		}
	}
}

func TestMarginAlarmChangesOnlyOnANewLevel(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	s := broker(14700)
	s.ExcessLiquidity = 1000
	if obs := observe(t, g, "broker:1", s); !obs.Urgent {
		t.Fatal("urgent")
	}
	s.AsOf = at.Add(time.Minute)
	if obs := observe(t, g, "broker:2", s); obs.Alarm != nil || obs.Urgent {
		t.Errorf("still urgent: no new alarm: %+v", obs)
	}
	s.AsOf, s.ExcessLiquidity = at.Add(2*time.Minute), 5000
	if obs := observe(t, g, "broker:3", s); obs.Alarm == nil || obs.Alarm.Level != event.MarginAlarmClear {
		t.Errorf("cleared: %+v", obs.Alarm)
	}
}

func TestNoReadingForThirtyMinutesWhileOpenIsUrgent(t *testing.T) {
	blind := func(minutes int, open bool) event.BrokerSnapshotPayload {
		s := broker(14700)
		s.AsOf, s.MarketOpen = at.Add(time.Duration(minutes)*time.Minute), open
		s.Unreadable, s.ExcessLiquidity = []string{event.BrokerComponentExcessLiquidity}, 0
		return s
	}
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:0", broker(14700))
	if obs := observe(t, g, "broker:29", blind(29, true)); obs.Alarm != nil {
		t.Errorf("29 minutes: %+v", obs.Alarm)
	}
	if obs := observe(t, g, "broker:31", blind(31, false)); obs.Alarm != nil {
		t.Errorf("market closed: %+v", obs.Alarm)
	}
	obs := observe(t, g, "broker:32", blind(32, true))
	if obs.Alarm == nil || !obs.Urgent || obs.Alarm.Cause != event.MarginAlarmCauseNoReading || !obs.Alarm.LastReadingAt.Equal(at) {
		t.Fatalf("32 minutes open: %+v", obs.Alarm)
	}
	// Exactly 30 minutes counts.
	g = gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:0", broker(14700))
	if obs := observe(t, g, "broker:30", blind(30, true)); !obs.Urgent {
		t.Error("30 minutes open is urgent")
	}
	// No reading since the run began, while open, is urgent at once.
	g = gate(t, limits(), event.TradingModeDryRun)
	if obs := observe(t, g, "broker:0", blind(0, true)); !obs.Urgent || obs.Alarm.LastReadingSet {
		t.Errorf("never read: %+v", obs.Alarm)
	}
	// An unreadable net liquidation is no reading either.
	g = gate(t, limits(), event.TradingModeDryRun)
	s := blind(0, true)
	s.Unreadable, s.NetLiquidation, s.ExcessLiquidity = []string{event.BrokerComponentNetLiquidation}, 0, 100
	if obs := observe(t, g, "broker:0", s); !obs.Urgent {
		t.Error("no net liquidation, no ratio")
	}
	// An IRA has no margin alarm.
	ira := iraGate(t, 0)
	if obs := observe(t, ira, "broker:0", blind(0, true)); obs.Alarm != nil {
		t.Errorf("IRA alarm: %+v", obs.Alarm)
	}
}

func TestUrgentAlarmSellsBackTo98InNormal(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	s := boosted()
	s.ExcessLiquidity = 1500
	obs := observe(t, g, "broker:1", s)
	if !obs.Urgent {
		t.Fatal("urgent")
	}
	sale, ok := requestSale(t, g, event.SaleTriggerMarginUrgent)
	if !ok || sale.Order == nil || sale.Order.Sale.Kind != event.SaleKindUrgent || sale.Order.Tag != obs.AlarmID || sale.Order.Quantity != 54 {
		t.Fatalf("urgent sale: %+v %v", sale, ok)
	}
	got := check(t, g, saleOrder(sale.Order))[0]
	released(t, got)
	if got.State != nil || g.State() != risklimit.StateNormal {
		t.Error("the urgent sale changes no state")
	}
	if _, ok := requestSale(t, g, event.SaleTriggerMarginUrgent); ok {
		t.Error("once per urgent alarm")
	}
	// Without an urgent alarm there is no urgent sale.
	g = gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", boosted())
	if _, ok := requestSale(t, g, event.SaleTriggerMarginUrgent); ok {
		t.Error("no alarm, no sale")
	}
}

func TestBrokerAndKillSwitchInputsAreChecked(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	other := broker(1)
	other.AccountID = "ira"
	if _, err := g.ObserveBroker("broker:1", other); err == nil {
		t.Error("another account's snapshot accepted")
	}
	bad := broker(1)
	bad.NetLiquidation = -1
	if _, err := g.ObserveBroker("broker:1", bad); err == nil {
		t.Error("an invalid snapshot accepted")
	}
	if _, err := g.ObserveBroker("", broker(1)); err == nil {
		t.Error("a snapshot without its ID accepted")
	}
	observe(t, g, "broker:1", broker(1))
	if _, err := g.ObserveBroker("broker:2", broker(1)); err == nil || !strings.Contains(err.Error(), "follow") {
		t.Errorf("a snapshot not after the last: %v", err)
	}
	if err := g.SetKillSwitch("kill:1", event.KillSwitchPayload{AccountID: "ira", AsOf: at, Engaged: true, Operator: "o", Detail: "d"}); err == nil {
		t.Error("another account's switch accepted")
	}
	if err := g.SetKillSwitch("kill:1", event.KillSwitchPayload{AccountID: "joint"}); err == nil {
		t.Error("an invalid switch accepted")
	}
	if err := g.SetKillSwitch("", event.KillSwitchPayload{AccountID: "joint", AsOf: at, Engaged: true, Operator: "o", Detail: "d"}); err == nil {
		t.Error("a switch without its ID accepted")
	}
	noCushion := limits()
	noCushion.ExcessLiquidityWarning, noCushion.ExcessLiquidityUrgent, noCushion.ExcessLiquidityStaleMinutes = 0, 0, 0
	if _, err := risklimit.NewGate("joint", noCushion, event.TradingModeDryRun); err == nil || !strings.Contains(err.Error(), "excess liquidity") {
		t.Errorf("a margin gate without the alarm's thresholds: %v", err)
	}
	cushioned := iraLimits()
	cushioned.ExcessLiquidityWarning, cushioned.ExcessLiquidityUrgent, cushioned.ExcessLiquidityStaleMinutes = 0.4, 0.2, 30
	if _, err := risklimit.NewIRAGate("joint", cushioned, event.TradingModeDryRun, 0); err == nil || !strings.Contains(err.Error(), "no margin cushion") {
		t.Errorf("an IRA gate with a margin cushion: %v", err)
	}
	if _, _, err := g.Sale("whim", "SPY", 0.98, 0.05); err == nil {
		t.Error("an unknown trigger accepted")
	}
	halt(t, g)
	if _, _, err := g.Sale(event.SaleTriggerExit, "SPY", 0, 0.05); err == nil {
		t.Error("a sale to no target accepted")
	}
}

// TestARestoredStateNamesTheIncident: the first restored record out of
// Normal is the incident, and a later one (Degraded to Halted) is the same
// incident (ADR 0019, amended 2026-10-17, reading 3), so the restored run's
// one sale back to 98% is tagged with the first.
func TestARestoredStateNamesTheIncident(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	degraded := degrade(t, g)
	halted := halt(t, g)

	r := gate(t, limits(), event.TradingModeDryRun)
	if err := r.RestoreState(degraded.StateID, *degraded.State, degraded.StateSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := r.RestoreState(halted.StateID, *halted.State, halted.StateSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if r.State() != risklimit.StateHalted || r.Incident() != degraded.StateID {
		t.Fatalf("state %s incident %s, want halted and %s", r.State(), r.Incident(), degraded.StateID)
	}
	observe(t, r, "broker:1", boosted())
	s, ok := requestSale(t, r, event.SaleTriggerExit)
	if !ok || s.Order == nil || s.Order.Tag != degraded.StateID {
		t.Fatalf("restored incident's sale: %+v %v", s, ok)
	}
	released(t, check(t, r, saleOrder(s.Order))[0])
	if _, ok := requestSale(t, r, event.SaleTriggerExit); ok {
		t.Error("the incident's sale is attempted once")
	}
}

// TestASellReleasedSinceTheSnapshotIsCountedAsWorking: a released sell is
// counted against every later sell until a broker snapshot recorded after it
// shows it, so two sells never both take the same shares (ADR 0019 line 241,
// "back to 98% of net liquidation and never below it").
func TestASellReleasedSinceTheSnapshotIsCountedAsWorking(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100, event.BrokerOrder{
		BrokerOrderID: "ib-1", ClientOrderID: "tw-x", InstrumentID: "SPY", Side: event.OrderSideSell,
		Status: event.BrokerOrderWorking, RemainingQuantity: 10,
	}))
	if got := g.PendingSells("SPY"); got != 10 {
		t.Fatalf("pending sells before any release: %d", got)
	}
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	if got := g.PendingSells("SPY"); got != 70 {
		t.Fatalf("the released sell is pending until a snapshot shows it: %d", got)
	}
	again := order(event.OrderSideSell, 31, 100)
	again.Planned.ClientOrderID = "tw-again"
	refusedFor(t, check(t, g, again)[0], event.RefusedBrokerHolding)
	later := broker(40)
	later.AsOf = later.AsOf.Add(time.Minute)
	later.Unreadable = []string{event.BrokerComponentOpenOrders}
	observe(t, g, "broker:2", later)
	if got := g.PendingSells("SPY"); got != 0 {
		t.Errorf("a later snapshot accounts for the released sell: %d", got)
	}
}
