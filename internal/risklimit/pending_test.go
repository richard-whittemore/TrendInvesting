package risklimit_test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// The rule these tests pin (ADR 0019, amended 2026-10-15, sizing "net of
// every working or pending sell", cancel-pending included; ADR 0026, points
// 2.5 and 4): a released sell is pending from its release until recorded
// broker facts resolve it. Its pending quantity is what was released, less
// what fill reports or a broker snapshot show filled, less the remainder a
// lifecycle report or a broker snapshot shows cancelled. A snapshot alone
// never clears it.

// sellFill records an execution of the released sell client.
func sellFill(t *testing.T, g *risklimit.Gate, client, fillID string, quantity int64) {
	t.Helper()
	if err := g.ObserveFill(event.TargetWeightFillPayload{
		AccountID: "joint", ClientOrderID: client, FillID: fillID, InstrumentID: "SPY",
		Side: event.OrderSideSell, Quantity: quantity, Price: 1, FilledAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// sellStatus records a lifecycle report on the released sell client of
// quantity at limit.
func sellStatus(t *testing.T, g *risklimit.Gate, client, status string, quantity int64, limit float64) {
	t.Helper()
	if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
		AccountID: "joint", ClientOrderID: client, OrderID: "ib-" + client, InstrumentID: "SPY",
		Side: event.OrderSideSell, Status: status, Quantity: quantity, LimitPrice: limit, OccurredAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// later returns s taken minutes after the fixture time.
func later(s event.BrokerSnapshotPayload, minutes int) event.BrokerSnapshotPayload {
	s.AsOf = at.Add(time.Duration(minutes) * time.Minute)
	return s
}

func listed(client, status string, remaining int64) event.BrokerOrder {
	return event.BrokerOrder{BrokerOrderID: "ib-" + client, ClientOrderID: client, InstrumentID: "SPY", Side: event.OrderSideSell, Status: status, RemainingQuantity: remaining}
}

// urgentSale releases ADR 0019's worked example: 54 of 147 shares, from an
// urgent broker snapshot.
func urgentSale(t *testing.T, g *risklimit.Gate) event.OrderPlannedPayload {
	t.Helper()
	s := boosted()
	s.ExcessLiquidity = 1500
	if !observe(t, g, "broker:1", s).Urgent {
		t.Fatal("setup: not urgent")
	}
	sale, ok := requestSale(t, g, event.SaleTriggerMarginUrgent)
	if !ok || sale.Order == nil || sale.Order.Quantity != 54 {
		t.Fatalf("setup: urgent sale %+v", sale)
	}
	released(t, check(t, g, saleOrder(sale.Order))[0])
	return *sale.Order
}

func pendingIs(t *testing.T, g *risklimit.Gate, want int64, why string) {
	t.Helper()
	if got := g.PendingSells("SPY"); got != want {
		t.Errorf("%s: pending sells %d, want %d", why, got, want)
	}
}

// An account snapshot recorded after an urgent sale still shows the holding
// it was sized from while the sale is working, so the sale stays pending and
// the same session's exit cannot sell those shares again.
func TestAnAccountSnapshotNeverClearsAWorkingSell(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	urgentSale(t, g)
	g.ObserveAccount()
	pendingIs(t, g, 54, "the account snapshot shows 147 while the sale works")
	exit := order(event.OrderSideSell, 94, 147)
	refusedFor(t, check(t, g, exit)[0], event.RefusedBrokerHolding)
}

// A broker snapshot that no longer lists the urgent sale and shows the
// holding unchanged shows it cancelled unfilled, so it no longer shrinks or
// blocks the exit.
func TestABrokerSnapshotShowingASellCancelledUnfilledFreesTheExit(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount()
	urgentSale(t, g)
	pendingIs(t, g, 54, "released after the account snapshot")
	observe(t, g, "broker:2", later(boosted(), 1))
	pendingIs(t, g, 0, "dropped from the open orders with the holding unchanged: cancelled")
	exit := order(event.OrderSideSell, 54, 147)
	exit.Planned.ClientOrderID = "tw-exit"
	released(t, check(t, g, exit)[0])
}

// A partial fill leaves the rest pending; the filled shares are still not
// shown by a snapshot taken before them; a cancellation ends the rest.
func TestAPartialFillLeavesTheRestPending(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount()
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellFill(t, g, "tw-sell", "f1", 20)
	pendingIs(t, g, 60, "40 working and 20 filled since the account snapshot")
	g.ObserveAccount()
	pendingIs(t, g, 40, "an account snapshot after the fill shows the 20")
	again := order(event.OrderSideSell, 41, 100)
	again.Planned.ClientOrderID = "tw-again"
	o := check(t, g, again)[0]
	refusedFor(t, o, event.RefusedBrokerHolding)
	if o.Refused.Figures.WorkingSells != 60 {
		t.Errorf("against the broker snapshot taken before the fill, 40 working and 20 filled: %d", o.Refused.Figures.WorkingSells)
	}
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1)
	pendingIs(t, g, 0, "the cancellation ends the remainder")
}

// A cancel-pending sell may still fill, so it stays pending in full until
// the cancellation is confirmed.
func TestACancelPendingSellStaysPendingInFull(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount()
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCancelPending, 60, 1)
	pendingIs(t, g, 60, "cancel-pending")
	observe(t, g, "broker:2", later(broker(100, listed("tw-sell", event.BrokerOrderCancelPending, 60)), 1))
	pendingIs(t, g, 60, "listed cancel-pending by the broker")
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1)
	pendingIs(t, g, 0, "cancelled")
}

// A broker snapshot resolves a sell: listed, it is pending at the broker's
// remaining quantity; dropped, the fall in the holding shows what filled
// and the rest is cancelled; unreadable, it resolves nothing.
func TestABrokerSnapshotResolvesASell(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount()
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])

	blind := later(broker(100), 1)
	blind.Unreadable = []string{event.BrokerComponentOpenOrders}
	observe(t, g, "broker:2", blind)
	pendingIs(t, g, 60, "a snapshot that cannot read the open orders resolves nothing")

	observe(t, g, "broker:3", later(broker(75, listed("tw-sell", event.BrokerOrderWorking, 35)), 2))
	pendingIs(t, g, 60, "35 working and 25 filled since the account snapshot")
	g.ObserveAccount()
	pendingIs(t, g, 35, "pending at the broker's remaining quantity")

	observe(t, g, "broker:4", later(broker(55), 3))
	pendingIs(t, g, 20, "20 more filled since the account snapshot; the other 15 cancelled")
	g.ObserveAccount()
	pendingIs(t, g, 0, "resolved")
	sellFill(t, g, "tw-sell", "f1", 45)
	pendingIs(t, g, 0, "a fill report of shares a snapshot already showed is not counted twice")
}

// A broker snapshot with the sell dropped but no earlier holding to compare
// cannot tell a fill from a cancellation, so the sell stays pending until a
// report resolves it.
func TestADroppedSellWithNoEarlierHoldingStaysPending(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount()
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	observe(t, g, "broker:1", broker(100))
	pendingIs(t, g, 60, "no earlier broker holding")
	observe(t, g, "broker:2", later(broker(100), 1))
	pendingIs(t, g, 60, "still unresolved")
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusInvalid, 60, 1)
	pendingIs(t, g, 0, "the venue refused it")
}

// A lifecycle report resolves the remainder; a fill reported after it moves
// shares from cancelled to filled, never past the release.
func TestALifecycleReportResolvesASell(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount()
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusSubmitted, 60, 1)
	pendingIs(t, g, 60, "acknowledged")
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1)
	pendingIs(t, g, 0, "cancelled unfilled")
	sellFill(t, g, "tw-sell", "late", 10)
	pendingIs(t, g, 10, "a late fill since the account snapshot")
	g.ObserveAccount()
	pendingIs(t, g, 0, "shown")
}

func TestFillAndLifecycleReportsAreChecked(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	good := event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-sell", FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 10, Price: 1, FilledAt: at}
	if err := g.ObserveFill(good); err != nil {
		t.Fatal(err)
	}
	if err := g.ObserveFill(good); err != nil {
		t.Errorf("a redelivered execution is ignored: %v", err)
	}
	for name, mutate := range map[string]func(*event.TargetWeightFillPayload){
		"invalid":         func(p *event.TargetWeightFillPayload) { p.FillID = "" },
		"another account": func(p *event.TargetWeightFillPayload) { p.FillID, p.AccountID = "f2", "other" },
		"never released":  func(p *event.TargetWeightFillPayload) { p.FillID, p.ClientOrderID = "f2", "tw-unknown" },
		"other side":      func(p *event.TargetWeightFillPayload) { p.FillID, p.Side = "f2", event.OrderSideBuy },
		"past the order":  func(p *event.TargetWeightFillPayload) { p.FillID, p.Quantity = "f2", 51 },
		"reused fill id":  func(p *event.TargetWeightFillPayload) { p.Quantity = 11 },
	} {
		p := good
		mutate(&p)
		if err := g.ObserveFill(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	report := event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: "tw-sell", OrderID: "ib-1", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.OrderLifecycleStatusSubmitted, Quantity: 60, LimitPrice: 1, OccurredAt: at}
	for name, mutate := range map[string]func(*event.LimitOrderLifecyclePayload){
		"invalid":         func(p *event.LimitOrderLifecyclePayload) { p.OrderID = "" },
		"another account": func(p *event.LimitOrderLifecyclePayload) { p.AccountID = "other" },
		"never released":  func(p *event.LimitOrderLifecyclePayload) { p.ClientOrderID = "tw-unknown" },
		"other quantity":  func(p *event.LimitOrderLifecyclePayload) { p.Quantity = 61 },
		"other limit":     func(p *event.LimitOrderLifecyclePayload) { p.LimitPrice = 2 },
	} {
		p := report
		mutate(&p)
		if err := g.ObserveLifecycle(p); err == nil {
			t.Errorf("lifecycle %s: accepted", name)
		}
	}
	if err := g.ObserveLifecycle(report); err != nil {
		t.Error(err)
	}
	// A released buy's reports are accepted and pend no sell.
	buy := order(event.OrderSideBuy, 10, 100)
	b := gate(t, limits(), event.TradingModeDryRun)
	released(t, check(t, b, buy)[0])
	if err := b.ObserveFill(event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-buy", FillID: "b1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 10, Price: 1, FilledAt: at}); err != nil {
		t.Error(err)
	}
	if err := b.ObserveLifecycle(event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: "tw-buy", OrderID: "ib-2", InstrumentID: "SPY", Side: event.OrderSideBuy, Status: event.OrderLifecycleStatusCanceled, Quantity: 10, LimitPrice: 1, OccurredAt: at}); err != nil {
		t.Error(err)
	}
	if got := b.PendingSells("SPY"); got != 0 {
		t.Errorf("a buy pends no sell: %d", got)
	}
}

// Whatever order fills, lifecycle reports and broker snapshots arrive in, a
// sell's pending quantity never goes below zero and never exceeds what was
// released.
func TestPendingStaysWithinZeroAndTheRelease(t *testing.T) {
	statuses := []string{event.OrderLifecycleStatusSubmitted, event.OrderLifecycleStatusCancelPending, event.OrderLifecycleStatusCanceled, event.OrderLifecycleStatusInvalid}
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		g := gate(t, limits(), event.TradingModeDryRun)
		observe(t, g, "broker:0", broker(100))
		const release = 60
		released(t, check(t, g, order(event.OrderSideSell, release, 100))[0])
		var reported int64
		fills := 0
		for step := 1; step <= 12; step++ {
			switch rng.Intn(3) {
			case 0:
				if reported < release {
					q := 1 + rng.Int63n(release-reported)
					reported += q
					fills++
					sellFill(t, g, "tw-sell", "f"+string(rune('a'+fills)), q)
				}
			case 1:
				sellStatus(t, g, "tw-sell", statuses[rng.Intn(len(statuses))], release, 1)
			default:
				s := later(broker(40+rng.Int63n(61)), step)
				if rng.Intn(2) == 0 {
					s.OpenOrders = []event.BrokerOrder{listed("tw-sell", event.BrokerOrderWorking, 1+rng.Int63n(release))}
				}
				if rng.Intn(4) == 0 {
					s.Unreadable = []string{event.BrokerComponentOpenOrders}
					s.OpenOrders = nil
				}
				observe(t, g, "broker:"+string(rune('a'+step)), s)
			}
			g.ObserveAccount()
			if got := g.PendingSells("SPY"); got < 0 || got > release {
				t.Fatalf("seed %d step %d: pending %d outside [0, %d]", seed, step, got, release)
			}
		}
	}
}

// A fall in the holding that an unknown sell's own fills explain is not
// taken as a fill of ours.
func TestAnUnknownSellsFillsExplainTheirShareOfTheFall(t *testing.T) {
	unknown := func(remaining int64) event.BrokerOrder {
		return event.BrokerOrder{BrokerOrderID: "ib-hand", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: remaining}
	}
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100, unknown(30)))
	g.ObserveAccount()
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	pendingIs(t, g, 90, "ours and the unknown sell")
	observe(t, g, "broker:2", later(broker(80, unknown(10)), 1))
	pendingIs(t, g, 10, "the unknown sell sold the 20; ours was cancelled")
}
