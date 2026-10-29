package report_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// marginAlarm records a change of the margin-cushion alarm's level, as the
// target-weight handler journals it (risk.margin.alarm).
func (s *stream) marginAlarm(n int, id, level string) {
	p := event.MarginAlarmPayload{AccountID: "joint", SnapshotID: "broker:" + id, AsOf: day(n), Level: level, Cause: event.MarginAlarmCauseRatio,
		ExcessLiquidity: 1200, NetLiquidation: 4000, Ratio: 0.3, ReadingSet: true, LastReadingAt: day(n), LastReadingSet: true,
		Warning: 0.4, Urgent: 0.2, StaleMinutes: 30, Detail: "excess liquidity 30% of net liquidation"}
	switch level {
	case event.MarginAlarmUrgent:
		p.ExcessLiquidity, p.Ratio = 400, 0.1
	case event.MarginAlarmClear:
		p.ExcessLiquidity, p.Ratio = 2000, 0.5
	}
	s.add(event.MarginAlarmEventType, event.MarginAlarmSchemaVersion, "alarm:"+id, day(n), p)
}

// salePlanned is a sale back to 98% of net liquidation: holding 60 SPY at
// 100 with 4000 net liquidation keeps ceil(0.98 x 4000 / 100) = 40 and
// sells 20 (event.SaleFigures).
func (s *stream) salePlanned(n int, clientID string) {
	s.add(event.OrderPlannedEventType, event.OrderPlannedSafetyVersion, "plan:"+clientID, day(n),
		event.OrderPlannedPayload{AccountID: "joint", Tag: "alarm:u", ClientOrderID: clientID, InstrumentID: "SPY",
			Side: event.OrderSideSell, Phase: event.OrderSideSell, OrderType: event.OrderTypeLimit, Quantity: 20, LimitPrice: 95,
			TargetWeight: 0.98, NetLiquidation: 4000, HeldQuantity: 60, ReferencePrice: 100, PriceBand: 0.05,
			Sale: event.SaleFigures{Kind: event.SaleKindUrgent, Trigger: event.SaleTriggerMarginUrgent, SnapshotID: "broker:u",
				KeepShares: 40, ToTarget: 20, WithinHolding: 60, NeverBelow: 20}})
}

// saleReleased is the gate's release of a sale, sent in paper (send true)
// or journalled only in dry-run.
func (s *stream) saleReleased(n int, clientID string, send bool) {
	mode := event.TradingModePaper
	if !send {
		mode = event.TradingModeDryRun
	}
	s.add(event.OrderReleasedEventType, event.OrderReleasedSafetyVersion, "release:"+clientID, day(n),
		event.OrderReleasedPayload{AccountID: "joint", PlanID: "plan:" + clientID, Tag: "alarm:u", ClientOrderID: clientID,
			InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 20, LimitPrice: 95, Mode: mode, Send: send,
			Figures: event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 1.5, ExposureAfter: 0.98,
				OrderFraction: 0.5, OrdersInDecision: 1, RiskReducing: true, BrokerSet: true, BrokerHolding: 60}})
}

// twFill is one execution of a target-weight order (execution.fill v5).
func (s *stream) twFill(n int, id, clientID string, quantity int64) {
	s.add(event.FillEventType, event.FillTargetWeightVersion, id, day(n),
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: clientID, FillID: id, InstrumentID: "SPY",
			Side: event.OrderSideSell, Quantity: quantity, Price: 96, Commission: 1, FilledAt: day(n)})
}

// lifecycle is the venue's report on a sale (execution.order.lifecycle v2).
func (s *stream) lifecycle(n int, id, clientID, status string) {
	s.add(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, id, day(n),
		event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: clientID, OrderID: "ib-1", InstrumentID: "SPY",
			Side: event.OrderSideSell, Status: status, Quantity: 20, LimitPrice: 95, OccurredAt: day(n), Message: "venue says " + status})
}

// saleWithheld is a sale back to 98% the gate did not plan, for reason.
func (s *stream) saleWithheld(n int, clientID, reason string) {
	s.add(event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, fmt.Sprintf("sale-withheld:%s:%s", clientID, reason), day(n),
		event.SaleWithheldPayload{AccountID: "joint", Tag: "alarm:u", ClientOrderID: clientID, InstrumentID: "SPY",
			Kind: event.SaleKindUrgent, Trigger: event.SaleTriggerMarginUrgent, Reason: reason, Detail: "why: " + reason, SnapshotID: "broker:u"})
}

func alertsOf(t *testing.T, s *stream) []report.Alert {
	t.Helper()
	instance := "boost"
	_, strategy, err := report.AccountOf(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if strategy == event.TargetWeightStrategyMomentum {
		instance = iraInstance
	}
	got, err := report.Alerts(s.out, instance)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func ofKind(alerts []report.Alert, kind report.Kind) []report.Alert {
	var out []report.Alert
	for _, a := range alerts {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// TestEachMarginAlarmLevelChangeIsAlerted: each change of the alarm's level
// is recorded as risk.margin.alarm and alerted (plan.md section 5, "A
// margin-cushion alarm"; docs/runbooks/margin-warning.md): a warning is
// checked the same day, an urgent level now, and a clear says it cleared.
func TestEachMarginAlarmLevelChangeIsAlerted(t *testing.T) {
	s := newStream(t)
	s.marginAlarm(0, "w", event.MarginAlarmWarning)
	s.marginAlarm(1, "u", event.MarginAlarmUrgent)
	s.marginAlarm(2, "c", event.MarginAlarmClear)
	got := ofKind(alertsOf(t, s), report.KindMarginAlarm)
	if len(got) != 3 {
		t.Fatalf("margin alerts = %+v, want one per level change", got)
	}
	want := []struct {
		level    string
		severity report.Severity
	}{{event.MarginAlarmWarning, report.Routine}, {event.MarginAlarmUrgent, report.Urgent}, {event.MarginAlarmClear, report.Routine}}
	keys := map[string]bool{}
	for i, w := range want {
		a := got[i]
		if a.Severity != w.severity || !contains(a.Title, w.level) || a.AccountID != "joint" || a.Repeat {
			t.Errorf("alert %d = %+v, want %s at %s, sent once", i, a, w.level, w.severity)
		}
		keys[a.Key] = true
	}
	if len(keys) != 3 {
		t.Errorf("the three level changes share keys: %v", keys)
	}
	if !contains(got[1].Body, "10.00%") || !contains(got[1].Body, "sells back to 98%") {
		t.Errorf("the urgent alert does not state the ratio and what the program does: %q", got[1].Body)
	}
}

// TestAnUnreadableMarginAlarmRefusesTheAlerts: the alarm is a decision, so
// one this build cannot read is a build mismatch, never skipped.
func TestAnUnreadableMarginAlarmRefusesTheAlerts(t *testing.T) {
	s := newStream(t)
	s.marginAlarm(0, "w", event.MarginAlarmWarning)
	s.out[len(s.out)-1].Payload = []byte(`{`)
	if _, err := report.Alerts(s.out, "boost"); err == nil {
		t.Fatal("Alerts accepted a corrupt margin alarm")
	}
}

// TestTheSaleBackTo98IsAlertedWhenSentAndWhenItFills: plan D6 tells the
// owner each step of the sale back to 98%: when it is sent, and when it
// fills (ADR 0019, amended 2026-10-15). A partial fill says how much is
// left; the last fill says it is done. A redelivered fill is not counted
// twice.
func TestTheSaleBackTo98IsAlertedWhenSentAndWhenItFills(t *testing.T) {
	s := newStream(t)
	s.salePlanned(0, "sale-1")
	s.saleReleased(0, "sale-1", true)
	s.twFill(1, "fill:a", "sale-1", 5)
	s.twFill(1, "fill:a", "sale-1", 5)
	s.twFill(1, "fill:b", "sale-1", 15)
	got := alertsOf(t, s)
	sent := ofKind(got, report.KindSaleSent)
	if len(sent) != 1 || !contains(sent[0].Body, "sell 20 SPY") || !contains(sent[0].Body, "sale-1") || !contains(sent[0].Title, "sent") {
		t.Fatalf("sent alerts = %+v", sent)
	}
	fills := ofKind(got, report.KindSaleFilled)
	if len(fills) != 2 {
		t.Fatalf("fill alerts = %+v, want one per distinct fill", fills)
	}
	if !contains(fills[0].Body, "5 of 20") || !contains(fills[0].Body, "15 still to fill") {
		t.Errorf("the partial fill's alert = %q", fills[0].Body)
	}
	if !contains(fills[1].Body, "20 of 20") || !contains(fills[1].Body, "complete") {
		t.Errorf("the last fill's alert = %q", fills[1].Body)
	}
	if fills[0].Key == fills[1].Key {
		t.Error("two fills share a key")
	}
}

// TestADryRunSaleIsAlertedAsReleasedNotSent: a dry-run engine releases the
// sale with send false and never sends it (ADR 0026, point 3, "Mode"), so the
// alert says it was not sent, at routine urgency.
func TestADryRunSaleIsAlertedAsReleasedNotSent(t *testing.T) {
	s := newStream(t)
	s.salePlanned(0, "sale-1")
	s.saleReleased(0, "sale-1", false)
	sent := ofKind(alertsOf(t, s), report.KindSaleSent)
	if len(sent) != 1 || !contains(sent[0].Title, "not sent") || sent[0].Severity != report.Routine {
		t.Fatalf("dry-run sale alerts = %+v", sent)
	}
}

// TestASaleTheVenueEndsUnfilledIsAlertedAsFailed: a sale IBKR cancels or
// refuses is not retried (docs/manual/states-and-kill-switch.md), so the
// owner is told it failed, how much filled, and to sell the rest by hand.
// A report that does not describe the order as released is not the
// engine's to read and says nothing.
func TestASaleTheVenueEndsUnfilledIsAlertedAsFailed(t *testing.T) {
	for _, status := range []string{event.OrderLifecycleStatusCanceled, event.OrderLifecycleStatusInvalid} {
		s := newStream(t)
		s.salePlanned(0, "sale-1")
		s.saleReleased(0, "sale-1", true)
		s.lifecycle(0, "life:sub", "sale-1", event.OrderLifecycleStatusSubmitted)
		s.twFill(1, "fill:a", "sale-1", 5)
		s.lifecycle(1, "life:end", "sale-1", status)
		failed := ofKind(alertsOf(t, s), report.KindSaleFailed)
		if len(failed) != 1 || failed[0].Severity != report.Urgent || !contains(failed[0].Body, status) ||
			!contains(failed[0].Body, "5 of 20") || !contains(failed[0].Body, "by hand") {
			t.Errorf("%s: failed alerts = %+v", status, failed)
		}
	}
	s := newStream(t)
	s.salePlanned(0, "sale-1")
	s.saleReleased(0, "sale-1", true)
	s.add(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "life:other", day(1),
		event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: "sale-1", OrderID: "ib-1", InstrumentID: "SPY",
			Side: event.OrderSideSell, Status: event.OrderLifecycleStatusCanceled, Quantity: 99, LimitPrice: 95, OccurredAt: day(1)})
	if failed := ofKind(alertsOf(t, s), report.KindSaleFailed); len(failed) != 0 {
		t.Errorf("a report that does not describe the sale raised %+v", failed)
	}
}

// TestOrdinaryOrdersRaiseNoSaleAlerts: only a planned order carrying sale
// figures is a sale back to 98%; an ordinary order's release and fills send
// none of these alerts, and neither does an unreadable fill.
func TestOrdinaryOrdersRaiseNoSaleAlerts(t *testing.T) {
	s := newStream(t)
	s.planned(0, "tw-a", 3)
	s.released(0, "tw-a", 3)
	s.twFill(1, "fill:a", "tw-a", 3)
	s.twFill(1, "fill:x", "sale-1", 3)
	s.out[len(s.out)-1].Payload = []byte(`{`)
	for _, a := range alertsOf(t, s) {
		if a.Kind == report.KindSaleSent || a.Kind == report.KindSaleFilled || a.Kind == report.KindSaleFailed {
			t.Errorf("an ordinary order raised %+v", a)
		}
	}
}

// TestAWithheldSaleTheOwnerMustMakeByHandIsAlertedUrgently: a sale withheld
// as broker-values-unreadable or unknown-working-sell counts as attempted
// and is never tried again automatically, so the alert asks the owner to act
// by hand (ADR 0019, amended 2026-10-15, "No order when it cannot be
// checked"; docs/manual/states-and-kill-switch.md). Nothing to sell and a
// lapse send no such alert; a lapse goes in the daily summary. A
// non-positive withhold pages too (TestANonPositiveWithheldSalePagesSellByHand).
func TestAWithheldSaleTheOwnerMustMakeByHandIsAlertedUrgently(t *testing.T) {
	for _, reason := range []string{event.SaleWithheldUnreadable, event.SaleWithheldUnknownWorkingSell} {
		s := newStream(t)
		s.saleWithheld(0, "sale-1", reason)
		got := ofKind(alertsOf(t, s), report.KindSaleByHand)
		if len(got) != 1 || got[0].Severity != report.Urgent || !contains(got[0].Title, reason) ||
			!contains(got[0].Body, "by hand") || !contains(got[0].Body, "will not try again") {
			t.Errorf("%s: alerts = %+v", reason, got)
		}
	}
	if !strings.Contains(byHandBody(t, event.SaleWithheldUnknownWorkingSell), "CONTAINMENT REQUIRED") {
		t.Error("an unknown working sell's alert does not say CONTAINMENT REQUIRED")
	}
	for _, reason := range []string{event.SaleWithheldNothingToSell, event.SaleWithheldLapsed} {
		s := newStream(t)
		s.saleWithheld(0, "sale-1", reason)
		if got := alertsOf(t, s); len(got) != 0 {
			t.Errorf("%s raised %+v", reason, got)
		}
	}
}

func byHandBody(t *testing.T, reason string) string {
	s := newStream(t)
	s.saleWithheld(0, "sale-1", reason)
	got := ofKind(alertsOf(t, s), report.KindSaleByHand)
	if len(got) != 1 {
		t.Fatalf("alerts = %+v", got)
	}
	return got[0].Body
}

// TestSaleAlertsAreStableAcrossReadings: every key names its record, so a
// later reading of a grown log raises the same keys and the reporter sends
// each once.
func TestSaleAlertsAreStableAcrossReadings(t *testing.T) {
	s := newStream(t)
	s.marginAlarm(0, "u", event.MarginAlarmUrgent)
	s.salePlanned(0, "sale-1")
	s.saleReleased(0, "sale-1", true)
	first := alertsOf(t, s)
	s.twFill(1, "fill:a", "sale-1", 20)
	second := alertsOf(t, s)
	for i, a := range first {
		if second[i].Key != a.Key {
			t.Errorf("alert %d's key moved from %q to %q", i, a.Key, second[i].Key)
		}
	}
}

// TestAnUnreadablePlanRefusesTheAlerts: a plan is a decision, and the one
// that names a sale back to 98%; one this build cannot read would silence
// that sale's sent, filled and failed alerts, so the alerts refuse the log
// over it, naming the record, and the reporter pages that it cannot read the
// journal (ADR 0019, "Alerting": an alert is a safety requirement).
func TestAnUnreadablePlanRefusesTheAlerts(t *testing.T) {
	for name, spoil := range map[string]func(e *event.Envelope){
		"unknown version": func(e *event.Envelope) { e.SchemaVersion = 99 },
		"corrupt payload": func(e *event.Envelope) { e.Payload = []byte(`{`) },
	} {
		s := newStream(t)
		s.salePlanned(0, "sale-1")
		spoil(&s.out[len(s.out)-1])
		s.saleReleased(0, "sale-1", true)
		s.refused(0, "tw-b", event.RefusedOrderSize)
		_, err := report.Alerts(s.out, "boost")
		if err == nil || !strings.Contains(err.Error(), "plan:sale-1") {
			t.Errorf("%s: Alerts err = %v, want a refusal naming plan:sale-1", name, err)
		}
	}
}

// TestAMarginAlarmWithNoRatioSaysWhy: the alarm's other two causes carry no
// ratio (plan.md section 5): no reading for the stale period while the market
// is open, and a net liquidation of zero or less. The alert says which, and
// when the last reading was.
func TestAMarginAlarmWithNoRatioSaysWhy(t *testing.T) {
	s := newStream(t)
	noReading := event.MarginAlarmPayload{AccountID: "joint", SnapshotID: "broker:n", AsOf: day(0), Level: event.MarginAlarmUrgent,
		Cause: event.MarginAlarmCauseNoReading, Warning: 0.4, Urgent: 0.2, StaleMinutes: 30, Detail: "no reading"}
	s.add(event.MarginAlarmEventType, event.MarginAlarmSchemaVersion, "alarm:n1", day(0), noReading)
	noReading.LastReadingAt, noReading.LastReadingSet = day(0).Add(-time.Hour), true
	s.add(event.MarginAlarmEventType, event.MarginAlarmSchemaVersion, "alarm:n2", day(1), noReading)
	s.add(event.MarginAlarmEventType, event.MarginAlarmSchemaVersion, "alarm:p", day(2), event.MarginAlarmPayload{AccountID: "joint",
		SnapshotID: "broker:p", AsOf: day(2), Level: event.MarginAlarmUrgent, Cause: event.MarginAlarmCauseNonPositive, ExcessLiquidity: -5,
		NetLiquidation: -1, ReadingSet: true, Warning: 0.4, Urgent: 0.2, StaleMinutes: 30, Detail: "negative"})
	got := ofKind(alertsOf(t, s), report.KindMarginAlarm)
	if len(got) != 3 {
		t.Fatalf("alerts = %+v", got)
	}
	for i, want := range []string{"no Excess Liquidity reading for 30 minutes", "last reading: 2024-01-02T20:00:00Z", "net liquidation of -1.00, zero or less"} {
		if !contains(got[i].Body, want) || got[i].Severity != report.Urgent {
			t.Errorf("alert %d = %q, want %q at urgent", i, got[i].Body, want)
		}
	}
	if !contains(got[0].Body, "none in this run") {
		t.Errorf("a run with no reading yet says %q", got[0].Body)
	}
}

// TestAnUnreadableReleaseRefusesTheAlerts: a release is a decision, which
// the order lifecycle already refuses the log over, so the alerts refuse it
// too rather than guess whether it was a sale.
func TestAnUnreadableReleaseRefusesTheAlerts(t *testing.T) {
	s := newStream(t)
	s.salePlanned(0, "sale-1")
	s.saleReleased(0, "sale-1", true)
	s.out[len(s.out)-1].Payload = []byte(`{`)
	if _, err := report.Alerts(s.out, "boost"); err == nil {
		t.Fatal("Alerts accepted a corrupt release")
	}
}

// TestAVenueReportThatDoesNotReadRaisesNoSaleAlert: a lifecycle report at
// another version, or one that does not read, is not the engine's to read
// and says nothing about the sale; the unread-report alert covers it.
func TestAVenueReportThatDoesNotReadRaisesNoSaleAlert(t *testing.T) {
	s := newStream(t)
	s.salePlanned(0, "sale-1")
	s.saleReleased(0, "sale-1", true)
	s.lifecycle(1, "life:bad", "sale-1", event.OrderLifecycleStatusCanceled)
	s.out[len(s.out)-1].Payload = []byte(`{`)
	s.lifecycle(1, "life:v1", "sale-1", event.OrderLifecycleStatusCanceled)
	s.out[len(s.out)-1].SchemaVersion = 1
	if failed := ofKind(alertsOf(t, s), report.KindSaleFailed); len(failed) != 0 {
		t.Errorf("an unread report raised %+v", failed)
	}
}
