package report_test

import (
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
	for _, body := range []string{got[0].Body, got[0].PublicBody} {
		if !strings.Contains(body, "Halted") || !strings.Contains(body, "could not be read") {
			t.Errorf("body:\n%s", body)
		}
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
	for name, version := range map[string]uint32{"result": 9, "acknowledgement": 9} {
		s := newStream(t)
		eventType := event.ReconciliationResultEventType
		if name == "acknowledgement" {
			eventType = event.AlertAcknowledgedEventType
		}
		s.add(eventType, version, "x", day(1), map[string]string{})
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
