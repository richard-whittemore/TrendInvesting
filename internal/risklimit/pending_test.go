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
// what is known filled: fill reports, a broker snapshot that lists it at a
// remaining quantity, or a fall in the holding, seen by an account or broker
// snapshot, that known fills do not explain. Only a canceled or invalid
// lifecycle report ends the remainder: a snapshot that no longer lists a sell
// never shows it cancelled.

// sellFill records an execution of the released sell client, executed
// minutes after the fixture time.
func sellFill(t *testing.T, g *risklimit.Gate, client, fillID string, quantity int64, minutes int) {
	t.Helper()
	sellFillAt(t, g, client, fillID, quantity, at.Add(time.Duration(minutes)*time.Minute))
}

// sellFillAt records an execution of the released sell client at filledAt.
func sellFillAt(t *testing.T, g *risklimit.Gate, client, fillID string, quantity int64, filledAt time.Time) {
	t.Helper()
	if err := g.ObserveFill(event.TargetWeightFillPayload{
		AccountID: "joint", ClientOrderID: client, FillID: fillID, InstrumentID: "SPY",
		Side: event.OrderSideSell, Quantity: quantity, Price: 1, FilledAt: filledAt,
	}); err != nil {
		t.Fatal(err)
	}
}

// sellStatus records a lifecycle report on the released sell client of
// quantity at limit, which occurred minutes after the fixture time.
func sellStatus(t *testing.T, g *risklimit.Gate, client, status string, quantity int64, limit float64, minutes int) {
	t.Helper()
	if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
		AccountID: "joint", ClientOrderID: client, OrderID: "ib-" + client, InstrumentID: "SPY",
		Side: event.OrderSideSell, Status: status, Quantity: quantity, LimitPrice: limit, OccurredAt: at.Add(time.Duration(minutes) * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
}

// account is an account snapshot holding held SPY, taken minutes after the
// fixture time.
func account(held int64, minutes int) event.AccountPositionsSnapshotPayload {
	p := event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: at.Add(time.Duration(minutes) * time.Minute), NetLiquidation: 10000, Currency: "USD"}
	if held != 0 {
		p.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: held}}
	}
	return p
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
	g.ObserveAccount(account(147, 1))
	pendingIs(t, g, 54, "the account snapshot shows 147 while the sale works")
	exit := order(event.OrderSideSell, 94, 147)
	refusedFor(t, check(t, g, exit)[0], event.RefusedBrokerHolding)
}

// A broker snapshot that does not list a sell released before it, with the
// holding unchanged, shows nothing: the sell may not have reached IBKR yet.
// It stays pending, so the same session's exit cannot also sell its shares:
// the urgent sale of 54 and an exit of 54 would otherwise sell 108 of 147,
// far below 98%. A later listing, a fill or a holding fall, or a
// cancellation report, resolves it.
func TestADroppedSellWithTheHoldingUnchangedStaysPending(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount(account(147, -1))
	sale := urgentSale(t, g)
	pendingIs(t, g, 54, "released after the account snapshot")
	observe(t, g, "broker:2", later(boosted(), 1))
	pendingIs(t, g, 54, "not yet listed, the holding unchanged: still pending")
	exit := order(event.OrderSideSell, 94, 147)
	exit.Planned.ClientOrderID = "tw-exit"
	o := check(t, g, exit)[0]
	refusedFor(t, o, event.RefusedBrokerHolding)
	if o.Refused.Figures.WorkingSells != 54 {
		t.Errorf("the exit is checked net of the unlisted sale: %+v", o.Refused.Figures)
	}
	observe(t, g, "broker:3", later(boosted(listed(sale.ClientOrderID, event.BrokerOrderWorking, 54)), 2))
	pendingIs(t, g, 54, "listed working at last")
	observe(t, g, "broker:4", later(boosted(), 3))
	pendingIs(t, g, 54, "dropped again, the holding unchanged")
	sellStatus(t, g, sale.ClientOrderID, event.OrderLifecycleStatusCanceled, 54, sale.LimitPrice, 4)
	pendingIs(t, g, 0, "only the cancellation report ends it")
}

// A completed sell that the account snapshot shows before any broker
// snapshot does is not counted again when the broker snapshot then infers
// the fill: the urgent sale of 54 of 147 fills, the account snapshot shows
// 93, and neither the next exit's sizing nor the broker-holding check counts
// the 54 a second time.
func TestACompletedSellTheAccountSnapshotShowsFirstIsNotCountedTwice(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount(account(147, -1))
	sale := urgentSale(t, g)
	g.ObserveAccount(account(93, 60))
	pendingIs(t, g, 0, "the account snapshot's 93 already shows the 54 sold")
	b := later(boosted(), 65)
	b.Positions[0].Quantity = 93
	observe(t, g, "broker:2", b)
	pendingIs(t, g, 0, "the broker snapshot infers the same 54, already shown")
	sellFill(t, g, sale.ClientOrderID, "late", 54, 30)
	pendingIs(t, g, 0, "a late fill report of the same 54 is not counted again")
	o := check(t, g, order(event.OrderSideSell, 1, 93))[0]
	released(t, o)
	if o.Released.Figures.WorkingSells != 0 || o.Released.Figures.BrokerHolding != 93 {
		t.Errorf("nothing left working against the broker's 93: %+v", o.Released.Figures)
	}
}

// With no broker snapshot between them, two account snapshots show a fill
// by the fall in the holding alone.
func TestAnAccountSnapshotsHoldingFallShowsAFill(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	g.ObserveAccount(account(75, 10))
	pendingIs(t, g, 35, "25 shown sold; 35 still pending")
	sellFill(t, g, "tw-sell", "f1", 25, 5)
	pendingIs(t, g, 35, "the report of the same 25 changes nothing")
}

// A partial fill leaves the rest pending; the filled shares are still not
// shown by a snapshot taken before them; a cancellation ends the rest.
func TestAPartialFillLeavesTheRestPending(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 1))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellFill(t, g, "tw-sell", "f1", 20, 2)
	pendingIs(t, g, 60, "40 working and 20 filled since the account snapshot")
	g.ObserveAccount(account(80, 3))
	pendingIs(t, g, 40, "an account snapshot after the fill shows the 20")
	again := order(event.OrderSideSell, 41, 100)
	again.Planned.ClientOrderID = "tw-again"
	o := check(t, g, again)[0]
	refusedFor(t, o, event.RefusedBrokerHolding)
	if o.Refused.Figures.WorkingSells != 60 {
		t.Errorf("against the broker snapshot taken before the fill, 40 working and 20 filled: %d", o.Refused.Figures.WorkingSells)
	}
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 4)
	pendingIs(t, g, 0, "the cancellation ends the remainder")
}

// A cancel-pending sell may still fill, so it stays pending in full until
// the cancellation is confirmed.
func TestACancelPendingSellStaysPendingInFull(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCancelPending, 60, 1, 1)
	pendingIs(t, g, 60, "cancel-pending")
	observe(t, g, "broker:2", later(broker(100, listed("tw-sell", event.BrokerOrderCancelPending, 60)), 1))
	pendingIs(t, g, 60, "listed cancel-pending by the broker")
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 2)
	pendingIs(t, g, 0, "cancelled")
}

// A broker snapshot resolves what it shows of a sell: listed, it is pending
// at the broker's remaining quantity; dropped, the fall in the holding shows
// what filled and the rest stays pending; unreadable, it resolves nothing.
func TestABrokerSnapshotResolvesASell(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])

	blind := later(broker(100), 1)
	blind.Unreadable = []string{event.BrokerComponentOpenOrders}
	observe(t, g, "broker:2", blind)
	pendingIs(t, g, 60, "a snapshot that cannot read the open orders resolves nothing")

	observe(t, g, "broker:3", later(broker(75, listed("tw-sell", event.BrokerOrderWorking, 35)), 2))
	pendingIs(t, g, 60, "35 working and 25 filled since the account snapshot")
	g.ObserveAccount(account(75, 2))
	pendingIs(t, g, 35, "pending at the broker's remaining quantity")

	observe(t, g, "broker:4", later(broker(55), 3))
	pendingIs(t, g, 35, "20 more filled since the account snapshot; the other 15 still pending")
	g.ObserveAccount(account(55, 3))
	pendingIs(t, g, 15, "the dropped 15 are not taken as cancelled")
	sellFill(t, g, "tw-sell", "f1", 45, 3)
	pendingIs(t, g, 15, "a fill report of shares a snapshot already showed is not counted twice")
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 4)
	pendingIs(t, g, 0, "the cancellation report ends the 15")
}

// A broker snapshot with the sell dropped but no earlier holding to compare
// cannot tell a fill from a cancellation, so the sell stays pending until a
// report resolves it.
func TestADroppedSellWithNoEarlierHoldingStaysPending(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	observe(t, g, "broker:1", broker(100))
	pendingIs(t, g, 60, "no earlier broker holding")
	observe(t, g, "broker:2", later(broker(100), 1))
	pendingIs(t, g, 60, "still unresolved")
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusInvalid, 60, 1, 2)
	pendingIs(t, g, 0, "the venue refused it")
}

// A lifecycle report resolves the remainder; a fill reported after it moves
// shares from cancelled to filled, never past the release.
func TestALifecycleReportResolvesASell(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusSubmitted, 60, 1, 1)
	pendingIs(t, g, 60, "acknowledged")
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 2)
	pendingIs(t, g, 0, "cancelled unfilled")
	sellFill(t, g, "tw-sell", "late", 10, 1)
	pendingIs(t, g, 10, "a late fill since the account snapshot")
	g.ObserveAccount(account(90, 3))
	pendingIs(t, g, 0, "shown")
}

// An ended sell is counted filled, from its end's time, by what the facts
// before the end show: a fill report stamped after the end, recorded before
// it, is credited only from its own time, so a snapshot taken between the
// end and that fill still nets it (a snapshot is credited with no fill it
// cannot show).
func TestAFillStampedAfterTheEndIsNotCreditedFromTheEnd(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellFill(t, g, "tw-sell", "after-end", 10, 5)
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 2)
	g.ObserveAccount(account(100, 3))
	pendingIs(t, g, 10, "filled after the snapshot between the end and the fill")
	g.ObserveAccount(account(90, 6))
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
					sellFill(t, g, "tw-sell", "f"+string(rune('a'+fills)), q, step)
				}
			case 1:
				sellStatus(t, g, "tw-sell", statuses[rng.Intn(len(statuses))], release, 1, step)
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
			g.ObserveAccount(account(40+rng.Int63n(61), step))
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
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	pendingIs(t, g, 90, "ours and the unknown sell")
	observe(t, g, "broker:2", later(broker(80, unknown(10)), 1))
	pendingIs(t, g, 70, "the unknown sell sold the 20; ours, dropped, stays pending")
}

// An account snapshot's fall in the holding is taken first as the fills of
// any unknown sell the broker lists, which may be what sold, so ours stays
// pending rather than be counted as sold too early.
func TestAnAccountSnapshotsFallIsFirstAnUnknownSells(t *testing.T) {
	unknown := event.BrokerOrder{BrokerOrderID: "ib-hand", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: 30}
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100, unknown))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	g.ObserveAccount(account(60, 10))
	pendingIs(t, g, 80, "a fall of 40: 30 may be the unknown sell's, so only 10 are taken as ours; 50 pending and the unknown 30")
}

// A broker snapshot taken before a fill that a newer account snapshot already
// showed, but journalled after it, never moves what the gate knows backwards:
// its holding of 147 does not show the 54 the urgent sale sold, so the 54 stay
// working against it. A sell of 117 against those 147 would otherwise pass
// the no-short check with 93 actually held.
func TestAnOlderBrokerSnapshotNeverRegressesAFillANewerSnapshotShowed(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount(account(147, -1))
	urgentSale(t, g)
	g.ObserveAccount(account(93, 60))
	pendingIs(t, g, 0, "the account snapshot at 16:00 shows the 54 sold")
	observe(t, g, "broker:2", later(boosted(), 59))
	pendingIs(t, g, 0, "an older broker snapshot does not unsell what the account snapshot showed")
	sell := order(event.OrderSideSell, 117, 147)
	sell.Planned.ClientOrderID = "tw-117"
	o := check(t, g, sell)[0]
	refusedFor(t, o, event.RefusedBrokerHolding)
	if o.Refused.Figures.BrokerHolding != 147 || o.Refused.Figures.WorkingSells != 54 {
		t.Errorf("against the pre-fill snapshot's 147, the 54 sold stay working: %+v", o.Refused.Figures)
	}
}

// A holding fall taken as filled by the oldest pending sell and a later fill
// report of the sell that actually executed are the same shares: the inferred
// fills are a floor on what the instrument's sells filled, not an addition to
// each sell's reports. A of 60 and B of 40 are released against 100; B fills
// 30; the account shows 70; B's report of 30 follows. A's 60 and B's 10 are
// still pending: 70, never 40, however many snapshots of 70 follow.
func TestAnInferredFillAndAnotherSellsReportAreNotBothCounted(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount(account(100, 0))
	a := order(event.OrderSideSell, 60, 100)
	a.Planned.ClientOrderID = "tw-a"
	released(t, check(t, g, a)[0])
	b := order(event.OrderSideSell, 40, 100)
	b.Planned.ClientOrderID = "tw-b"
	released(t, check(t, g, b)[0])
	pendingIs(t, g, 100, "both released")
	g.ObserveAccount(account(70, 10))
	pendingIs(t, g, 70, "30 shown sold")
	sellFill(t, g, "tw-b", "b1", 30, 5)
	pendingIs(t, g, 70, "B's report is the 30 the account snapshot showed")
	g.ObserveAccount(account(70, 20))
	pendingIs(t, g, 70, "a second snapshot of 70 shows nothing new")
	observe(t, g, "broker:1", later(broker(70), 21))
	sell := order(event.OrderSideSell, 1, 70)
	sell.Planned.ClientOrderID = "tw-one"
	o := check(t, g, sell)[0]
	refusedFor(t, o, event.RefusedBrokerHolding)
	if o.Refused.Figures.WorkingSells != 70 {
		t.Errorf("A's 60 and B's 10 work against the broker's 70: %+v", o.Refused.Figures)
	}
}

// An account snapshot older than a broker snapshot already seen, recorded
// after it, shows nothing newer: its lower holding is not taken as a fill,
// and the shares the broker snapshot cannot show stay pending.
func TestAnOlderAccountSnapshotShowsNothingNewer(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	observe(t, g, "broker:1", later(broker(100, listed("tw-sell", event.BrokerOrderWorking, 60)), 10))
	g.ObserveAccount(account(70, 5))
	pendingIs(t, g, 60, "a snapshot from before the broker's shows no fill")
}

// A snapshot shows only the fills that had happened when it was taken. Fill
// reports are pushed and snapshots polled, so a report can be recorded before
// a snapshot taken before the fill: the urgent sale of 54 of 147 fills at
// 16:00:02, its report is recorded, then an account snapshot taken at 16:00
// still holding 147 is recorded. That snapshot does not show the 54, so they
// stay pending against it, and a broker snapshot taken before the fill keeps
// them working against its 147. Counting them shown would let the session's
// exit sell another 54 and leave 39 of 147, far below 98%.
func TestAFillReportedBeforeAnOlderSnapshotIsStillPendingFromIt(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	sale := urgentSale(t, g)
	sellFillAt(t, g, sale.ClientOrderID, "s1", 54, at.Add(time.Hour+2*time.Second))
	g.ObserveAccount(account(147, 60))
	pendingIs(t, g, 54, "the 16:00 snapshot's 147 does not show the 16:00:02 fill")
	sell := order(event.OrderSideSell, 94, 147)
	sell.Planned.ClientOrderID = "tw-exit"
	o := check(t, g, sell)[0]
	refusedFor(t, o, event.RefusedBrokerHolding)
	if o.Refused.Figures.WorkingSells != 54 {
		t.Errorf("the broker snapshot from before the fill still nets the 54: %+v", o.Refused.Figures)
	}
	g.ObserveAccount(account(93, 61))
	pendingIs(t, g, 0, "a snapshot after the fill shows the 54 sold")
}

// A broker snapshot taken before a fill whose report was recorded first lists
// the sell at its full remaining quantity; it is credited with no fill, so the
// account snapshot after the fill measures its fall against what that broker
// snapshot showed, and no phantom fall moves onto another pending sell.
func TestAFillReportedBeforeAnOlderBrokerSnapshotIsNotInferredTwice(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(200))
	g.ObserveAccount(account(200, 0))
	e := order(event.OrderSideSell, 60, 200)
	e.Planned.ClientOrderID = "tw-e"
	released(t, check(t, g, e)[0])
	s := order(event.OrderSideSell, 47, 200)
	s.Planned.ClientOrderID = "tw-s"
	released(t, check(t, g, s)[0])
	sellFill(t, g, "tw-s", "s1", 47, 3)
	observe(t, g, "broker:2", later(broker(200, listed("tw-e", event.BrokerOrderWorking, 60), listed("tw-s", event.BrokerOrderWorking, 47)), 2))
	pendingIs(t, g, 107, "the account snapshot at 0 shows neither")
	again := order(event.OrderSideSell, 94, 200)
	again.Planned.ClientOrderID = "tw-again"
	o := check(t, g, again)[0]
	refusedFor(t, o, event.RefusedBrokerHolding)
	if o.Refused.Figures.WorkingSells != 107 {
		t.Errorf("E's 60 working and S's 47 filled after the broker snapshot: %+v", o.Refused.Figures)
	}
	g.ObserveAccount(account(153, 4))
	pendingIs(t, g, 60, "the fall of 47 is S's reported fill; E's 60 stay pending")
}

// An account snapshot that is older than the latest broker snapshot, recorded
// after it, still shows the fills that had happened when it was taken: 100
// held, 60 released, 30 reported filled at 15:01; a broker snapshot at 15:03
// shows 70 held and 30 working; an account snapshot at 15:02 showing 70 is
// recorded after it. The planner sizes from that snapshot, which already
// shows the 30 sold, so only the 30 working are pending.
func TestAnOlderAccountSnapshotStillShowsTheFillsBeforeIt(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellFill(t, g, "tw-sell", "f1", 30, 1)
	observe(t, g, "broker:1", later(broker(70, listed("tw-sell", event.BrokerOrderWorking, 30)), 3))
	g.ObserveAccount(account(70, 2))
	pendingIs(t, g, 30, "the 15:02 snapshot shows the 30 filled at 15:01")
}

// workingSells is the working sells an outcome's broker-holding figures net.
func workingSells(o risklimit.Outcome) int64 {
	if o.Refused != nil {
		return o.Refused.Figures.WorkingSells
	}
	return o.Released.Figures.WorkingSells
}

// sameSecond walks ADR 0019's worked example with the reports at reported,
// and an account and a broker snapshot both stamped 16:00:02 that still hold
// the 147: it returns the pending sells the account snapshot leaves and the
// working sells the broker-holding check nets against the broker snapshot.
func sameSecond(t *testing.T, report func(g *risklimit.Gate, sale event.OrderPlannedPayload)) (pending, working int64) {
	t.Helper()
	g := gate(t, limits(), event.TradingModeDryRun)
	sale := urgentSale(t, g)
	report(g, sale)
	snapshot := at.Add(time.Hour + 2*time.Second)
	a := account(147, 0)
	a.AsOf = snapshot
	g.ObserveAccount(a)
	b := boosted()
	b.AsOf = snapshot
	observe(t, g, "broker:2", b)
	one := order(event.OrderSideSell, 1, 147)
	one.Planned.ClientOrderID = "tw-one"
	return g.PendingSells("SPY"), workingSells(check(t, g, one)[0])
}

// IBKR stamps executions in whole seconds and a snapshot carries no broker
// time of its own, so a fill stamped in the same second as a snapshot may
// have happened after the snapshot read its positions. It is assumed to have
// (fail closed): the urgent sale of 54 of 147 fills at 16:00:02 and both
// snapshots of 16:00:02 still hold 147, so the 54 stay pending and working
// against them, and the session's exit cannot sell them again. A fill a
// second earlier is shown by them.
func TestASameSecondFillIsStillPendingFromTheSnapshot(t *testing.T) {
	fill := at.Add(time.Hour + 2*time.Second)
	for _, c := range []struct {
		name     string
		filledAt time.Time
		want     int64
	}{
		{"the same second", fill, 54},
		{"a second earlier", fill.Add(-time.Second), 0},
	} {
		pending, working := sameSecond(t, func(g *risklimit.Gate, sale event.OrderPlannedPayload) {
			sellFillAt(t, g, sale.ClientOrderID, "s1", 54, c.filledAt)
		})
		if pending != c.want || working != c.want {
			t.Errorf("%s: pending sells %d and working sells %d, want %d", c.name, pending, working, c.want)
		}
	}
}

// A lifecycle report that ends a sell in the same second as a snapshot is
// likewise assumed to post-date it: a partial fill of 20 and the
// cancellation of the rest, both stamped 16:00:02, leave the 20 working
// against snapshots of 16:00:02 that still hold 147. Ended a second earlier,
// the sell is shown by them.
func TestASameSecondLifecycleEndStillCountsAsWorking(t *testing.T) {
	end := at.Add(time.Hour + 2*time.Second)
	for _, c := range []struct {
		name  string
		endAt time.Time
		want  int64
	}{
		{"the same second", end, 20},
		{"a second earlier", end.Add(-time.Second), 0},
	} {
		pending, working := sameSecond(t, func(g *risklimit.Gate, sale event.OrderPlannedPayload) {
			sellFillAt(t, g, sale.ClientOrderID, "s1", 20, c.endAt)
			if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
				AccountID: "joint", ClientOrderID: sale.ClientOrderID, OrderID: "ib-" + sale.ClientOrderID, InstrumentID: "SPY",
				Side: event.OrderSideSell, Status: event.OrderLifecycleStatusCanceled, Quantity: sale.Quantity, LimitPrice: sale.LimitPrice, OccurredAt: c.endAt,
			}); err != nil {
				t.Fatal(err)
			}
		})
		if pending != c.want || working != c.want {
			t.Errorf("%s: pending sells %d and working sells %d, want %d", c.name, pending, working, c.want)
		}
	}
}
