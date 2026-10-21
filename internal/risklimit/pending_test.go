package risklimit_test

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// The rule these tests pin (ADR 0019, amended 2026-10-15, sizing "net of
// every working or pending sell", cancel-pending included; ADR 0026, points
// 2.5 and 4): the shares being sold that a snapshot cannot show are its
// holding less the most that is sure to be held once every working sell has
// filled, and that is, at the best snapshot that read the holding, its
// holding less what can still sell after it: each released sell's release
// less what its fill reports and listings show it filled, nothing once it
// ended before that snapshot, and each hand sell's remaining at the latest
// listing. Only a canceled or invalid lifecycle report ends a sell: a
// snapshot that no longer lists a sell never shows it cancelled.

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
// cancellation report and a snapshot after it, resolves it.
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
	pendingIs(t, g, 54, "cancelled, it may have sold before its end with no report yet")
	g.ObserveAccount(account(147, 5))
	pendingIs(t, g, 0, "a snapshot after the cancellation shows it sold none")
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
// shown by a snapshot taken before them; a cancellation ends the rest once a
// snapshot after it shows what it finally sold.
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
	pendingIs(t, g, 40, "the remainder may have sold before the cancellation")
	g.ObserveAccount(account(80, 5))
	pendingIs(t, g, 0, "a snapshot after the cancellation shows the remainder unsold")
}

// A cancel-pending sell may still fill, so it stays pending in full until
// the cancellation is confirmed and a snapshot after it shows what sold.
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
	pendingIs(t, g, 60, "cancelled, with no snapshot after it")
	g.ObserveAccount(account(100, 3))
	pendingIs(t, g, 0, "cancelled unfilled")
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
	sellFillAt(t, g, "tw-sell", "f1", 25, at.Add(90*time.Second))
	sellFillAt(t, g, "tw-sell", "f2", 20, at.Add(150*time.Second))
	pendingIs(t, g, 15, "fill reports of shares the snapshots already showed are not counted twice")
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 4)
	pendingIs(t, g, 15, "the 15 may have sold before the cancellation")
	g.ObserveAccount(account(55, 5))
	pendingIs(t, g, 0, "a snapshot after the cancellation shows the 15 unsold")
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

// A lifecycle report ends the remainder, but until a snapshot taken after it
// reads the holding, every share the sell may have sold before its end, its
// fill reported or not, still counts as being sold.
func TestALifecycleReportResolvesASell(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusSubmitted, 60, 1, 1)
	pendingIs(t, g, 60, "acknowledged")
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 2)
	pendingIs(t, g, 60, "cancelled; any of the 60 may have sold before")
	sellFill(t, g, "tw-sell", "late", 10, 1)
	pendingIs(t, g, 60, "a late fill shows 10 sold, and no fewer may have")
	g.ObserveAccount(account(90, 3))
	pendingIs(t, g, 0, "a snapshot after the end shows the 10 sold and the rest unsold")
}

// A fill report stamped after a sell's end shows the end no earlier than the
// fill: a snapshot taken between them cannot show what the sell finally
// sold, so every share it may have sold after that snapshot still counts.
func TestAFillStampedAfterTheEndIsNotCreditedFromTheEnd(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellFill(t, g, "tw-sell", "after-end", 10, 5)
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 2)
	g.ObserveAccount(account(100, 3))
	pendingIs(t, g, 60, "the sell may have sold any of the 60 after the snapshot between the end and the fill")
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
	// The venue's echo of the limit need not be bit-equal to the engine's:
	// within event.LimitPriceTolerance it is the same price.
	echo := report
	echo.LimitPrice += 0.000000001
	if err := g.ObserveLifecycle(echo); err != nil {
		t.Errorf("a limit within the tolerance: %v", err)
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
// taken as a fill of ours, and is still being sold against an older
// snapshot.
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
	pendingIs(t, g, 90, "against the account's 100: the unknown sell's 20 sold since, its 10 left and our 60")
	if got := oneMore(t, g); got != 70 {
		t.Errorf("working sells %d against the broker's 80, want the unknown 10 and our 60", got)
	}
}

// A fall in the holding is put down to no sell: whether ours or a hand sell
// the broker lists sold the account snapshot's fall of 40, what will be left
// once both have filled is the same, 100 less the hand sell's 30 and our 60,
// so 50 of the account's 60 are still being sold.
func TestAFallIsPutDownToNoSellWhenAHandSellIsListed(t *testing.T) {
	unknown := event.BrokerOrder{BrokerOrderID: "ib-hand", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: 30}
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100, unknown))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	g.ObserveAccount(account(60, 10))
	pendingIs(t, g, 50, "a fall of 40 of the 90 being sold")
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
// after it, is netted toward what the newer holding leaves: a sell of 60 is
// released against 100; a broker snapshot of 15:10 still holds 100 and lists
// it at 60; an account snapshot of 15:05 holding 70 is recorded after it.
// Whatever moved the holding between them, what will be left is the
// broker's 100 less the sell's 60, so 30 of the account's 70 are being sold.
func TestAnOlderAccountSnapshotIsNettedTowardWhatANewerHoldingLeaves(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	observe(t, g, "broker:1", later(broker(100, listed("tw-sell", event.BrokerOrderWorking, 60)), 10))
	g.ObserveAccount(account(70, 5))
	pendingIs(t, g, 30, "the broker's 100 less the 60 working leave 40")
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
// cancellation of the rest, both stamped 16:00:02, leave all 54 working
// against snapshots of 16:00:02 that still hold 147, since more may have
// sold before the cancellation with no report yet. Ended a second earlier,
// the sell is shown by them.
func TestASameSecondLifecycleEndStillCountsAsWorking(t *testing.T) {
	end := at.Add(time.Hour + 2*time.Second)
	for _, c := range []struct {
		name  string
		endAt time.Time
		want  int64
	}{
		{"the same second", end, 54},
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

// A cancellation stamped in a snapshot's own second may follow its read: a
// sell of 60 against a holding of 100 is cancelled at 15:00:02 with no fill
// reported, and a broker snapshot of 15:00:02 holds 70 and no longer lists
// it. Any of its other 30 may have sold after that read, so they work
// against the broker's 70, and all 60 against the account snapshot of
// 15:00:00 that held 100. A broker snapshot of 15:00:03 that still holds 70
// shows what it finally sold: nothing works against it, and the 30 sold
// still count against the account snapshot.
func TestACancellationInASnapshotsSecondMayFollowItsRead(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	end := at.Add(2 * time.Second)
	if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
		AccountID: "joint", ClientOrderID: "tw-sell", OrderID: "ib-tw-sell", InstrumentID: "SPY",
		Side: event.OrderSideSell, Status: event.OrderLifecycleStatusCanceled, Quantity: 60, LimitPrice: 1, OccurredAt: end,
	}); err != nil {
		t.Fatal(err)
	}
	s := broker(70)
	s.AsOf = end
	observe(t, g, "broker:2", s)
	big := order(event.OrderSideSell, 100, 100)
	big.Planned.ClientOrderID = "tw-big"
	o := check(t, g, big)[0]
	refusedFor(t, o, event.RefusedBrokerHolding)
	if got := workingSells(o); got != 30 {
		t.Errorf("working sells %d, want 30: the cancellation may follow the broker's read", got)
	}
	pendingIs(t, g, 60, "against the account snapshot of 15:00:00")
	next := broker(70)
	next.AsOf = end.Add(time.Second)
	observe(t, g, "broker:3", next)
	if got := workingSells(check(t, g, big)[0]); got != 0 {
		t.Errorf("working sells %d against the snapshot after the cancellation, want 0", got)
	}
	pendingIs(t, g, 30, "the 30 sold since the account snapshot")
}

// A broker snapshot that cannot read the open orders is not one later falls
// are measured from: a sell of 60 against a holding of 100 is cancelled with
// no fill reported, a snapshot that cannot read the open orders holds 70,
// and the next, which can, still holds 70 and no longer lists it. The fall
// of 30 is measured by the second against the account snapshot's 100, so
// the 30 still count as being sold against that account snapshot.
func TestAFallIsMeasuredPastASnapshotThatCannotReadTheOpenOrders(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 0))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 1)
	blind := later(broker(70), 2)
	blind.Unreadable = []string{event.BrokerComponentOpenOrders}
	observe(t, g, "broker:2", blind)
	observe(t, g, "broker:3", later(broker(70), 3))
	if got := g.PendingSells("SPY"); got != 30 {
		t.Errorf("pending sells %d, want 30: the fall since the account snapshot", got)
	}
}

// An account and a broker snapshot stamped in the same second are taken as
// the same instant: neither can show the order of what happened within that
// second, so a fill stamped in it, or a fall the other snapshot saw, is
// credited to neither, while a broker listing stays its own snapshot's fact
// (fail closed). The urgent sale of 54 of 147 fills in 16:00:02 between an
// account snapshot that still holds 147 and a broker snapshot of the same
// second that shows the fill, either as a fall to 93 or as a listing at 24
// left with the holding at 117. Recorded in either order, the account
// snapshot still counts all 54 as being sold, and the broker snapshot only
// what it lists working.
func TestSnapshotsInOneSecondShowNoFillOfThatSecondToEachOther(t *testing.T) {
	for _, c := range []struct {
		name         string
		filled, held int64
		listing      []event.BrokerOrder
		wantWorking  int64
		brokerFirst  bool
	}{
		{name: "a fall, account first", filled: 54, held: 93, wantWorking: 0},
		{name: "a fall, broker first", filled: 54, held: 93, wantWorking: 0, brokerFirst: true},
		{name: "a listing, account first", filled: 30, held: 117, listing: []event.BrokerOrder{listed("tw-urgent", event.BrokerOrderWorking, 24)}, wantWorking: 24},
		{name: "a listing, broker first", filled: 30, held: 117, listing: []event.BrokerOrder{listed("tw-urgent", event.BrokerOrderWorking, 24)}, wantWorking: 24, brokerFirst: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := gate(t, limits(), event.TradingModeDryRun)
			sale := urgentSale(t, g)
			for i := range c.listing {
				c.listing[i].ClientOrderID, c.listing[i].BrokerOrderID = sale.ClientOrderID, "ib-"+sale.ClientOrderID
			}
			second := at.Add(time.Hour + 2*time.Second)
			sellFillAt(t, g, sale.ClientOrderID, "s1", c.filled, second)
			a := account(147, 0)
			a.AsOf = second
			b := boosted(c.listing...)
			b.AsOf = second
			b.Positions[0].Quantity = c.held
			if c.brokerFirst {
				observe(t, g, "broker:2", b)
				g.ObserveAccount(a)
			} else {
				g.ObserveAccount(a)
				observe(t, g, "broker:2", b)
			}
			pendingIs(t, g, 54, "the account snapshot may have read 147 before the fill")
			one := order(event.OrderSideSell, 1, 147)
			one.Planned.ClientOrderID = "tw-one"
			if got := workingSells(check(t, g, one)[0]); got != c.wantWorking {
				t.Errorf("working sells %d against the broker snapshot, want %d", got, c.wantWorking)
			}
		})
	}
}

// hand is a sell IBKR lists that the gate never released, remaining left.
func hand(remaining int64) event.BrokerOrder {
	return event.BrokerOrder{BrokerOrderID: "ib-hand", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: remaining}
}

// oneMore is the working sells the broker-holding check nets for a sell of
// one more share now.
func oneMore(t *testing.T, g *risklimit.Gate) int64 {
	t.Helper()
	one := order(event.OrderSideSell, 1, 100)
	one.Planned.ClientOrderID = "tw-one"
	return workingSells(check(t, g, one)[0])
}

// A holding an account snapshot read is its own fact, never lowered by a
// broker snapshot of the same second: sells A of 60 and B of 40 are released
// against 100; A fills 30 in 15:00:02, with no report yet, and is cancelled
// in that second; an account snapshot of 15:00:02 holds 70, and a broker
// snapshot of 15:00:02, read before the fill, holds 100 and lists A at 60 and
// B at 40. A's cancellation may have come after both reads, so every one of
// its 60 may have sold since the broker's read, and with B's 40 all 100 work
// against the broker's 100 (fail closed); netting 40 would let the sale back
// to 98% and the broker-holding check sell 60 of the 70 truly held while B's
// 40 work. A broker snapshot of 15:00:03 that holds 70 shows A ended: only
// B's 40 work against it.
func TestASameSecondAccountSnapshotsHoldingIsNeverLoweredByABrokerListing(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	a := order(event.OrderSideSell, 60, 100)
	a.Planned.ClientOrderID = "tw-a"
	released(t, check(t, g, a)[0])
	b := order(event.OrderSideSell, 40, 100)
	b.Planned.ClientOrderID = "tw-b"
	released(t, check(t, g, b)[0])
	second := at.Add(2 * time.Second)
	if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
		AccountID: "joint", ClientOrderID: "tw-a", OrderID: "ib-tw-a", InstrumentID: "SPY",
		Side: event.OrderSideSell, Status: event.OrderLifecycleStatusCanceled, Quantity: 60, LimitPrice: 1, OccurredAt: second,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := account(70, 0)
	snapshot.AsOf = second
	g.ObserveAccount(snapshot)
	s := broker(100, listed("tw-a", event.BrokerOrderWorking, 60), listed("tw-b", event.BrokerOrderWorking, 40))
	s.AsOf = second
	observe(t, g, "broker:1", s)
	if got := oneMore(t, g); got != 100 {
		t.Errorf("working sells %d against the broker's 100, want 100: A's 60 may all have sold after its read", got)
	}
	next := broker(70)
	next.AsOf = second.Add(time.Second)
	observe(t, g, "broker:2", next)
	if got := oneMore(t, g); got != 40 {
		t.Errorf("working sells %d against the broker's 70 of 15:00:03, want B's 40", got)
	}
}

// A hand sell IBKR listed stays counted while a broker snapshot that cannot
// read the open orders is the latest: the last snapshot that read them is
// the one the unknown sells are taken from, never the blind one.
func TestAHandSellListedBeforeABlindSnapshotIsStillCounted(t *testing.T) {
	blind := func(held int64, minutes int) event.BrokerSnapshotPayload {
		s := later(broker(held), minutes)
		s.Unreadable, s.OpenOrders = []string{event.BrokerComponentOpenOrders}, nil
		return s
	}
	t.Run("the account snapshot after it", func(t *testing.T) {
		// A hand sell of 20 is listed against 100; a sell of 60 is released,
		// and the hand sell fills; a blind snapshot and an account snapshot
		// hold 80. All 60 of ours still work: the 20 sold are the hand
		// sell's, so the planner may sell 20 of the 80, never 40.
		g := gate(t, limits(), event.TradingModeDryRun)
		observe(t, g, "broker:1", broker(100, hand(20)))
		released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
		observe(t, g, "broker:2", blind(80, 30))
		g.ObserveAccount(account(80, 40))
		pendingIs(t, g, 60, "ours, all working")
		observe(t, g, "broker:3", later(broker(80), 50))
		if got := oneMore(t, g); got != 60 {
			t.Errorf("working sells %d against the broker's 80, want ours, 60", got)
		}
	})
	t.Run("the next readable snapshot", func(t *testing.T) {
		// A hand sell of 30 is listed against 100; a sell of 40 is released;
		// the hand sell fills 20; a blind snapshot holds 80, and the next
		// readable one still 80, with the hand sell at 10 and ours not yet
		// listed. Ours and the hand sell's 10 work: 50, never 30.
		g := gate(t, limits(), event.TradingModeDryRun)
		observe(t, g, "broker:1", broker(100, hand(30)))
		released(t, check(t, g, order(event.OrderSideSell, 40, 100))[0])
		observe(t, g, "broker:2", blind(80, 1))
		observe(t, g, "broker:3", later(broker(80, hand(10)), 2))
		if got := oneMore(t, g); got != 50 {
			t.Errorf("working sells %d against the broker's 80, want 50", got)
		}
	})
}

// A listed hand sell's remaining never frees a cancelled sell's unreported
// fill: a hand sell of 20 is listed against 100; a sell of 60 is released,
// fills 30 with no report yet, and is cancelled; an account snapshot after
// the cancellation holds 70. Against the broker's 100 the 30 sold and the
// hand sell's 20 work, 50; against the account's 70 the hand sell's 20.
// Netting 30 would let the sale back to 98% sell 70 of the 70 held while
// the hand sell's 20 work.
func TestAHandSellsRemainingNeverFreesACancelledSellsFill(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100, hand(20)))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusCanceled, 60, 1, 20)
	g.ObserveAccount(account(70, 30))
	pendingIs(t, g, 20, "the hand sell's 20 against the account's 70")
	if got := oneMore(t, g); got != 50 {
		t.Errorf("working sells %d against the broker's 100, want 50", got)
	}
}

// A hand sell whose size the owner raises in TWS keeps its order ID, so
// IBKR lists the same order with a larger remaining. No snapshot credited
// with a listing from before the rise bounds what the hand sell sold since:
// each is skipped, and the hand sell is counted from the raised figure.
func TestARaisedHandSellIsCountedFromTheRaisedFigure(t *testing.T) {
	t.Run("raised above the listing credited", func(t *testing.T) {
		// A hand sell is listed at 10 against 100; an account snapshot holds
		// 100; a sell of 30 is released; the owner raises the hand sell to
		// 50, and IBKR lists it at 50 and ours at 30 against 100. 80 work:
		// netting 40 would let an exit of 60 pass, 40 short once all fill.
		g := gate(t, limits(), event.TradingModeDryRun)
		observe(t, g, "broker:1", broker(100, hand(10)))
		g.ObserveAccount(account(100, 1))
		released(t, check(t, g, order(event.OrderSideSell, 30, 100))[0])
		observe(t, g, "broker:2", later(broker(100, hand(50), listed("tw-sell", event.BrokerOrderWorking, 30)), 2))
		pendingIs(t, g, 80, "the raised hand sell's 50 and our 30")
		if got := oneMore(t, g); got != 80 {
			t.Errorf("working sells %d against the broker's 100, want 80", got)
		}
	})
	t.Run("raised after a fall", func(t *testing.T) {
		// A hand sell is listed at 20 against 100; an account snapshot holds
		// 100; a sell of 30 is released; the hand sell fills 15, and IBKR
		// lists it at 5 against 85; the owner raises it by 10, and IBKR lists
		// it at 15. The rise is below the first listing's 20, yet 15 + 15 of
		// the hand sell are sold since the account snapshot or still to sell:
		// with our 30, 60 of its 100, and 45 of the broker's 85.
		g := gate(t, limits(), event.TradingModeDryRun)
		observe(t, g, "broker:1", broker(100, hand(20)))
		g.ObserveAccount(account(100, 1))
		released(t, check(t, g, order(event.OrderSideSell, 30, 100))[0])
		observe(t, g, "broker:2", later(broker(85, hand(5), listed("tw-sell", event.BrokerOrderWorking, 30)), 2))
		observe(t, g, "broker:3", later(broker(85, hand(15), listed("tw-sell", event.BrokerOrderWorking, 30)), 3))
		pendingIs(t, g, 60, "against the account's 100")
		if got := oneMore(t, g); got != 45 {
			t.Errorf("working sells %d against the broker's 85, want 45", got)
		}
	})
}

// A hand sell placed after a snapshot, and first listed by a later one, was
// sold none of before the earlier snapshot, which cannot bound it: a broker
// snapshot and an account snapshot hold 100 with nothing listed; the owner
// sells 20 by hand, which fills 10, and IBKR lists it at 10 against 90. 20 of
// the account's 100 are sold since or still to sell, and 10 of the broker's
// 90; counting neither snapshot's 100 as a floor.
func TestAHandSellFirstListedLaterIsNotBoundedByAnEarlierSnapshot(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	g.ObserveAccount(account(100, 1))
	observe(t, g, "broker:2", later(broker(90, hand(10)), 2))
	pendingIs(t, g, 20, "the hand sell's 10 sold since and 10 left")
	if got := oneMore(t, g); got != 10 {
		t.Errorf("working sells %d against the broker's 90, want 10", got)
	}
}

// A buy IBKR lists is never counted as being sold.
func TestAListedBuyIsNoUnknownSell(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	buy := hand(25)
	buy.Side = event.OrderSideBuy
	observe(t, g, "broker:1", broker(100, buy))
	if got := oneMore(t, g); got != 0 {
		t.Errorf("working sells %d, want 0", got)
	}
}

// TestFilledCountsEachExecutionOnce: the shares a released order's fill
// reports executed, a redelivered execution counted once, and none for an
// order the gate never released.
func TestFilledCountsEachExecutionOnce(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(100))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	sellFill(t, g, "tw-sell", "f1", 10, 1)
	sellFill(t, g, "tw-sell", "f1", 10, 1)
	sellFill(t, g, "tw-sell", "f2", 5, 2)
	if got := g.Filled("tw-sell"); got != 15 {
		t.Errorf("Filled = %d, want 15", got)
	}
	if got := g.Filled("tw-unknown"); got != 0 {
		t.Errorf("Filled of an order never released = %d, want 0", got)
	}
}

// restoredSell is a sell of quantity an earlier run released, which a
// restarted run watches (ADR 0026, point 5).
func restoredSell(quantity int64) event.OrderReleasedPayload {
	return event.OrderReleasedPayload{AccountID: "joint", ClientOrderID: "tw-old", InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: quantity, LimitPrice: 1}
}

// restoredFill is an execution of the restored sell, minutes after the
// fixture time.
func restoredFill(id string, quantity int64, minutes int) event.TargetWeightFillPayload {
	return event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-old", FillID: id, InstrumentID: "SPY",
		Side: event.OrderSideSell, Quantity: quantity, Price: 1, FilledAt: at.Add(time.Duration(minutes) * time.Minute)}
}

// TestARestoredOrderReadsItsReportsFromTheFillsBeforeTheRestart: an order
// an earlier run released and a restarted run watches (ADR 0026, point 5)
// is restored with the executions reported before the restart, so the
// venue's later reports on it are read, the fills counting on from those
// shares and never past the order's own; a second restore, one for another
// account, one with every share filled, or one with a fill that is not the
// order's, is refused.
func TestARestoredOrderReadsItsReportsFromTheFillsBeforeTheRestart(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	o := restoredSell(39)
	if err := g.RestoreReleased(o, []event.TargetWeightFillPayload{restoredFill("f0", 20, -1)}, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	sellFill(t, g, "tw-old", "f1", 10, 1)
	if got := g.Filled("tw-old"); got != 30 {
		t.Errorf("Filled = %d, want the 20 before the restart and 10 after", got)
	}
	if err := g.ObserveFill(restoredFill("f2", 10, 2)); err == nil {
		t.Error("a fill past the order's 39 shares must be refused")
	}
	if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: "tw-old", OrderID: "ib-1", InstrumentID: "SPY",
		Side: event.OrderSideSell, Status: event.OrderLifecycleStatusCanceled, Quantity: 39, LimitPrice: 1, OccurredAt: at}); err != nil {
		t.Errorf("the restored order's cancel: %v", err)
	}
	if err := g.RestoreReleased(o, nil, time.Time{}, ""); err == nil {
		t.Error("a second restore of the same order must be refused")
	}
	other := o
	other.ClientOrderID, other.AccountID = "tw-other", "ira"
	if err := g.RestoreReleased(other, nil, time.Time{}, ""); err == nil {
		t.Error("a restore for another account must be refused")
	}
	for name, fills := range map[string][]event.TargetWeightFillPayload{
		"every share filled": {restoredFill("f0", 39, -1)},
		"another order's": {func() event.TargetWeightFillPayload {
			f := restoredFill("f0", 1, -1)
			f.ClientOrderID = "tw-other"
			return f
		}()},
		"a buy's": {func() event.TargetWeightFillPayload {
			f := restoredFill("f0", 1, -1)
			f.Side = event.OrderSideBuy
			return f
		}()},
		"an invalid one":    {restoredFill("", 1, -1)},
		"one fill id twice": {restoredFill("f0", 1, -1), restoredFill("f0", 2, -1)},
		// MaxInt64 + MaxInt64 + 3 wraps to 1, fewer than 39.
		"fills overflowing their sum": {restoredFill("f0", math.MaxInt64, -1), restoredFill("f1", math.MaxInt64, -1), restoredFill("f2", 3, -1)},
	} {
		g := gate(t, limits(), event.TradingModeDryRun)
		if err := g.RestoreReleased(o, fills, time.Time{}, ""); err == nil {
			t.Errorf("a restore with %s fill must be refused", name)
		}
		if got := g.Filled("tw-old"); got != 0 {
			t.Errorf("a refused restore with %s fill counts %d shares", name, got)
		}
	}
	if err := gate(t, limits(), event.TradingModeDryRun).RestoreReleased(restoredSell(0), nil, time.Time{}, ""); err == nil {
		t.Error("a restore of an order with no shares released must be refused")
	}
	applied := gate(t, limits(), event.TradingModeDryRun)
	observe(t, applied, "broker:1", broker(100))
	released(t, check(t, applied, order(event.OrderSideSell, 60, 100))[0])
	sellFill(t, applied, "tw-sell", "f0", 10, 1)
	if err := applied.RestoreReleased(o, []event.TargetWeightFillPayload{restoredFill("f0", 1, -1)}, time.Time{}, ""); err == nil {
		t.Error("a restore stating a fill id another order's execution holds must be refused")
	}
}

// TestARestoredOrderCountsAnExecutionFromBeforeTheRestartOnce: the venue
// re-reports an execution the earlier run had already applied; the restored
// order knows its identity, so the redelivery changes nothing, as on an
// order this run released (ADR 0019, "Explained means"), and the same fill
// id for a different execution is refused.
func TestARestoredOrderCountsAnExecutionFromBeforeTheRestartOnce(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	before := restoredFill("f1", 10, -1)
	if err := g.RestoreReleased(restoredSell(39), []event.TargetWeightFillPayload{before}, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := g.ObserveFill(before); err != nil {
		t.Fatalf("the redelivered execution: %v", err)
	}
	if got := g.Filled("tw-old"); got != 10 {
		t.Errorf("Filled = %d, want the execution before the restart counted once", got)
	}
	changed := before
	changed.Quantity = 20
	if err := g.ObserveFill(changed); err == nil {
		t.Error("the fill id of an execution before the restart, reused for another, must be refused")
	}
	if got := g.Fills("tw-old"); len(got) != 1 || !got[0].SameExecution(before) {
		t.Errorf("Fills = %+v, want the one execution", got)
	}
	if got := g.Fills("tw-unknown"); got != nil {
		t.Errorf("Fills of an order never released = %+v, want none", got)
	}
}

// TestARestoredSellIsNettedAsAWorkingSell: a sell an earlier run released
// and a restarted run watches is still being sold, so it is netted as any
// working sell (ADR 0019, amended 2026-10-15, sized "net of every working
// or pending sell"): 54 released, 20 executed before an account snapshot of
// 127, so 34 may still sell after it. Its later execution is shown by no
// snapshot before it, and its end, once a later snapshot follows it, frees
// what it left unsold.
func TestARestoredSellIsNettedAsAWorkingSell(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	if err := g.RestoreReleased(restoredSell(54), []event.TargetWeightFillPayload{restoredFill("f1", 20, -10)}, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	g.ObserveAccount(account(127, -5))
	pendingIs(t, g, 34, "the restored sell's 34 still to sell")
	sellFill(t, g, "tw-old", "f2", 4, 1)
	pendingIs(t, g, 34, "an account snapshot before the later execution")
	g.ObserveAccount(account(123, 2))
	pendingIs(t, g, 30, "an account snapshot after it")

	ended := gate(t, limits(), event.TradingModeDryRun)
	if err := ended.RestoreReleased(restoredSell(54), []event.TargetWeightFillPayload{restoredFill("f1", 20, -10)}, at.Add(-5*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	ended.ObserveAccount(account(127, -7))
	pendingIs(t, ended, 34, "an account snapshot before the restored sell ended")
	ended.ObserveAccount(account(127, -1))
	pendingIs(t, ended, 0, "an account snapshot after it ended")
}

// TestARestoredSellListedBeforeItsRestoreIsCountedOnce: a broker snapshot
// read before the restore lists the earlier run's working sell, which the
// gate does not yet know, so it is filed as an unknown sell. The restore
// takes that listing onto the restored sell, matched by its client order ID
// and instrument as a listing of a sell this run released is, so the sell
// is counted once (the pending rule's invariant "Unknown sells are counted
// once"; ADR 0019, amended 2026-10-15): 54 released, 20 executed, so 34
// still to sell, not 68. A hand sell listed beside it stays unknown.
func TestARestoredSellListedBeforeItsRestoreIsCountedOnce(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", later(broker(127, listed("tw-old", event.BrokerOrderWorking, 34)), -5))
	if err := g.RestoreReleased(restoredSell(54), []event.TargetWeightFillPayload{restoredFill("f1", 20, -10)}, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	g.ObserveAccount(account(127, -1))
	pendingIs(t, g, 34, "the restored sell, listed before its restore")

	withHand := gate(t, limits(), event.TradingModeDryRun)
	observe(t, withHand, "broker:1", later(broker(127, hand(5), listed("tw-old", event.BrokerOrderWorking, 34)), -5))
	if err := withHand.RestoreReleased(restoredSell(54), []event.TargetWeightFillPayload{restoredFill("f1", 20, -10)}, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	withHand.ObserveAccount(account(127, -1))
	pendingIs(t, withHand, 39, "the restored sell's 34 and the hand sell's 5")
}

// unechoed is IBKR's listing of a sell with no client order ID echoed, under
// venue order ID id.
func unechoed(id string, remaining int64) event.BrokerOrder {
	return event.BrokerOrder{BrokerOrderID: id, InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: remaining}
}

// TestARestoredSellListedWithoutItsClientOrderIDIsMatchedByItsVenueOrderID:
// IBKR does not always echo the client order ID. A broker snapshot read
// before the restore lists the earlier run's sell with none, and the
// decision recorded the sell's venue order ID, so the restore takes that
// listing, and every later one under the same venue order ID, onto the
// restored sell: 54 released, 20 executed, so 34 still to sell, counted
// once (the pending rule's invariant "Unknown sells are counted once"; ADR
// 0019, amended 2026-10-15). A sell of the same size under another venue
// order ID is someone else's and is counted beside it.
func TestARestoredSellListedWithoutItsClientOrderIDIsMatchedByItsVenueOrderID(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", later(broker(127, unechoed("ib-tw-old", 34)), -5))
	if err := g.RestoreReleased(restoredSell(54), []event.TargetWeightFillPayload{restoredFill("f1", 20, -10)}, time.Time{}, "ib-tw-old"); err != nil {
		t.Fatal(err)
	}
	g.ObserveAccount(account(127, -4))
	pendingIs(t, g, 34, "the restored sell, listed before its restore with no client order ID")
	observe(t, g, "broker:2", later(broker(127, unechoed("ib-tw-old", 34)), -3))
	g.ObserveAccount(account(127, -2))
	pendingIs(t, g, 34, "the restored sell, listed again after its restore with no client order ID")

	other := gate(t, limits(), event.TradingModeDryRun)
	observe(t, other, "broker:1", later(broker(127, unechoed("ib-hand", 34), unechoed("ib-tw-old", 34)), -5))
	if err := other.RestoreReleased(restoredSell(54), []event.TargetWeightFillPayload{restoredFill("f1", 20, -10)}, time.Time{}, "ib-tw-old"); err != nil {
		t.Fatal(err)
	}
	other.ObserveAccount(account(127, -1))
	pendingIs(t, other, 68, "the restored sell's 34 and a hand sell of 34 under another venue order ID")
}

// TestARestoredSellWithNoVenueOrderIDIsCountedBesideAnUnechoedListing: the
// decision recorded no venue order ID, and a broker snapshot read before
// the restore lists, with no client order ID, a sell of exactly the shares
// the restored sell still has to sell. Nothing proves the two are one
// order, so both are counted, 34 and 34: a brief over-count, which sells
// less, the pending rule's fail-closed direction (ADR 0019, amended
// 2026-10-15; ADR 0026, point 5, amended 2026-10-18). A later listing or
// lifecycle report under another venue order ID keeps both counted. The
// double count ends when the restored sell's own identity is shown: a
// lifecycle report on it naming the listing's venue order ID, or a listing
// of it under its client order ID with that venue order ID.
func TestARestoredSellWithNoVenueOrderIDIsCountedBesideAnUnechoedListing(t *testing.T) {
	restored := func(t *testing.T) *risklimit.Gate {
		t.Helper()
		g := gate(t, limits(), event.TradingModeDryRun)
		observe(t, g, "broker:1", later(broker(127, unechoed("ib-a", 34)), -5))
		if err := g.RestoreReleased(restoredSell(54), []event.TargetWeightFillPayload{restoredFill("f1", 20, -10)}, time.Time{}, ""); err != nil {
			t.Fatal(err)
		}
		g.ObserveAccount(account(127, -4))
		pendingIs(t, g, 68, "the restored sell's 34 and the unechoed listing's 34, both counted")
		return g
	}
	report := func(t *testing.T, g *risklimit.Gate, venueID string, minutes int) {
		t.Helper()
		if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
			AccountID: "joint", ClientOrderID: "tw-old", OrderID: venueID, InstrumentID: "SPY", Side: event.OrderSideSell,
			Status: event.OrderLifecycleStatusSubmitted, Quantity: 54, LimitPrice: 1, OccurredAt: at.Add(time.Duration(minutes) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("another venue order ID keeps both counted", func(t *testing.T) {
		g := restored(t)
		observe(t, g, "broker:2", later(broker(127, unechoed("ib-a", 34), listed("tw-old", event.BrokerOrderWorking, 34)), -3))
		g.ObserveAccount(account(127, -2))
		pendingIs(t, g, 68, "the restored sell listed under ib-tw-old beside the listing under ib-a")
		report(t, g, "ib-b", -1)
		observe(t, g, "broker:3", later(broker(127, unechoed("ib-a", 34)), 0))
		g.ObserveAccount(account(127, 1))
		pendingIs(t, g, 68, "a report naming ib-b, and the listing under ib-a again")
	})
	t.Run("a report naming the listing's venue order ID ends the double count", func(t *testing.T) {
		g := restored(t)
		report(t, g, "ib-a", -3)
		g.ObserveAccount(account(127, -2))
		pendingIs(t, g, 34, "the restored sell, once its report names ib-a")
		observe(t, g, "broker:2", later(broker(127, unechoed("ib-a", 34)), -1))
		g.ObserveAccount(account(127, 1))
		pendingIs(t, g, 34, "a later listing under ib-a, the restored sell's own")
	})
	t.Run("its own listing under that venue order ID ends the double count", func(t *testing.T) {
		g := restored(t)
		own := listed("tw-old", event.BrokerOrderWorking, 34)
		own.BrokerOrderID = "ib-a"
		observe(t, g, "broker:2", later(broker(127, own), -3))
		g.ObserveAccount(account(127, -2))
		pendingIs(t, g, 34, "the restored sell, listed under its client order ID and ib-a")
		observe(t, g, "broker:3", later(broker(127, unechoed("ib-a", 34)), -1))
		g.ObserveAccount(account(127, 1))
		pendingIs(t, g, 34, "a later listing under ib-a with no client order ID, the restored sell's own")
	})
}

// TestAReleasedSellListedWithoutItsClientOrderIDIsMatchedByItsVenueOrderID:
// the venue's lifecycle report names a released sell's venue order ID, so a
// broker listing under that ID with no client order ID echoed, read before
// the report or after it, is the sell's own and counted once: 60 released,
// 60 still to sell, not 120.
func TestAReleasedSellListedWithoutItsClientOrderIDIsMatchedByItsVenueOrderID(t *testing.T) {
	for name, listedFirst := range map[string]bool{"listed after its report": false, "listed before its report": true} {
		t.Run(name, func(t *testing.T) {
			g := gate(t, limits(), event.TradingModeDryRun)
			observe(t, g, "broker:1", broker(100))
			released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
			if listedFirst {
				observe(t, g, "broker:2", later(broker(100, unechoed("ib-tw-sell", 60)), 1))
			}
			sellStatus(t, g, "tw-sell", event.OrderLifecycleStatusSubmitted, 60, 1, 1)
			if !listedFirst {
				observe(t, g, "broker:2", later(broker(100, unechoed("ib-tw-sell", 60)), 1))
			}
			g.ObserveAccount(account(100, 2))
			pendingIs(t, g, 60, "the released sell, listed with no client order ID")
		})
	}
}

// TestARestoreRefusesAVenueOrderIDAnotherSellCarries: a venue order ID
// names one order. Two restored sells carrying the same one would let an
// unechoed listing match whichever was restored last, so the restore
// refuses the second and restores nothing of it (the pending rule's
// fail-closed direction, ADR 0019, amended 2026-10-15; ADR 0026, point 5,
// amended 2026-10-18).
func TestARestoreRefusesAVenueOrderIDAnotherSellCarries(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	if err := g.RestoreReleased(restoredSell(54), nil, time.Time{}, "ib-dup"); err != nil {
		t.Fatal(err)
	}
	second := restoredSell(40)
	second.ClientOrderID = "tw-old2"
	err := g.RestoreReleased(second, nil, time.Time{}, "ib-dup")
	if err == nil || !strings.Contains(err.Error(), "venue order") {
		t.Fatalf("a second restored sell under venue order ib-dup: err = %v, want a venue order refusal", err)
	}
	if err := g.RestoreReleased(second, nil, time.Time{}, "ib-other"); err != nil {
		t.Fatalf("nothing of the refused restore was kept, so the order restores under another venue order ID: %v", err)
	}
}

// TestAListingMatchingTwoSellsByVenueOrderIDIsNotTakenOntoEither: two
// released sells that lifecycle reports both tie to one venue order ID
// cannot say which an unechoed listing under it is, so the listing is
// counted beside them (an over-count, which sells less) rather than taken
// onto the first (ADR 0019, amended 2026-10-15; fail closed, AGENTS.md).
func TestAListingMatchingTwoSellsByVenueOrderIDIsNotTakenOntoEither(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	observe(t, g, "broker:1", broker(200))
	released(t, check(t, g, order(event.OrderSideSell, 60, 100))[0])
	if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
		AccountID: "joint", ClientOrderID: "tw-sell", OrderID: "ib-dup", InstrumentID: "SPY", Side: event.OrderSideSell,
		Status: event.OrderLifecycleStatusSubmitted, Quantity: 60, LimitPrice: 1, OccurredAt: at.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.RestoreReleased(restoredSell(54), nil, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := g.ObserveLifecycle(event.LimitOrderLifecyclePayload{
		AccountID: "joint", ClientOrderID: "tw-old", OrderID: "ib-dup", InstrumentID: "SPY", Side: event.OrderSideSell,
		Status: event.OrderLifecycleStatusSubmitted, Quantity: 54, LimitPrice: 1, OccurredAt: at.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	observe(t, g, "broker:2", later(broker(200, unechoed("ib-dup", 30)), 3))
	g.ObserveAccount(account(200, 4))
	pendingIs(t, g, 144, "the two sells, 60 and 54, and the ambiguous listing's 30 beside them")
}
