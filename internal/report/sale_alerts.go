package report

import (
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// marginAlarmAlert states one change of the margin-cushion alarm's level
// (plan.md section 5, "A margin-cushion alarm"; ADR 0026, point 6): a warning
// the owner checks the same day, an urgent level on which the program sells
// back to 98% once, and the clear. The engine records the alarm only when its
// level changes, so each record is one alert, keyed by the record.
func marginAlarmAlert(account string, e event.Envelope, p event.MarginAlarmPayload) Alert {
	severity := Routine
	var reading string
	switch p.Cause {
	case event.MarginAlarmCauseNoReading:
		last := "none in this run"
		if p.LastReadingSet {
			last = p.LastReadingAt.UTC().Format(time.RFC3339)
		}
		reading = fmt.Sprintf("no Excess Liquidity reading for %d minutes while the market is open (last reading: %s)", p.StaleMinutes, last)
	case event.MarginAlarmCauseNonPositive:
		reading = fmt.Sprintf("a net liquidation of %.2f, zero or less", p.NetLiquidation)
	default:
		reading = fmt.Sprintf("Excess Liquidity %.2f of net liquidation %.2f, %.2f%%", p.ExcessLiquidity, p.NetLiquidation, p.Ratio*100)
	}
	thresholds := fmt.Sprintf("warning below %.0f%%, urgent below %.0f%%", p.Warning*100, p.Urgent*100)
	action := "Check the account at IBKR the same day: docs/runbooks/margin-warning.md, \"A warning\"."
	switch p.Level {
	case event.MarginAlarmUrgent:
		severity = Urgent
		action = "The program sells back to 98% of net liquidation once for this alarm; watch for the sale's alerts, and act by hand if it is withheld: docs/runbooks/margin-warning.md, \"An urgent alarm\"."
	case event.MarginAlarmClear:
		action = "The alarm has cleared; nothing to do."
	}
	return Alert{
		Key: "margin-alarm:" + e.ID + "@" + account, Kind: KindMarginAlarm, Severity: severity, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: margin cushion %s", account, p.Level),
		Body: fmt.Sprintf("The margin-cushion alarm is %s since %s: %s (%s; broker snapshot %s). %s %s",
			p.Level, p.AsOf.UTC().Format(time.RFC3339), reading, thresholds, p.SnapshotID, p.Detail, action),
	}
}

// sale is one sale back to 98% the log records: its plan, its release once
// the gate passed it, and the executions read for it.
type sale struct {
	planned  event.OrderPlannedPayload
	released *event.OrderReleasedPayload
	filled   int64
	fills    map[string]bool
	failed   bool
}

// sales follows the log's sales back to 98% by client order ID, for the
// steps plan.md decision D6 tells the owner of.
type sales map[string]*sale

// plan records a planned order that carries sale figures; any other order
// is not a sale and is ignored.
func (s sales) plan(p event.OrderPlannedPayload) {
	if p.Sale == (event.SaleFigures{}) {
		return
	}
	if _, ok := s[p.ClientOrderID]; !ok {
		s[p.ClientOrderID] = &sale{planned: p, fills: map[string]bool{}}
	}
}

// release returns the alert that a planned sale was released, once: sent
// in paper or live, journalled and never sent in dry-run (ADR 0026, point 3,
// "Mode"). The log is written before the send, so a release is "send
// requested", never known to be sent (ADR 0027, section 4).
func (s sales) release(account string, e event.Envelope, p event.OrderReleasedPayload) (Alert, bool) {
	sl, ok := s[p.ClientOrderID]
	if !ok || sl.released != nil {
		return Alert{}, false
	}
	sl.released = &p
	order := fmt.Sprintf("sell %d %s, limit %.2f, client order ID %s", p.Quantity, p.InstrumentID, p.LimitPrice, p.ClientOrderID)
	why := fmt.Sprintf("%s sale fired by %s", sl.planned.Sale.Kind, sl.planned.Sale.Trigger)
	a := Alert{
		Key: "sale-sent:" + p.ClientOrderID + "@" + account, Kind: KindSaleSent, Severity: Routine, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: sale back to 98%% sent (%s)", account, p.InstrumentID),
		Body: fmt.Sprintf("The sale back to 98%% of net liquidation (%s) was released to be sent at %s: %s, sized from broker snapshot %s. The log is written before the send, so check IBKR for an order whose orderRef is %s; you are told when it fills or IBKR ends it.",
			why, e.EventTime.UTC().Format(time.RFC3339), order, sl.planned.Sale.SnapshotID, p.ClientOrderID),
	}
	if !p.Send {
		a.Title = fmt.Sprintf("%s: sale back to 98%% released, not sent (%s)", account, p.Mode)
		a.Body = fmt.Sprintf("The sale back to 98%% of net liquidation (%s) was released at %s in %s mode, which sends nothing: %s.",
			why, e.EventTime.UTC().Format(time.RFC3339), p.Mode, order)
	}
	return a, true
}

// fill returns the alert for one execution of a released sale (execution.fill
// version 5), once per fill ID: a redelivered fill is the same execution.
func (s sales) fill(account string, e event.Envelope, p event.TargetWeightFillPayload) (Alert, bool) {
	sl, ok := s[p.ClientOrderID]
	if !ok || sl.released == nil || p.AccountID != account || sl.fills[p.FillID] {
		return Alert{}, false
	}
	sl.fills[p.FillID] = true
	sl.filled += p.Quantity
	want := sl.released.Quantity
	state := fmt.Sprintf("%d still to fill", want-sl.filled)
	if sl.filled >= want {
		state = "the sale is complete"
	}
	return Alert{
		Key: "sale-filled:" + e.ID + "@" + account, Kind: KindSaleFilled, Severity: Routine, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: sale back to 98%% filled %d of %d (%s)", account, sl.filled, want, p.InstrumentID),
		Body: fmt.Sprintf("Order %s sold %d %s at %.2f at %s (fill %s): %d of %d filled, %s.",
			p.ClientOrderID, p.Quantity, p.InstrumentID, p.Price, p.FilledAt.UTC().Format(time.RFC3339), p.FillID, sl.filled, want, state),
	}, true
}

// lifecycle returns the alert that IBKR ended a released sale before it
// filled in full: canceled or invalid, by a venue's report (version 2) that
// describes the order as released, which is the report the engine reads. The
// sale is not retried (ADR 0019, amended 2026-10-15: once per incident), so
// the owner sells what is left by hand.
func (s sales) lifecycle(account string, e event.Envelope, p event.LimitOrderLifecyclePayload) (Alert, bool) {
	sl, ok := s[p.ClientOrderID]
	if !ok || sl.released == nil || sl.failed || !p.Terminal() || !p.Describes(*sl.released) || sl.filled >= sl.released.Quantity {
		return Alert{}, false
	}
	sl.failed = true
	want := sl.released.Quantity
	return Alert{
		Key: "sale-failed:" + p.ClientOrderID + "@" + account, Kind: KindSaleFailed, Severity: Urgent, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: sale back to 98%% failed (%s, %s)", account, p.Status, p.InstrumentID),
		Body: fmt.Sprintf("IBKR reported order %s %s at %s (%s): %d of %d filled. The program will not try the sale again; sell the remaining %d by hand at IBKR if exposure is still above 98%% of net liquidation, after checking no other sell is working (docs/runbooks/margin-warning.md).",
			p.ClientOrderID, p.Status, p.OccurredAt.UTC().Format(time.RFC3339), p.Message, sl.filled, want, want-sl.filled),
	}, true
}

// refused returns the page that the gate refused a planned sale as
// broker-holding, once: a sale refused at the gate spends its one attempt
// (ADR 0019, amended 2026-10-15), so the program never makes it again in that
// incident, engagement or alarm and the owner sells by hand, following the
// hand-sale steps. A refusal of an ordinary order, or of a sale for another
// reason, is the refusal alert alone.
func (s sales) refused(account string, e event.Envelope, p event.OrderRefusedPayload) (Alert, bool) {
	sl, ok := s[p.ClientOrderID]
	if !ok || sl.failed || p.Reason != event.RefusedBrokerHolding {
		return Alert{}, false
	}
	sl.failed = true
	return Alert{
		Key: "sale-refused:" + p.ClientOrderID + "@" + account, Kind: KindSaleByHand, Severity: Urgent, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: sale back to 98%% refused, sell by hand (%s)", account, p.InstrumentID),
		Body: fmt.Sprintf("The risk gate refused the %s sale back to 98%% of net liquidation (%s, order %s, sell %d %s) as %s: it is larger than IBKR's holding net of working sells. %s. The refusal spends this incident's one attempt, so the program will not try again: if the account is still above 98%% of net liquidation, sell back to 98%% by hand (docs/runbooks/margin-warning.md). %s",
			sl.planned.Sale.Kind, sl.planned.Sale.Trigger, p.ClientOrderID, p.Quantity, p.InstrumentID, p.Reason, p.Detail, handSaleSteps(account)),
	}, true
}

// byHandAlert states a sale back to 98% withheld for a reason that counts as
// the attempt (event.SaleWithheldPayload.Attempted): the program never tries
// it again automatically, so the owner sells by hand (ADR 0019, amended
// 2026-10-15, "No order when it cannot be checked"). A sale withheld because
// IBKR's net liquidation or mark is not strictly positive is not an attempt,
// but nothing sells until a positive snapshot comes, so it pages too, once
// per sale (its tag and instrument): the hand-sale steps stop the adapter,
// then the engine, first, so the sale decided again cannot go out beside the
// owner's. Any other reason decides again, has nothing to sell, or lapsed,
// and raises no alert here: a lapse goes in the daily summary.
func byHandAlert(account string, e event.Envelope, p event.SaleWithheldPayload) (Alert, bool) {
	if p.Reason == event.SaleWithheldNotPositive {
		return Alert{
			Key: "sale-by-hand:" + p.Reason + ":" + p.Tag + ":" + p.InstrumentID + "@" + account, Kind: KindSaleByHand, Severity: Urgent, AccountID: account, At: e.EventTime,
			Title: fmt.Sprintf("%s: sale of %s withheld, sell by hand (%s)", account, p.InstrumentID, p.Reason),
			Body: fmt.Sprintf("The %s sale back to 98%% of %s (%s, order %s) was not placed: IBKR reported a net liquidation or mark of zero or less, so the shares to keep cannot be worked out. %s. It is not an attempt: the sale is decided again from the next positive IBKR snapshot while what fired it is in force, and until one comes nothing sells. Check the account at IBKR; if you must sell by hand now: %s",
				p.Kind, p.InstrumentID, p.Trigger, p.ClientOrderID, p.Detail, handSaleSteps(account)),
		}, true
	}
	if !p.Attempted() {
		return Alert{}, false
	}
	check := "IBKR's values could not be read"
	if p.Reason == event.SaleWithheldUnknownWorkingSell {
		check = "CONTAINMENT REQUIRED: a sell is working at IBKR that this program did not release; cancel or settle it first"
	}
	return Alert{
		Key: "sale-by-hand:" + e.ID + "@" + account, Kind: KindSaleByHand, Severity: Urgent, AccountID: account, At: e.EventTime,
		Title: fmt.Sprintf("%s: sale of %s withheld, sell by hand (%s)", account, p.InstrumentID, p.Reason),
		Body: fmt.Sprintf("The %s sale back to 98%% of %s (%s, order %s) was not placed: %s. %s. It counts as this incident's one attempt, so the program will not try again: act by hand at IBKR now and sell back to 98%% of net liquidation (docs/runbooks/margin-warning.md). %s",
			p.Kind, p.InstrumentID, p.Trigger, p.ClientOrderID, check, p.Detail, handSaleSteps(account)),
	}, true
}
