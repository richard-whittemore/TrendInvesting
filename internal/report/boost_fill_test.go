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
// gate refuses acknowledges nothing (ADR 0022; ADR 0019, "Alerting").
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
	cases := map[string]struct {
		add          func(*logRun)
		acknowledged bool
		by           string
	}{
		"a fill of the whole order": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at, fill("f1", 9, nil))
		}, true, "fill"},
		"a part fill": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at, fill("f1", 4, nil))
		}, true, "fill"},
		"a fill after a cancellation, which the gate accepts": {func(r *logRun) {
			r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "cancel", at, cancel)
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:late", at.Add(time.Minute), fill("f1", 4, nil))
		}, true, "canceled"},
		"the same execution twice": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at, fill("f1", 4, nil))
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:again", at.Add(time.Minute), fill("f1", 4, nil))
		}, true, "fill"},
		"a fill past the order's quantity": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:big", at, fill("f1", 10, nil))
		}, false, ""},
		"a fill of the wrong side": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:side", at, fill("f1", 4, func(p *event.TargetWeightFillPayload) { p.Side = event.OrderSideSell }))
		}, false, ""},
		"a fill for another account": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:account", at, fill("f1", 4, func(p *event.TargetWeightFillPayload) { p.AccountID = "ira" }))
		}, false, ""},
		"a fill that does not validate": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:zero", at, fill("f1", 0, nil))
		}, false, ""},
		"a fill id reused for another execution": {func(r *logRun) {
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:1", at, fill("f1", 9, nil))
			r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:reused", at, fill("f1", 1, nil))
		}, true, "fill"},
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
			if len(unread) != 0 {
				t.Errorf("unread %+v: a Boost fill raises no unread-report alert", unread)
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
