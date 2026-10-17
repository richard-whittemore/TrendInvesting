package orderplan_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// workedExample is ADR 0019's 2026-10-15 example: 147 shares, $4,700
// borrowed and a mark of $90, so net liquidation is $8,530.
func workedExample() *event.BrokerSnapshotPayload {
	return &event.BrokerSnapshotPayload{
		AccountID: "joint", AsOf: session, MarketOpen: true, NetLiquidation: 8530, ExcessLiquidity: 5222.5,
		Positions:  []event.BrokerPosition{{InstrumentID: "SPY", Quantity: 147, Mark: 90, MarkSet: true}},
		OpenOrders: []event.BrokerOrder{},
	}
}

func saleInput(s *event.BrokerSnapshotPayload) orderplan.SaleInput {
	return orderplan.SaleInput{
		AccountID: "joint", Tag: "engine-state:joint:tw-1", InstrumentID: "SPY",
		Kind: event.SaleKindIncident, Trigger: event.SaleTriggerExit, Target: 0.98, PriceBand: 0.05,
		SnapshotID: "broker-snapshot:1", Snapshot: s, Recognised: map[string]bool{"tw-ours": true},
		At: session, MaxSnapshotAge: 30 * time.Minute,
	}
}

func sale(t *testing.T, in orderplan.SaleInput) orderplan.Sale {
	t.Helper()
	got, err := orderplan.PlanSale(in)
	if err != nil {
		t.Fatal(err)
	}
	if (got.Order == nil) == (got.Withheld == nil) {
		t.Fatalf("exactly one of order and withheld: %+v", got)
	}
	if got.Order != nil {
		if err := got.Order.Validate(); err != nil {
			t.Fatalf("planned sale is invalid: %v", err)
		}
	} else if err := got.Withheld.Validate(); err != nil {
		t.Fatalf("withheld sale is invalid: %v", err)
	}
	return got
}

func TestSaleBackToNinetyEightPercentWorkedExample(t *testing.T) {
	// Keep ceil(0.98 x 8530 / 90) = ceil(92.88) = 93 shares ($8,370, 98.1%): sell 54.
	got := sale(t, saleInput(workedExample()))
	o := got.Order
	if o.Quantity != 54 || o.Side != event.OrderSideSell || o.Sale.KeepShares != 93 {
		t.Fatalf("sale: %+v", o)
	}
	if o.HeldQuantity != 147 || o.NetLiquidation != 8530 || o.ReferencePrice != 90 || o.TargetWeight != 0.98 {
		t.Errorf("one price basis, the broker's snapshot: %+v", o)
	}
	if o.LimitPrice != 85.5 {
		t.Errorf("limit %v, want the band's lower edge 85.50", o.LimitPrice)
	}
	if o.Tag != "engine-state:joint:tw-1" || o.ClientOrderID != orderplan.SaleClientOrderID("engine-state:joint:tw-1", "SPY") {
		t.Errorf("identity derives from the incident: %+v", o)
	}
	if o.ClientOrderID == orderplan.ClientOrderID(o.Tag, "SPY", event.OrderSideSell) {
		t.Error("a sale's client ID must differ from an ordinary sell's under the same tag")
	}
	if o.Sale.Kind != event.SaleKindIncident || o.Sale.Trigger != event.SaleTriggerExit || o.Sale.SnapshotID != "broker-snapshot:1" {
		t.Errorf("figures: %+v", o.Sale)
	}
}

func TestSaleIsSizedNetOfAWorkingSell(t *testing.T) {
	s := workedExample()
	s.OpenOrders = []event.BrokerOrder{{
		BrokerOrderID: "ib-1", ClientOrderID: "tw-ours", InstrumentID: "SPY", Side: event.OrderSideSell,
		Status: event.BrokerOrderCancelPending, RemainingQuantity: 20,
	}, {
		BrokerOrderID: "ib-2", InstrumentID: "SPY", Side: event.OrderSideBuy, Status: event.BrokerOrderWorking, RemainingQuantity: 9,
	}}
	in := saleInput(s)
	in.WorkingSells = 20
	got := sale(t, in)
	// The cancel-pending sell may still execute, so the caller counts it; the
	// buy is no sell. To target 54, within holding 127, never below 34: sell 34.
	f := got.Order.Sale
	if f.WorkingSells != 20 || f.ToTarget != 54 || f.WithinHolding != 127 || f.NeverBelow != 34 || got.Order.Quantity != 34 {
		t.Fatalf("net of a working sell: %+v quantity %d", f, got.Order.Quantity)
	}
	// Working sells that cover more than the holding above 98% leave nothing.
	s.OpenOrders[0].RemainingQuantity = 60
	in.WorkingSells = 60
	if w := sale(t, in).Withheld; w.Reason != event.SaleWithheldNothingToSell {
		t.Fatalf("withheld %+v", w)
	}
}

// TestSaleIsSizedNetOfEveryPendingSellTheCallerCounts: the sale nets the
// working and pending sells its caller counts against the snapshot by the
// gate's one pending rule, including a released sell the snapshot does not
// list yet, never only the sells the snapshot lists. Boost's exit of 54,
// released and not yet listed, already sells 147 down to the 93 kept.
func TestSaleIsSizedNetOfEveryPendingSellTheCallerCounts(t *testing.T) {
	in := saleInput(workedExample())
	in.WorkingSells = 54
	got := sale(t, in)
	if got.Withheld == nil || got.Withheld.Reason != event.SaleWithheldNothingToSell {
		t.Fatalf("the unlisted exit of 54 leaves nothing to sell: %+v", got)
	}
	in.WorkingSells = 20
	if o := sale(t, in).Order; o == nil || o.Quantity != 34 || o.Sale.WorkingSells != 20 {
		t.Fatalf("net of 20 pending: %+v", o)
	}
	in.WorkingSells = -1
	if _, err := orderplan.PlanSale(in); err == nil {
		t.Error("negative working sells accepted")
	}
}

func TestNoSaleWithAnUnknownWorkingSell(t *testing.T) {
	s := workedExample()
	s.OpenOrders = []event.BrokerOrder{{
		BrokerOrderID: "ib-9", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: 1,
	}}
	w := sale(t, saleInput(s)).Withheld
	if w.Reason != event.SaleWithheldUnknownWorkingSell || !strings.Contains(w.Detail, "CONTAINMENT REQUIRED") || !strings.Contains(w.Detail, "ib-9") {
		t.Fatalf("withheld %+v", w)
	}
	// A client ID the journal never released is just as unknown.
	s.OpenOrders[0].ClientOrderID = "tw-someone-else"
	if w := sale(t, saleInput(s)).Withheld; w.Reason != event.SaleWithheldUnknownWorkingSell {
		t.Fatalf("withheld %+v", w)
	}
	// An unknown buy is not a working sell, and an unknown sell on another
	// instrument is that instrument's containment, not this sale's.
	s.OpenOrders[0].Side = event.OrderSideBuy
	s.OpenOrders = append(s.OpenOrders, event.BrokerOrder{
		BrokerOrderID: "ib-a", InstrumentID: "IEF", Side: event.OrderSideSell, Status: event.BrokerOrderWorking, RemainingQuantity: 1,
	})
	if got := sale(t, saleInput(s)); got.Order == nil || got.Order.Quantity != 54 {
		t.Fatalf("sale: %+v", got)
	}
	// The urgent sale in Normal is sized net of every working sell but has no
	// recognition check (ADR 0026, point 4).
	s.OpenOrders[0].Side = event.OrderSideSell
	in := saleInput(s)
	in.Kind, in.Trigger, in.Tag = event.SaleKindUrgent, event.SaleTriggerMarginUrgent, "margin-alarm:joint:broker-snapshot:1"
	in.WorkingSells = 1
	if got := sale(t, in); got.Order == nil || got.Order.Quantity != 53 {
		t.Fatalf("urgent sale: %+v", got)
	}
}

func TestNoSaleWithoutReadableBrokerValues(t *testing.T) {
	for name, mutate := range map[string]func(*event.BrokerSnapshotPayload){
		"net liquidation": func(s *event.BrokerSnapshotPayload) {
			s.Unreadable, s.NetLiquidation = []string{event.BrokerComponentNetLiquidation}, 0
		},
		"positions": func(s *event.BrokerSnapshotPayload) {
			s.Unreadable, s.Positions = []string{event.BrokerComponentPositions}, nil
		},
		"open orders": func(s *event.BrokerSnapshotPayload) { s.Unreadable = []string{event.BrokerComponentOpenOrders} },
		"mark":        func(s *event.BrokerSnapshotPayload) { s.Positions[0].Mark, s.Positions[0].MarkSet = 0, false },
	} {
		s := workedExample()
		mutate(s)
		w := sale(t, saleInput(s)).Withheld
		if w.Reason != event.SaleWithheldUnreadable {
			t.Errorf("%s unreadable: %+v", name, w)
		}
	}
	none := saleInput(nil)
	none.SnapshotID = ""
	w := sale(t, none).Withheld
	if w.Reason != event.SaleWithheldUnreadable || w.SnapshotID != "" {
		t.Errorf("no snapshot at all: %+v", w)
	}
	// Excess Liquidity is not needed to size the sale.
	s := workedExample()
	s.Unreadable, s.ExcessLiquidity = []string{event.BrokerComponentExcessLiquidity}, 0
	if got := sale(t, saleInput(s)); got.Order == nil {
		t.Errorf("an unreadable Excess Liquidity does not stop the sale: %+v", got.Withheld)
	}
}

func TestNothingToSellAtOrBelowNinetyEightPercent(t *testing.T) {
	s := workedExample()
	s.Positions[0].Quantity = 93
	if w := sale(t, saleInput(s)).Withheld; w.Reason != event.SaleWithheldNothingToSell {
		t.Fatalf("at 98.1%%: %+v", w)
	}
	s.Positions = []event.BrokerPosition{}
	if w := sale(t, saleInput(s)).Withheld; w.Reason != event.SaleWithheldNothingToSell {
		t.Fatalf("nothing held: %+v", w)
	}
	s.Positions = []event.BrokerPosition{{InstrumentID: "SPY", Quantity: -5, Mark: 90, MarkSet: true}}
	if w := sale(t, saleInput(s)).Withheld; w.Reason != event.SaleWithheldNothingToSell || !strings.Contains(w.Detail, "short") {
		t.Fatalf("a short is never sold further: %+v", w)
	}
}

func TestSaleRefusesBadInput(t *testing.T) {
	for name, mutate := range map[string]func(*orderplan.SaleInput){
		"account":    func(in *orderplan.SaleInput) { in.AccountID = "" },
		"tag":        func(in *orderplan.SaleInput) { in.Tag = "" },
		"instrument": func(in *orderplan.SaleInput) { in.InstrumentID = "" },
		"kind":       func(in *orderplan.SaleInput) { in.Kind = "" },
		"trigger":    func(in *orderplan.SaleInput) { in.Trigger = "" },
		"target":     func(in *orderplan.SaleInput) { in.Target = 0 },
		"band":       func(in *orderplan.SaleInput) { in.PriceBand = 1 },
		"snapshot id": func(in *orderplan.SaleInput) {
			in.SnapshotID = ""
		},
		"snapshot id without snapshot": func(in *orderplan.SaleInput) { in.Snapshot = nil },
		"other account":                func(in *orderplan.SaleInput) { in.Snapshot.AccountID = "ira" },
		"invalid snapshot": func(in *orderplan.SaleInput) {
			in.Snapshot.Unreadable = []string{"no-such-component"}
		},
	} {
		in := saleInput(workedExample())
		mutate(&in)
		if _, err := orderplan.PlanSale(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A mark so small that the shares to keep overflow is refused, never wrapped.
	tiny := workedExample()
	tiny.Positions[0].Mark = 1e-300
	if _, err := orderplan.PlanSale(saleInput(tiny)); err == nil {
		t.Error("an unrepresentable share count accepted")
	}
}

// TestNoSaleFromANetLiquidationOrMarkThatIsNotPositive: keep =
// ceil(Target x V / mark) is zero or negative when net liquidation or the mark
// is not strictly positive, and the smallest of the three bounds would then
// sell the whole holding or past it into a short, which ADR 0019's no-short
// rule forbids. Such a snapshot is withheld with its own reason, never sized
// from, and the withhold is not the attempt.
func TestNoSaleFromANetLiquidationOrMarkThatIsNotPositive(t *testing.T) {
	for name, mutate := range map[string]func(*event.BrokerSnapshotPayload){
		"zero net liquidation":     func(s *event.BrokerSnapshotPayload) { s.NetLiquidation = 0 },
		"negative net liquidation": func(s *event.BrokerSnapshotPayload) { s.NetLiquidation = -500 },
		"zero mark":                func(s *event.BrokerSnapshotPayload) { s.Positions[0].Mark = 0 },
		"negative mark":            func(s *event.BrokerSnapshotPayload) { s.Positions[0].Mark = -90 },
	} {
		s := workedExample()
		mutate(s)
		got := sale(t, saleInput(s))
		if got.Withheld == nil || got.Withheld.Reason != event.SaleWithheldNotPositive || got.Withheld.Attempted() {
			t.Errorf("%s: want withheld as not positive, not an attempt, got %+v", name, got)
		}
	}
}

// TestNoSaleFromAStaleSnapshot: a snapshot older at the trigger than the
// maximum age is withheld as stale; one exactly that old still sizes.
func TestNoSaleFromAStaleSnapshot(t *testing.T) {
	in := saleInput(workedExample())
	in.At = session.Add(31 * time.Minute)
	if got := sale(t, in); got.Withheld == nil || got.Withheld.Reason != event.SaleWithheldStaleBrokerSnapshot || got.Withheld.Attempted() {
		t.Errorf("31 minutes old: %+v", got)
	}
	in.At = session.Add(30 * time.Minute)
	if got := sale(t, in); got.Order == nil {
		t.Errorf("30 minutes old: %+v", got)
	}
	// A snapshot taken after the trigger is stale, never fresh, and the
	// withhold records the skew.
	in.At = session.Add(-time.Minute)
	if got := sale(t, in); got.Withheld == nil || got.Withheld.Reason != event.SaleWithheldStaleBrokerSnapshot || !strings.Contains(got.Withheld.Detail, "after the trigger") {
		t.Errorf("taken after the trigger: %+v", got)
	}
	// Freshness is checked before the values: stale and not positive is stale.
	both := saleInput(workedExample())
	both.Snapshot.NetLiquidation, both.At = -1, session.Add(31*time.Minute)
	if got := sale(t, both); got.Withheld == nil || got.Withheld.Reason != event.SaleWithheldStaleBrokerSnapshot {
		t.Errorf("stale and not positive: %+v", got)
	}
	// An unreadable snapshot is unreadable, however old.
	blind := saleInput(workedExample())
	blind.Snapshot.Unreadable, blind.Snapshot.OpenOrders, blind.At = []string{event.BrokerComponentOpenOrders}, nil, session.Add(time.Hour)
	if got := sale(t, blind); got.Withheld == nil || got.Withheld.Reason != event.SaleWithheldUnreadable {
		t.Errorf("unreadable and old: %+v", got)
	}
	in.MaxSnapshotAge = 0
	if _, err := orderplan.PlanSale(in); err == nil {
		t.Error("no maximum age accepted")
	}
}
