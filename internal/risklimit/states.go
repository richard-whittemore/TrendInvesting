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
	// RetryExit is set when a sale Boost's exit fired, withheld from a
	// snapshot that could not size it, is decided again from this one.
	RetryExit bool
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
// id: it resolves what it shows of each released sell (resolve), becomes the
// latest broker observation, and for a margin account the
// margin-cushion alarm is evaluated (plan.md section 5; ADR 0026, point 6):
// Excess Liquidity over net liquidation below the warning threshold warns,
// below the urgent threshold is urgent, and a snapshot without that reading,
// while the market is open, is urgent once there has been no reading for the
// stale period (a net liquidation that is not positive gives no ratio, so no
// reading): since the last reading, or, with none yet, since the run's
// first broker snapshot, which starts the clock. Without a reading and short
// of that, the level stands. An alarm is returned only when its level
// changes.
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
	g.resolve(s)
	if g.broker == nil {
		g.firstBroker = s.AsOf
	}
	g.broker, g.brokerID = &s, id
	if g.ira {
		return Observation{}, nil
	}
	retryExit := g.exitRetry != "" && g.refusesExit(g.exitRetry) && sizable(s, g.exitRetry)
	if g.exitRetry != "" && !g.refusesExit(g.exitRetry) {
		g.exitRetry = ""
	}
	alarm := event.MarginAlarmPayload{
		AccountID: g.accountID, SnapshotID: id, AsOf: s.AsOf,
		Warning: g.limits.ExcessLiquidityWarning, Urgent: g.limits.ExcessLiquidityUrgent, StaleMinutes: g.limits.ExcessLiquidityStaleMinutes,
	}
	stale := time.Duration(g.limits.ExcessLiquidityStaleMinutes) * time.Minute
	switch {
	case s.Readable(event.BrokerComponentExcessLiquidity) && s.Readable(event.BrokerComponentNetLiquidation) && s.NetLiquidation > 0:
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
	case s.MarketOpen && s.AsOf.Sub(g.noReadingSince()) >= stale:
		alarm.Cause, alarm.Level = event.MarginAlarmCauseNoReading, event.MarginAlarmUrgent
		alarm.Detail = fmt.Sprintf("no excess liquidity reading for %d minutes while the market is open", g.limits.ExcessLiquidityStaleMinutes)
		if !g.lastReadingSet {
			alarm.Detail = fmt.Sprintf("no excess liquidity reading in the %d minutes since the run's first broker snapshot, while the market is open", g.limits.ExcessLiquidityStaleMinutes)
		}
	default:
		return Observation{RetryExit: retryExit}, nil
	}
	alarm.LastReadingAt, alarm.LastReadingSet = g.lastReading, g.lastReadingSet
	if alarm.Level == g.alarmLevel {
		// Still urgent, with the alarm's sale withheld from a snapshot that
		// could not size it: the producing condition is still in force, so
		// a readable, positive snapshot decides the sale again.
		urgent := alarm.Level == event.MarginAlarmUrgent && g.urgentRetry && g.urgentInstrument != "" && sizable(s, g.urgentInstrument)
		return Observation{Urgent: urgent, RetryExit: retryExit && !urgent}, nil
	}
	g.urgentRetry = false
	alarmID := "margin-alarm:" + g.accountID + ":" + id
	g.alarmLevel = alarm.Level
	obs := Observation{Alarm: &alarm, AlarmID: alarmID, RetryExit: retryExit}
	if alarm.Level == event.MarginAlarmUrgent {
		g.alarmID = alarmID
		g.saleTags[alarmID] = true
		obs.Urgent, obs.RetryExit = true, false
	}
	return obs, nil
}

// refusesExit reports whether the account still refuses Boost's exit on
// instrumentID, the condition that fires the sale back to 98%: Halted,
// Degraded on it, or the kill switch engaged.
func (g *Gate) refusesExit(instrumentID string) bool {
	return g.state == StateHalted || g.state == StateDegraded && slices.Contains(g.scope, instrumentID) || g.killEngaged
}

// sizable reports whether broker snapshot s can size a sale of
// instrumentID: readable, with a net liquidation and a mark that are
// strictly positive. It is fresh by construction, being the latest.
func sizable(s event.BrokerSnapshotPayload, instrumentID string) bool {
	for _, c := range []string{event.BrokerComponentNetLiquidation, event.BrokerComponentOpenOrders, event.BrokerComponentPositions} {
		if !s.Readable(c) {
			return false
		}
	}
	position, held := s.Position(instrumentID)
	return s.NetLiquidation > 0 && (!held || position.MarkSet && position.Mark > 0)
}

// LapseExitSale ends the retry of a sale Boost's exit fired: the account's
// next strategy.targets.decided no longer plans the sell that fired it. The
// attempt stays unspent.
func (g *Gate) LapseExitSale() { g.exitRetry = "" }

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
// The sale is sized from the latest broker snapshot only, and only while it
// is no older at the trigger time at than the configured maximum age (ADR
// 0019: "a stale matching snapshot is not success").
//
// Each sale is attempted once for its tag and instrument: a sale planned, or
// withheld because the latest snapshot is unreadable or a working sell is
// unknown, is never planned again, so a sale placed by hand after the alert
// is never duplicated. A sale withheld with nothing to sell, from a stale
// snapshot, or from a net liquidation or mark that is not strictly positive
// is not an attempt: it changes no state, and the next readable, positive
// broker snapshot decides it again (ObserveBroker's Urgent or RetryExit)
// while what fired it is in force: the alarm still urgent, or the exit
// still refused and still planned (LapseExitSale); otherwise it lapses. The
// tag is picked afresh each time, and an older snapshot is never used in
// place of the latest (event.SaleWithheldPayload.Attempted). The planned
// sale must then pass Check.
func (g *Gate) Sale(trigger, instrumentID string, at time.Time, target, band float64) (orderplan.Sale, bool, error) {
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
		At: at, MaxSnapshotAge: g.maxSnapshotAge,
	})
	if err != nil {
		return orderplan.Sale{}, false, fmt.Errorf("risklimit: %w", err)
	}
	switch {
	case sale.Order != nil:
		g.attempted[sale.Order.ClientOrderID] = true
		g.pending[sale.Order.ClientOrderID] = true
	case sale.Withheld.Attempted():
		g.attempted[sale.Withheld.ClientOrderID] = true
	}
	// A sale withheld from a snapshot that could not size it is decided
	// again on the next readable, positive snapshot while what fired it is
	// in force: the alarm still urgent, or the exit still refused.
	retry := sale.Withheld != nil &&
		(sale.Withheld.Reason == event.SaleWithheldStaleBrokerSnapshot || sale.Withheld.Reason == event.SaleWithheldNotPositive)
	if trigger == event.SaleTriggerMarginUrgent {
		g.urgentRetry, g.urgentInstrument = retry, instrumentID
	} else if g.exitRetry = ""; retry {
		g.exitRetry = instrumentID
	}
	return sale, true, nil
}

// saleAttempted reports whether the sale for tag and instrumentID was already
// attempted: planned, or withheld for a person (ADR 0019, amended 2026-10-15:
// the sale is attempted once and never retried automatically), in this run
// or, restored by RestoreSaleAttempt, in an earlier run of the incident.
func (g *Gate) saleAttempted(tag, instrumentID string) bool {
	return g.attempted[orderplan.SaleClientOrderID(tag, instrumentID)]
}

// RestoreSaleAttempt records that an earlier run already attempted the
// restored incident's sale of instrumentID under tag (ADR 0026, point 5:
// "Whether the incident sale has been attempted"), so this run never
// attempts it again. It is an unwired seam: the operator run-approval input,
// which carries the earlier run's planned or withheld sale, wires it when it
// lands. Until then nothing calls it, since a crash resumes by replaying the
// durable log's inputs, never from recorded decisions (ADR 0027, section 6).
// It needs the incident restored first (RestoreState), and tag must be that
// incident.
func (g *Gate) RestoreSaleAttempt(tag, instrumentID string) error {
	switch {
	case g.incident == "":
		return errors.New("risklimit: a restored sale attempt needs its incident restored first")
	case tag != g.incident:
		return fmt.Errorf("risklimit: a restored sale attempt under %q names no restored incident; the incident is %q", tag, g.incident)
	case instrumentID == "":
		return errors.New("risklimit: a restored sale attempt needs its instrument")
	}
	g.attempted[orderplan.SaleClientOrderID(tag, instrumentID)] = true
	return nil
}

// noReadingSince is when the "no reading" clock started: the last Excess
// Liquidity reading, or, with none yet, the run's first broker snapshot.
func (g *Gate) noReadingSince() time.Time {
	if g.lastReadingSet {
		return g.lastReading
	}
	return g.firstBroker
}
