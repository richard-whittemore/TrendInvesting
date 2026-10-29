package report

import (
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// KindSellIncomplete pages an unfinished momentum sell once per session.
// Sells finish first, then buys fit settled cash; a cancellation can leave
// unsold holdings (docs/manual/momentum.md, "What can go wrong"; ADR 0026).
const KindSellIncomplete Kind = "sell-incomplete"

// IncompleteSellAlerts returns one urgent page for each released sell of a
// momentum run that is not terminal when a session closes after the session
// of its release: once per session while it stays incomplete, keyed by the
// order and the session. Orders are followed by the rule the engine applies
// to the venue's reports (event.OrderReports), so a report it refused
// completes nothing. A dry-run release ends by its synthetic report, and a
// run of any other strategy has no such sells, so neither pages. entries are
// the log's committed records in recording order. The caller supplies the
// deployment instance, never the journal's broker account ID; empty or invalid means
// unknown and omits unit commands (ADR 0019, "Alerting";
// docs/live-trading/lessons-2026-10.md L8).
func IncompleteSellAlerts(instance string, entries []journal.Entry, strategyID string) []Alert {
	if !isMomentum(strategyID) {
		return nil
	}
	if !deploymentInstancePattern.MatchString(instance) {
		instance = ""
	}
	reports := event.NewOrderReports()
	released := map[string]time.Time{} // sell client order ID -> the session closed when it was released
	var order []string
	var lastSession time.Time
	var sessionEvent event.Envelope
	var out []Alert
	for _, entry := range entries {
		e := entry.Envelope
		switch e.Type {
		case event.SessionClosedEventType:
			var p event.SessionClosedPayload
			if read(e, sessionVersions, &p) != nil {
				continue
			}
			if p.PeriodEnd.After(lastSession) {
				lastSession, sessionEvent = p.PeriodEnd, e
			}
		case event.OrderReleasedEventType:
			var p event.OrderReleasedPayload
			if read(e, releasedVersions, &p) != nil || p.Side != event.OrderSideSell {
				continue
			}
			if _, seen := released[p.ClientOrderID]; !seen {
				reports.Release(p)
				released[p.ClientOrderID] = lastSession
				order = append(order, p.ClientOrderID)
			}
		case event.OrderLifecycleEventType:
			var p event.LimitOrderLifecyclePayload
			if entry.Kind == journal.KindInput && read(e, lifecycleVersions, &p) == nil ||
				entry.Kind == journal.KindDecision && read(e, syntheticVersions, &p) == nil {
				_, _ = reports.Lifecycle(p)
			}
		case event.FillEventType:
			var p event.TargetWeightFillPayload
			if read(e, targetWeightFillVersions, &p) == nil {
				_, _, _ = reports.Fill(p)
			}
		case event.OrderRetiredEventType:
			var p event.OrderRetiredPayload
			if read(e, retiredVersions, &p) == nil {
				_, _ = reports.Retire(p)
			}
		case event.RebalanceClearedEventType:
			applyClearance(e, reports)
		}
	}
	// Only the latest session's standing pages are actionable, with the final
	// accepted quantities (CONTEXT.md, "Order lifecycle report").
	for _, id := range order {
		o, _ := reports.Order(id)
		if o != nil && !o.Terminal && lastSession.After(released[id]) {
			out = append(out, incompleteSellAlert(instance, sessionEvent, o, lastSession))
		}
	}
	return out
}

func incompleteSellAlert(instance string, at event.Envelope, o *event.ReportedOrder, session time.Time) Alert {
	r := o.Released
	left := r.Quantity - o.Filled
	day := session.UTC().Format(time.DateOnly)
	label := instance
	stop := unknownInstance + "; follow that reporter's instructions to stop the account's adapter, then its engine, and confirm the stops succeeded"
	if instance == "" {
		label = "instance unknown"
	} else {
		stop = fmt.Sprintf("stop the account's adapter (sudo systemctl stop %s), then its engine (sudo systemctl stop ti-engine@%s)", adapterUnitOf(instance), instance)
	}
	return Alert{
		Key: fmt.Sprintf("sell-incomplete:%s:%s@%s", r.ClientOrderID, day, r.AccountID), Kind: KindSellIncomplete, Severity: Urgent,
		AccountID: r.AccountID, At: at.EventTime, Standing: true,
		Title: fmt.Sprintf("%s: sell of %s incomplete after the session ending %s (%d shares outstanding)", label, r.InstrumentID, day, left),
		Body: fmt.Sprintf("Order %s, the rebalance's sell of %s, has not completed: the current journal shows %d of its %d shares outstanding. Sells must finish by fill or another accepted terminal event before buys are budgeted from settled cash; unsold holdings may remain (docs/manual/momentum.md). To act safely: (1) %s; (2) in IBKR cancel the program's working order for this instrument and confirm no open order exists for the account from any client, including cancel-pending orders, then check the actual position; (3) only then sell the outstanding shares by hand; (4) record the hand sale as docs/runbooks/missed-rebalance.md says: only after the manual order has filled or been canceled, start a fresh run approved with the post-sale book, then use operator rebalance-cleared to clear the carried unfinished rebalance. A fresh run carries the unfinished rebalance and withholds every month until the owner clears it (docs/manual/momentum.md; docs/runbooks/missed-rebalance.md, 'A restart mid-rebalance'); it does not plan this rebalance again. This page comes once per session while the sell is incomplete.",
			r.ClientOrderID, r.InstrumentID, left, r.Quantity, stop),
	}
}
