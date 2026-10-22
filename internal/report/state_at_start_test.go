package report_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// stateAtStart is the account's standing read from s, with the alerts the
// same log raises.
func stateAtStart(t *testing.T, s *stream) (report.Alert, bool) {
	t.Helper()
	alerts, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := report.StateAtStartAlert(s.out, alerts)
	if err != nil {
		t.Fatal(err)
	}
	return a, ok
}

// TestTheStateAtReporterStartNamesWhatIsInForce: a channel that begins
// after the log's earlier events is told the account's standing once: the
// engine state in force with its scope, the kill switch engaged, a pending
// cash read and every open alert that follows it (ADR 0019, "Alerting").
func TestTheStateAtReporterStartNamesWhatIsInForce(t *testing.T) {
	halted := newStream(t)
	halted.engineState(1, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	a, ok := stateAtStart(t, halted)
	if !ok || a.Severity != report.Urgent || a.Kind != report.KindStateAtStart {
		t.Fatalf("a Halted account's standing = %+v, %v; want an urgent state-at-start alert", a, ok)
	}
	for _, want := range []string{"state at reporter start", "Halted", "risk-limit-breached", "1 open alert", "joint: engine halted"} {
		if !strings.Contains(a.Title+"\n"+a.Body, want) {
			t.Errorf("the standing does not say %q:\n%s\n%s", want, a.Title, a.Body)
		}
	}

	degraded := newStream(t)
	degraded.engineState(1, event.EngineStateDegraded, event.EngineStateReasonRiskLimitBreached, "SPY", "TLT")
	if a, ok := stateAtStart(t, degraded); !ok || !strings.Contains(a.Body, "Degraded") || !strings.Contains(a.Body, "instruments [SPY,TLT]") {
		t.Errorf("a Degraded account's standing must name its scope: %+v, %v", a, ok)
	}

	// An incident a fresh run's approval carries in force has no engine-state
	// record of its own in this log, and is in force all the same.
	approved := newStream(t)
	approved.add(event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "operator-run-approved:x", day(0), event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
		ApprovedBy: "owner", ApprovedAt: day(0), Boost: event.BoostStateInForce{Latched: new(false)},
		IncidentInForce: &event.IncidentInForce{IncidentID: "engine-state:joint:0", State: event.EngineStateDegraded,
			ScopeInstrumentIDs: []string{"SPY"}, ScopeCashComponents: []string{}, SaleAttempted: new(true)},
	})
	if a, ok := stateAtStart(t, approved); !ok || !strings.Contains(a.Body, "Degraded") || !strings.Contains(a.Body, "instruments [SPY]") || !strings.Contains(a.Body, "engine-state:joint:0") {
		t.Errorf("the incident the approval carries in force is not named: %+v, %v", a, ok)
	}

	killed := newStream(t)
	killed.add(event.KillSwitchEventType, event.KillSwitchSchemaVersion, "kill-switch:1", day(1),
		event.KillSwitchPayload{AccountID: "joint", AsOf: day(1), Engaged: true, Operator: "owner", Detail: "by hand"})
	if a, ok := stateAtStart(t, killed); !ok || !strings.Contains(a.Body, "kill switch is engaged") {
		t.Errorf("an engaged kill switch is not named: %+v, %v", a, ok)
	}
	released := newStream(t)
	released.add(event.KillSwitchEventType, event.KillSwitchSchemaVersion, "kill-switch:1", day(1),
		event.KillSwitchPayload{AccountID: "joint", AsOf: day(1), Engaged: true, Operator: "owner", Detail: "by hand"})
	released.add(event.KillSwitchEventType, event.KillSwitchSchemaVersion, "kill-switch:2", day(2),
		event.KillSwitchPayload{AccountID: "joint", AsOf: day(2), Engaged: false, Operator: "owner", Detail: "by hand"})
	if a, ok := stateAtStart(t, released); ok {
		t.Errorf("a released kill switch is in force: %+v", a)
	}

	pending := newStream(t)
	pending.pendingRead(1, "reconciliation:1", -50000, "")
	a, ok = stateAtStart(t, pending)
	if !ok || !strings.Contains(a.Body, "pending cash read") || !strings.Contains(a.Body, "close:reconciliation:1") {
		t.Errorf("a pending cash read is not named: %+v, %v", a, ok)
	}
	if strings.Contains(a.PublicBody, "500.00") || a.PublicBody == "" {
		t.Errorf("the public text must carry no figure:\n%s", a.PublicBody)
	}

	alarmed := newStream(t)
	alarmed.marginAlarm(1, "1", event.MarginAlarmUrgent)
	if a, ok := stateAtStart(t, alarmed); !ok || !strings.Contains(a.Body, "margin-cushion alarm is urgent") {
		t.Errorf("a margin-cushion alarm not cleared is not named: %+v, %v", a, ok)
	}
}

// TestTheStateAtReporterStartIsNothingWhenNothingIsInForce: a page means an
// incident, so an account with nothing in force has no standing to page.
func TestTheStateAtReporterStartIsNothingWhenNothingIsInForce(t *testing.T) {
	quiet := newStream(t)
	quiet.refused(1, "tw-a", event.RefusedOrderSize)
	if a, ok := stateAtStart(t, quiet); ok {
		t.Errorf("a refusal long past is not in force: %+v", a)
	}
	acknowledged := newStream(t)
	acknowledged.engineState(1, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	alerts, err := report.Alerts(acknowledged.out)
	if err != nil {
		t.Fatal(err)
	}
	acknowledged.ack("ack:1", event.AlertAcknowledgedSchemaVersion, event.AlertAcknowledgedPayload{
		AccountID: "joint", AlertKey: alerts[0].Key, AcknowledgedBy: "owner", AcknowledgedAt: day(2)})
	// The state is still Halted: acknowledging the alert does not leave it.
	a, ok := stateAtStart(t, acknowledged)
	if !ok || !strings.Contains(a.Body, "Halted") || strings.Contains(a.Body, "open alert") {
		t.Errorf("an acknowledged halt still holds the account, with no open alert: %+v, %v", a, ok)
	}
}

// TestTheStateAtReporterStartRefusesWhatItCannotRead: a record that decides
// what is in force and does not read is an error, never skipped, except the
// kill switch's, which is named as possibly engaged, since the gate refuses
// every order while it is (ADR 0026, point 3). A switch for another account
// is not this one's.
func TestTheStateAtReporterStartRefusesWhatItCannotRead(t *testing.T) {
	if _, _, err := report.StateAtStartAlert(nil, nil); err == nil {
		t.Error("a log with no configuration names no account, and must be an error")
	}
	for _, eventType := range []string{event.OperatorRunApprovedEventType, event.EngineStateEventType, event.TargetsDecidedEventType, event.ReconciliationResultEventType, event.MarginAlarmEventType} {
		s := newStream(t)
		s.add(eventType, 99, "unreadable", day(1), map[string]string{})
		if _, _, err := report.StateAtStartAlert(s.out, nil); err == nil {
			t.Errorf("a %s record this build cannot read was skipped", eventType)
		}
	}
	unread := newStream(t)
	unread.add(event.KillSwitchEventType, 99, "kill-switch:1", day(1), map[string]string{})
	if a, ok, err := report.StateAtStartAlert(unread.out, nil); err != nil || !ok || !strings.Contains(a.Body, "kill switch may be engaged") {
		t.Errorf("an unreadable kill switch must be named as possibly engaged: %+v, %v, %v", a, ok, err)
	}
	other := newStream(t)
	other.add(event.KillSwitchEventType, event.KillSwitchSchemaVersion, "kill-switch:1", day(1),
		event.KillSwitchPayload{AccountID: "ira", AsOf: day(1), Engaged: true, Operator: "owner", Detail: "by hand"})
	if a, ok, err := report.StateAtStartAlert(other.out, nil); err != nil || ok {
		t.Errorf("another account's kill switch is in force here: %+v, %v, %v", a, ok, err)
	}
}

// TestTheStateAtReporterStartNamesBoostsStoppedBuys: two orders the venue
// rejected in a row stop Boost's buys until the owner starts a fresh run
// (ADR 0026, point 2, "Boost", amended 2026-10-18). The page that announced
// it is sent once, not repeated, so a channel that begins after it is told
// the stop in the standing, with the latch it holds, or the owner sees no
// reason the account buys nothing (ADR 0019, "Alerting").
func TestTheStateAtReporterStartNamesBoostsStoppedBuys(t *testing.T) {
	stoppedAt := func(n int, rejections int, latched bool) event.TargetsDecidedPayload {
		p := event.TargetsDecidedPayload{AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, DecisionTime: day(n), SessionPeriodEnd: day(n),
			Outcome: event.TargetsOutcomeDecided, Targets: []event.TargetWeight{{InstrumentID: "SPY", Weight: 0.98}},
			Boost: event.BoostFigures{InstrumentID: "SPY", Close: 100, LatchedBefore: latched, LatchedAfter: latched, VenueRejections: rejections}}
		p.Boost.RejectedInARow = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-6"}, {ClientOrderID: "tw-7"}}[:rejections]
		return p
	}
	stopped := newStream(t)
	first := stoppedAt(0, 2, true)
	first.Boost.ReplanWithheld = event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: "tw-7", Released: 37}
	stopped.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets:0", day(0), first)
	stopped.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets:1", day(1), stoppedAt(1, 2, true))
	a, ok := stateAtStart(t, stopped)
	if !ok || a.Severity != report.Urgent {
		t.Fatalf("Boost's stopped buys are not in force: %+v, %v", a, ok)
	}
	for _, want := range []string{"buys are stopped", "tw-6", "tw-7", "fresh run", "boost is on"} {
		if !strings.Contains(a.Body, want) || !strings.Contains(a.PublicBody, want) {
			t.Errorf("the standing does not say %q:\n%s\n%s", want, a.Body, a.PublicBody)
		}
	}

	counting := newStream(t)
	counting.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets:0", day(0), stoppedAt(0, 1, false))
	if a, ok := stateAtStart(t, counting); ok {
		t.Errorf("one rejection does not stop buys: %+v", a)
	}
}
