package risklimit

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// Observation is what one broker snapshot changed: the margin alarm's new
// level when it changed, to be journalled under AlarmID, and Urgent when the
// alarm has just turned urgent, which fires the sale back to 98%.
type Observation struct {
	Alarm   *event.MarginAlarmPayload
	AlarmID string
	Urgent  bool
}

// brokerHolding returns the latest broker snapshot's holding of instrumentID
// and its working sells, with every sell released since that snapshot, when
// the snapshot read both positions and open orders.
func (g *Gate) brokerHolding(instrumentID string) (holding, working int64, ok bool) {
	s := g.broker
	if s == nil || !s.Readable(event.BrokerComponentPositions) || !s.Readable(event.BrokerComponentOpenOrders) {
		return 0, 0, false
	}
	position, _ := s.Position(instrumentID)
	return position.Quantity, s.WorkingSells(instrumentID) + g.inFlight[instrumentID], true
}

// PendingSells returns the shares of instrumentID already being sold: every
// working or pending sell the latest broker snapshot reports, when it read
// its open orders, and every sell released since that snapshot. A planner
// nets them from a new sell, so a sell and one already working never
// together take exposure below the target (ADR 0019, amended 2026-10-15,
// "never below it"; ADR 0026, point 2.5).
func (g *Gate) PendingSells(instrumentID string) int64 {
	working := g.inFlight[instrumentID]
	if s := g.broker; s != nil && s.Readable(event.BrokerComponentOpenOrders) {
		working += s.WorkingSells(instrumentID)
	}
	return working
}

// riskReducing reports whether p is risk-reducing (ADR 0026, point 4): a sell
// within the latest broker snapshot's holding net of every working or pending
// sell, or, with no readable broker snapshot, within the account snapshot's
// holding it was sized from. A long holding's sell always moves exposure
// toward 98% or lower. No buy is risk-reducing, even within a rebalance that
// also sells.
func (g *Gate) riskReducing(p event.OrderPlannedPayload) bool {
	if p.Side != event.OrderSideSell {
		return false
	}
	if holding, working, ok := g.brokerHolding(p.InstrumentID); ok {
		return holding > 0 && p.Quantity <= holding-working
	}
	return p.Quantity <= p.HeldQuantity
}

// checkSale decides a sale back to 98% (ADR 0019, amended 2026-10-15 and
// 2026-10-16; ADR 0026, point 4). It goes in every state and through the kill
// switch, exempt from count, size and band, but stays subject to the
// journalled-decision check (the gate must have issued it) and the no-short
// sizing against the latest broker snapshot. A sale already checked once is
// never released again.
func (g *Gate) checkSale(o Order, f event.RiskFigures) Outcome {
	p := o.Planned
	switch {
	case !g.saleTags[p.Tag]:
		out := Outcome{Refused: g.refusal(o, f, event.RefusedUndecidedOrder, fmt.Sprintf("sale tag %q names no incident, kill-switch engagement or urgent alarm in this run", p.Tag))}
		if g.state != StateHalted {
			g.halt(&out, fmt.Sprintf("sale %s refused: %s; a defect in the order path, so its scope cannot be bounded", p.ClientOrderID, event.RefusedUndecidedOrder), p.ClientOrderID)
		}
		return out
	case !g.pending[p.ClientOrderID]:
		return Outcome{Refused: g.refusal(o, f, event.RefusedSaleAttempted, fmt.Sprintf("sale %s was already attempted for %s and is never retried automatically", p.ClientOrderID, p.InstrumentID))}
	}
	delete(g.pending, p.ClientOrderID)
	if !f.BrokerSet || p.Quantity > f.BrokerHolding-f.WorkingSells {
		return Outcome{Refused: g.refusal(o, f, event.RefusedBrokerHolding, fmt.Sprintf("sale of %d exceeds the latest broker holding %d net of %d working sells (snapshot %s)", p.Quantity, f.BrokerHolding, f.WorkingSells, g.brokerID))}
	}
	return g.release(o, f)
}

// SetKillSwitch applies a recorded operator.kill-switch input with envelope
// ID id. An engagement while already engaged is the same engagement, so the
// sale it allows is still attempted once. It changes no engine state (ADR
// 0026, point 3; owner decision 2026-10-16).
func (g *Gate) SetKillSwitch(id string, p event.KillSwitchPayload) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("risklimit: %w", err)
	}
	if id == "" {
		return errors.New("risklimit: a kill switch input needs its envelope ID")
	}
	if p.AccountID != g.accountID {
		return fmt.Errorf("risklimit: kill switch for account %q; this gate is %q", p.AccountID, g.accountID)
	}
	switch {
	case p.Engaged && !g.killEngaged:
		g.killEngaged, g.killID = true, id
		g.saleTags[id] = true
	case !p.Engaged:
		g.killEngaged, g.killID = false, ""
	}
	return nil
}

// ObserveBroker applies a recorded account.broker.snapshot with envelope ID
// id: it becomes the latest broker observation, and for a margin account the
// margin-cushion alarm is evaluated (plan.md section 5): Excess Liquidity
// over net liquidation below the warning threshold warns, below the urgent
// threshold is urgent, and a snapshot without that reading, while the market
// is open, is urgent once the last reading is the stale period old or there
// has been none. Without a reading and short of that, the level stands. An
// alarm is returned only when its level changes.
func (g *Gate) ObserveBroker(id string, s event.BrokerSnapshotPayload) (Observation, error) {
	if err := s.Validate(); err != nil {
		return Observation{}, fmt.Errorf("risklimit: %w", err)
	}
	if id == "" {
		return Observation{}, errors.New("risklimit: a broker snapshot needs its envelope ID")
	}
	if s.AccountID != g.accountID {
		return Observation{}, fmt.Errorf("risklimit: broker snapshot for account %q; this gate is %q", s.AccountID, g.accountID)
	}
	if g.broker != nil && !s.AsOf.After(g.broker.AsOf) {
		return Observation{}, fmt.Errorf("risklimit: broker snapshot as of %s does not follow the last, %s", s.AsOf, g.broker.AsOf)
	}
	// A snapshot recorded after a release shows that sell as working or
	// in its holding, so the sells in flight before it are counted by it.
	g.broker, g.brokerID = &s, id
	clear(g.inFlight)
	if g.ira {
		return Observation{}, nil
	}
	alarm := event.MarginAlarmPayload{
		AccountID: g.accountID, SnapshotID: id, AsOf: s.AsOf,
		Warning: g.limits.ExcessLiquidityWarning, Urgent: g.limits.ExcessLiquidityUrgent, StaleMinutes: g.limits.ExcessLiquidityStaleMinutes,
	}
	stale := time.Duration(g.limits.ExcessLiquidityStaleMinutes) * time.Minute
	switch {
	case s.Readable(event.BrokerComponentExcessLiquidity) && s.Readable(event.BrokerComponentNetLiquidation):
		g.lastReading, g.lastReadingSet = s.AsOf, true
		alarm.Cause, alarm.ReadingSet = event.MarginAlarmCauseRatio, true
		alarm.ExcessLiquidity, alarm.NetLiquidation = s.ExcessLiquidity, s.NetLiquidation
		alarm.Ratio = s.ExcessLiquidity / s.NetLiquidation
		switch {
		case alarm.Ratio < g.limits.ExcessLiquidityUrgent:
			alarm.Level = event.MarginAlarmUrgent
		case alarm.Ratio < g.limits.ExcessLiquidityWarning:
			alarm.Level = event.MarginAlarmWarning
		default:
			alarm.Level = event.MarginAlarmClear
		}
		alarm.Detail = fmt.Sprintf("excess liquidity %v is %v of net liquidation %v (warning below %v, urgent below %v)",
			s.ExcessLiquidity, alarm.Ratio, s.NetLiquidation, alarm.Warning, alarm.Urgent)
	case s.MarketOpen && (!g.lastReadingSet || s.AsOf.Sub(g.lastReading) >= stale):
		alarm.Cause, alarm.Level = event.MarginAlarmCauseNoReading, event.MarginAlarmUrgent
		alarm.Detail = fmt.Sprintf("no excess liquidity reading for %d minutes while the market is open", g.limits.ExcessLiquidityStaleMinutes)
		if !g.lastReadingSet {
			alarm.Detail = "no excess liquidity reading since the run began, while the market is open"
		}
	default:
		return Observation{}, nil
	}
	alarm.LastReadingAt, alarm.LastReadingSet = g.lastReading, g.lastReadingSet
	if alarm.Level == g.alarmLevel {
		return Observation{}, nil
	}
	alarmID := "margin-alarm:" + g.accountID + ":" + id
	g.alarmLevel = alarm.Level
	obs := Observation{Alarm: &alarm, AlarmID: alarmID}
	if alarm.Level == event.MarginAlarmUrgent {
		g.alarmID = alarmID
		g.saleTags[alarmID] = true
		obs.Urgent = true
	}
	return obs, nil
}

// Sale decides whether a sale back to target (the unlevered weight, 98%) of
// net liquidation goes for instrumentID now that trigger has fired, and plans
// it from the latest broker snapshot (orderplan.PlanSale). It reports false
// when no sale applies:
//
//   - an IRA never gets one (ADR 0019, amended 2026-10-16; ADR 0026, point 4);
//   - Halted, or Degraded with instrumentID named: ADR 0019's one sale,
//     tagged with the incident (amended 2026-10-15 and 2026-10-16);
//   - otherwise, with the kill switch engaged: the same sale, tagged with the
//     engagement, so leverage is never kept because the switch is on
//     (plan.md section 5, "Kill switch");
//   - otherwise, on the margin alarm turning urgent: ADR 0026 point 4's
//     urgent Excess Liquidity sale, tagged with the alarm;
//   - otherwise the exit is an ordinary order and no sale applies.
//
// Each sale is attempted once for its tag and instrument: a sale planned, or
// withheld because the broker's values could not be read or a working sell is
// unknown, is never planned again, so a sale placed by hand after the alert
// is never duplicated. A sale withheld with nothing to sell is not an
// attempt. The planned sale must then pass Check.
func (g *Gate) Sale(trigger, instrumentID string, target, band float64) (orderplan.Sale, bool, error) {
	if trigger != event.SaleTriggerExit && trigger != event.SaleTriggerMarginUrgent {
		return orderplan.Sale{}, false, fmt.Errorf("risklimit: sale trigger %q is not exit or margin-urgent", trigger)
	}
	if g.ira {
		return orderplan.Sale{}, false, nil
	}
	var tag, kind string
	switch {
	case g.state == StateHalted || g.state == StateDegraded && slices.Contains(g.scope, instrumentID):
		tag, kind = g.incident, event.SaleKindIncident
	case g.killEngaged:
		tag, kind = g.killID, event.SaleKindIncident
	case trigger == event.SaleTriggerMarginUrgent && g.alarmLevel == event.MarginAlarmUrgent:
		tag, kind = g.alarmID, event.SaleKindUrgent
	default:
		return orderplan.Sale{}, false, nil
	}
	if g.saleAttempted(tag, instrumentID) {
		return orderplan.Sale{}, false, nil
	}
	sale, err := orderplan.PlanSale(orderplan.SaleInput{
		AccountID: g.accountID, Tag: tag, InstrumentID: instrumentID, Kind: kind, Trigger: trigger,
		Target: target, PriceBand: band, SnapshotID: g.brokerID, Snapshot: g.broker, Recognised: g.released,
	})
	if err != nil {
		return orderplan.Sale{}, false, fmt.Errorf("risklimit: %w", err)
	}
	switch {
	case sale.Order != nil:
		g.attempted[sale.Order.ClientOrderID] = true
		g.pending[sale.Order.ClientOrderID] = true
	case sale.Withheld.Reason != event.SaleWithheldNothingToSell:
		g.attempted[sale.Withheld.ClientOrderID] = true
	}
	return sale, true, nil
}

// saleAttempted reports whether the sale for tag and instrumentID was already
// attempted: planned, or withheld for a person (ADR 0019, amended 2026-10-15:
// the sale is attempted once and never retried automatically). It knows this
// run's attempts only. A fresh run after a restart learns an incident's
// earlier attempt from the owner's run approval (ADR 0027's restart model);
// that per-incident ledger is consulted here, and its attempted client order
// ID marked attempted but not pending, once the approval reaches the gate.
func (g *Gate) saleAttempted(tag, instrumentID string) bool {
	return g.attempted[orderplan.SaleClientOrderID(tag, instrumentID)]
}
