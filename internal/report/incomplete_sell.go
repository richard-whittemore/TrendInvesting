package report

import (
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// KindSellIncomplete is a momentum rebalance's sell that has not completed
// by a session after its release. The IRA buys only after that rebalance's
// sells have filled (ADR 0026, point 2, "IRA execution"), so one sell that
// never completes (a halted or delisted stock) leaves the account in cash for
// the month; the owner is told on the first session it is still incomplete
// and again at each session after.
const KindSellIncomplete Kind = "sell-incomplete"

// IncompleteSellAlerts returns one urgent page for each released sell of a
// momentum run that is not terminal when a session closes after the session
// of its release: once per session while it stays incomplete, keyed by the
// order and the session. Orders are followed by the rule the engine applies
// to the venue's reports (event.OrderReports), so a report it refused
// completes nothing. A dry-run release ends by its synthetic report, and a
// run of any other strategy has no such sells, so neither pages. entries are
// the log's committed records in recording order; account is the one its
// configuration names, whose engine instance the page's units name.
func IncompleteSellAlerts(account string, entries []journal.Entry, strategyID string) []Alert {
	if !isMomentum(strategyID) {
		return nil
	}
	instance := instanceOf(strategyID, account)
	reports := event.NewOrderReports()
	released := map[string]time.Time{} // sell client order ID -> the session closed when it was released
	var order []string
	var lastSession time.Time
	var out []Alert
	for _, entry := range entries {
		e := entry.Envelope
		switch e.Type {
		case event.SessionClosedEventType:
			var p event.SessionClosedPayload
			if read(e, sessionVersions, &p) != nil {
				continue
			}
			for _, id := range order {
				o, _ := reports.Order(id)
				if o != nil && !o.Terminal && p.PeriodEnd.After(released[id]) {
					out = append(out, incompleteSellAlert(instance, e, o, p.PeriodEnd))
				}
			}
			lastSession = p.PeriodEnd
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
			if read(e, lifecycleVersions, &p) == nil || read(e, syntheticVersions, &p) == nil {
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
	return out
}

func incompleteSellAlert(instance string, at event.Envelope, o *event.ReportedOrder, session time.Time) Alert {
	r := o.Released
	left := r.Quantity - o.Filled
	day := session.UTC().Format(time.DateOnly)
	return Alert{
		Key: fmt.Sprintf("sell-incomplete:%s:%s@%s", r.ClientOrderID, day, r.AccountID), Kind: KindSellIncomplete, Severity: Urgent,
		AccountID: r.AccountID, At: at.EventTime,
		Title: fmt.Sprintf("%s: sell of %s incomplete after the session ending %s (%d shares outstanding)", r.AccountID, r.InstrumentID, day, left),
		Body: fmt.Sprintf("Order %s, the rebalance's sell of %s, has not completed: %d of its %d shares are still unsold. The IRA buys only after every sell of the rebalance has filled, so until this one completes or is cleared the account stays in cash for the month. If the stock is halted or delisted, sell the %d shares by hand at IBKR; the program does not sell them again. To do so safely, stop the adapter, then the engine (sudo systemctl stop %s, then sudo systemctl stop ti-engine@%s), sell, then start a fresh run approved with the post-sale book, which plans the rebalance again from the book as it is. This page comes once per session while the sell is incomplete.",
			r.ClientOrderID, r.InstrumentID, left, r.Quantity, left, adapterUnitOf(instance), instance),
	}
}
