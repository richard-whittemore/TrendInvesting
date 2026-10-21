package report_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// caused marks the stream's last record as caused by input, as the engine
// stamps every decision with the input that produced it.
func (s *stream) caused(input string) { s.out[len(s.out)-1].CausationID = input }

// mismatch records a morning reconciliation that found SPY 7 short and the
// cash $12.30 short, its result, and the Degraded state it caused.
func (s *stream) mismatch(n int, input string) {
	s.add(event.ReconciliationEventType, event.ReconciliationSchemaVersion, input, day(n), event.ReconciliationPayload{
		AccountID: "joint", ReconciliationID: "morning:" + input, Trigger: event.ReconciliationTriggerMorning, AsOf: day(n),
		Unreadable: []string{}, Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 140}}, TotalCash: -4712.3, Currency: "USD",
	})
	s.add(event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "reconciliation-result:joint:"+input, day(n), event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "morning:" + input, InputID: input, AnchorID: "snapshot:0",
		Classification: event.ReconciliationUnexplained, Action: event.ReconciliationActionDegrade, CashCompared: true, Currency: "USD",
		Components: []event.ReconciliationComponent{
			{Component: event.ReconciliationComponentCash, Book: -470000, Expected: -470000, Observed: -471230, Residual: -1230, Explanations: []string{}},
			{Component: event.ReconciliationComponentPosition, InstrumentID: "SPY", Book: 147, Expected: 147, Observed: 140, Residual: -7, Explanations: []string{}},
		},
		ScopeInstrumentIDs: []string{"SPY"}, ScopeCashComponents: []string{event.ReconciliationComponentCash},
		Detail: "unexplained: total cash: expected -4700.00 USD, broker -4712.30 (residual -12.30); SPY: book 147, expected 147, broker 140 (residual -7)",
	})
	s.caused(input)
	s.add(event.EngineStateEventType, event.EngineStateScopedVersion, "engine-state:joint:"+input, day(n), event.EngineStatePayload{
		State: event.EngineStateDegraded, Reason: event.EngineStateReasonReconciliationUnexplained, Detail: "reconciliation morning " + input,
		ScopeInstrumentIDs: []string{"SPY"}, ScopeCashComponents: []string{event.ReconciliationComponentCash},
	})
	s.caused(input)
}

func TestAMismatchIsOneAlertThatRepeatsUntilAcknowledged(t *testing.T) {
	s := newStream(t)
	s.mismatch(1, "reconciliation:1")
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	// The state record the mismatch caused is the same incident: one alert.
	if len(got) != 1 || got[0].Kind != report.KindReconciliation || got[0].Severity != report.Urgent {
		t.Fatalf("alerts = %+v", got)
	}
	a := got[0]
	if !a.Repeat || a.Acknowledged {
		t.Errorf("a mismatch repeats until acknowledged: %+v", a)
	}
	for _, want := range []string{"SPY", "-12.30", "Degraded", "frozen", "docs/runbooks/reconciliation.md", a.Key} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, a.Body)
		}
	}
	// The public relay's text names what is in doubt, never a balance.
	if a.PublicBody == "" || strings.Contains(a.PublicBody, "4712") || strings.Contains(a.PublicBody, "12.30") || !strings.Contains(a.PublicBody, "SPY") || !strings.Contains(a.PublicBody, a.Key) {
		t.Errorf("public body:\n%s", a.PublicBody)
	}

	s.add(event.AlertAcknowledgedEventType, event.AlertAcknowledgedSchemaVersion, "ack:1", day(2), event.AlertAcknowledgedPayload{
		AccountID: "joint", AlertKey: a.Key, AcknowledgedBy: "owner", AcknowledgedAt: day(2),
	})
	got, err = report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Acknowledged {
		t.Fatalf("the acknowledgement is recorded on its alert: %+v", got)
	}

	// A second mismatch is a second alert, unacknowledged.
	s.mismatch(3, "reconciliation:2")
	got, _ = report.Alerts(s.out)
	if len(got) != 2 || got[1].Acknowledged || got[1].Key == got[0].Key {
		t.Fatalf("a later mismatch: %+v", got)
	}
}

func TestAMatchingReconciliationSendsNothing(t *testing.T) {
	s := newStream(t)
	s.add(event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "reconciliation-result:joint:r", day(1), event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "morning", InputID: "r", AnchorID: "snapshot:0",
		Classification: event.ReconciliationMatched, Action: event.ReconciliationActionContinue,
		Components: []event.ReconciliationComponent{{Component: event.ReconciliationComponentPosition, InstrumentID: "SPY", Book: 147, Expected: 147, Observed: 147, Explanations: []string{}, Explained: true}},
		Detail:     "matched",
	})
	if got, err := report.Alerts(s.out); err != nil || len(got) != 0 {
		t.Fatalf("a match alerts nothing: %+v, %v", got, err)
	}
}

func TestAnUnverifiableReconciliationSaysHalted(t *testing.T) {
	s := newStream(t)
	s.add(event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "reconciliation-result:joint:r", day(1), event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "morning", InputID: "r", Classification: event.ReconciliationUnverifiable, Action: event.ReconciliationActionHalt,
		Components: []event.ReconciliationComponent{}, Unverifiable: []string{"the broker's positions could not be read"}, Detail: "unverifiable",
	})
	got, err := report.Alerts(s.out)
	if err != nil || len(got) != 1 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	if !strings.Contains(got[0].Body, "Halted") || !strings.Contains(got[0].Body, "could not be read") {
		t.Errorf("body:\n%s", got[0].Body)
	}
	if !strings.Contains(got[0].PublicBody, "Halted") || !strings.Contains(got[0].PublicBody, "could not be verified") {
		t.Errorf("public body:\n%s", got[0].PublicBody)
	}
}

// TestAnUnverifiableReasonNeverReachesThePublicRelay: a reason the
// reconciliation could not compare can carry cents, dollars and share counts,
// and the public relay carries no figure (owner decision 2026-10-17), so the
// public text says only that the reconciliation could not be verified and
// sends the owner to the private channel, which carries the reasons.
func TestAnUnverifiableReasonNeverReachesThePublicRelay(t *testing.T) {
	reasons := []string{"SPY: the broker reports 140 shares and a cost of 4712.30 USD, which do not agree", "total cash: 8530.00 USD is in a second currency"}
	s := newStream(t)
	s.add(event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "reconciliation-result:joint:r", day(1), event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "morning", InputID: "r", Classification: event.ReconciliationUnverifiable, Action: event.ReconciliationActionHalt,
		Components: []event.ReconciliationComponent{}, Unverifiable: reasons, Detail: "unverifiable",
	})
	got, err := report.Alerts(s.out)
	if err != nil || len(got) != 1 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	for _, reason := range reasons {
		if !strings.Contains(got[0].Body, reason) {
			t.Errorf("the private body carries the reason %q:\n%s", reason, got[0].Body)
		}
	}
	for _, figure := range []string{"140", "4712", "8530", "SPY: the broker", "second currency"} {
		if strings.Contains(got[0].PublicBody, figure) {
			t.Errorf("the public body carries %q from a reason:\n%s", figure, got[0].PublicBody)
		}
	}
	if !strings.Contains(got[0].PublicBody, "could not be verified; see the private channel") {
		t.Errorf("the public body sends the owner to the private channel:\n%s", got[0].PublicBody)
	}
}

func TestAnEngineStateAlertRepeatsAndAnAcknowledgementStopsIt(t *testing.T) {
	s := newStream(t)
	s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	s.refused(0, "tw-a", event.RefusedOrderSize)
	got, _ := report.Alerts(s.out)
	if len(got) != 2 || !got[0].Repeat || got[1].Repeat {
		t.Fatalf("the state repeats and the refusal does not: %+v", got)
	}
	if !strings.Contains(got[0].Body, got[0].Key) {
		t.Errorf("a repeating alert states the key that acknowledges it: %s", got[0].Body)
	}
	s.add(event.AlertAcknowledgedEventType, event.AlertAcknowledgedSchemaVersion, "ack", day(1), event.AlertAcknowledgedPayload{
		AccountID: "joint", AlertKey: got[0].Key, AcknowledgedBy: "owner", AcknowledgedAt: day(1),
	})
	if got, _ = report.Alerts(s.out); !got[0].Acknowledged || got[1].Acknowledged {
		t.Errorf("only the named alert is acknowledged: %+v", got)
	}
}

func TestReconciliationRecordsAreReadLikeAnyOther(t *testing.T) {
	for name, eventType := range map[string]string{
		"result": event.ReconciliationResultEventType, "acknowledgement": event.AlertAcknowledgedEventType,
		"approval": event.OperatorRunApprovedEventType, "caused state": event.EngineStateEventType,
	} {
		s := newStream(t)
		s.add(eventType, 9, "x", day(1), map[string]string{})
		s.caused("reconciliation:1")
		if _, err := report.Alerts(s.out); err == nil || !strings.Contains(err.Error(), "schema version 9") {
			t.Errorf("%s at an unknown version: %v", name, err)
		}
	}
}

func TestDueRepeatsOnlyWhatIsUnacknowledged(t *testing.T) {
	a := report.Alert{Key: "k", Repeat: true}
	at := day(0)
	if !report.Due(a, false, at, at, 0) {
		t.Error("an alert never delivered is due")
	}
	if report.Due(a, true, at, at.Add(29*time.Minute), 30*time.Minute) {
		t.Error("a repeat is not due before its interval")
	}
	if !report.Due(a, true, at, at.Add(30*time.Minute), 30*time.Minute) {
		t.Error("a repeat is due at its interval")
	}
	a.Acknowledged = true
	if report.Due(a, true, at, at.Add(time.Hour), 30*time.Minute) {
		t.Error("an acknowledged alert is not repeated")
	}
	if report.Due(report.Alert{Key: "once"}, true, at, at.Add(time.Hour), 30*time.Minute) {
		t.Error("an alert that does not repeat is sent once")
	}
}

// mismatchAlone records mismatch's input and result with no engine-state
// record: the gate records none when the account's state does not move, as
// when it is already Halted or already Degraded over the whole scope.
func (s *stream) mismatchAlone(n int, input string) {
	s.mismatch(n, input)
	s.out = s.out[:len(s.out)-1]
}

// TestAReconciliationAlertStatesTheAccountsActualState: every alert states
// the state and what the system still does and has stopped doing (ADR 0019,
// "Alerting"), so a reconciliation alert describes the gate's state after
// it, not the result's own view: an account already Halted stays Halted (a
// Degraded result never loosens it), and a Degraded scope is the gate's
// whole scope, the union with what was already frozen.
func TestAReconciliationAlertStatesTheAccountsActualState(t *testing.T) {
	halted := newStream(t)
	halted.engineState(0, event.EngineStateHalted, event.EngineStateReasonReconciliationUnverifiable)
	halted.mismatchAlone(1, "reconciliation:1")
	got, err := report.Alerts(halted.out)
	if err != nil || len(got) != 2 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	for _, body := range []string{got[1].Body, got[1].PublicBody} {
		if !strings.Contains(body, "The account is Halted") || strings.Contains(body, "Degraded on") || strings.Contains(body, "sells elsewhere continue") {
			t.Errorf("an account already Halted is told it is Halted:\n%s", body)
		}
		if !strings.Contains(body, "SPY") {
			t.Errorf("the alert still names what is in doubt:\n%s", body)
		}
	}

	wider := newStream(t)
	wider.engineState(0, event.EngineStateDegraded, event.EngineStateReasonRiskLimitBreached, "TLT")
	wider.mismatch(1, "reconciliation:1")
	last := wider.out[len(wider.out)-1]
	var p event.EngineStatePayload
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	p.ScopeInstrumentIDs = []string{"SPY", "TLT"}
	wider.out = wider.out[:len(wider.out)-1]
	wider.add(event.EngineStateEventType, event.EngineStateScopedVersion, last.ID, last.EventTime, p)
	wider.caused("reconciliation:1")
	got, err = report.Alerts(wider.out)
	if err != nil || len(got) != 2 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	if !strings.Contains(got[1].Body, "Degraded on instruments [SPY,TLT] and cash [total-cash]") {
		t.Errorf("the gate's whole scope:\n%s", got[1].Body)
	}

	unchanged := newStream(t)
	unchanged.engineState(0, event.EngineStateDegraded, event.EngineStateReasonRiskLimitBreached, "SPY", "TLT")
	unchanged.mismatchAlone(1, "reconciliation:1")
	got, err = report.Alerts(unchanged.out)
	if err != nil || len(got) != 2 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	if !strings.Contains(got[1].Body, "Degraded on instruments [SPY,TLT] and cash [total-cash]") {
		t.Errorf("an unchanged state is the earlier scope with this result's:\n%s", got[1].Body)
	}

	// With no state record at all, the result's own scope is the state.
	alone := newStream(t)
	alone.mismatchAlone(1, "reconciliation:1")
	if got, err = report.Alerts(alone.out); err != nil || len(got) != 1 || !strings.Contains(got[0].Body, "Degraded on instruments [SPY] and cash [total-cash]") {
		t.Errorf("no state record: %+v, %v", got, err)
	}

	// A fresh run's approval carries the incident in force; the reporter
	// reads it as the state the run starts in.
	approved := newStream(t)
	approved.add(event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "operator-run-approved:x", day(0), event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
		ApprovedBy: "owner", ApprovedAt: day(0), Boost: event.BoostStateInForce{Latched: new(false)},
		IncidentInForce: &event.IncidentInForce{IncidentID: "engine-state:joint:0", State: event.EngineStateHalted, SaleAttempted: new(true)},
	})
	approved.mismatchAlone(1, "reconciliation:1")
	got, err = report.Alerts(approved.out)
	if err != nil || len(got) != 1 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	if !strings.Contains(got[0].Body, "The account is Halted") {
		t.Errorf("an approved Halted incident stays Halted:\n%s", got[0].Body)
	}

	// An approval that carries no incident in force starts the run clear:
	// the earlier run's Halted state does not outlive it.
	cleared := newStream(t)
	cleared.add(event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "operator-run-approved:a", day(0), event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
		ApprovedBy: "owner", ApprovedAt: day(0), Boost: event.BoostStateInForce{Latched: new(false)},
		IncidentInForce: &event.IncidentInForce{IncidentID: "engine-state:joint:0", State: event.EngineStateHalted, SaleAttempted: new(true)},
	})
	cleared.add(event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "operator-run-approved:b", day(1), event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-1.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
		ApprovedBy: "owner", ApprovedAt: day(1), Boost: event.BoostStateInForce{Latched: new(false)},
	})
	cleared.mismatchAlone(2, "reconciliation:1")
	got, err = report.Alerts(cleared.out)
	if err != nil || len(got) != 1 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	if strings.Contains(got[0].Body, "The account is Halted") || !strings.Contains(got[0].Body, "Degraded on instruments [SPY]") {
		t.Errorf("an approval with no incident clears the earlier run's state:\n%s", got[0].Body)
	}
}

// TestAnApprovalIsReadAtItsRecordedSchemaVersion: a version 1 approval could
// not state an incident's cash scope, so the reporter refuses one that names
// it rather than reading it as a version 2 approval (ADR 0015).
func TestAnApprovalIsReadAtItsRecordedSchemaVersion(t *testing.T) {
	payload := event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
		ApprovedBy: "owner", ApprovedAt: day(0), Boost: event.BoostStateInForce{Latched: new(false)},
		IncidentInForce: &event.IncidentInForce{
			IncidentID: "engine-state:joint:0", State: event.EngineStateDegraded,
			ScopeCashComponents: []string{event.ReconciliationComponentCash}, SaleAttempted: new(true),
		},
	}
	v2 := newStream(t)
	v2.add(event.OperatorRunApprovedEventType, 2, "operator-run-approved:x", day(0), payload)
	v2.mismatchAlone(1, "reconciliation:1")
	if _, err := report.Alerts(v2.out); err != nil {
		t.Errorf("a version 2 approval naming a cash scope: %v", err)
	}
	v1 := newStream(t)
	v1.add(event.OperatorRunApprovedEventType, 1, "operator-run-approved:x", day(0), payload)
	v1.mismatchAlone(1, "reconciliation:1")
	if _, err := report.Alerts(v1.out); err == nil || !strings.Contains(err.Error(), "cash") {
		t.Errorf("a version 1 approval naming a cash scope was read: %v", err)
	}
}

// pendingRead records an IRA-style reconciliation at day(n) that found only
// the cash different, by residual cents, on a day the Flex reader has not
// read, and its pending-cash-read result, which alerts and moves no state
// (ADR 0019, amended 2026-10-21). repeatOf, when set, names the reading that
// first found the same difference.
func (s *stream) pendingRead(n int, input string, residual int64, repeatOf string) {
	s.add(event.ReconciliationEventType, event.ReconciliationSchemaVersion, input, day(n), event.ReconciliationPayload{
		AccountID: "joint", ReconciliationID: "close:" + input, Trigger: event.ReconciliationTriggerMorning, AsOf: day(n),
		Unreadable: []string{}, Positions: []event.AccountPosition{}, TotalCash: float64(100000+residual) / 100, Currency: "USD",
	})
	s.add(event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "reconciliation-result:joint:"+input, day(n), event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "close:" + input, InputID: input, AnchorID: "snapshot:0",
		Classification: event.ReconciliationPendingCashRead, Action: event.ReconciliationActionAlert, CashCompared: true, Currency: "USD",
		Components: []event.ReconciliationComponent{
			{Component: event.ReconciliationComponentCash, Book: 100000, Expected: 100000, Observed: 100000 + residual, Residual: residual, Explanations: []string{}},
		},
		Detail: "pending-cash-read: total cash: the gate's detail", RepeatOf: repeatOf,
	})
	s.caused(input)
}

// TestAPendingCashReadStatesTheTruth: a pending cash read moves no state
// (ADR 0019, amended 2026-10-21), so its alert says how far the cash differs
// and which way, on what day, that the Flex reader has not covered that day,
// the state the account stays in, whether buys continue, and when it becomes
// Degraded; never that the account is Degraded. Its public text carries no
// figure (owner decision 2026-10-17).
func TestAPendingCashReadStatesTheTruth(t *testing.T) {
	increase := newStream(t)
	increase.pendingRead(1, "reconciliation:1", 50000, "")
	got, err := report.Alerts(increase.out)
	if err != nil || len(got) != 1 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	want := "joint: cash differs from the book by 500.00 USD (an increase, like a deposit) on 2024-01-03; the Flex reader has not covered that day yet; the account stays Normal; buys continue, sized without the increase until the reader explains it; it becomes Degraded if unexplained after the Flex reader covers 2024-01-03 or more than 2 sessions close since, whichever comes first."
	a := got[0]
	if !strings.HasPrefix(a.Body, want) {
		t.Errorf("body:\n%s\nwant it to start:\n%s", a.Body, want)
	}
	if a.Title != "joint: reconciliation pending-cash-read (close:reconciliation:1)" || a.Kind != report.KindReconciliation {
		t.Errorf("title %q, kind %q", a.Title, a.Kind)
	}
	publicWant := "joint: cash differs from the book (an increase, like a deposit) on 2024-01-03; the Flex reader has not covered that day yet; the account stays Normal; buys continue"
	if !strings.HasPrefix(a.PublicBody, publicWant) || strings.Contains(a.PublicBody, "500") || !strings.Contains(a.PublicBody, a.Key) {
		t.Errorf("public body:\n%s", a.PublicBody)
	}
	for _, body := range []string{a.Body, a.PublicBody} {
		if strings.Contains(body, "Degraded on") || strings.Contains(body, "is Degraded") {
			t.Errorf("a pending cash read says the account is Degraded:\n%s", body)
		}
	}

	decrease := newStream(t)
	decrease.engineState(0, event.EngineStateDegraded, event.EngineStateReasonRiskLimitBreached, "TLT")
	decrease.pendingRead(1, "reconciliation:1", -25050, "")
	got, err = report.Alerts(decrease.out)
	if err != nil || len(got) != 2 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	want = "joint: cash differs from the book by 250.50 USD (a decrease, like a withdrawal) on 2024-01-03; the Flex reader has not covered that day yet; the account stays Degraded on instruments [TLT]; buys are withheld until the reader explains it, and sells and exits continue; it becomes Degraded if unexplained after the Flex reader covers 2024-01-03 or more than 2 sessions close since, whichever comes first."
	if !strings.HasPrefix(got[1].Body, want) {
		t.Errorf("body:\n%s\nwant it to start:\n%s", got[1].Body, want)
	}

	// A Degraded state names each part of its scope; a result whose input
	// this build does not read names its own day.
	cash := newStream(t)
	cash.add(event.EngineStateEventType, event.EngineStateScopedVersion, "state:0", day(0), event.EngineStatePayload{
		State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "figures",
		ScopeInstrumentIDs: []string{"SPY"}, ScopeCashComponents: []string{event.ReconciliationComponentCash},
	})
	cash.pendingRead(4, "reconciliation:1", -100, "")
	cash.out[len(cash.out)-2].SchemaVersion = 9
	cash.out[len(cash.out)-1].EventTime = day(1)
	got, err = report.Alerts(cash.out)
	if err != nil || len(got) != 2 || !strings.Contains(got[1].Body, "on 2024-01-03; the Flex reader has not covered that day yet; the account stays Degraded on instruments [SPY] and cash [total-cash]; buys are withheld") {
		t.Errorf("a Degraded scope with cash, and an unread input: %+v, %v", got, err)
	}

	halted := newStream(t)
	halted.engineState(0, event.EngineStateHalted, event.EngineStateReasonReconciliationUnverifiable)
	halted.pendingRead(1, "reconciliation:1", -100, "")
	if got, err = report.Alerts(halted.out); err != nil || len(got) != 2 || !strings.Contains(got[1].Body, "; the account stays Halted;") {
		t.Errorf("an account already Halted stays Halted: %+v, %v", got, err)
	}
}

// TestAPendingCashDifferenceIsDecidedWithItsOwnBody: a later reading of the
// same pending difference stays silent, the decision that explains it sends
// nothing (an explained result continues), and the decision that degrades
// it keeps the Degraded reconciliation alert (ADR 0019, amended 2026-10-21:
// one difference, one reconciliation alert).
func TestAPendingCashDifferenceIsDecidedWithItsOwnBody(t *testing.T) {
	s := newStream(t)
	s.pendingRead(1, "reconciliation:1", 50000, "")
	s.pendingRead(2, "reconciliation:2", 50000, "close:reconciliation:1")
	s.add(event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "reconciliation-result:joint:3:pending-cash:reconciliation:1", day(3), event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "close:reconciliation:1", InputID: "reconciliation:1", AnchorID: "snapshot:0",
		Classification: event.ReconciliationExplained, Action: event.ReconciliationActionContinue, CashCompared: true, Currency: "USD",
		Components: []event.ReconciliationComponent{
			{Component: event.ReconciliationComponentCash, Book: 100000, Expected: 150000, Observed: 150000, Explanations: []string{"cash-movement:1"}, Explained: true},
		},
		Detail: "explained: the cash difference the reader has since recorded",
	})
	s.caused("reconciliation:3")
	got, err := report.Alerts(s.out)
	if err != nil || len(got) != 1 || got[0].Key != "reconciliation:reconciliation-result:joint:reconciliation:1@joint" {
		t.Fatalf("the first reading pages, its repeat and its explanation do not: %+v, %v", got, err)
	}

	degraded := newStream(t)
	degraded.pendingRead(1, "reconciliation:1", -25050, "")
	degraded.add(event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, "reconciliation-result:joint:4:pending-cash:reconciliation:1", day(4), event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "close:reconciliation:1", InputID: "reconciliation:1", AnchorID: "snapshot:0",
		Classification: event.ReconciliationUnexplained, Action: event.ReconciliationActionDegrade, CashCompared: true, Currency: "USD",
		Components: []event.ReconciliationComponent{
			{Component: event.ReconciliationComponentCash, Book: 100000, Expected: 100000, Observed: 74950, Residual: -25050, Explanations: []string{}},
		},
		ScopeCashComponents: []string{event.ReconciliationComponentCash},
		Detail:              "unexplained: total cash: the difference found pending a reading is not explained",
	})
	degraded.caused("reconciliation:4")
	degraded.add(event.EngineStateEventType, event.EngineStateScopedVersion, "engine-state:joint:4", day(4), event.EngineStatePayload{
		State: event.EngineStateDegraded, Reason: event.EngineStateReasonReconciliationUnexplained, Detail: "reconciliation close",
		ScopeCashComponents: []string{event.ReconciliationComponentCash},
	})
	degraded.caused("reconciliation:4")
	got, err = report.Alerts(degraded.out)
	if err != nil || len(got) != 2 {
		t.Fatalf("alerts = %+v, %v", got, err)
	}
	if !strings.Contains(got[1].Body, "The account is Degraded on instruments [] and cash [total-cash]") || strings.Contains(got[1].Body, "stays") {
		t.Errorf("the Degraded decision keeps its body:\n%s", got[1].Body)
	}
}
