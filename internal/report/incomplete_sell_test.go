package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func iraSell(clientID string) event.OrderReleasedPayload {
	o := releasedOrder(clientID, event.TradingModePaper)
	o.AccountID, o.InstrumentID, o.Side, o.Quantity = iraInstance, "AAA", event.OrderSideSell, 10
	return o
}

func (r *logRun) sellFill(n int, o event.OrderReleasedPayload, id string, qty int64) {
	at := day(n).Add(-time.Hour)
	r.add(journal.KindInput, event.FillEventType, event.FillTargetWeightVersion, "fill:"+id, at,
		event.TargetWeightFillPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, FillID: id, InstrumentID: o.InstrumentID,
			Side: o.Side, Quantity: qty, Price: 100, FilledAt: at})
}

// TestASellStillIncompleteOnTheFirstSessionPagesOncePerSession: the IRA buys
// only after that rebalance's sells have filled (ADR 0026, point 2, "IRA
// execution"), so one sell that has not completed by the first session after
// its release leaves the account in cash for the month. The page names the
// instrument and the shares outstanding, comes once per session while the
// sell is incomplete, and a completed sell pages nothing.
func TestASellStillIncompleteOnTheFirstSessionPagesOncePerSession(t *testing.T) {
	run := &logRun{t: t}
	sell, done := iraSell("tw-sell-a"), iraSell("tw-sell-b")
	run.session(0)
	run.release(0, sell)
	run.release(0, done)
	run.sellFill(1, done, "f-done", 10)
	run.sellFill(1, sell, "f-part", 4)

	alerts := report.IncompleteSellAlerts(iraInstance, run.out, event.TargetWeightStrategyMomentum)
	if len(alerts) != 0 {
		t.Fatalf("before any session has closed after the release: %+v", alerts)
	}
	run.session(1)
	alerts = report.IncompleteSellAlerts(iraInstance, run.out, event.TargetWeightStrategyMomentum)
	if len(alerts) != 1 {
		t.Fatalf("first session after the release: %d pages, want one for the incomplete sell only: %+v", len(alerts), alerts)
	}
	a := alerts[0]
	if a.Kind != report.KindSellIncomplete || a.Severity != report.Urgent || a.AccountID != iraInstance {
		t.Errorf("page = %+v", a)
	}
	text := a.Title + "\n" + a.Body
	for _, want := range []string{"AAA", "6 shares", "tw-sell-a", "by hand", "ti-adapter@" + iraInstance} {
		if !strings.Contains(text, want) {
			t.Errorf("the page does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(strings.ToLower(text), "boost") {
		t.Errorf("the page names Boost:\n%s", text)
	}

	run.session(2)
	later := report.IncompleteSellAlerts(iraInstance, run.out, event.TargetWeightStrategyMomentum)
	if len(later) != 2 || later[0].Key != a.Key || later[1].Key == a.Key {
		t.Fatalf("a second session: %+v, want the first session's page again (same key) and one page of its own", later)
	}

	run.sellFill(3, sell, "f-rest", 6)
	run.session(3)
	if got := report.IncompleteSellAlerts(iraInstance, run.out, event.TargetWeightStrategyMomentum); len(got) != 2 {
		t.Errorf("once completed no more pages: %d, want the two already raised", len(got))
	}
}

func TestACompletedOrRetiredSellAndABoostRunPageNothing(t *testing.T) {
	run := &logRun{t: t}
	sell := iraSell("tw-sell-a")
	run.session(0)
	run.release(0, sell)
	run.sellFill(1, sell, "f-all", 10)
	run.session(1)
	if got := report.IncompleteSellAlerts(iraInstance, run.out, event.TargetWeightStrategyMomentum); len(got) != 0 {
		t.Errorf("a completed sell paged: %+v", got)
	}
	open := &logRun{t: t}
	open.session(0)
	open.release(0, iraSell("tw-sell-c"))
	open.session(1)
	if got := report.IncompleteSellAlerts("joint", open.out, event.TargetWeightStrategyBoost); len(got) != 0 {
		t.Errorf("a Boost run paged an incomplete sell: %+v", got)
	}
}

// TestASellTheEngineReadsAsEndedPagesNothing: a sell ends by the venue's
// cancellation, a dry-run run's synthetic report, its retirement as never
// sent, or the owner's clearance (event.OrderReports), and then no session
// pages it; a record that does not read, and a buy, change nothing.
func TestASellTheEngineReadsAsEndedPagesNothing(t *testing.T) {
	at := day(0).Add(time.Hour)
	ends := map[string]func(r *logRun, o event.OrderReleasedPayload){
		"canceled": func(r *logRun, o event.OrderReleasedPayload) {
			r.add(journal.KindInput, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "end", at,
				event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "7", InstrumentID: o.InstrumentID,
					Side: o.Side, Status: event.OrderLifecycleStatusCanceled, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at})
		},
		"synthetic": func(r *logRun, o event.OrderReleasedPayload) {
			r.add(journal.KindDecision, event.OrderLifecycleEventType, event.OrderLifecycleSyntheticVersion, "end", at,
				event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID,
					Side: o.Side, Status: event.OrderLifecycleStatusCanceled, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at,
					Message: "dry run: never sent", Synthetic: true})
		},
		"retired": func(r *logRun, o event.OrderReleasedPayload) {
			r.add(journal.KindInput, event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "end", at,
				event.OrderRetiredPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side,
					Quantity: o.Quantity, LimitPrice: o.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "session over",
					RetiredBy: event.OrderRetiredByAdapter, RetiredAt: at})
		},
		"cleared": func(r *logRun, o event.OrderReleasedPayload) {
			r.add(journal.KindInput, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "end", at,
				event.RebalanceClearedPayload{AccountID: o.AccountID, AsOf: at, DecisionID: "targets", ClientOrderIDs: []string{o.ClientOrderID},
					Operator: "owner", Detail: "sold by hand"})
		},
	}
	for name, end := range ends {
		run := &logRun{t: t}
		sell := iraSell("tw-sell-a")
		run.session(0)
		run.release(0, sell)
		run.add(journal.KindDecision, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "buy", day(0), func() event.OrderReleasedPayload {
			b := releasedOrder("tw-buy", event.TradingModePaper)
			b.AccountID = iraInstance
			return b
		}())
		end(run, sell)
		run.add(journal.KindInput, event.SessionClosedEventType, 99, "unreadable", day(1), map[string]string{})
		run.session(2)
		got := report.IncompleteSellAlerts(iraInstance, run.out, event.TargetWeightStrategyMomentum)
		if len(got) != 0 {
			t.Errorf("%s: an ended sell paged: %+v", name, got)
		}
	}
}

// TestAFailedIRAFlexReaderIsWordedAsRunAgainByItsTimer: the Flex reader's page
// says its timer runs it again and gives its manual row.
func TestAFailedIRAFlexReaderIsWordedAsRunAgainByItsTimer(t *testing.T) {
	at := day(0)
	page := report.UnitFailedAlert("lightsail", "ti-flex@trad-ira.service", "0123456789abcdef0123456789abcdef", "refused", at)
	if !strings.Contains(page.Body, "Flex reader is run again by its timer") || strings.Contains(page.Body, "does not restart") ||
		!strings.Contains(page.Body, "docs/manual/reconciliation.md") {
		t.Errorf("the Flex reader's page:\n%s", page.Body)
	}
}
