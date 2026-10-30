package risklimit_test

import (
	"math"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

func fractionalGate(t *testing.T, fees float64) *risklimit.Gate {
	t.Helper()
	g := iraGate(t, fees)
	if err := g.SetQuantityScale(10000); err != nil {
		t.Fatal(err)
	}
	return g
}

func fractionalOrder(id, side string, ticks int64) risklimit.Order {
	o := iraOrder(id, side, ticks)
	o.Planned.QuantityScale = 10000
	return o
}

// ADR 0026 D7-F: limits value exact four-place quantities, never tick counts.
func TestFractionalLimitsUseSharesInsteadOfTicks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		quantity int64
		held     int64
		cash     float64
		want     string
	}{
		{"fits", 72500, 0, 8.25, ""},
		{"cash", 72500, 0, 8.2499, event.RefusedInsufficientCash},
		{"order fraction", 70000001, 0, 10000, event.RefusedOrderSize},
		{"exposure", 60000001, 40000000, 10000, event.RefusedIRAExposure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book := risklimit.Book{QuantityScale: 10000, Holdings: map[string]int64{"BBB": tc.held}, Prices: map[string]float64{"BBB": 1}, CashAvailable: tc.cash}
			got := checkBook(t, fractionalGate(t, 1), book, fractionalOrder("AAA", event.OrderSideBuy, tc.quantity))[0]
			if tc.want != "" {
				refusedFor(t, got, tc.want)
				return
			}
			released(t, got)
			if got.Released.QuantityScale != 10000 || got.Released.Quantity != 72500 || got.Released.Figures.BuyCost != 8.25 || got.Released.Figures.OrderFraction != 7.25/10000 {
				t.Fatalf("release: %+v", got.Released)
			}
		})
	}
}

func TestFractionalFillLeavesExactOutstandingQuantity(t *testing.T) {
	g := fractionalGate(t, 0)
	sale := fractionalOrder("AAA", event.OrderSideSell, 72500)
	sale.Planned.HeldQuantity = 123456
	book := risklimit.Book{QuantityScale: 10000, Holdings: map[string]int64{"AAA": 123456}, Prices: map[string]float64{"AAA": 1}}
	out := checkBook(t, g, book, sale)[0]
	released(t, out)
	fill := event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: sale.Planned.ClientOrderID, FillID: "fill", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 35000, QuantityScale: 10000, Price: 1, FilledAt: at.Add(time.Minute)}
	if err := g.ObserveFill(fill); err != nil {
		t.Fatal(err)
	}
	if got := g.PendingSells("AAA"); got != 37500 {
		t.Fatalf("outstanding ticks %d; want 37500 (3.75 shares)", got)
	}
	fill.QuantityScale = 0
	fill.FillID = "wrong-units"
	if err := g.ObserveFill(fill); err == nil {
		t.Fatal("whole-share fill accepted against fractional order")
	}
	lifecycle := event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: sale.Planned.ClientOrderID, OrderID: "venue", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 72500, QuantityScale: 10000, LimitPrice: 1, OccurredAt: at.Add(2 * time.Minute), Status: event.OrderLifecycleStatusCanceled}
	if err := g.ObserveLifecycle(lifecycle); err != nil {
		t.Fatal(err)
	}
	if got := g.PendingSells("AAA"); got != 0 {
		t.Fatalf("canceled remainder still pending: %d", got)
	}
}

func TestFractionalGateRefusesMixedUnits(t *testing.T) {
	g := fractionalGate(t, 0)
	if _, err := g.Check(risklimit.Book{At: at, CashAvailable: 100}, nil); err == nil {
		t.Fatal("whole-share book accepted")
	}
	if _, err := iraGate(t, 0).Check(risklimit.Book{At: at, QuantityScale: 10000}, nil); err == nil {
		t.Fatal("fractional book accepted on minimum-lot account")
	}
	if _, err := g.Check(risklimit.Book{At: at, QuantityScale: 10000, CashAvailable: 100}, []risklimit.Order{iraOrder("AAA", event.OrderSideBuy, 1)}); err == nil {
		t.Fatal("whole-share order accepted")
	}
	if err := g.ObserveAccount(event.AccountPositionsSnapshotPayload{}); err == nil {
		t.Fatal("whole-share snapshot accepted")
	}
	b := broker(1)
	if _, err := g.ObserveBroker("mixed", b); err == nil {
		t.Fatal("whole-share broker snapshot accepted")
	}
	if err := g.SetQuantityScale(100); err == nil {
		t.Fatal("unsupported precision accepted")
	}
	if err := gate(t, limits(), event.TradingModeDryRun).SetQuantityScale(10000); err == nil {
		t.Fatal("margin account enabled fractions")
	}
}

func TestFractionalBookUnitsCannotChangeAfterCapitalArrives(t *testing.T) {
	g := fractionalGate(t, 0)
	if err := g.SetBook(reconcile.Book{}); err == nil {
		t.Fatal("whole-share book installed")
	}
	good := reconcile.Book{AnchorID: "decimal-book", AsOf: at, QuantityScale: 10000, Positions: map[string]int64{"AAA": 123456}}
	if err := g.SetBook(good); err != nil {
		t.Fatal(err)
	}
	if err := g.SetQuantityScale(0); err == nil {
		t.Fatal("live book units changed")
	}
	bad := good
	bad.AnchorID = "whole-book"
	bad.QuantityScale = 0
	out, recorded, bookErr := g.CheckBook(bad)
	if bookErr == nil {
		t.Fatal("mixed book error was not propagated")
	}
	if !recorded || out.Result.Action != event.ReconciliationActionHalt || g.State() != risklimit.StateHalted {
		t.Fatalf("mixed book: %+v, recorded %v", out, recorded)
	}
	if err := out.Result.Validate(); err != nil {
		t.Fatal(err)
	}
	obs := morningOf(0, 123456)
	if _, err := g.Reconcile("wrong-scale", obs); err == nil {
		t.Fatal("whole-share reconciliation accepted")
	}
	obs.QuantityScale = 10000
	out, err := g.Reconcile("decimal-observation", obs)
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.AnchorID != "decimal-book" {
		t.Fatalf("mixed book replaced correct book: %+v", out.Result)
	}
}

func TestFractionalRecoveryRetainsUnitsAndRejectsMixedFills(t *testing.T) {
	g := fractionalGate(t, 0)
	o := event.OrderReleasedPayload{AccountID: "joint", ClientOrderID: "restored", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 72500, QuantityScale: 10000, LimitPrice: 1}
	f := event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "restored", FillID: "restored-fill", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 35000, QuantityScale: 10000, Price: 1, FilledAt: at}
	bad := f
	bad.QuantityScale = 0
	if err := g.RestoreReleased(o, []event.TargetWeightFillPayload{bad}, time.Time{}, "venue"); err == nil {
		t.Fatal("mixed-scale restored fill accepted")
	}
	whole := o
	whole.QuantityScale = 0
	if err := g.RestoreReleased(whole, nil, time.Time{}, "venue"); err == nil {
		t.Fatal("mixed-scale restored order accepted")
	}
	if err := g.RestoreReleased(o, []event.TargetWeightFillPayload{f}, time.Time{}, "venue"); err != nil {
		t.Fatal(err)
	}
	if got := g.PendingSells("AAA"); got != 37500 {
		t.Fatalf("restored outstanding %d, want 37500", got)
	}
	if err := g.ObserveFill(f); err != nil {
		t.Fatal(err)
	}
	lifecycle := event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: "restored", OrderID: "venue", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 72500, QuantityScale: 10000, LimitPrice: 1, OccurredAt: at.Add(time.Minute), Status: event.OrderLifecycleStatusCanceled}
	badEnd := lifecycle
	badEnd.QuantityScale = 0
	if err := g.ObserveLifecycle(badEnd); err == nil {
		t.Fatal("whole-share lifecycle accepted")
	}
	if err := g.ObserveLifecycle(lifecycle); err != nil {
		t.Fatal(err)
	}
}

// ADR 0026 D7-F: individually representable quantities must not wrap the
// projected holding or partially release a plan before an overflow is found.
func TestFractionalProjectedQuantityOverflowRefusesBeforeAnyRelease(t *testing.T) {
	for _, quantities := range [][]int64{{6000000000000000000}, {3000000000000000000, 3000000000000000000}} {
		g := fractionalGate(t, 0)
		book := risklimit.Book{QuantityScale: 10000, At: at, Holdings: map[string]int64{"AAA": 6000000000000000000}, Prices: map[string]float64{"AAA": 1}, CashAvailable: 1e15}
		var plan []risklimit.Order
		for i, q := range quantities {
			o := fractionalOrder("AAA", event.OrderSideBuy, q)
			o.Planned.ClientOrderID += string(rune('a' + i))
			o.PlanID = o.Planned.ClientOrderID
			o.Planned.HeldQuantity = book.Holdings["AAA"]
			o.Planned.NetLiquidation = 1e15
			plan = append(plan, o)
		}
		if out, err := g.Check(book, plan); err == nil {
			t.Fatalf("overflow accepted: %+v", out)
		}
		if g.State() != risklimit.StateNormal {
			t.Fatal("overflow mutated gate state")
		}
		fill := event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: plan[0].Planned.ClientOrderID, InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 1, QuantityScale: 10000, FillID: "should-not-release", Price: 1, FilledAt: at}
		if err := g.ObserveFill(fill); err == nil {
			t.Fatal("earlier order released before later overflow")
		}
	}
}

func TestFractionalOutstandingSellsSaturateInsteadOfWrapping(t *testing.T) {
	for _, known := range []bool{false, true} {
		g := fractionalGate(t, 0)
		if known {
			for _, id := range []string{"first", "second"} {
				o := event.OrderReleasedPayload{AccountID: "joint", ClientOrderID: id, InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 6000000000000000000, QuantityScale: 10000, LimitPrice: 1}
				if err := g.RestoreReleased(o, nil, time.Time{}, ""); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			b := broker(0, event.BrokerOrder{BrokerOrderID: "first", InstrumentID: "AAA", Side: event.OrderSideSell, RemainingQuantity: 6000000000000000000, Status: event.BrokerOrderWorking}, event.BrokerOrder{BrokerOrderID: "second", InstrumentID: "AAA", Side: event.OrderSideSell, RemainingQuantity: 6000000000000000000, Status: event.BrokerOrderWorking})
			b.QuantityScale = 10000
			b.Positions = []event.BrokerPosition{}
			if _, err := g.ObserveBroker("broker", b); err != nil {
				t.Fatal(err)
			}
		}
		if got := g.PendingSells("AAA"); got != math.MaxInt64 {
			t.Fatalf("known=%v pending=%d; want conservative saturation", known, got)
		}
	}
}

func TestFractionalListingAndLaterFillsCannotOverflowFilledBound(t *testing.T) {
	g := fractionalGate(t, 0)
	o := event.OrderReleasedPayload{AccountID: "joint", ClientOrderID: "huge", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: math.MaxInt64, QuantityScale: 10000, LimitPrice: 1}
	if err := g.RestoreReleased(o, nil, time.Time{}, "venue"); err != nil {
		t.Fatal(err)
	}
	b := broker(0, event.BrokerOrder{BrokerOrderID: "venue", ClientOrderID: "huge", InstrumentID: "AAA", Side: event.OrderSideSell, RemainingQuantity: 1, Status: event.BrokerOrderWorking})
	b.QuantityScale = 10000
	b.Positions = []event.BrokerPosition{}
	if _, err := g.ObserveBroker("broker", b); err != nil {
		t.Fatal(err)
	}
	f := event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "huge", FillID: "large-fill", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 6000000000000000000, QuantityScale: 10000, Price: 1, FilledAt: at.Add(time.Second)}
	if err := g.ObserveFill(f); err != nil {
		t.Fatal(err)
	}
	if got := g.PendingSells("AAA"); got != 0 {
		t.Fatalf("listing and fill bound wrapped: pending %d", got)
	}
}

func TestFractionalSellPlanUnderflowRefusesBeforeStateChanges(t *testing.T) {
	g := fractionalGate(t, 0)
	first := fractionalOrder("AAA", event.OrderSideSell, math.MaxInt64)
	second := first
	second.Planned.ClientOrderID = "second"
	second.PlanID = "second"
	if _, err := g.Check(risklimit.Book{QuantityScale: 10000, At: at}, []risklimit.Order{first, second}); err == nil {
		t.Fatal("sell total underflow accepted")
	}
	if g.State() != risklimit.StateNormal {
		t.Fatal("underflow was found after mutating gate state")
	}
}

func TestPendingSellsCannotTurnAShortIntoASalableHolding(t *testing.T) {
	g := iraGate(t, 0)
	book := risklimit.Book{Holdings: map[string]int64{"AAA": -2}, Prices: map[string]float64{"AAA": 1}, WorkingSells: map[string]int64{"AAA": math.MaxInt64}}
	out := checkBook(t, g, book, iraOrder("AAA", event.OrderSideSell, 1))[0]
	refusedFor(t, out, event.RefusedSellExceedsHolding)
}

func TestFractionalFillSumCannotWrapPastItsRelease(t *testing.T) {
	g := fractionalGate(t, 0)
	o := event.OrderReleasedPayload{AccountID: "joint", ClientOrderID: "max-fill", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: math.MaxInt64, QuantityScale: 10000, LimitPrice: 1}
	f := event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: o.ClientOrderID, FillID: "first", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: math.MaxInt64 - 1, QuantityScale: 10000, Price: 1, FilledAt: at}
	if err := g.RestoreReleased(o, []event.TargetWeightFillPayload{f}, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	f.FillID = "overflow"
	f.Quantity = 2
	if err := g.ObserveFill(f); err == nil {
		t.Fatal("fill wrapped past release")
	}
	if got := g.Filled(o.ClientOrderID); got != math.MaxInt64-1 {
		t.Fatalf("refused fill changed filled quantity: %d", got)
	}
	f.FillID = "last"
	f.Quantity = 1
	if err := g.ObserveFill(f); err != nil {
		t.Fatal(err)
	}
	if got := g.Filled(o.ClientOrderID); got != math.MaxInt64 {
		t.Fatalf("max quantity boundary changed: %d", got)
	}
}

// ADR 0026 D7-F: readers using CheckFill directly must enforce the same
// quantity units as the gate, even when every report identity matches.
func TestCheckFillRefusesMatchingIdentityWithDifferentQuantityUnits(t *testing.T) {
	order := &risklimit.FillOrder{AccountID: "joint", ClientOrderID: "fractional-order", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 72500, QuantityScale: 10000}
	fill := event.TargetWeightFillPayload{AccountID: order.AccountID, ClientOrderID: order.ClientOrderID, InstrumentID: order.InstrumentID, Side: order.Side, FillID: "whole-share-report", Quantity: 1, Price: 1, FilledAt: at}
	if err := fill.Validate(); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := risklimit.CheckFill(fill, "joint", order, nil); err == nil || duplicate {
		t.Fatalf("mixed units accepted: duplicate=%v, error=%v", duplicate, err)
	}
	if order.Reported != 0 {
		t.Fatal("refused fill changed reported quantity")
	}
}
