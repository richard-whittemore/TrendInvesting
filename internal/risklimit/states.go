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
	// Lapsed holds a risk.sale.withheld with reason lapsed for each sale,
	// withheld from a snapshot that could not size it, whose trigger this
	// snapshot ended (Lapse).
	Lapsed []event.SaleWithheldPayload
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
//
// The owner's instructions count in the order of the times they state, not
// the order they arrive in, since the operator may backdate one: a kill
// switch stating a time earlier than the one last applied is refused,
// naming it, so a stale instruction never undoes a later one (ADR 0019,
// amendment of 2026-10-17, "operator instructions count in the order of the
// times they state"). One stating the same time is applied, the later
// recorded winning.
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
	if g.lastKillSet && p.AsOf.Before(g.lastKillAt) {
		return fmt.Errorf("risklimit: kill switch %s states %s, earlier than %s, already applied for account %s at %s; the owner's instructions are ordered by the time they state, so this one would undo a later one: record it again with the current time if it is still meant",
			id, p.AsOf.UTC().Format(time.RFC3339), g.lastKill, p.AccountID, g.lastKillAt.UTC().Format(time.RFC3339))
	}
	g.lastKill, g.lastKillAt, g.lastKillSet = id, p.AsOf, true
	switch {
	case p.Engaged && !g.killEngaged:
		g.killEngaged, g.killID, g.killAlerted = true, id, false
		g.saleTags[id] = true
	case !p.Engaged:
		g.killEngaged, g.killID = false, ""
	}
	return nil
}

// ObserveBroker applies a recorded account.broker.snapshot with envelope ID
// id: it is recorded for the pending rule (recordBroker), becomes the
// latest broker observation, and ends the account's stale episode, being
// fresh by its own time (ADR 0019, amendment of 2026-10-17, reading 10). For
// a margin account the margin-cushion alarm is evaluated (plan.md section 5;
// ADR 0026, point 6): Excess Liquidity over net liquidation below the warning
// threshold warns, below the urgent threshold is urgent, and a readable net
// liquidation of zero or less, below every threshold though it gives no
// ratio, is urgent at once. A snapshot without that reading, while the
// market is open, is urgent once there has been no reading for the stale
// period: since the last reading, or, with none yet, since the run's first
// broker snapshot, which starts the clock. Without a reading and short of
// that, the level stands, and a sale the urgent alarm fired, withheld from a
// snapshot that could not size it, is decided again from one that can. An
// alarm is returned only when its level changes.
// A sale withheld from a snapshot that could not size it lapses when what
// fired it is no longer in force: the account no longer refuses Boost's
// exit, or the alarm is no longer urgent.
func (g *Gate) ObserveBroker(id string, s event.BrokerSnapshotPayload) (Observation, error) {
	if err := g.checkBroker(id, s); err != nil {
		return Observation{}, err
	}
	g.recordBroker(s)
	if g.broker == nil {
		g.firstBroker = s.AsOf
	}
	g.broker, g.brokerID = &s, id
	g.staleOpen = false
	if g.ira {
		return Observation{}, nil
	}
	var lapsed []event.SaleWithheldPayload
	retryExit := g.retriesExit(s)
	if g.exitRetry != "" && !g.refusesExit(g.exitRetry) {
		if l := g.lapseExit(fmt.Sprintf("broker snapshot %s finds the account no longer refusing the exit on %s", id, g.exitRetry)); l != nil {
			lapsed = append(lapsed, *l)
		}
	}
	alarm, decided := g.alarmFor(id, s)
	if alarm.ReadingSet {
		g.lastReading, g.lastReadingSet = s.AsOf, true
	}
	changed := decided && alarm.Level != g.alarmLevel
	urgent, exit := g.fires(s, alarm.Level, changed, retryExit)
	if !changed {
		return Observation{Urgent: urgent, RetryExit: exit, Lapsed: lapsed}, nil
	}
	if g.urgentRetry {
		// The level changed, so the alarm that fired the withheld sale is no
		// longer urgent: a new urgent alarm is a new sale.
		w := g.urgentWithheld
		g.urgentRetry, g.urgentWithheld = false, nil
		if alarm.Level != event.MarginAlarmUrgent && w != nil && !armed(g.exitWithheld, w.ClientOrderID) {
			lapsed = append(lapsed, lapse(*w, fmt.Sprintf("broker snapshot %s finds the margin alarm %s, no longer urgent", id, alarm.Level)))
		}
	}
	alarmID := "margin-alarm:" + g.accountID + ":" + id
	g.alarmLevel = alarm.Level
	if alarm.Level == event.MarginAlarmUrgent {
		g.alarmID = alarmID
		g.saleTags[alarmID] = true
	}
	return Observation{Alarm: &alarm, AlarmID: alarmID, Urgent: urgent, RetryExit: exit, Lapsed: lapsed}, nil
}

// BrokerFires reports which sale back to 98% ObserveBroker would fire from
// broker snapshot s with envelope ID id, by the same rules, changing
// nothing: urgent for the urgent alarm's, retryExit for Boost's exit's
// withheld one, and ObserveBroker's error for a snapshot it would refuse. A
// caller that must refuse such a sale reads it before ObserveBroker, so the
// refusal leaves the alarm and every retry as they were (fail closed,
// AGENTS.md).
func (g *Gate) BrokerFires(id string, s event.BrokerSnapshotPayload) (urgent, retryExit bool, err error) {
	if err := g.checkBroker(id, s); err != nil {
		return false, false, err
	}
	if g.ira {
		return false, false, nil
	}
	alarm, decided := g.alarmFor(id, s)
	urgent, retryExit = g.fires(s, alarm.Level, decided && alarm.Level != g.alarmLevel, g.retriesExit(s))
	return urgent, retryExit, nil
}

// checkBroker is every refusal of broker snapshot s with envelope ID id
// that ObserveBroker makes before it changes anything.
func (g *Gate) checkBroker(id string, s event.BrokerSnapshotPayload) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("risklimit: %w", err)
	}
	if id == "" {
		return errors.New("risklimit: a broker snapshot needs its envelope ID")
	}
	if s.AccountID != g.accountID {
		return fmt.Errorf("risklimit: broker snapshot for account %q; this gate is %q", s.AccountID, g.accountID)
	}
	if g.broker != nil && !s.AsOf.After(g.broker.AsOf) {
		return fmt.Errorf("risklimit: broker snapshot as of %s does not follow the last, %s", s.AsOf, g.broker.AsOf)
	}
	return g.ledger.refuseSharedVenue(id, s.OpenOrders)
}

// retriesExit reports whether snapshot s decides again a sale Boost's exit
// fired, withheld from a snapshot that could not size it: the account still
// refuses the exit and s can size the sale.
func (g *Gate) retriesExit(s event.BrokerSnapshotPayload) bool {
	return g.exitRetry != "" && g.refusesExit(g.exitRetry) && sizable(s, g.exitRetry)
}

// alarmFor is the margin-cushion alarm broker snapshot s with envelope ID id
// gives (ObserveBroker), and decided false when it gives no level: no
// reading, short of the stale period, so the level stands. It changes
// nothing; the run's first broker snapshot starts the stale clock at its own
// time, as ObserveBroker records it.
func (g *Gate) alarmFor(id string, s event.BrokerSnapshotPayload) (alarm event.MarginAlarmPayload, decided bool) {
	alarm = event.MarginAlarmPayload{
		AccountID: g.accountID, SnapshotID: id, AsOf: s.AsOf,
		Warning: g.limits.ExcessLiquidityWarning, Urgent: g.limits.ExcessLiquidityUrgent, StaleMinutes: g.limits.ExcessLiquidityStaleMinutes,
	}
	since := g.noReadingSince()
	if g.broker == nil {
		since = s.AsOf
	}
	stale := time.Duration(g.limits.ExcessLiquidityStaleMinutes) * time.Minute
	// One price basis per rule (ADR 0019, amended 2026-10-18): Excess
	// Liquidity over IBKR's own net liquidation at the same read.
	nl := s.AlarmNetLiquidation()
	switch {
	case s.Readable(event.BrokerComponentExcessLiquidity) && s.Readable(event.BrokerComponentNetLiquidation) && !(nl > 0):
		// A reading, and the most alarming: no ratio, but below every
		// threshold, so urgent at once rather than awaited as no reading.
		alarm.Cause, alarm.ReadingSet, alarm.Level = event.MarginAlarmCauseNonPositive, true, event.MarginAlarmUrgent
		alarm.ExcessLiquidity, alarm.NetLiquidation = s.ExcessLiquidity, nl
		alarm.Detail = fmt.Sprintf("net liquidation %v is not positive (excess liquidity %v): below every threshold", nl, s.ExcessLiquidity)
	case s.Readable(event.BrokerComponentExcessLiquidity) && s.Readable(event.BrokerComponentNetLiquidation):
		alarm.Cause, alarm.ReadingSet = event.MarginAlarmCauseRatio, true
		alarm.ExcessLiquidity, alarm.NetLiquidation = s.ExcessLiquidity, nl
		alarm.Ratio = s.ExcessLiquidity / nl
		switch {
		case alarm.Ratio < g.limits.ExcessLiquidityUrgent:
			alarm.Level = event.MarginAlarmUrgent
		case alarm.Ratio < g.limits.ExcessLiquidityWarning:
			alarm.Level = event.MarginAlarmWarning
		default:
			alarm.Level = event.MarginAlarmClear
		}
		alarm.Detail = fmt.Sprintf("excess liquidity %v is %v of net liquidation %v (warning below %v, urgent below %v)",
			s.ExcessLiquidity, alarm.Ratio, nl, alarm.Warning, alarm.Urgent)
	case s.MarketOpen && s.AsOf.Sub(since) >= stale:
		alarm.Cause, alarm.Level = event.MarginAlarmCauseNoReading, event.MarginAlarmUrgent
		alarm.Detail = fmt.Sprintf("no excess liquidity reading for %d minutes while the market is open", g.limits.ExcessLiquidityStaleMinutes)
		if !g.lastReadingSet {
			alarm.Detail = fmt.Sprintf("no excess liquidity reading in the %d minutes since the run's first broker snapshot, while the market is open", g.limits.ExcessLiquidityStaleMinutes)
		}
	default:
		return alarm, false
	}
	alarm.LastReadingAt, alarm.LastReadingSet = g.lastReading, g.lastReadingSet
	if alarm.ReadingSet {
		alarm.LastReadingAt, alarm.LastReadingSet = s.AsOf, true
	}
	return alarm, true
}

// fires is which sale snapshot s fires: a level changed to urgent fires the
// urgent alarm's sale; while the level stands at urgent, with the alarm's
// sale withheld from a snapshot that could not size it, the producing
// condition is still in force, so a readable, positive snapshot decides the
// sale again (ADR 0019, amendment of 2026-10-17, reading 1), whether or not
// s carries a reading; Boost's exit's withheld sale is decided again
// (retryExit) unless the urgent one is.
func (g *Gate) fires(s event.BrokerSnapshotPayload, level string, changed, retryExit bool) (urgent, exit bool) {
	if changed {
		urgent = level == event.MarginAlarmUrgent
	} else {
		urgent = g.alarmLevel == event.MarginAlarmUrgent && g.urgentRetry && g.urgentInstrument != "" && sizable(s, g.urgentInstrument)
	}
	return urgent, retryExit && !urgent
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
// next strategy.targets.decided, decided, no longer plans the sell that
// fired it. It returns the risk.sale.withheld with reason lapsed to journal
// when such a sale was still withheld, else nil. The attempt stays unspent
// (ADR 0019, amendment of 2026-10-17, reading 1, "Lapse").
func (g *Gate) LapseExitSale() *event.SaleWithheldPayload {
	if g.exitRetry == "" {
		return nil
	}
	return g.lapseExit("the decision no longer plans the sell that fired the sale")
}

// lapseExit ends the exit's retry for why, returning its lapse, or nil while
// the urgent alarm still retries the same sale: "when neither holds and the
// sale is still withheld, the gate journals one" lapse (ADR 0019, amendment
// of 2026-10-17, reading 1, "Lapse").
func (g *Gate) lapseExit(why string) *event.SaleWithheldPayload {
	if g.exitWithheld == nil {
		// Armed by a retired sale (RetireSale), not by a withhold: nothing
		// was withheld, so nothing lapses.
		g.exitRetry = ""
		return nil
	}
	w := *g.exitWithheld
	g.exitRetry, g.exitWithheld = "", nil
	if armed(g.urgentWithheld, w.ClientOrderID) {
		return nil
	}
	l := lapse(w, why)
	return &l
}

// armed reports whether withheld, a trigger's withheld sale, is the sale
// client.
func armed(withheld *event.SaleWithheldPayload, client string) bool {
	return withheld != nil && withheld.ClientOrderID == client
}

// disarm ends every trigger's retry of the sale client, which has been
// decided: planned, or withheld for a person. While the account refuses the
// exit, the exit and the urgent alarm fire one sale under one tag (ADR 0019,
// amendment of 2026-10-17, reading 2), so a sale withheld under one and
// decided under the other is no longer withheld: it is neither retried nor
// lapsed (reading 1, "Lapse": only a sale "still withheld" lapses).
func (g *Gate) disarm(client string) {
	if armed(g.urgentWithheld, client) {
		g.urgentRetry, g.urgentWithheld = false, nil
	}
	if armed(g.exitWithheld, client) {
		g.exitRetry, g.exitWithheld = "", nil
	}
}

// lapse is withheld sale w's risk.sale.withheld with reason lapsed.
func lapse(w event.SaleWithheldPayload, why string) event.SaleWithheldPayload {
	w.Reason, w.Alert = event.SaleWithheldLapsed, false
	w.Detail = why + "; the sale is not decided again and its attempt stays unspent"
	return w
}

// StaleEpisodeOpen reports whether the account's stale episode is open: a
// stale-broker-snapshot refusal or withhold raised its one urgent alert, and
// no fresh broker snapshot or fresh gate check has ended it since.
func (g *Gate) StaleEpisodeOpen() bool { return g.staleOpen }

// RestoreStaleEpisode restores whether an earlier run left the account's
// stale episode open, which ADR 0026 point 5 lists among the state a restart
// recovers from the journal ("Whether a stale-snapshot episode is open for
// the account, from its opening `stale-broker-snapshot` record" and what
// closes it), so a restart within it does not alert again. A crash resume
// rebuilds the episode by replaying the durable log's inputs (ADR 0027,
// section 6), so it never calls this. A fresh run's approval does not
// restore it either: the approval is the run's input before any other, and
// a stale check needs a latest broker snapshot, whose ObserveBroker ends
// the episode, so an episode restored open would be ended before any check
// could read it. The fresh run's first stale check alerts.
func (g *Gate) RestoreStaleEpisode(open bool) { g.staleOpen = open }

// freshCheck applies a gate check of the latest broker snapshot at at, the
// time the check's trigger was journalled, to the account's stale episode: a
// readable snapshot fresh at at ends it. A replay applies inputs in journal
// sequence, so episodes are ordered by their triggering events' sequence.
func (g *Gate) freshCheck(at time.Time) {
	s := g.broker
	if s != nil && s.Readable(event.BrokerComponentPositions) && s.Readable(event.BrokerComponentOpenOrders) &&
		orderplan.StaleDetail(g.brokerID, s.AsOf, at, g.maxSnapshotAge) == "" {
		g.staleOpen = false
	}
}

// openStale records a stale-broker-snapshot refusal or withhold: it opens
// the account's stale episode when none is open, and reports whether this
// record carries the episode's one urgent alert.
func (g *Gate) openStale() bool {
	alert := !g.staleOpen
	g.staleOpen = true
	return alert
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
	tag, kind := g.saleTag(trigger, instrumentID)
	if tag == "" {
		return orderplan.Sale{}, false, nil
	}
	retired := g.restoredRetired(tag) + g.retiredSales[saleKey(tag, instrumentID)]
	if g.saleAttemptedFor(tag, instrumentID) {
		g.disarm(orderplan.SaleAttemptClientOrderID(tag, instrumentID, retired))
		return orderplan.Sale{}, false, nil
	}
	// Sized net of every sell the gate counts against the latest broker
	// snapshot, by the broker-holding check's own rule, so a released sell
	// that snapshot does not list yet is never sold again.
	_, working, _ := g.brokerHolding(instrumentID)
	sale, err := orderplan.PlanSale(orderplan.SaleInput{
		AccountID: g.accountID, Tag: tag, InstrumentID: instrumentID, Kind: kind, Trigger: trigger,
		Target: target, PriceBand: band, SnapshotID: g.brokerID, Snapshot: g.broker, Recognised: g.released,
		WorkingSells: working, At: at, MaxSnapshotAge: g.maxSnapshotAge, Retired: retired,
	})
	if err != nil {
		return orderplan.Sale{}, false, fmt.Errorf("risklimit: %w", err)
	}
	switch {
	case sale.Order != nil:
		g.recordAttempt(tag, instrumentID)
		g.pending[sale.Order.ClientOrderID] = true
	case sale.Withheld.Attempted():
		g.recordAttempt(tag, instrumentID)
	}
	// The decision is a gate check of the latest snapshot: fresh, it ends
	// the stale episode; a stale withhold opening one carries its alert.
	g.freshCheck(at)
	if sale.Withheld != nil && sale.Withheld.Reason == event.SaleWithheldStaleBrokerSnapshot {
		sale.Withheld.Alert = g.openStale()
	}
	// A sale withheld from a snapshot that could not size it is decided
	// again on the next readable, positive snapshot while what fired it is
	// in force: the alarm still urgent, or the exit still refused.
	retry := sale.Withheld != nil &&
		(sale.Withheld.Reason == event.SaleWithheldStaleBrokerSnapshot || sale.Withheld.Reason == event.SaleWithheldNotPositive)
	var withheld *event.SaleWithheldPayload
	if retry {
		w := *sale.Withheld
		withheld = &w
	}
	if trigger == event.SaleTriggerMarginUrgent {
		g.urgentRetry, g.urgentInstrument, g.urgentWithheld = retry, instrumentID, withheld
	} else if g.exitRetry, g.exitWithheld = "", nil; retry {
		g.exitRetry, g.exitWithheld = instrumentID, withheld
	}
	if sale.Order != nil {
		g.disarm(sale.Order.ClientOrderID)
	} else if sale.Withheld.Attempted() {
		g.disarm(sale.Withheld.ClientOrderID)
	}
	return sale, true, nil
}

// RetireSale reads the retirement of order clientOrderID as never sent, when
// it is a sale back to 98% this gate released, and reports whether it was
// one, with the tag it was planned under and the number of the attempt
// retired, the first being 1, which a decision records so a run restarted
// from it gives the next attempt an ID of its own (RestoreRetiredSale). Nothing was sent, so the attempt is not spent (ADR 0019, amended
// 2026-10-17, reading 4, as the owner's decision of 2026-10-22 reads it;
// ADR 0026, point 2, "Boost", amended 2026-10-22): the sale is attempted
// again under its own rule at the next opportunity, its tag and trigger
// unchanged, as reading 1 retries a sale withheld from a snapshot that could
// not size it: the next readable, fresh, positive broker snapshot, while
// what fired it is still in force (the exit still refused, or the alarm
// still urgent), or the trigger's next firing. The next attempt has a
// client order ID of its own (orderplan.SaleAttemptClientOrderID). The
// caller ends the order in the ledger, as a cancellation with nothing
// filled would, so its shares are no longer counted as being sold.
func (g *Gate) RetireSale(clientOrderID string) (tag string, attempt int, ok bool) {
	o := g.orders[clientOrderID]
	if o == nil || o.planned.Sale == (event.SaleFigures{}) {
		return "", 0, false
	}
	tag, instrumentID := o.planned.Tag, o.planned.InstrumentID
	if a := g.attempts[tag]; a != nil {
		delete(a.instruments, instrumentID)
	}
	g.retiredSales[saleKey(tag, instrumentID)]++
	attempt = g.restoredRetired(tag) + g.retiredSales[saleKey(tag, instrumentID)]
	switch o.planned.Sale.Trigger {
	case event.SaleTriggerMarginUrgent:
		// Armed by what fired it, whatever its tag: an urgent sale fired
		// while Halted, Degraded on the instrument or under the kill switch
		// carries the incident's or the engagement's tag, and is in force
		// while the alarm stays urgent all the same; Sale picks the tag
		// afresh (ADR 0019, amended 2026-10-17, reading 1).
		if g.alarmLevel == event.MarginAlarmUrgent {
			g.urgentRetry, g.urgentInstrument, g.urgentWithheld = true, instrumentID, nil
		}
	case event.SaleTriggerExit:
		if g.refusesExit(instrumentID) {
			g.exitRetry, g.exitWithheld = instrumentID, nil
		}
	}
	return tag, attempt, true
}

// SaleTag is the tag a sale back to 98% for instrumentID fired by trigger
// would carry now, as Sale picks it, or "" when Sale would plan none: the
// incident's while Halted or Degraded on the instrument, the kill switch
// engagement's while it is engaged, and the urgent alarm's for an urgent
// sale. A caller reads it to refuse a sale whose attempts it cannot yet
// state (ADR 0019, amended 2026-10-15: one attempt per incident).
func (g *Gate) SaleTag(trigger, instrumentID string) string {
	if g.ira {
		return ""
	}
	tag, _ := g.saleTag(trigger, instrumentID)
	return tag
}

// saleTag is the tag and kind Sale plans a sale under (SaleTag).
func (g *Gate) saleTag(trigger, instrumentID string) (tag, kind string) {
	switch {
	case g.state == StateHalted || g.state == StateDegraded && slices.Contains(g.scope, instrumentID):
		return g.incident, event.SaleKindIncident
	case g.killEngaged:
		return g.killID, event.SaleKindIncident
	case trigger == event.SaleTriggerMarginUrgent && g.alarmLevel == event.MarginAlarmUrgent:
		return g.alarmID, event.SaleKindUrgent
	}
	return "", ""
}

// saleKey keys a sale back to 98% by its tag and instrument.
func saleKey(tag, instrumentID string) string { return tag + "\x00" + instrumentID }

// saleAttemptedFor reports whether the sale for tag and instrumentID was
// already attempted: planned, or withheld for a person (ADR 0019, amended
// 2026-10-15: the sale is attempted once and never retried automatically),
// in this run, or, for an incident whose attempt the owner's approval of a
// fresh run restored (RestoreSaleAttempt), in an earlier run. It reads the
// tag's one attempt record.
func (g *Gate) saleAttemptedFor(tag, instrumentID string) bool {
	a := g.attempts[tag]
	return a != nil && (a.earlier || a.instruments[instrumentID])
}

// OwnLiveSale is the sale back to 98% under tag for instrumentID that this
// run attempted itself and released, while it is still live: no earlier
// run's attempt spent the sale, it has not ended and no fill of it is
// reported. A decision records it so a run restarted from the decision
// holds it as this run's own attempt, which a retirement as never sent
// frees (ADR 0019, amended 2026-10-17, reading 4; ADR 0026, point 5:
// recovered, never recomputed). ok is false otherwise.
func (g *Gate) OwnLiveSale(tag, instrumentID string) (event.OrderPlannedPayload, bool) {
	a := g.attempts[tag]
	if a == nil || a.earlier || !a.instruments[instrumentID] {
		return event.OrderPlannedPayload{}, false
	}
	id := orderplan.SaleAttemptClientOrderID(tag, instrumentID, g.restoredRetired(tag)+g.retiredSales[saleKey(tag, instrumentID)])
	o, s := g.orders[id], g.ledger.sells[id]
	if o == nil || o.planned.Sale == (event.SaleFigures{}) || s == nil || s.ended || o.reported != 0 || len(s.fills) != 0 {
		return event.OrderPlannedPayload{}, false
	}
	return o.planned, true
}

// RestoreOwnSale restores, from a recovered decision, the sale back to 98%
// that the run which made the decision attempted itself and released, still
// live (OwnLiveSale): o, fired by trigger, under the incident whose attempt
// RestoreSaleAttempt restored as not spent by an earlier run. It is held as
// this run's own attempt, released, so a retirement of it as never sent
// frees the attempt as in the full run (RetireSale), and it is never
// attempted again while it stands (ADR 0019, amended 2026-10-15 and
// 2026-10-17, reading 4; ADR 0026, point 5: recovered, never recomputed).
// Its client order ID must be the attempt's that follows every retired one
// (ADR 0026, point 2, "Order identity"); anything else is refused (fail
// closed, AGENTS.md).
func (g *Gate) RestoreOwnSale(o event.OrderReleasedPayload, trigger string) error {
	a := g.attempts[o.Tag]
	switch {
	case o.Tag == "" || o.Tag != g.restored || a == nil || a.earlier:
		return fmt.Errorf("risklimit: restored sale %s is under %q, not the incident whose attempt was restored as not spent by an earlier run", o.ClientOrderID, o.Tag)
	case a.instruments[o.InstrumentID]:
		return fmt.Errorf("risklimit: incident %q's sale of %s is attempted already; its order %s is restored once", o.Tag, o.InstrumentID, o.ClientOrderID)
	case o.Side != event.OrderSideSell:
		return fmt.Errorf("risklimit: restored sale %s is a %s; a sale back to 98%% is a sell", o.ClientOrderID, o.Side)
	case trigger != event.SaleTriggerExit && trigger != event.SaleTriggerMarginUrgent:
		return fmt.Errorf("risklimit: restored sale %s was fired by %q, not exit or margin-urgent", o.ClientOrderID, trigger)
	}
	if want := orderplan.SaleAttemptClientOrderID(o.Tag, o.InstrumentID, g.restoredRetired(o.Tag)+g.retiredSales[saleKey(o.Tag, o.InstrumentID)]); o.ClientOrderID != want {
		return fmt.Errorf("risklimit: restored sale %s is not the next attempt of incident %q's sale, whose client order ID is %s", o.ClientOrderID, o.Tag, want)
	}
	if err := g.RestoreReleased(o, nil, time.Time{}, ""); err != nil {
		return err
	}
	g.orders[o.ClientOrderID].planned.Sale = event.SaleFigures{Kind: event.SaleKindIncident, Trigger: trigger}
	g.recordAttempt(o.Tag, o.InstrumentID)
	return nil
}

// recordAttempt adds instrumentID's sale under tag to the tag's one attempt
// record, so it is never planned again (ADR 0019, amended 2026-10-15).
func (g *Gate) recordAttempt(tag, instrumentID string) {
	a := g.attempts[tag]
	if a == nil {
		a = &saleAttempt{instruments: make(map[string]bool)}
		g.attempts[tag] = a
	}
	a.instruments[instrumentID] = true
}

// noReadingSince is when the "no reading" clock started: the last Excess
// Liquidity reading, or, with none yet, the run's first broker snapshot.
func (g *Gate) noReadingSince() time.Time {
	if g.lastReadingSet {
		return g.lastReading
	}
	return g.firstBroker
}
