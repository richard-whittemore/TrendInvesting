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

func check(t *testing.T, g *risklimit.Gate, plan ...risklimit.Order) []risklimit.Outcome {
	t.Helper()
	outcomes, err := g.Check(plan)
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
	// A sell beyond the holding is not risk-reducing, so the size limit applies.
	refusedFor(t, check(t, gate(t, limits(), event.TradingModeDryRun), order(event.OrderSideSell, 5001, 100))[0], event.RefusedOrderSize)
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
	// A risk-reducing sell is exempt from the count (ADR 0026, point 3,
	// lines 188-189), so it does not push the plan's buy over it.
	for _, o := range check(t, gate(t, l, event.TradingModeDryRun), sell, buy) {
		released(t, o)
	}
	// A sell beyond its holding is counted; over the count, the plan's buys
	// are refused and its sells are checked as usual.
	short := sell
	short.Planned.Quantity = 101
	outcomes := check(t, gate(t, l, event.TradingModeDryRun), short, buy)
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
		// Schema version 2: risk-limit-breached is not a version-1 reason.
		if got.State == nil || got.State.State != event.EngineStateHalted || got.StateSchemaVersion != event.EngineStateScopedVersion {
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
	// A sell beyond its holding would open a short: nothing that opens a
	// position goes while not Normal (ADR 0019, amended 2026-09-24,
	// "Risk-reducing management").
	otherShort := order(event.OrderSideSell, 11, 10)
	otherShort.Planned.InstrumentID = "IEF"
	refusedFor(t, check(t, g, otherShort)[0], event.RefusedEngineDegraded)
	otherBuy := order(event.OrderSideBuy, 1, 10)
	otherBuy.Planned.InstrumentID = "IEF"
	refusedFor(t, check(t, g, otherBuy)[0], event.RefusedEngineDegraded)
}

// TestDegradedKeepsManagingInstrumentsItDoesNotName: while Degraded,
// "risk-reducing sells continue for instruments the discrepancy does not
// name" (ADR 0026, point 4, lines 204-205), and they stay exempt from the
// mark, band and size checks (point 3, lines 188-191), so a late mark on one
// of them neither refuses it nor widens the scope.
func TestDegradedKeepsManagingInstrumentsItDoesNotName(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	check(t, g, order(event.OrderSideBuy, 5001, 0))
	o := order(event.OrderSideSell, 60, 100)
	o.Planned.InstrumentID = "IEF"
	o.MarkSet, o.Mark = false, 0
	got := check(t, g, o)[0]
	released(t, got)
	if got.State != nil || g.State() != risklimit.StateDegraded {
		t.Errorf("a released sell changes no state: %+v, gate %s", got.State, g.State())
	}
	o.MarkSet, o.Mark = true, 2
	released(t, check(t, g, o)[0])
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
	if _, err := g.Check([]risklimit.Order{bad}); err == nil {
		t.Error("an invalid planned order must be an error, not a refusal")
	}
	wrongAccount := order(event.OrderSideBuy, 1, 0)
	wrongAccount.Planned.AccountID = "ira"
	if _, err := g.Check([]risklimit.Order{wrongAccount}); err == nil {
		t.Error("another account's order must be an error")
	}
	noPlan := order(event.OrderSideBuy, 1, 0)
	noPlan.PlanID = ""
	if _, err := g.Check([]risklimit.Order{noPlan}); err == nil {
		t.Error("an order without its plan ID must be an error")
	}
	badMark := order(event.OrderSideBuy, 1, 0)
	badMark.Mark = -1
	if _, err := g.Check([]risklimit.Order{badMark}); err == nil {
		t.Error("a negative recorded mark must be an error")
	}
	if g.State() != risklimit.StateNormal {
		t.Errorf("errors change no state: %s", g.State())
	}
}

// sellFrom147 is Boost's exit: 14,700 shares at 1 against net liquidation
// 10,000 (147%) sold back to 9,800 (98%), 49% of net liquidation.
func sellFrom147() risklimit.Order {
	o := order(event.OrderSideSell, 4900, 14700)
	o.Planned.TargetWeight = 0.98
	return o
}

// TestARiskReducingSellIsExemptFromMarkBandAndSize: risk-reducing orders
// "are exempt from the order count, the order size and the price band"
// (ADR 0026, point 3, lines 188-191), and a risk-reducing order is "a sell
// that moves the account's exposure toward 98% or lower, sized within the
// broker-reported holding" (point 4, lines 195-200). The mark is the band's
// only input, so a missing mark cannot refuse it either: one late mark does
// not refuse Boost's exit back to 98%, nor degrade the account and freeze it.
func TestARiskReducingSellIsExemptFromMarkBandAndSize(t *testing.T) {
	l := limits()
	l.MaxOrderFraction = 0.4 // the exit is 49% of net liquidation
	for name, mutate := range map[string]func(*risklimit.Order){
		"no mark":         func(o *risklimit.Order) { o.Mark, o.MarkSet = 0, false },
		"outside band":    func(o *risklimit.Order) { o.Mark = 1.2 },
		"over size":       func(*risklimit.Order) {},
		"to below 98%":    func(o *risklimit.Order) { o.Planned.Quantity = 14700; o.Planned.TargetWeight = 0 },
		"no mark, sell 1": func(o *risklimit.Order) { o.Planned.Quantity, o.Mark, o.MarkSet = 1, 0, false },
	} {
		g := gate(t, l, event.TradingModeDryRun)
		o := sellFrom147()
		mutate(&o)
		got := check(t, g, o)[0]
		if got.Released == nil {
			t.Errorf("%s: refused for %s: %s", name, got.Refused.Reason, got.Refused.Detail)
			continue
		}
		if got.State != nil || g.State() != risklimit.StateNormal {
			t.Errorf("%s: a released sell changes no state", name)
		}
	}
}

// TestOnlyARiskReducingOrderIsExempt: no buy is risk-reducing (ADR 0026,
// point 4, lines 199-200), and a sell beyond the holding is not "sized within
// the broker-reported holding" (line 197); both stay subject to the mark, the
// band and the size.
func TestOnlyARiskReducingOrderIsExempt(t *testing.T) {
	l := limits()
	l.MaxOrderFraction = 0.4
	buy := order(event.OrderSideBuy, 10, 9800)
	buy.Mark, buy.MarkSet = 0, false
	refusedFor(t, check(t, gate(t, l, event.TradingModeDryRun), buy)[0], event.RefusedMissingMark)
	beyond := order(event.OrderSideSell, 101, 100)
	beyond.Mark, beyond.MarkSet = 0, false
	refusedFor(t, check(t, gate(t, l, event.TradingModeDryRun), beyond)[0], event.RefusedMissingMark)
	beyond.Mark, beyond.MarkSet = 1.2, true
	refusedFor(t, check(t, gate(t, l, event.TradingModeDryRun), beyond)[0], event.RefusedPriceBand)
	big := order(event.OrderSideSell, 4001, 4000)
	refusedFor(t, check(t, gate(t, l, event.TradingModeDryRun), big)[0], event.RefusedOrderSize)
}

// TestTheNamedInstrumentStaysFrozen: while Degraded, "named instruments are
// frozen"; the only order on one that may go is ADR 0019's one sale back to
// 98% (ADR 0026, point 4, lines 205-212; ADR 0019, amended 2026-10-16),
// which is sized from IBKR's own holding, net liquidation and open orders,
// and "no order is sent" when those cannot be read (ADR 0019, amended
// 2026-10-15). This gate reads no open orders, so it sends no such sale:
// every order on a named instrument is refused, risk-reducing or not. While
// Halted, likewise, no order goes (lines 213-217).
func TestTheNamedInstrumentStaysFrozen(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	check(t, g, order(event.OrderSideBuy, 5001, 0))
	exit := sellFrom147()
	refusedFor(t, check(t, g, exit)[0], event.RefusedEngineDegraded)

	h := gate(t, limits(), event.TradingModeDryRun)
	untagged := order(event.OrderSideBuy, 1, 0)
	untagged.Planned.Tag = "targets-decided:joint:never"
	check(t, h, untagged)
	refusedFor(t, check(t, h, exit)[0], event.RefusedEngineHalted)
}

// TestRestoreState: a restart while Degraded or Halted "resumes in that same
// state and scope, read back from the run's durable record" (ADR 0019,
// amended 2026-09-24, "The state survives a restart"; ADR 0026, point 5,
// line 242). Leaving either is never automatic, so a restored state never
// loosens: Halted stays Halted, and a scope only widens.
func TestRestoreState(t *testing.T) {
	degraded := func(ids ...string) event.EngineStatePayload {
		return event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "recorded", ScopeInstrumentIDs: ids}
	}
	halted := event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "recorded"}

	g := gate(t, limits(), event.TradingModeDryRun)
	if err := g.RestoreState(degraded("SPY"), event.EngineStateScopedVersion); err != nil {
		t.Fatal(err)
	}
	if g.State() != risklimit.StateDegraded {
		t.Fatalf("restored state %s", g.State())
	}
	refusedFor(t, check(t, g, sellFrom147())[0], event.RefusedEngineDegraded)
	refusedFor(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0], event.RefusedEngineDegraded)
	other := order(event.OrderSideSell, 1, 10)
	other.Planned.InstrumentID = "IEF"
	released(t, check(t, g, other)[0])
	// A later record naming only IEF keeps SPY frozen.
	if err := g.RestoreState(degraded("IEF"), event.EngineStateScopedVersion); err != nil {
		t.Fatal(err)
	}
	refusedFor(t, check(t, g, sellFrom147())[0], event.RefusedEngineDegraded)
	refusedFor(t, check(t, g, other)[0], event.RefusedEngineDegraded)

	if err := g.RestoreState(halted, event.EngineStateScopedVersion); err != nil {
		t.Fatal(err)
	}
	refusedFor(t, check(t, g, other)[0], event.RefusedEngineHalted)
	if err := g.RestoreState(degraded("SPY"), event.EngineStateScopedVersion); err == nil || g.State() != risklimit.StateHalted {
		t.Errorf("a Degraded record after a Halted one must be refused: %v, %s", err, g.State())
	}

	for name, bad := range map[string]struct {
		p       event.EngineStatePayload
		version uint32
	}{
		"invalid":          {event.EngineStatePayload{State: "dazed"}, event.EngineStateScopedVersion},
		"degraded at v1":   {degraded("SPY"), event.EngineStateSchemaVersion},
		"risk limit at v1": {halted, event.EngineStateSchemaVersion},
		"order scope":      {event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x", ScopeOrderIDs: []string{"tw-1"}}, event.EngineStateScopedVersion},
		"cash scope":       {event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x", ScopeCashComponents: []string{"USD"}}, event.EngineStateScopedVersion},
	} {
		n := gate(t, limits(), event.TradingModeDryRun)
		if err := n.RestoreState(bad.p, bad.version); err == nil {
			t.Errorf("%s: restored", name)
		}
		if n.State() != risklimit.StateNormal {
			t.Errorf("%s: a refused record changes no state", name)
		}
	}
}
