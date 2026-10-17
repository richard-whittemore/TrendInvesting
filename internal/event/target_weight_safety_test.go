package event_test

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validKillSwitch() event.KillSwitchPayload {
	return event.KillSwitchPayload{AccountID: "joint", AsOf: twSession, Engaged: true, Operator: "owner", Detail: "switch file created"}
}

func validBrokerSnapshot() event.BrokerSnapshotPayload {
	return event.BrokerSnapshotPayload{
		AccountID: "joint", AsOf: twSession, MarketOpen: true,
		NetLiquidation: 8530, ExcessLiquidity: 5222.5,
		Positions: []event.BrokerPosition{{InstrumentID: "SPY", Quantity: 147, Mark: 90, MarkSet: true}},
		OpenOrders: []event.BrokerOrder{{
			BrokerOrderID: "ib-1", ClientOrderID: "tw-1", InstrumentID: "SPY", Side: event.OrderSideSell,
			Status: event.BrokerOrderWorking, RemainingQuantity: 10,
		}},
	}
}

func validMarginAlarm() event.MarginAlarmPayload {
	return event.MarginAlarmPayload{
		AccountID: "joint", SnapshotID: "broker-snapshot:1", AsOf: twSession,
		Level: event.MarginAlarmUrgent, Cause: event.MarginAlarmCauseRatio,
		ExcessLiquidity: 1500, NetLiquidation: 8530, Ratio: 1500.0 / 8530, ReadingSet: true,
		LastReadingAt: twSession, LastReadingSet: true,
		Warning: 0.4, Urgent: 0.2, StaleMinutes: 30, Detail: "excess liquidity 17.6% of net liquidation",
	}
}

func validSaleWithheld() event.SaleWithheldPayload {
	return event.SaleWithheldPayload{
		AccountID: "joint", Tag: "engine-state:joint:tw-1", ClientOrderID: "tw-2", InstrumentID: "SPY",
		Kind: event.SaleKindIncident, Trigger: event.SaleTriggerExit, Reason: event.SaleWithheldUnknownWorkingSell,
		Detail: "working sell ib-9 is not one the journal released: CONTAINMENT REQUIRED", SnapshotID: "broker-snapshot:1",
	}
}

// validSaleOrder is ADR 0019's 2026-10-15 worked example: 147 shares, $4,700
// borrowed, a mark of $90: net liquidation $8,530, keep 93, sell 54.
func validSaleOrder() event.OrderPlannedPayload {
	return event.OrderPlannedPayload{
		AccountID: "joint", Tag: "engine-state:joint:tw-1", ClientOrderID: "tw-2",
		InstrumentID: "SPY", Side: event.OrderSideSell, Phase: event.OrderSideSell, OrderType: event.OrderTypeLimit,
		Quantity: 54, LimitPrice: 85.5, TargetWeight: 0.98, NetLiquidation: 8530, HeldQuantity: 147,
		ReferencePrice: 90, PriceBand: 0.05,
		Sale: event.SaleFigures{
			Kind: event.SaleKindIncident, Trigger: event.SaleTriggerExit, SnapshotID: "broker-snapshot:1",
			WorkingSells: 0, KeepShares: 93, ToTarget: 54, WithinHolding: 147, NeverBelow: 54,
		},
	}
}

func TestSafetyPayloadFixturesAreValid(t *testing.T) {
	for _, p := range []validator{validKillSwitch(), validBrokerSnapshot(), validMarginAlarm(), validSaleWithheld(), validSaleOrder()} {
		if err := p.Validate(); err != nil {
			t.Errorf("%T: %v", p, err)
		}
	}
}

func TestKillSwitchValidate(t *testing.T) {
	v := validKillSwitch()
	type P = event.KillSwitchPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "as of", v, func(p *P) { p.AsOf = time.Time{} }, "as of")
	expectInvalid(t, "operator", v, func(p *P) { p.Operator = "" }, "operator")
	expectInvalid(t, "detail", v, func(p *P) { p.Detail = "" }, "detail")
	released := v
	released.Engaged = false
	if err := released.Validate(); err != nil {
		t.Errorf("a disengaged switch is a valid input: %v", err)
	}
}

func TestBrokerSnapshotValidate(t *testing.T) {
	v := validBrokerSnapshot()
	type P = event.BrokerSnapshotPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "as of", v, func(p *P) { p.AsOf = time.Time{} }, "as of")
	expectInvalid(t, "net liquidation", v, func(p *P) { p.NetLiquidation = math.Inf(-1) }, "net liquidation")
	// IBKR can report a net liquidation or mark that is not positive: the
	// snapshot records it as observed, and the gate refuses to size from it.
	for name, mutate := range map[string]func(*P){
		"zero net liquidation":     func(p *P) { p.NetLiquidation = 0 },
		"negative net liquidation": func(p *P) { p.NetLiquidation = -500 },
		"set mark zero": func(p *P) {
			p.Positions = slicesClone(p.Positions)
			p.Positions[0].Mark = 0
		},
		"set mark negative": func(p *P) {
			p.Positions = slicesClone(p.Positions)
			p.Positions[0].Mark = -1
		},
	} {
		p := v
		mutate(&p)
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	expectInvalid(t, "excess liquidity", v, func(p *P) { p.ExcessLiquidity = math.NaN() }, "excess liquidity")
	expectInvalid(t, "unknown component", v, func(p *P) { p.Unreadable = []string{"weather"} }, "component")
	expectInvalid(t, "unsorted components", v, func(p *P) {
		p.Unreadable = []string{event.BrokerComponentOpenOrders, event.BrokerComponentNetLiquidation}
		p.NetLiquidation, p.OpenOrders = 0, nil
	}, "ascending")
	// An unreadable component is not zero: it carries no value at all.
	expectInvalid(t, "unreadable net liquidation with a value", v, func(p *P) { p.Unreadable = []string{event.BrokerComponentNetLiquidation} }, "unreadable")
	expectInvalid(t, "unreadable excess with a value", v, func(p *P) { p.Unreadable = []string{event.BrokerComponentExcessLiquidity} }, "unreadable")
	expectInvalid(t, "unreadable positions with rows", v, func(p *P) { p.Unreadable = []string{event.BrokerComponentPositions} }, "unreadable")
	expectInvalid(t, "unreadable orders with rows", v, func(p *P) { p.Unreadable = []string{event.BrokerComponentOpenOrders} }, "unreadable")
	expectInvalid(t, "position instrument", v, func(p *P) { p.Positions = []event.BrokerPosition{{Quantity: 1}} }, "instrument")
	expectInvalid(t, "zero position", v, func(p *P) { p.Positions = slicesClone(p.Positions); p.Positions[0].Quantity = 0 }, "zero")
	expectInvalid(t, "mark without set", v, func(p *P) { p.Positions = slicesClone(p.Positions); p.Positions[0].MarkSet = false }, "mark")
	expectInvalid(t, "set mark not finite", v, func(p *P) { p.Positions = slicesClone(p.Positions); p.Positions[0].Mark = math.NaN() }, "mark")
	expectInvalid(t, "unsorted positions", v, func(p *P) {
		p.Positions = []event.BrokerPosition{{InstrumentID: "SPY", Quantity: 1}, {InstrumentID: "IEF", Quantity: 1}}
	}, "ascending")
	expectInvalid(t, "order broker id", v, func(p *P) { p.OpenOrders = slicesClone(p.OpenOrders); p.OpenOrders[0].BrokerOrderID = "" }, "broker order id")
	expectInvalid(t, "order instrument", v, func(p *P) { p.OpenOrders = slicesClone(p.OpenOrders); p.OpenOrders[0].InstrumentID = "" }, "instrument")
	expectInvalid(t, "order side", v, func(p *P) { p.OpenOrders = slicesClone(p.OpenOrders); p.OpenOrders[0].Side = "short" }, "side")
	expectInvalid(t, "order status", v, func(p *P) { p.OpenOrders = slicesClone(p.OpenOrders); p.OpenOrders[0].Status = "filled" }, "status")
	expectInvalid(t, "order quantity", v, func(p *P) { p.OpenOrders = slicesClone(p.OpenOrders); p.OpenOrders[0].RemainingQuantity = 0 }, "remaining")
	expectInvalid(t, "unsorted orders", v, func(p *P) {
		second := p.OpenOrders[0]
		second.BrokerOrderID = "ib-0"
		p.OpenOrders = append(slicesClone(p.OpenOrders), second)
	}, "ascending")

	unknown := v
	unknown.OpenOrders = append(slicesClone(v.OpenOrders), event.BrokerOrder{
		BrokerOrderID: "ib-2", InstrumentID: "SPY", Side: event.OrderSideSell, Status: event.BrokerOrderCancelPending, RemainingQuantity: 5,
	}, event.BrokerOrder{
		BrokerOrderID: "ib-3", InstrumentID: "SPY", Side: event.OrderSideBuy, Status: event.BrokerOrderPending, RemainingQuantity: 7,
	})
	if err := unknown.Validate(); err != nil {
		t.Fatalf("an order with no client ID is a valid observation (it is unknown): %v", err)
	}
	if p, ok := unknown.Position("SPY"); !ok || p.Quantity != 147 {
		t.Errorf("position: %+v %v", p, ok)
	}
	if _, ok := unknown.Position("IEF"); ok {
		t.Error("IEF is not held")
	}
	if !unknown.Readable(event.BrokerComponentPositions) {
		t.Error("positions are readable")
	}

	blind := P{AccountID: "joint", AsOf: twSession, Unreadable: []string{
		event.BrokerComponentExcessLiquidity, event.BrokerComponentNetLiquidation, event.BrokerComponentOpenOrders, event.BrokerComponentPositions,
	}}
	if err := blind.Validate(); err != nil {
		t.Fatalf("a snapshot reading nothing is a valid observation: %v", err)
	}
	if blind.Readable(event.BrokerComponentNetLiquidation) {
		t.Error("net liquidation is unreadable")
	}
}

func slicesClone[T any](s []T) []T { return append([]T(nil), s...) }

func TestMarginAlarmValidate(t *testing.T) {
	v := validMarginAlarm()
	type P = event.MarginAlarmPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "snapshot", v, func(p *P) { p.SnapshotID = "" }, "snapshot")
	expectInvalid(t, "as of", v, func(p *P) { p.AsOf = time.Time{} }, "as of")
	expectInvalid(t, "level", v, func(p *P) { p.Level = "panic" }, "level")
	expectInvalid(t, "cause", v, func(p *P) { p.Cause = "vibes" }, "cause")
	expectInvalid(t, "ratio without reading", v, func(p *P) { p.ReadingSet = false }, "reading")
	expectInvalid(t, "no-reading cause with a reading", v, func(p *P) { p.Cause = event.MarginAlarmCauseNoReading }, "reading")
	expectInvalid(t, "negative ratio", v, func(p *P) { p.Ratio = -1 }, "ratio")
	expectInvalid(t, "last reading without set", v, func(p *P) { p.LastReadingSet = false }, "last reading")
	expectInvalid(t, "thresholds", v, func(p *P) { p.Urgent = 0.5 }, "threshold")
	expectInvalid(t, "stale minutes", v, func(p *P) { p.StaleMinutes = 0 }, "stale")
	expectInvalid(t, "detail", v, func(p *P) { p.Detail = "" }, "detail")
	expectInvalid(t, "net liquidation", v, func(p *P) { p.NetLiquidation = math.Inf(1) }, "net liquidation")
	stale := P{
		AccountID: "joint", SnapshotID: "broker-snapshot:2", AsOf: twSession, Level: event.MarginAlarmUrgent,
		Cause: event.MarginAlarmCauseNoReading, Warning: 0.4, Urgent: 0.2, StaleMinutes: 30, Detail: "no reading since the run began",
	}
	if err := stale.Validate(); err != nil {
		t.Fatalf("an urgent alarm for no reading: %v", err)
	}
	expectInvalid(t, "no reading but clear", stale, func(p *P) { p.Level = event.MarginAlarmClear }, "urgent")
	// A readable net liquidation of zero or less is a reading with no ratio,
	// and always urgent.
	deficit := P{
		AccountID: "joint", SnapshotID: "broker-snapshot:3", AsOf: twSession, Level: event.MarginAlarmUrgent,
		Cause: event.MarginAlarmCauseNonPositive, ReadingSet: true, ExcessLiquidity: -500, NetLiquidation: -200,
		LastReadingAt: twSession, LastReadingSet: true,
		Warning: 0.4, Urgent: 0.2, StaleMinutes: 30, Detail: "net liquidation -200 is not positive",
	}
	if err := deficit.Validate(); err != nil {
		t.Fatalf("an urgent alarm for a non-positive net liquidation: %v", err)
	}
	zero := deficit
	zero.NetLiquidation = 0
	if err := zero.Validate(); err != nil {
		t.Errorf("zero net liquidation: %v", err)
	}
	expectInvalid(t, "non-positive but warning", deficit, func(p *P) { p.Level = event.MarginAlarmWarning }, "urgent")
	expectInvalid(t, "non-positive without the reading", deficit, func(p *P) { p.ReadingSet, p.ExcessLiquidity, p.NetLiquidation = false, 0, 0 }, "reading")
	expectInvalid(t, "non-positive with a positive net liquidation", deficit, func(p *P) { p.NetLiquidation = 1 }, "net liquidation")
	expectInvalid(t, "non-positive with a ratio", deficit, func(p *P) { p.Ratio = 2.5 }, "ratio")
	expectInvalid(t, "ratio from a non-positive net liquidation", v, func(p *P) { p.NetLiquidation, p.Ratio = 0, 0 }, "net liquidation")
}

func TestSaleWithheldValidate(t *testing.T) {
	v := validSaleWithheld()
	type P = event.SaleWithheldPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "tag", v, func(p *P) { p.Tag = "" }, "tag")
	expectInvalid(t, "client", v, func(p *P) { p.ClientOrderID = "" }, "client order id")
	expectInvalid(t, "instrument", v, func(p *P) { p.InstrumentID = "" }, "instrument")
	expectInvalid(t, "kind", v, func(p *P) { p.Kind = "whim" }, "kind")
	expectInvalid(t, "trigger", v, func(p *P) { p.Trigger = "whim" }, "trigger")
	expectInvalid(t, "reason", v, func(p *P) { p.Reason = "whim" }, "reason")
	expectInvalid(t, "detail", v, func(p *P) { p.Detail = "" }, "detail")
	// Only a withhold that asks the owner to act by hand is the attempt.
	for reason, attempt := range map[string]bool{
		event.SaleWithheldUnreadable: true, event.SaleWithheldUnknownWorkingSell: true,
		event.SaleWithheldNothingToSell: false, event.SaleWithheldStaleBrokerSnapshot: false, event.SaleWithheldNotPositive: false,
		event.SaleWithheldLapsed: false,
	} {
		p := v
		p.Reason = reason
		if p.Attempted() != attempt {
			t.Errorf("%s: attempted %v, want %v", reason, p.Attempted(), attempt)
		}
	}
	for _, reason := range []string{event.SaleWithheldUnreadable, event.SaleWithheldUnknownWorkingSell, event.SaleWithheldNothingToSell, event.SaleWithheldStaleBrokerSnapshot, event.SaleWithheldNotPositive, event.SaleWithheldLapsed} {
		p := v
		p.Reason = reason
		if err := p.Validate(); err != nil {
			t.Errorf("reason %s: %v", reason, err)
		}
	}
	// Only a stale withhold opening a stale episode carries the alert.
	alert := v
	alert.Reason, alert.Alert = event.SaleWithheldStaleBrokerSnapshot, true
	if err := alert.Validate(); err != nil {
		t.Errorf("a stale withhold's alert: %v", err)
	}
	expectInvalid(t, "alert on another reason", v, func(p *P) { p.Alert = true }, "alert")
}

func TestSaleOrderValidate(t *testing.T) {
	v := validSaleOrder()
	type P = event.OrderPlannedPayload
	expectInvalid(t, "a sale buys", v, func(p *P) { p.Side, p.Phase, p.LimitPrice = event.OrderSideBuy, event.OrderSideBuy, 94.5 }, "sell")
	expectInvalid(t, "kind", v, func(p *P) { p.Sale.Kind = "whim" }, "kind")
	expectInvalid(t, "trigger", v, func(p *P) { p.Sale.Trigger = "" }, "trigger")
	expectInvalid(t, "snapshot", v, func(p *P) { p.Sale.SnapshotID = "" }, "snapshot")
	expectInvalid(t, "keep", v, func(p *P) { p.Sale.KeepShares = 92 }, "keep")
	expectInvalid(t, "to target", v, func(p *P) { p.Sale.ToTarget = 55 }, "to target")
	expectInvalid(t, "within holding", v, func(p *P) { p.Sale.WithinHolding = 140 }, "within holding")
	expectInvalid(t, "never below", v, func(p *P) { p.Sale.NeverBelow = 50 }, "never below")
	expectInvalid(t, "quantity", v, func(p *P) { p.Quantity = 55 }, "smallest")
	expectInvalid(t, "working sells", v, func(p *P) { p.Sale.WorkingSells = -1 }, "working sells")

	// Net of a working sell of 10: 147 - 10 - 93 = 44.
	net := v
	net.Sale.WorkingSells, net.Sale.WithinHolding, net.Sale.NeverBelow, net.Quantity = 10, 137, 44, 44
	if err := net.Validate(); err != nil {
		t.Errorf("a sale net of a working sell: %v", err)
	}

	// An ordinary planned order encodes no sale field, so its bytes are unchanged.
	raw, err := json.Marshal(validOrderPlanned())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sale") {
		t.Errorf("an ordinary order encodes a sale field: %s", raw)
	}
}

func TestRiskFiguresBrokerFields(t *testing.T) {
	r := validOrderReleased()
	r.Side = event.OrderSideSell
	r.Figures.RiskReducing, r.Figures.BrokerSet, r.Figures.BrokerHolding, r.Figures.WorkingSells = true, true, 147, 10
	if err := r.Validate(); err != nil {
		t.Fatalf("broker figures: %v", err)
	}
	type R = event.OrderReleasedPayload
	expectInvalid(t, "broker values without broker", r, func(p *R) { p.Figures.BrokerSet = false }, "broker")
	expectInvalid(t, "negative working sells", r, func(p *R) { p.Figures.WorkingSells = -1 }, "working sells")
	expectInvalid(t, "a risk-reducing buy", r, func(p *R) { p.Side = event.OrderSideBuy }, "risk-reducing")
	// With no broker snapshot, a sell within the account snapshot's holding
	// is still risk-reducing, and the figures say so.
	account := r
	account.Figures.BrokerSet, account.Figures.BrokerHolding, account.Figures.WorkingSells = false, 0, 0
	if err := account.ValidateSchema(event.OrderReleasedSafetyVersion); err != nil {
		t.Errorf("risk-reducing without a broker snapshot: %v", err)
	}
	// The broker fields are omitted when unset, so figures without a broker snapshot keep their bytes.
	raw, err := json.Marshal(validRiskFigures())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"risk_reducing", "broker"} {
		if strings.Contains(string(raw), field) {
			t.Errorf("unset figures encode %s: %s", field, raw)
		}
	}
	f := validOrderRefused()
	for _, reason := range []string{event.RefusedKillSwitch, event.RefusedBrokerHolding, event.RefusedSaleAttempted} {
		p := f
		p.Reason = reason
		if err := p.Validate(); err != nil {
			t.Errorf("reason %s: %v", reason, err)
		}
	}
}

func TestExcessLiquidityLimits(t *testing.T) {
	v := validTargetWeightConfiguration()
	type P = event.TargetWeightConfigurationPayload
	expectInvalid(t, "warning", v, func(p *P) { p.Limits.ExcessLiquidityWarning = 1 }, "excess liquidity")
	expectInvalid(t, "urgent", v, func(p *P) { p.Limits.ExcessLiquidityUrgent = 0 }, "excess liquidity")
	expectInvalid(t, "urgent above warning", v, func(p *P) { p.Limits.ExcessLiquidityUrgent = 0.5 }, "excess liquidity")
	expectInvalid(t, "stale", v, func(p *P) { p.Limits.ExcessLiquidityStaleMinutes = 0 }, "stale")
}

// TestSafetyFieldsJoinAtTheirVersions: a sale's figures join
// strategy.order.planned at version 2. The risk figures' broker fields and
// the three safety refusal reasons join risk.order.released and
// risk.order.refused at version 3, after version 2's account-wide exposure,
// IRA cash figures and IRA refusal reasons. A writer journals each payload at
// the lowest version that holds it, never below the current base, and an
// earlier version keeps its meaning and refuses a later version's field, so a
// record is never read as carrying what its version cannot hold (ADR 0015).
func TestSafetyFieldsJoinAtTheirVersions(t *testing.T) {
	type versioned interface {
		SchemaVersion() uint32
		ValidateSchema(uint32) error
	}
	withBroker := validRiskFigures()
	withBroker.BrokerSet, withBroker.BrokerHolding, withBroker.WorkingSells, withBroker.RiskReducing = true, 147, 10, true
	brokerReleased := validOrderReleased()
	brokerReleased.Figures, brokerReleased.Side = withBroker, event.OrderSideSell
	cashReleased := validOrderReleased()
	cashReleased.Figures.CashChecked, cashReleased.Figures.BuyCost, cashReleased.Figures.CashAvailable = true, 900, 980
	brokerRefused := validOrderRefused()
	brokerRefused.Figures, brokerRefused.Side = withBroker, event.OrderSideSell
	killRefused := validOrderRefused()
	killRefused.Reason = event.RefusedKillSwitch
	sellRefused := validOrderRefused()
	sellRefused.Reason = event.RefusedSellExceedsHolding
	for name, c := range map[string]struct {
		p      versioned
		want   uint32 // the version a writer journals it at
		oldest uint32 // the earliest version that holds it
		latest uint32 // the latest version of its type
	}{
		"ordinary order":           {validOrderPlanned(), event.OrderPlannedSchemaVersion, 1, 2},
		"sale":                     {validSaleOrder(), event.OrderPlannedSafetyVersion, 2, 2},
		"release without a broker": {validOrderReleased(), event.OrderReleasedSchemaVersion, 1, 3},
		"release with IRA cash":    {cashReleased, event.OrderReleasedSchemaVersion, 2, 3},
		"release with a broker":    {brokerReleased, event.OrderReleasedSafetyVersion, 3, 3},
		"refusal without a broker": {validOrderRefused(), event.OrderRefusedSchemaVersion, 1, 3},
		"IRA refusal":              {validIRARefused(), event.OrderRefusedSchemaVersion, 2, 3},
		"sell-exceeds-holding":     {sellRefused, event.OrderRefusedSchemaVersion, 2, 3},
		"refusal with a broker":    {brokerRefused, event.OrderRefusedSafetyVersion, 3, 3},
		"kill-switch refusal":      {killRefused, event.OrderRefusedSafetyVersion, 3, 3},
	} {
		if got := c.p.SchemaVersion(); got != c.want {
			t.Errorf("%s: version %d, want %d", name, got, c.want)
		}
		for v := uint32(1); v <= c.latest; v++ {
			err := c.p.ValidateSchema(v)
			if v >= c.oldest && err != nil {
				t.Errorf("%s at version %d: %v", name, v, err)
			}
			if v < c.oldest && err == nil {
				t.Errorf("%s: version %d accepted a field of version %d", name, v, c.oldest)
			}
		}
		if err := c.p.ValidateSchema(c.latest + 1); err == nil {
			t.Errorf("%s: an unknown version accepted", name)
		}
		if err := c.p.ValidateSchema(0); err == nil {
			t.Errorf("%s: version 0 accepted", name)
		}
	}
	if event.OrderReleasedSchemaVersion != 2 || event.OrderReleasedSafetyVersion != 3 ||
		event.OrderRefusedSchemaVersion != 2 || event.OrderRefusedSafetyVersion != 3 ||
		event.OrderPlannedSchemaVersion != 1 || event.OrderPlannedSafetyVersion != 2 {
		t.Error("the released and refused versions are 2 and 3, the planned versions 1 and 2")
	}
	for name, p := range map[string]versioned{
		"planned":  event.OrderPlannedPayload{},
		"released": event.OrderReleasedPayload{},
		"refused":  event.OrderRefusedPayload{},
	} {
		if err := p.ValidateSchema(2); err == nil {
			t.Errorf("an invalid %s payload accepted at version 2", name)
		}
	}
}

// TestOnlyAMarginAccountHasACushion: the Excess Liquidity thresholds are
// required for Boost's margin account and absent for an IRA, which has no
// margin cushion to watch (ADR 0026, point 6; plan.md section 5).
func TestOnlyAMarginAccountHasACushion(t *testing.T) {
	type P = event.TargetWeightConfigurationPayload
	boost := validTargetWeightConfiguration()
	expectInvalid(t, "boost without thresholds", boost, func(p *P) {
		p.Limits.ExcessLiquidityWarning, p.Limits.ExcessLiquidityUrgent, p.Limits.ExcessLiquidityStaleMinutes = 0, 0, 0
	}, "excess liquidity")
	ira := validMomentumConfiguration()
	expectInvalid(t, "IRA with thresholds", ira, func(p *P) {
		p.Limits.ExcessLiquidityWarning, p.Limits.ExcessLiquidityUrgent, p.Limits.ExcessLiquidityStaleMinutes = 0.4, 0.2, 30
	}, "no margin cushion")
	expectInvalid(t, "IRA with stale minutes only", ira, func(p *P) { p.Limits.ExcessLiquidityStaleMinutes = 30 }, "no margin cushion")
	if err := ira.Limits.Validate(); err != nil {
		t.Errorf("an IRA's limits without thresholds: %v", err)
	}
	half := ira.Limits
	half.ExcessLiquidityUrgent = 0.2
	if err := half.Validate(); err == nil {
		t.Error("limits with some thresholds and not others accepted")
	}
	if err := boost.Limits.Validate(); err != nil {
		t.Errorf("a margin account's limits: %v", err)
	}
}

// An input that does not state whether the switch is engaged must never
// release it: ADR 0026 line 173 refuses every order but the sale while it is
// engaged, so only an explicit false releases it.
func TestKillSwitchRequiresAnExplicitEngaged(t *testing.T) {
	base := `"account_id":"joint","as_of":"2026-01-02T21:00:00Z","operator":"owner","detail":"switch file removed"`
	for name, raw := range map[string]string{
		"omitted":  `{` + base + `}`,
		"null":     `{` + base + `,"engaged":null}`,
		"a string": `{` + base + `,"engaged":"false"}`,
	} {
		var p event.KillSwitchPayload
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			t.Errorf("%s engaged decoded as %+v; want an error", name, p)
		}
	}
	for _, engaged := range []bool{false, true} {
		var p event.KillSwitchPayload
		raw := fmt.Sprintf(`{%s,"engaged":%t}`, base, engaged)
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Engaged != engaged {
			t.Errorf("engaged %t: %+v, %v", engaged, p, err)
		}
	}
	if _, err := json.Marshal(validKillSwitch()); err != nil {
		t.Fatal(err)
	}
}
