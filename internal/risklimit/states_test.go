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

// clock is each gate's latest broker snapshot time: a test's trigger is at
// it, or at the fixture time before any snapshot, so the latest snapshot is
// never taken after the trigger unless a test says so.
var clock = map[*risklimit.Gate]time.Time{}

// now is g's trigger time.
func now(g *risklimit.Gate) time.Time {
	if c, ok := clock[g]; ok && c.After(at) {
		return c
	}
	return at
}

func observe(t *testing.T, g *risklimit.Gate, id string, s event.BrokerSnapshotPayload) risklimit.Observation {
	t.Helper()
	obs, err := g.ObserveBroker(id, s)
	if err != nil {
		t.Fatal(err)
	}
	clock[g] = s.AsOf
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
	s, ok, err := g.Sale(trigger, "SPY", now(g), 0.98, 0.05)
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
// its figures record the exemption and no broker snapshot, so the journal
// alone explains why the mark, band and size were not checked.
func TestWithoutABrokerSnapshotASellIsExemptWithinTheAccountHolding(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	o := check(t, g, order(event.OrderSideSell, 5001, 9800))[0]
	released(t, o)
	if !o.Released.Figures.RiskReducing || o.Released.Figures.BrokerSet {
		t.Errorf("the exemption is recorded, without a broker snapshot: %+v", o.Released.Figures)
	}
	if err := o.Released.ValidateSchema(o.Released.SchemaVersion()); err != nil {
		t.Errorf("the figures journal: %v", err)
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
	t.Run("the latest snapshot, never an older readable one", func(t *testing.T) {
		g := gate(t, limits(), event.TradingModeDryRun)
		halt(t, g)
		observe(t, g, "broker:1", boosted())
		blind := boosted()
		blind.AsOf, blind.Unreadable = at.Add(time.Minute), []string{event.BrokerComponentOpenOrders}
		observe(t, g, "broker:2", blind)
		s, ok := requestSale(t, g, event.SaleTriggerExit)
		if !ok || s.Withheld == nil || s.Withheld.Reason != event.SaleWithheldUnreadable || s.Withheld.SnapshotID != "broker:2" {
			t.Fatalf("the latest snapshot is unreadable; the older readable one is not used: %+v", s)
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
			At: at, MaxSnapshotAge: 30 * time.Minute,
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
	// With no reading since the run began, the run's first snapshot starts
	// the clock: urgent once the stale period has passed since it, not at once.
	g = gate(t, limits(), event.TradingModeDryRun)
	if obs := observe(t, g, "broker:0", blind(0, true)); obs.Alarm != nil {
		t.Errorf("the run's first snapshot, without a reading: %+v", obs.Alarm)
	}
	if obs := observe(t, g, "broker:29", blind(29, true)); obs.Alarm != nil {
		t.Errorf("29 minutes since the first snapshot: %+v", obs.Alarm)
	}
	if obs := observe(t, g, "broker:30", blind(30, true)); !obs.Urgent || obs.Alarm.LastReadingSet {
		t.Errorf("never read in 30 minutes: %+v", obs.Alarm)
	}
	// An unreadable net liquidation is no reading either.
	g = gate(t, limits(), event.TradingModeDryRun)
	s := blind(0, true)
	s.Unreadable, s.NetLiquidation, s.ExcessLiquidity = []string{event.BrokerComponentNetLiquidation}, 0, 100
	observe(t, g, "broker:0", s)
	s.AsOf = at.Add(30 * time.Minute)
	if obs := observe(t, g, "broker:30", s); !obs.Urgent {
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
	bad.Unreadable = []string{"no-such-component"}
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
	if _, err := risklimit.NewGate("joint", noCushion, event.TradingModeDryRun, 30*time.Minute); err == nil || !strings.Contains(err.Error(), "excess liquidity") {
		t.Errorf("a margin gate without the alarm's thresholds: %v", err)
	}
	cushioned := iraLimits()
	cushioned.ExcessLiquidityWarning, cushioned.ExcessLiquidityUrgent, cushioned.ExcessLiquidityStaleMinutes = 0.4, 0.2, 30
	if _, err := risklimit.NewIRAGate("joint", cushioned, event.TradingModeDryRun, 0, 30*time.Minute); err == nil || !strings.Contains(err.Error(), "no margin cushion") {
		t.Errorf("an IRA gate with a margin cushion: %v", err)
	}
	if _, _, err := g.Sale("whim", "SPY", at, 0.98, 0.05); err == nil {
		t.Error("an unknown trigger accepted")
	}
	halt(t, g)
	if _, _, err := g.Sale(event.SaleTriggerExit, "SPY", at, 0, 0.05); err == nil {
		t.Error("a sale to no target accepted")
	}
}

// TestARestoredStateNamesTheIncident: the first restored record out of
// Normal is the incident, and a later one (Degraded to Halted) is the same
// incident (ADR 0019, amendment of 2026-10-17: Degraded to Halted is one
// incident), so the restored run's
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
// counted against every later sell, beside every other sell the broker
// reports working, so two sells never both take the same shares (ADR 0019,
// amended 2026-10-15, "back to 98% of net liquidation and never below it").
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
		t.Fatalf("the released sell is pending beside the broker's: %d", got)
	}
	again := order(event.OrderSideSell, 31, 100)
	again.Planned.ClientOrderID = "tw-again"
	refusedFor(t, check(t, g, again)[0], event.RefusedBrokerHolding)
}

// TestASaleIsNeverSizedFromAStaleBrokerSnapshot: the sale is sized from the
// latest broker snapshot only while it is no older than the configured
// maximum age at the trigger (ADR 0019: "a stale matching snapshot is not
// success"; the maximum snapshot age is required configuration). The last
// snapshot shows 147 at 15:00; the owner sells 100 by hand before Boost's
// exit fires at 23:00, so selling 54 from the 15:00 figures would open a
// short. The sale is withheld instead, the state unchanged; the withhold is
// not the attempt, so the next fresh snapshot is sized from again.
func TestASaleIsNeverSizedFromAStaleBrokerSnapshot(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", boosted())
	incident := halt(t, g).StateID
	trigger := at.Add(8 * time.Hour)
	s, ok, err := g.Sale(event.SaleTriggerExit, "SPY", trigger, 0.98, 0.05)
	if err != nil || !ok || s.Withheld == nil || s.Withheld.Reason != event.SaleWithheldStaleBrokerSnapshot {
		t.Fatalf("a snapshot 8 hours old at the trigger: %+v %v %v", s, ok, err)
	}
	if err := s.Withheld.Validate(); err != nil {
		t.Errorf("withheld record: %v", err)
	}
	if g.State() != risklimit.StateHalted || g.Incident() != incident {
		t.Errorf("state %s incident %s: unchanged by the withhold", g.State(), g.Incident())
	}
	// Not the attempt: a fresh snapshot at the next trigger sells.
	fresh := boosted()
	fresh.AsOf = trigger
	observe(t, g, "broker:2", fresh)
	if s, ok, _ := g.Sale(event.SaleTriggerExit, "SPY", trigger, 0.98, 0.05); !ok || s.Order == nil || s.Order.Tag != incident {
		t.Errorf("re-evaluated on a fresh snapshot under the same tag: %+v", s)
	}
	// Exactly the maximum age is still fresh.
	g = gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", boosted())
	halt(t, g)
	s, ok, err = g.Sale(event.SaleTriggerExit, "SPY", at.Add(30*time.Minute), 0.98, 0.05)
	if err != nil || !ok || s.Order == nil || s.Order.Quantity != 54 {
		t.Fatalf("a snapshot 30 minutes old: %+v %v %v", s, ok, err)
	}
}

// TestARestoredIncidentsEarlierAttemptIsNotRepeated: a fresh run that
// restores an incident also restores whether its sale was attempted (ADR
// 0026, point 5: "Whether the incident sale has been attempted"), so the one
// sale is never attempted twice across a restart (ADR 0019, amended
// 2026-10-15: "A failed or cancelled attempt is not retried automatically").
func TestARestoredIncidentsEarlierAttemptIsNotRepeated(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	halted := halt(t, g)

	r := gate(t, limits(), event.TradingModeDryRun)
	if err := r.RestoreSaleAttempt(halted.StateID, "SPY"); err == nil {
		t.Error("an attempt restored before its incident was accepted")
	}
	if err := r.RestoreState(halted.StateID, *halted.State, halted.StateSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := r.RestoreSaleAttempt("engine-state:joint:other", "SPY"); err == nil {
		t.Error("an attempt under another tag was accepted")
	}
	if err := r.RestoreSaleAttempt(halted.StateID, ""); err == nil {
		t.Error("an attempt with no instrument was accepted")
	}
	if err := r.RestoreSaleAttempt(halted.StateID, "SPY"); err != nil {
		t.Fatal(err)
	}
	observe(t, r, "broker:1", boosted())
	if s, ok := requestSale(t, r, event.SaleTriggerExit); ok {
		t.Fatalf("the restored incident's sale was attempted again: %+v", s)
	}
}

// TestAnOrdinarySellIsNeverCheckedAgainstAStaleSnapshot: the latest broker
// snapshot gates only the broker-holding check. A sell checked against one
// older than MaxSnapshotAge at its planned time is refused
// (stale-broker-snapshot) with no state change, and the first such refusal
// for that snapshot raises the urgent alert; a later snapshot is a new
// condition. A net liquidation or mark that is not positive does not refuse
// an ordinary sell: only the sale's keep reads them.
func TestAnOrdinarySellIsNeverCheckedAgainstAStaleSnapshot(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	sell := order(event.OrderSideSell, 10, 100)
	at31 := bookOf(sell)
	at31.At = at.Add(31 * time.Minute)
	refused := func(g *risklimit.Gate, book risklimit.Book, o risklimit.Order) *event.OrderRefusedPayload {
		t.Helper()
		out, err := g.Check(book, []risklimit.Order{o})
		if err != nil {
			t.Fatal(err)
		}
		refusedFor(t, out[0], event.RefusedStaleBrokerSnapshot)
		if out[0].State != nil {
			t.Errorf("the refusal changes no state: %+v", out[0].State)
		}
		if err := out[0].Refused.ValidateSchema(out[0].Refused.SchemaVersion()); err != nil {
			t.Error(err)
		}
		return out[0].Refused
	}
	if r := refused(g, at31, sell); !r.Alert {
		t.Errorf("the first refusal for the stale snapshot raises the alert: %+v", r)
	}
	again := sell
	again.Planned.ClientOrderID = "tw-again"
	if r := refused(g, at31, again); r.Alert {
		t.Error("once per snapshot condition")
	}
	next := broker(100)
	next.AsOf = at.Add(time.Minute)
	observe(t, g, "broker:2", next)
	third := sell
	third.Planned.ClientOrderID = "tw-third"
	at40 := at31
	at40.At = at.Add(40 * time.Minute)
	if r := refused(g, at40, third); !r.Alert {
		t.Error("a later stale snapshot is a new condition")
	}
	if g.State() != risklimit.StateNormal {
		t.Errorf("state %s", g.State())
	}
	// A broker snapshot taken after the sell was planned is stale too, never
	// fresh, and the refusal records the skew.
	ahead := gate(t, limits(), event.TradingModeDryRun)
	future := broker(100)
	future.AsOf = at.Add(5 * time.Minute)
	observe(t, ahead, "broker:1", future)
	if r := refused(ahead, bookOf(sell), sell); !strings.Contains(r.Detail, "after") {
		t.Errorf("the skew is recorded: %s", r.Detail)
	}
	for name, mutate := range map[string]func(*event.BrokerSnapshotPayload){
		"net liquidation": func(s *event.BrokerSnapshotPayload) { s.NetLiquidation = -1 },
		"mark":            func(s *event.BrokerSnapshotPayload) { s.Positions[0].Mark = 0 },
	} {
		g := gate(t, limits(), event.TradingModeDryRun)
		s := broker(100)
		mutate(&s)
		observe(t, g, "broker:1", s)
		if o := check(t, g, order(event.OrderSideSell, 10, 100))[0]; o.Released == nil {
			t.Errorf("%s not positive: an ordinary sell is not refused for it: %+v", name, o.Refused)
		}
	}
	if _, err := g.Check(risklimit.Book{}, []risklimit.Order{sell}); err == nil {
		t.Error("a check without its planned time")
	}
	blind := gate(t, limits(), event.TradingModeDryRun)
	unread := broker(100)
	unread.Unreadable = []string{event.BrokerComponentOpenOrders}
	observe(t, blind, "broker:1", unread)
	if out, err := blind.Check(at31, []risklimit.Order{sell}); err != nil || out[0].Released == nil {
		t.Errorf("an unreadable snapshot sizes as without one: %+v %v", out, err)
	}
}

// TestTheStateAndKillSwitchAreCheckedBeforeTheSnapshot: the engine-state and
// kill-switch refusals come before the snapshot checks, so a stale snapshot
// never stops Boost's exit from firing the sale back to 98%.
func TestTheStateAndKillSwitchAreCheckedBeforeTheSnapshot(t *testing.T) {
	late := func(g *risklimit.Gate) risklimit.Outcome {
		t.Helper()
		observe(t, g, "broker:9", func() event.BrokerSnapshotPayload { b := boosted(); b.AsOf = at.Add(-time.Hour); return b }())
		exit := order(event.OrderSideSell, 54, 147)
		exit.Planned.ClientOrderID = "tw-exit"
		book := bookOf(exit)
		out, err := g.Check(book, []risklimit.Order{exit})
		if err != nil {
			t.Fatal(err)
		}
		return out[0]
	}
	halted := gate(t, limits(), event.TradingModeDryRun)
	halt(t, halted)
	refusedFor(t, late(halted), event.RefusedEngineHalted)
	degraded := gate(t, limits(), event.TradingModeDryRun)
	degrade(t, degraded)
	refusedFor(t, late(degraded), event.RefusedEngineDegraded)
	killed := gate(t, limits(), event.TradingModeDryRun)
	kill(t, killed, "kill:1", true)
	refusedFor(t, late(killed), event.RefusedKillSwitch)
	if s, ok := requestSale(t, killed, event.SaleTriggerExit); !ok || s.Withheld == nil || s.Withheld.Reason != event.SaleWithheldStaleBrokerSnapshot {
		t.Errorf("the exit fired; the sale is withheld as stale: %+v %v", s, ok)
	}
}

// TestAWithheldExitSaleIsRetriedWhileTheExitIsInForce: a sale Boost's exit
// fired, withheld from a stale snapshot, is decided again on the next
// readable, positive broker snapshot while the account still refuses the
// exit; it lapses, attempt unspent, once a decision no longer plans the sell
// or the state that refused the exit has ended.
func TestAWithheldExitSaleIsRetriedWhileTheExitIsInForce(t *testing.T) {
	stale := func(g *risklimit.Gate) {
		t.Helper()
		observe(t, g, "broker:1", boosted())
		s, ok, err := g.Sale(event.SaleTriggerExit, "SPY", at.Add(time.Hour), 0.98, 0.05)
		if err != nil || !ok || s.Withheld == nil || s.Withheld.Reason != event.SaleWithheldStaleBrokerSnapshot {
			t.Fatalf("setup: %+v %v %v", s, ok, err)
		}
	}
	snap := func(minutes int, mutate func(*event.BrokerSnapshotPayload)) event.BrokerSnapshotPayload {
		b := boosted()
		b.AsOf = at.Add(time.Duration(minutes) * time.Minute)
		if mutate != nil {
			mutate(&b)
		}
		return b
	}
	g := gate(t, limits(), event.TradingModeDryRun)
	incident := halt(t, g).StateID
	stale(g)
	blind := func(b *event.BrokerSnapshotPayload) {
		b.Unreadable, b.OpenOrders = []string{event.BrokerComponentOpenOrders}, nil
	}
	if obs := observe(t, g, "broker:2", snap(61, blind)); obs.RetryExit {
		t.Error("an unreadable snapshot does not retry")
	}
	if obs := observe(t, g, "broker:3", snap(62, func(b *event.BrokerSnapshotPayload) { b.NetLiquidation = -1 })); obs.RetryExit {
		t.Error("a non-positive snapshot does not retry")
	}
	if obs := observe(t, g, "broker:4", snap(63, nil)); !obs.RetryExit {
		t.Fatal("the next readable, positive snapshot retries the exit's sale")
	}
	s, ok, err := g.Sale(event.SaleTriggerExit, "SPY", at.Add(63*time.Minute), 0.98, 0.05)
	if err != nil || !ok || s.Order == nil || s.Order.Tag != incident {
		t.Fatalf("retried under the incident: %+v %v %v", s, ok, err)
	}
	if obs := observe(t, g, "broker:5", snap(64, nil)); obs.RetryExit {
		t.Error("once planned, not retried")
	}

	// A decision that no longer plans the sell ends it.
	g = gate(t, limits(), event.TradingModeDryRun)
	halt(t, g)
	stale(g)
	g.LapseExitSale()
	if obs := observe(t, g, "broker:2", snap(61, nil)); obs.RetryExit {
		t.Error("lapsed with the decision")
	}

	// The state that refused the exit ending ends it.
	g = gate(t, limits(), event.TradingModeDryRun)
	kill(t, g, "kill:1", true)
	stale(g)
	kill(t, g, "kill:2", false)
	if obs := observe(t, g, "broker:2", snap(61, nil)); obs.RetryExit {
		t.Error("lapsed when the kill switch was turned off")
	}
}

// TestANonPositiveSnapshotWithholdsTheSaleUntilAPositiveOne: the sale is
// withheld (non-positive-broker-values), the state is unchanged and no
// attempt is spent, so the next readable, positive, fresh snapshot sizes it.
func TestANonPositiveSnapshotWithholdsTheSaleUntilAPositiveOne(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	incident := halt(t, g).StateID
	deficit := boosted()
	deficit.NetLiquidation = -200
	observe(t, g, "broker:1", deficit)
	s, ok := requestSale(t, g, event.SaleTriggerExit)
	if !ok || s.Withheld == nil || s.Withheld.Reason != event.SaleWithheldNotPositive {
		t.Fatalf("withheld: %+v", s)
	}
	if g.State() != risklimit.StateHalted || g.Incident() != incident {
		t.Errorf("unchanged by the withhold: %s %s", g.State(), g.Incident())
	}
	positive := boosted()
	positive.AsOf = at.Add(time.Minute)
	observe(t, g, "broker:2", positive)
	s, ok = requestSale(t, g, event.SaleTriggerExit)
	if !ok || s.Order == nil || s.Order.Quantity != 54 || s.Order.Tag != incident {
		t.Fatalf("re-evaluated from the positive snapshot: %+v", s)
	}
}

// TestAnUrgentAlarmStillInForceRetriesAWithheldSale: a sale the urgent alarm
// fired, withheld because the latest snapshot reported a net liquidation
// that is not positive, is decided again on the next snapshot while the
// alarm is still urgent, though the alarm's level has not changed.
func TestAnUrgentAlarmStillInForceRetriesAWithheldSale(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:0", boosted())
	deficit := boosted()
	deficit.AsOf, deficit.NetLiquidation = at.Add(30*time.Minute), -200
	if obs := observe(t, g, "broker:30", deficit); !obs.Urgent {
		t.Fatalf("no reading for 30 minutes: %+v", obs.Alarm)
	}
	s, ok := requestSale(t, g, event.SaleTriggerMarginUrgent)
	if !ok || s.Withheld == nil || s.Withheld.Reason != event.SaleWithheldNotPositive {
		t.Fatalf("withheld: %+v", s)
	}
	low := boosted()
	low.AsOf, low.ExcessLiquidity = at.Add(31*time.Minute), 1500
	obs := observe(t, g, "broker:31", low)
	if obs.Alarm != nil || !obs.Urgent {
		t.Fatalf("still urgent, no new alarm, the sale decided again: %+v", obs)
	}
	if s, ok := requestSale(t, g, event.SaleTriggerMarginUrgent); !ok || s.Order == nil || s.Order.Quantity != 54 {
		t.Fatalf("re-evaluated: %+v", s)
	}
	later := boosted()
	later.AsOf, later.ExcessLiquidity = at.Add(32*time.Minute), 1500
	if obs := observe(t, g, "broker:32", later); obs.Urgent {
		t.Error("once planned, the alarm's sale is not decided again")
	}
}
