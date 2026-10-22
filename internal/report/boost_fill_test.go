package report_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// TestABoostFillAcknowledgesAnOrderByTheGatesRule: the Boost engine reads a
// fill by the risk gate's check (risklimit.Gate.ObserveFill), which accepts
// a fill the order's release describes whatever the lifecycle reports said
// before it, so the reporter counts a Boost order whose only report is such
// a fill as acknowledged and does not page it as unacknowledged. A fill the
// gate refuses acknowledges nothing, and on an order already acknowledged,
// where no unacknowledged page would follow, it is a venue report the engine
// did not read, as in a momentum run (ADR 0022; ADR 0019, "Alerting"). A
// fill that cannot be read at all is such a report whatever order it names,
// as in a momentum run.
func TestABoostFillAcknowledgesAnOrderByTheGatesRule(t *testing.T) {
	order := releasedOrder("tw-1", event.TradingModePaper)
	at := day(0).Add(time.Minute)
	fill := func(id string, quantity int64, mutate func(*event.TargetWeightFillPayload)) event.TargetWeightFillPayload {
		p := event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: id, InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: quantity, Price: 100, FilledAt: at}
		if mutate != nil {
			mutate(&p)
		}
		return p
	}
	cancel := event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: "tw-1", OrderID: "7", InstrumentID: "SPY", Side: event.OrderSideBuy,
		Status: event.OrderLifecycleStatusCanceled, Quantity: order.Quantity, LimitPrice: order.LimitPrice, OccurredAt: at}
	submitted := cancel
	submitted.Status = event.OrderLifecycleStatusSubmitted
	cases := map[string]struct {
		add          func(*logRun)
		acknowledged bool
		by           string
		unread       int
	}{
		"a fill of the whole order": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at, fill("f1", 9, nil))
		}, true, "fill", 0},
		"a part fill": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at, fill("f1", 4, nil))
		}, true, "fill", 0},
		"a fill after a cancellation, which the gate accepts": {func(r *logRun) {
			r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "cancel", at, cancel)
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:late", at.Add(time.Minute), fill("f1", 4, nil))
		}, true, "canceled", 0},
		"the same execution twice": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at, fill("f1", 4, nil))
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:again", at.Add(time.Minute), fill("f1", 4, nil))
		}, true, "fill", 0},
		"a fill past the order's quantity": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:big", at, fill("f1", 10, nil))
		}, false, "", 0},
		"a fill of the wrong side": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:side", at, fill("f1", 4, func(p *event.TargetWeightFillPayload) { p.Side = event.OrderSideSell }))
		}, false, "", 0},
		"a fill for another account": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:account", at, fill("f1", 4, func(p *event.TargetWeightFillPayload) { p.AccountID = "ira" }))
		}, false, "", 0},
		"a fill that does not validate": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:zero", at, fill("f1", 0, nil))
		}, false, "", 1},
		"a fill at a schema version this build does not read, on an acknowledged order": {func(r *logRun) {
			r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "submitted", at, submitted)
			r.add(journal.KindInput, event.FillEventType, 99, "fill:future", at.Add(time.Minute), fill("f1", 4, nil))
		}, true, "submitted", 1},
		"a fill id reused for another execution": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at, fill("f1", 9, nil))
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:reused", at, fill("f1", 1, nil))
		}, true, "fill", 1},
		"a fill past the order's quantity after a lifecycle report acknowledged it": {func(r *logRun) {
			r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "submitted", at, submitted)
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:big", at.Add(time.Minute), fill("f1", 10, nil))
		}, true, "submitted", 1},
		"a fill of the wrong side after a fill acknowledged the order": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at, fill("f1", 4, nil))
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:side", at.Add(time.Minute), fill("f2", 1, func(p *event.TargetWeightFillPayload) { p.Side = event.OrderSideSell }))
		}, true, "fill", 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			run := &logRun{t: t}
			run.session(0)
			run.release(0, order)
			c.add(run)
			orders, unread, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
			if err != nil {
				t.Fatal(err)
			}
			if len(unread) != c.unread {
				t.Errorf("unread %+v, want %d: a fill that cannot be read, or one the gate refuses on an order already acknowledged, is a report the engine did not read", unread, c.unread)
			}
			got := stateOf(t, orders, "tw-1")
			if (got.State == report.LifecycleAcknowledged) != c.acknowledged || got.Acknowledgement != c.by {
				t.Errorf("state %s by %q, want acknowledged=%v by %q", got.State, got.Acknowledgement, c.acknowledged, c.by)
			}
			alerts := report.UnacknowledgedAlerts(orders, at.Add(time.Hour), 10*time.Minute, func(string) bool { return false })
			if c.acknowledged && len(alerts) != 0 {
				t.Errorf("an acknowledged order pages: %+v", alerts)
			}
			if !c.acknowledged && len(alerts) != 1 {
				t.Errorf("an order whose only report the gate refuses must still page as unacknowledged: %+v", alerts)
			}
		})
	}
}

// TestAnUnreadableBoostFillPagesByItsInputID: a Boost fill at a schema
// version this build does not read names no client order ID that can be
// trusted, so it pages as a venue report the engine did not read, keyed by
// its input's ID, even when the order it was for is already acknowledged
// and nothing else would page (ADR 0019, "Alerting"; ADR 0015).
func TestAnUnreadableBoostFillPagesByItsInputID(t *testing.T) {
	run := &logRun{t: t}
	run.session(0)
	run.release(0, releasedOrder("tw-1", event.TradingModePaper))
	at := day(0).Add(time.Minute)
	run.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "submitted", at,
		event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: "tw-1", OrderID: "7", InstrumentID: "SPY", Side: event.OrderSideBuy,
			Status: event.OrderLifecycleStatusSubmitted, Quantity: 9, LimitPrice: 101, OccurredAt: at})
	run.add(journal.KindInput, event.FillEventType, 99, "fill:future", at.Add(time.Minute), map[string]string{"shape": "unknown"})
	_, unread, err := report.OrderLifecycles(run.out, event.TargetWeightStrategyBoost)
	if err != nil {
		t.Fatal(err)
	}
	alerts := report.UnreadReportAlerts("joint", unread)
	if len(alerts) != 1 || alerts[0].Key != "venue-report-unread:fill:future@joint" || alerts[0].Severity != report.Urgent {
		t.Fatalf("an unreadable Boost fill raised %+v, want one urgent page keyed by its input ID", alerts)
	}
}
