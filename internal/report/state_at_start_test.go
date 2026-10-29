package report_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// stateAtStart is the account's standing read from s, with the alerts the
// same log raises.
func stateAtStart(t *testing.T, s *stream) (report.Alert, bool) {
	t.Helper()
	alerts, err := report.Alerts(s.out, "boost")
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
	alerts, err := report.Alerts(acknowledged.out, "boost")
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
	for _, eventType := range []string{event.OperatorRunApprovedEventType, event.EngineStateEventType, event.ReconciliationResultEventType, event.MarginAlarmEventType} {
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
// the stop in the standing, with whether the boost is on, or the owner sees no
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

	if strings.Contains(strings.ToLower(a.Body+a.PublicBody), "latch") {
		t.Errorf("the owner-facing page must say the boost is on or off, not latch:\n%s", a.Body)
	}

	counting := newStream(t)
	counting.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets:0", day(0), stoppedAt(0, 1, false))
	if a, ok := stateAtStart(t, counting); ok {
		t.Errorf("one rejection does not stop buys: %+v", a)
	}
}

// TestAnUnreadableDecisionDoesNotSilenceTheStandingAndIsNamedAsPossiblyStopping:
// a targets-decided record this build cannot read may be the one that stopped
// Boost's buys, so the page still goes out, with everything else in force,
// and says to treat the buy stop as in force (fail closed, AGENTS.md).
func TestAnUnreadableDecisionDoesNotSilenceTheStandingAndIsNamedAsPossiblyStopping(t *testing.T) {
	s := newStream(t)
	s.engineState(1, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	s.add(event.TargetsDecidedEventType, 99, "targets:2", day(2), map[string]string{})
	a, ok, err := report.StateAtStartAlert(s.out, nil)
	if err != nil || !ok {
		t.Fatalf("an unreadable decision must not fail the page: %+v, %v, %v", a, ok, err)
	}
	for _, want := range []string{"targets:2", "cannot be read: treat the buy stop as possibly in force", "Halted"} {
		if !strings.Contains(a.Body, want) || !strings.Contains(a.PublicBody, want) {
			t.Errorf("the standing does not say %q:\n%s", want, a.Body)
		}
	}
}

// boostLog builds a Boost account's log as the engine journals it: each
// decision records its count, the orders it names and the orders still
// awaiting the venue's word, then its release follows; the venue's reports
// describe the order as released.
type boostLog struct {
	*stream
	inARow    []event.BoostRejectedOrder
	undecided []string
	before    int
}

func newBoostLog(t *testing.T) *boostLog { return &boostLog{stream: newStream(t)} }

// decide journals the decision of session n, which releases the orders ids.
func (l *boostLog) decide(n int, ids ...string) {
	l.undecided = append(l.undecided, ids...)
	b := event.BoostFigures{InstrumentID: "SPY", Close: 100, LatchedBefore: true, LatchedAfter: true,
		VenueRejections: len(l.inARow), RejectedInARow: l.inARow, UndecidedBeforeRejection: l.before}
	for _, id := range l.undecided {
		b.UndecidedOrders = append(b.UndecidedOrders, event.BoostUndecidedOrder{ClientOrderID: id, Side: event.OrderSideBuy, Released: 10, LimitPrice: 101})
	}
	l.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, fmt.Sprintf("targets:%d", n), day(n), event.TargetsDecidedPayload{
		AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, DecisionTime: day(n), SessionPeriodEnd: day(n),
		Outcome: event.TargetsOutcomeDecided, Targets: []event.TargetWeight{{InstrumentID: "SPY", Weight: 0.98}}, Boost: b})
	for _, id := range ids {
		l.send(n, id)
	}
}

// send journals the release of order id by the decision of session n.
func (l *boostLog) send(n int, id string) {
	l.add(event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release:"+id, day(n),
		event.OrderReleasedPayload{AccountID: "joint", PlanID: "plan:" + id, Tag: fmt.Sprintf("targets:%d", n), ClientOrderID: id,
			InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 10, LimitPrice: 101, Mode: event.TradingModePaper, Send: true, Figures: figures})
}

// report journals the venue's report of status on order id, as released.
func (l *boostLog) report(n int, id, status string) {
	l.reportOf(n, id, status, 10)
}

// reportOf is report for an order of quantity shares.
func (l *boostLog) reportOf(n int, id, status string, quantity int64) {
	l.add(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, fmt.Sprintf("life:%s:%s", id, status), day(n),
		event.LimitOrderLifecyclePayload{AccountID: "joint", ClientOrderID: id, OrderID: "ib-" + id, InstrumentID: "SPY",
			Side: event.OrderSideBuy, Status: status, Quantity: quantity, LimitPrice: 101, OccurredAt: day(n)})
}

// filled journals the venue's execution of order id in full.
func (l *boostLog) filled(n int, id string) {
	l.add(event.FillEventType, event.FillTargetWeightVersion, "fill:"+id, day(n),
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: id, FillID: "fill:" + id, InstrumentID: "SPY",
			Side: event.OrderSideBuy, Quantity: 10, Price: 100, Commission: 1, FilledAt: day(n)})
}

// TestTheSecondRejectionStopsBuysAtOnceInTheStanding: the decision that
// records the stop comes at the next session, but the second rejection is
// journalled as the venue's report, and a channel that begins in between is
// told the stop from the rejections themselves, counted by the engine's own
// rule from the decision's recorded count (ADR 0026, point 2, "Boost",
// amended 2026-10-18; point 5).
func TestTheSecondRejectionStopsBuysAtOnceInTheStanding(t *testing.T) {
	one := func(released ...string) *boostLog {
		l := newBoostLog(t)
		l.decide(0, "tw-6", "tw-7")
		l.report(1, "tw-6", event.OrderLifecycleStatusInvalid)
		l.inARow, l.undecided, l.before = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-tw-6"}}, []string{"tw-7"}, 0
		l.decide(1, released...)
		return l
	}
	// tw-9 is released by the decision of session 1, which lists it among
	// its undecided orders as the engine's journal does.
	second := one("tw-9")
	// None of these is a rejection the engine counts: a dry-run release, a
	// release made twice, a sale back to 98% (no Boost decision's order), a
	// report on an order no decision released, an acknowledgement, a report
	// the build cannot read and another account's.
	second.released(1, "tw-dry", 10)
	second.send(1, "tw-7")
	second.add(event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release:alarm", day(1),
		event.OrderReleasedPayload{AccountID: "joint", PlanID: "plan:alarm", Tag: "alarm:u", ClientOrderID: "alarm-1",
			InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 10, LimitPrice: 95, Mode: event.TradingModePaper, Send: true, Figures: figures})
	second.lifecycle(2, "life:alarm", "alarm-1", event.OrderLifecycleStatusInvalid)
	second.report(2, "tw-dry", event.OrderLifecycleStatusInvalid)
	second.report(2, "tw-unknown", event.OrderLifecycleStatusInvalid)
	second.report(2, "tw-7", event.OrderLifecycleStatusSubmitted)
	second.add(event.OrderLifecycleEventType, 99, "life:unread", day(2), map[string]string{})
	second.add(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "life:other", day(2),
		event.LimitOrderLifecyclePayload{AccountID: "ira", ClientOrderID: "tw-7", OrderID: "ib-9", InstrumentID: "SPY",
			Side: event.OrderSideBuy, Status: event.OrderLifecycleStatusInvalid, Quantity: 10, LimitPrice: 101, OccurredAt: day(2)})
	if a, ok := stateAtStart(t, second.stream); ok {
		t.Fatalf("what is not a counted rejection stops buys: %+v", a)
	}
	second.report(2, "tw-7", event.OrderLifecycleStatusInvalid)
	a, ok := stateAtStart(t, second.stream)
	if !ok {
		t.Fatal("the second rejection, journalled and not yet decided, does not stop buys in the standing")
	}
	for _, want := range []string{"buys are stopped", "rejected 2 orders", "tw-6", "tw-7", "boost is on"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("the standing does not say %q:\n%s", want, a.Body)
		}
	}

	// A third rejection, read once buys are stopped, counts nothing more:
	// the page still names the two orders and the count of 2.
	second.inARow = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-tw-6"}, {ClientOrderID: "tw-7", VenueOrderID: "ib-tw-7"}}
	second.undecided, second.before = nil, 0
	// tw-9 was released before the stop (session 1) and is reported after it; a
	// stopped engine releases no buy, so the decision that records the stop
	// lists it as awaiting the venue's word and releases nothing.
	second.undecided = []string{"tw-9"}
	second.decide(3)
	second.report(4, "tw-9", event.OrderLifecycleStatusInvalid)
	a, ok = stateAtStart(t, second.stream)
	if !ok || !strings.Contains(a.Body, "rejected 2 orders") || !strings.Contains(a.Body, "tw-6") || !strings.Contains(a.Body, "tw-7") || strings.Contains(a.Body, "tw-9") {
		t.Errorf("a rejection read once stopped must leave the count of 2 and the two orders named:\n%s", a.Body)
	}

	if a, ok := stateAtStart(t, one().stream); ok {
		t.Errorf("one rejection does not stop buys: %+v", a)
	}

	accepted := one()
	accepted.decide(2, "tw-8")
	accepted.report(3, "tw-8", event.OrderLifecycleStatusCanceled)
	accepted.report(3, "tw-7", event.OrderLifecycleStatusInvalid)
	if a, ok := stateAtStart(t, accepted.stream); ok {
		t.Errorf("an order accepted after the first rejection sets the count back, and a rejection of an older order does not stop buys: %+v", a)
	}
}

// TestAFillIsTheVenuesAcceptanceInTheStanding: an order the venue
// acknowledges as submitted and then fills in full sends no other report,
// and the engine counts its execution as the venue accepting it, so the
// count goes back to none and a late rejection of an older order is the
// first in a row, not the second: the page must not say buys are stopped
// while the engine still buys (ADR 0026, point 2, "Boost", amended
// 2026-10-18).
func TestAFillIsTheVenuesAcceptanceInTheStanding(t *testing.T) {
	l := newBoostLog(t)
	l.decide(0, "tw-5", "tw-6")
	l.report(1, "tw-6", event.OrderLifecycleStatusInvalid)
	l.inARow, l.undecided, l.before = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-tw-6"}}, []string{"tw-5"}, 1
	l.decide(1)
	l.decide(2, "tw-8")
	l.report(3, "tw-8", event.OrderLifecycleStatusSubmitted)
	l.filled(3, "tw-8")
	l.report(4, "tw-5", event.OrderLifecycleStatusInvalid)
	if a, ok := stateAtStart(t, l.stream); ok {
		t.Errorf("a fill accepted tw-8, so the late rejection of tw-5 is the first in a row; the page says:\n%s", a.Body)
	}
}

// TestAReportTheEngineRefusesIsNoRejectionInTheStanding: a venue report that
// does not describe the order as released (account, instrument, side,
// quantity and limit) is refused by the engine and decides nothing, so it is
// no rejection on the page either (ADR 0022).
func TestAReportTheEngineRefusesIsNoRejectionInTheStanding(t *testing.T) {
	l := newBoostLog(t)
	l.decide(0, "tw-6", "tw-7")
	l.report(1, "tw-6", event.OrderLifecycleStatusInvalid)
	l.inARow, l.undecided = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-tw-6"}}, []string{"tw-7"}
	l.decide(1)
	l.reportOf(2, "tw-7", event.OrderLifecycleStatusInvalid, 11)
	if a, ok := stateAtStart(t, l.stream); ok {
		t.Errorf("a rejection of 11 shares does not describe tw-7 as released (10); the page says:\n%s", a.Body)
	}
}

// TestAnUnreadableDecisionKeepsADefiniteStop: a decision record that cannot
// be read after the decision that stopped Boost's buys leaves that stop in
// force, named with its orders, and adds the unreadable record (fail
// closed, AGENTS.md).
func TestAnUnreadableDecisionKeepsADefiniteStop(t *testing.T) {
	l := newBoostLog(t)
	l.inARow = []event.BoostRejectedOrder{{ClientOrderID: "tw-6"}, {ClientOrderID: "tw-7"}}
	l.decide(1)
	l.add(event.TargetsDecidedEventType, 99, "targets:2", day(2), map[string]string{})
	a, ok, err := report.StateAtStartAlert(l.out, nil)
	if err != nil || !ok {
		t.Fatalf("the stop is not in force: %+v, %v, %v", a, ok, err)
	}
	for _, want := range []string{"buys are stopped", "tw-6", "tw-7", "targets:2", "cannot be read"} {
		if !strings.Contains(a.Body, want) || !strings.Contains(a.PublicBody, want) {
			t.Errorf("the standing does not say %q:\n%s", want, a.Body)
		}
	}
	if strings.Contains(a.Title, "possibly") {
		t.Errorf("a definite stop is not a possible one: %s", a.Title)
	}
}

// TestAnUnreadableMomentumDecisionNamesNoBoostStop: a momentum account has
// no Boost buy stop, so an unreadable decision record is named as
// unreadable, never as a possible Boost stop.
func TestAnUnreadableMomentumDecisionNamesNoBoostStop(t *testing.T) {
	s := &stream{t: t}
	s.add(event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", day(0).Add(-time.Hour), momentumConfiguration())
	s.add(event.TargetsDecidedEventType, 99, "targets:2", day(2), map[string]string{})
	a, ok, err := report.StateAtStartAlert(s.out, nil)
	if err != nil || !ok {
		t.Fatalf("an unreadable decision must still page: %+v, %v, %v", a, ok, err)
	}
	if !strings.Contains(a.Body, "targets:2") || !strings.Contains(a.Body, "cannot be read") {
		t.Errorf("the unreadable record is not named:\n%s", a.Body)
	}
	if text := strings.ToLower(a.Title + a.Body + a.PublicBody); strings.Contains(text, "boost") || strings.Contains(text, "buy stop") {
		t.Errorf("a momentum account is named as a Boost stop:\n%s\n%s", a.Title, a.Body)
	}
}

// momentumConfiguration is a momentum account's configuration.
func momentumConfiguration() event.TargetWeightConfigurationPayload {
	return event.TargetWeightConfigurationPayload{
		StrategyID: event.TargetWeightStrategyMomentum, AccountID: "ira-traditional", Mode: event.TradingModeDryRun, Adapter: event.AdapterIBKR,
		Momentum: event.MomentumConfiguration{
			Rule: event.MomentumRuleTrend, ListingType: "CS", UniverseSize: 500, MinPrice: 5,
			ScoreLookback: 252, ScoreSkip: 21, VolatilityReturns: 252, TopN: 25, SectorCap: 3,
			InverseVolatility: true, Invested: 0.98, TrendInstrument: "SPY", TrendCloses: 200,
			RiskOffInstrument: "IEF", RiskOffFraction: 0.5, RebalanceBand: 0.25,
			UniverseSource: "massive", SectorSources: []string{"ishares-iwv-gics", "nasdaq-screener"},
			UniverseInput: event.UniverseInputCompact, MaxUnlabelledShare: 0.20,
		},
		Glide: event.GlideConfiguration{
			Anchors:            []event.GlideAnchor{{AgeYears: 38, Share: 0}, {AgeYears: 53, Share: 0.308}, {AgeYears: 65, Share: 0.5}, {AgeYears: 72, Share: 0.7}},
			RetirementAgeYears: 65, BondFunds: []event.BondFund{{InstrumentID: "IEF", Fraction: 0.5}, {InstrumentID: "SHY", Fraction: 0.5}}, Birth: "1980-01",
		},
		Execution: event.ExecutionConfiguration{CashRule: event.CashRuleSettled, ShareQuantity: event.ShareQuantityMinimumLot, MaxFeesPerOrder: 1, MaxSnapshotAge: 30},
		Limits:    event.MarginLimits{MaxExposure: 1, MaxOrdersPerDecision: 60, MaxOrderFraction: 0.5, PriceBand: 0.05},
	}
}

// TestAFillTheGateRefusesIsNoAcceptanceInTheStanding: a fill the gate's
// fill rule refuses (risklimit.CheckFill), here more shares than released,
// is not the venue accepting the order, so it does not set the count back.
func TestAFillTheGateRefusesIsNoAcceptanceInTheStanding(t *testing.T) {
	l := newBoostLog(t)
	l.decide(0, "tw-6", "tw-7")
	l.report(1, "tw-6", event.OrderLifecycleStatusInvalid)
	l.inARow, l.undecided = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-tw-6"}}, []string{"tw-7"}
	l.decide(1, "tw-8")
	l.add(event.FillEventType, event.FillTargetWeightVersion, "fill:over", day(2),
		event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-8", FillID: "fill:over", InstrumentID: "SPY",
			Side: event.OrderSideBuy, Quantity: 11, Price: 100, Commission: 1, FilledAt: day(2)})
	l.report(3, "tw-8", event.OrderLifecycleStatusInvalid)
	if a, ok := stateAtStart(t, l.stream); !ok || !strings.Contains(a.Body, "buys are stopped") {
		t.Errorf("a refused fill accepted nothing, so tw-8's rejection is the second in a row: %+v, %v", a, ok)
	}
}

// TestAClearedOrderIsNoRejectionInTheStanding: an order the owner settled
// as never sent leaves the orders awaiting the venue's word, so a report on
// it counts for nothing (ADR 0026, point 2, "Boost", amended 2026-10-18).
func TestAClearedOrderIsNoRejectionInTheStanding(t *testing.T) {
	never := orderplan.ClientOrderID("targets:1", "SPY", event.OrderSideBuy)
	build := func(cleared bool) *boostLog {
		l := newBoostLog(t)
		l.decide(0, "tw-6")
		l.report(1, "tw-6", event.OrderLifecycleStatusInvalid)
		l.inARow, l.undecided = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-tw-6"}}, nil
		l.decide(1, never)
		if cleared {
			l.add(event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared:1", day(2), event.RebalanceClearedPayload{
				AccountID: "joint", AsOf: day(2), DecisionID: "targets:1", ClientOrderIDs: []string{never}, Operator: "owner", Detail: "never sent"})
		}
		l.report(3, never, event.OrderLifecycleStatusInvalid)
		return l
	}
	if a, ok := stateAtStart(t, build(false).stream); !ok {
		t.Fatalf("without the clearance the second rejection stops buys: %+v", a)
	}
	if a, ok := stateAtStart(t, build(true).stream); ok {
		t.Errorf("a report on a cleared order is no rejection: %+v", a)
	}
}

// TestAnUnreadableBoostDecisionAloneIsAPossibleStop: with nothing else in
// force, the page's headline names the possible stop.
func TestAnUnreadableBoostDecisionAloneIsAPossibleStop(t *testing.T) {
	s := newStream(t)
	s.add(event.TargetsDecidedEventType, 99, "targets:2", day(2), map[string]string{})
	a, ok, err := report.StateAtStartAlert(s.out, nil)
	if err != nil || !ok || !strings.HasSuffix(a.Title, "Normal, Boost's buys possibly stopped") {
		t.Errorf("an unreadable Boost decision alone: %+v, %v, %v; want a possible stop", a, ok, err)
	}
}

// TestARefusedClearanceIsNamedNotFatalInTheStanding: the reporter
// never crashes on a journalled record; a clearance the engine refuses (here
// one naming an order no decision released) is
// named on the page and the rest of the standing still goes out (ADR 0026,
// point 2, "Boost", amended 2026-10-18).
func TestARefusedClearanceIsNamedNotFatalInTheStanding(t *testing.T) {
	never := orderplan.ClientOrderID("targets:1", "SPY", event.OrderSideBuy)
	l := newBoostLog(t)
	l.decide(0)
	l.decide(1, never)
	l.add(event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared:twice", day(2), event.RebalanceClearedPayload{
		AccountID: "joint", AsOf: day(2), DecisionID: "targets:1", ClientOrderIDs: []string{never, "zz-unreleased"}, Operator: "owner", Detail: "never sent"})
	a, ok := stateAtStart(t, l.stream)
	if !ok || !strings.Contains(a.Body, "cleared:twice") {
		t.Errorf("a refused clearance must be named on the page: %+v, %v", a, ok)
	}
}

// TestAMomentumClearanceIsNeverARefusedBoostClearance: a momentum account has
// no Boost orders, and the momentum engine applies a clearance through its
// own order reports, so a valid clearance there names no refusal and pages
// nothing (ADR 0026, point 2, "Boost", amended 2026-10-18).
func TestAMomentumClearanceIsNeverARefusedBoostClearance(t *testing.T) {
	s := &stream{t: t}
	s.add(event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", day(0).Add(-time.Hour), momentumConfiguration())
	s.add(event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared:m", day(2), event.RebalanceClearedPayload{
		AccountID: "ira-traditional", AsOf: day(2), DecisionID: "targets:1", ClientOrderIDs: []string{"tw-1"}, Operator: "owner", Detail: "never sent"})
	if a, ok, err := report.StateAtStartAlert(s.out, nil); err != nil || ok {
		t.Errorf("a momentum clearance paged: %+v, %v, %v", a, ok, err)
	}
}

// TestAnUnreadableClearanceIsCountedAsAnUnreadableRecordInTheStanding: the
// engine refuses a clearance it cannot decode or validate, so on a Boost
// account the page counts it as an unreadable record (ADR 0026, point 2,
// "Boost", amended 2026-10-18).
func TestAnUnreadableClearanceIsCountedAsAnUnreadableRecordInTheStanding(t *testing.T) {
	l := newBoostLog(t)
	l.decide(0)
	l.add(event.RebalanceClearedEventType, 99, "cleared:bad", day(1), map[string]string{})
	a, ok := stateAtStart(t, l.stream)
	if !ok || !strings.Contains(a.Body, "cleared:bad") || !strings.Contains(a.Body, "cannot be read") {
		t.Errorf("an unreadable clearance must be counted as an unreadable record: %+v, %v", a, ok)
	}
}

// TestAClearanceAfterAFreshRunsApprovalIsJudgedBeforeItsFirstDecision: a
// Boost account is the one whose configuration names the Boost strategy,
// not the one that has journalled a decision since the last fresh run, so a
// clearance journalled after a fresh run's approval and before its first
// decision is judged as the engine judges it: refused (ADR 0026, point 2,
// "Boost", amended 2026-10-18; ADR 0027).
func TestAClearanceAfterAFreshRunsApprovalIsJudgedBeforeItsFirstDecision(t *testing.T) {
	for name, clearance := range map[string]func(*boostLog){
		"unreadable": func(l *boostLog) {
			l.add(event.RebalanceClearedEventType, 99, "cleared:bad", day(2), map[string]string{})
		},
		"refused": func(l *boostLog) {
			l.add(event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared:bad", day(2), event.RebalanceClearedPayload{
				AccountID: "joint", AsOf: day(2), DecisionID: "targets:1", ClientOrderIDs: []string{"tw-7"}, Operator: "owner", Detail: "never sent"})
		},
	} {
		l := newBoostLog(t)
		l.decide(0, "tw-7")
		l.add(event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approved:2", day(2), *approvalOf("", ""))
		clearance(l)
		a, ok := stateAtStart(t, l.stream)
		if !ok || !strings.Contains(a.Body, "cleared:bad") {
			t.Errorf("%s clearance before the fresh run's first decision must be named: %+v, %v", name, a, ok)
		}
	}
}

// TestARetiredOrderIsNoRejectionInTheStanding: an order retired as never
// sent leaves the orders awaiting the venue's word, so a later report on it,
// which the engine refuses, counts for nothing on the page: a retired order
// is never named as a rejection that stops buys (ADR 0026, point 2,
// "Boost", amended 2026-10-22).
func TestARetiredOrderIsNoRejectionInTheStanding(t *testing.T) {
	build := func(retired bool) *boostLog {
		l := newBoostLog(t)
		l.decide(0, "tw-6")
		l.report(1, "tw-6", event.OrderLifecycleStatusInvalid)
		l.inARow, l.undecided = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-tw-6"}}, nil
		l.decide(1, "tw-7")
		// None of these is a retirement the engine applies: one it cannot
		// read, another account's, one naming no released order and one
		// that does not describe tw-7 as released.
		bad := event.OrderRetiredPayload{AccountID: "joint", ClientOrderID: "tw-7", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 10, LimitPrice: 101,
			Reason: event.OrderRetiredSessionOver, Detail: "session over", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: day(2)}
		l.add(event.OrderRetiredEventType, 99, "retired:unread", day(2), map[string]string{})
		other, unknown, moved := bad, bad, bad
		other.AccountID, unknown.ClientOrderID, moved.Quantity = "ira", "tw-unknown", 11
		for id, p := range map[string]event.OrderRetiredPayload{"retired:other": other, "retired:unknown": unknown, "retired:moved": moved} {
			l.add(event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, id, day(2), p)
		}
		if retired {
			l.add(event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:1", day(2), event.OrderRetiredPayload{
				AccountID: "joint", ClientOrderID: "tw-7", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 10, LimitPrice: 101,
				Reason: event.OrderRetiredSessionOver, Detail: "session over", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: day(2)})
		}
		l.report(3, "tw-7", event.OrderLifecycleStatusInvalid)
		return l
	}
	if a, ok := stateAtStart(t, build(false).stream); !ok || !strings.Contains(a.Body, "tw-7") {
		t.Fatalf("without a retirement the engine applies, the second rejection stops buys: %+v", a)
	}
	if a, ok := stateAtStart(t, build(true).stream); ok {
		t.Errorf("a report on a retired order is no rejection: %+v", a)
	}
}

// TestARetirementOfAnOrderTheVenueNamedLeavesItsRejectionCounted: an order
// the count holds that IBKR has named, even only as submitted, was sent, so
// the engine refuses its retirement and its rejection still counts, as on
// the page (ADR 0026, point 2, "Boost", amended 2026-10-22; fail closed).
func TestARetirementOfAnOrderTheVenueNamedLeavesItsRejectionCounted(t *testing.T) {
	l := newBoostLog(t)
	l.decide(0, "tw-6")
	l.report(1, "tw-6", event.OrderLifecycleStatusInvalid)
	l.inARow, l.undecided = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-tw-6"}}, nil
	l.decide(1, "tw-7")
	l.report(2, "tw-7", event.OrderLifecycleStatusSubmitted)
	l.add(event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:1", day(2), event.OrderRetiredPayload{
		AccountID: "joint", ClientOrderID: "tw-7", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 10, LimitPrice: 101,
		Reason: event.OrderRetiredSessionOver, Detail: "session over", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: day(2)})
	l.report(3, "tw-7", event.OrderLifecycleStatusInvalid)
	if a, ok := stateAtStart(t, l.stream); !ok || !strings.Contains(a.Body, "tw-7") {
		t.Errorf("the retirement of an order IBKR named was applied, and its rejection not counted: %+v", a)
	}
}

// TestARetirementOfAnOrderIBKRListedLeavesItsRejectionCounted: a broker
// snapshot listing an order the count holds shows it reached IBKR, so the
// engine refuses its retirement and its later rejection still counts, as on
// the page (ADR 0026, point 2, "Boost", amended 2026-10-22; fail closed).
func TestARetirementOfAnOrderIBKRListedLeavesItsRejectionCounted(t *testing.T) {
	l := newBoostLog(t)
	l.decide(0, "tw-6")
	l.report(1, "tw-6", event.OrderLifecycleStatusInvalid)
	l.inARow, l.undecided = []event.BoostRejectedOrder{{ClientOrderID: "tw-6", VenueOrderID: "ib-tw-6"}}, nil
	l.decide(1, "tw-7")
	l.add(event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "broker:1", day(2), event.BrokerSnapshotPayload{
		AccountID: "joint", AsOf: day(2), NetLiquidation: 10000, ExcessLiquidity: 10000, Positions: []event.BrokerPosition{},
		OpenOrders: []event.BrokerOrder{{BrokerOrderID: "ib-tw-7", ClientOrderID: "tw-7", InstrumentID: "SPY", Side: event.OrderSideBuy,
			Status: event.BrokerOrderWorking, RemainingQuantity: 10}}})
	l.add(event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:1", day(2), event.OrderRetiredPayload{
		AccountID: "joint", ClientOrderID: "tw-7", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 10, LimitPrice: 101,
		Reason: event.OrderRetiredSessionOver, Detail: "session over", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: day(2)})
	l.report(3, "tw-7", event.OrderLifecycleStatusInvalid)
	if a, ok := stateAtStart(t, l.stream); !ok || !strings.Contains(a.Body, "tw-7") {
		t.Errorf("the retirement of an order IBKR listed was applied, and its rejection not counted: %+v", a)
	}
}

// TestAnApprovalOfEveryVersionAnEngineRecordedIsRead: the server's logs hold
// approvals an earlier build recorded, at schema version 5 among them, so
// the reporter reads each version an engine has recorded, the current one
// included (ADR 0015).
func TestAnApprovalOfEveryVersionAnEngineRecordedIsRead(t *testing.T) {
	for version := uint32(1); version <= event.OperatorRunApprovedSchemaVersion; version++ {
		s := newStream(t)
		p := approvalOf("run-0.log", "sha256:"+strings.Repeat("0", 64))
		if version < 3 {
			// Versions 1 and 2 hold no starting book key.
			s.add(event.OperatorRunApprovedEventType, version, "operator-run-approved:x", day(0), map[string]any{
				"account_id": "joint", "previous_log": "run-0.log", "previous_log_final_hash": "sha256:" + strings.Repeat("0", 64),
				"incident_id": "", "approved_by": "owner", "approved_at": day(0), "boost": map[string]any{"latched": false}, "incident_in_force": nil,
			})
		} else {
			s.add(event.OperatorRunApprovedEventType, version, "operator-run-approved:x", day(0), *p)
		}
		if _, err := report.Alerts(s.out, "boost"); err != nil {
			t.Errorf("alerts over an approval at schema version %d: %v", version, err)
		}
		if _, _, err := report.StateAtStartAlert(s.out, nil); err != nil {
			t.Errorf("an approval at schema version %d: %v", version, err)
		}
	}
}
