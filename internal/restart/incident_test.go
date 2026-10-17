package restart_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// engineStateEntry is a recorded strategy.engine.state decision.
func engineStateEntry(t *testing.T, p event.EngineStatePayload) journal.Entry {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return journal.Entry{Kind: journal.KindDecision, Envelope: event.Envelope{
		ID: "engine-state:joint:1", Type: event.EngineStateEventType, SchemaVersion: event.EngineStateScopedVersion, Payload: raw,
	}}
}

// TestAnApprovalNeverLoosensThePreviousRunsState: the previous log's
// strategy.engine.state records are the evidence of the state it ended in,
// and a fresh run resumes in that state and scope; leaving Degraded or
// Halted is never automatic (ADR 0019, amended 2026-09-24, "The state
// survives a restart"). So an approval of a run that ended Degraded or
// Halted either clears the incident by its ID (ADR 0019's recovery
// procedure) or carries it in force at least as tight as it ended.
func TestAnApprovalNeverLoosensThePreviousRunsState(t *testing.T) {
	degraded := func(ids ...string) journal.Entry {
		return engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x", ScopeInstrumentIDs: ids})
	}
	halted := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x"})
	inForce := func(state string, ids ...string) *event.IncidentInForce {
		return &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: state, ScopeInstrumentIDs: ids, SaleAttempted: new(false)}
	}
	for _, c := range []struct {
		name    string
		entries []journal.Entry
		clears  string
		in      *event.IncidentInForce
		refusal string
	}{
		{"normal, none in force", nil, "", nil, ""},
		{"degraded, cleared", []journal.Entry{degraded("SPY")}, "engine-state:joint:1", nil, ""},
		{"degraded, carried", []journal.Entry{degraded("SPY")}, "", inForce(event.EngineStateDegraded, "SPY"), ""},
		{"degraded, carried halted", []journal.Entry{degraded("SPY")}, "", inForce(event.EngineStateHalted), ""},
		{"degraded, scopes merged", []journal.Entry{degraded("SPY"), degraded("TLT")}, "", inForce(event.EngineStateDegraded, "SPY", "TLT"), ""},
		{"halted after degraded, carried", []journal.Entry{degraded("SPY"), halted}, "", inForce(event.EngineStateHalted), ""},
		{"degraded, dropped", []journal.Entry{degraded("SPY")}, "", nil, "ended degraded"},
		{"halted, dropped", []journal.Entry{halted}, "", nil, "ended halted"},
		{"halted, carried degraded", []journal.Entry{halted}, "", inForce(event.EngineStateDegraded, "SPY"), "ended halted"},
		{"degraded, scope narrowed", []journal.Entry{degraded("SPY"), degraded("TLT")}, "", inForce(event.EngineStateDegraded, "SPY"), "TLT"},
		{"degraded, another incident cleared", []journal.Entry{degraded("SPY")}, "incident-9", nil, "engine-state:joint:1"},
		{"halted, another incident cleared", []journal.Entry{halted}, "incident-9", nil, "engine-state:joint:1"},
		{"halted, carried under another id", []journal.Entry{halted}, "", otherIncident(event.EngineStateHalted), "engine-state:joint:1"},
		{"normal, an incident cleared", nil, "incident-9", nil, "no incident"},
		{"normal, an incident carried in force", nil, "", otherIncident(event.EngineStateHalted), "no incident"},
		{"normal, the would-be ID carried in force", nil, "", inForce(event.EngineStateDegraded, "SPY"), "no incident"},
	} {
		approval := event.OperatorRunApprovedPayload{IncidentID: c.clears, IncidentInForce: c.in}
		err := restart.CheckIncidentInForce(c.entries, approval, "run-1.log")
		switch {
		case c.refusal == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refusal != "" && (err == nil || !strings.Contains(err.Error(), c.refusal)):
			t.Errorf("%s: CheckIncidentInForce = %v, want a refusal saying %q", c.name, err, c.refusal)
		}
	}
}

// otherIncident is an incident in force under an ID the logs in these
// tests never record.
func otherIncident(state string, ids ...string) *event.IncidentInForce {
	return &event.IncidentInForce{IncidentID: "engine-state:joint:9", State: state, ScopeInstrumentIDs: ids, SaleAttempted: new(false)}
}

// approvalEntry is a recorded operator.run.approved input.
func approvalEntry(t *testing.T, p event.OperatorRunApprovedPayload) journal.Entry {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return journal.Entry{Kind: journal.KindInput, Envelope: event.Envelope{
		ID: "operator-run-approved:x", Type: event.OperatorRunApprovedEventType, SchemaVersion: event.OperatorRunApprovedSchemaVersion, Payload: raw,
	}}
}

// TestAnInheritedIncidentSurvivesARunThatRecordsNoStateChange: a fresh run
// restores the incident its approval carries in force without recording a
// strategy.engine.state decision, and refusing orders while held records
// none either, so the approved log's own approval is evidence of the state
// it ended in. An approval of that log must clear the incident or carry it
// at least as tight; leaving Degraded or Halted is never automatic (ADR
// 0019, amended 2026-09-24, "The state survives a restart").
func TestAnInheritedIncidentSurvivesARunThatRecordsNoStateChange(t *testing.T) {
	inForce := func(state string, ids ...string) *event.IncidentInForce {
		return &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: state, ScopeInstrumentIDs: ids, SaleAttempted: new(false)}
	}
	inherited := func(in *event.IncidentInForce) journal.Entry {
		return approvalEntry(t, event.OperatorRunApprovedPayload{
			AccountID: "joint", PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
			ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
			Boost: event.BoostStateInForce{Latched: new(false)}, IncidentInForce: in,
		})
	}
	degradedTLT := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x", ScopeInstrumentIDs: []string{"TLT"}})
	degradedTLT.Envelope.ID = "engine-state:joint:2"
	for _, c := range []struct {
		name    string
		entries []journal.Entry
		clears  string
		in      *event.IncidentInForce
		refusal string
	}{
		{"inherited none", []journal.Entry{inherited(nil)}, "", nil, ""},
		{"inherited halted, dropped", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "", nil, "ended halted"},
		{"inherited halted, carried degraded", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "", inForce(event.EngineStateDegraded, "SPY"), "ended halted"},
		{"inherited halted, carried", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "", inForce(event.EngineStateHalted), ""},
		{"inherited halted, cleared", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "engine-state:joint:1", nil, ""},
		{"inherited degraded, dropped", []journal.Entry{inherited(inForce(event.EngineStateDegraded, "SPY"))}, "", nil, "ended degraded"},
		{"inherited degraded, widened later", []journal.Entry{inherited(inForce(event.EngineStateDegraded, "SPY")), degradedTLT}, "", inForce(event.EngineStateDegraded, "TLT"), "SPY"},
		{"inherited degraded, widened later, carried", []journal.Entry{inherited(inForce(event.EngineStateDegraded, "SPY")), degradedTLT}, "", inForce(event.EngineStateDegraded, "SPY", "TLT"), ""},
		{"inherited halted, degraded later", []journal.Entry{inherited(inForce(event.EngineStateHalted)), degradedTLT}, "", inForce(event.EngineStateDegraded, "TLT"), "ended halted"},
		{"inherited halted, another incident cleared", []journal.Entry{inherited(inForce(event.EngineStateHalted))}, "engine-state:joint:9", nil, "engine-state:joint:1"},
		{"inherited degraded, widened later, the later record cleared", []journal.Entry{inherited(inForce(event.EngineStateDegraded, "SPY")), degradedTLT}, "engine-state:joint:2", nil, "engine-state:joint:1"},
	} {
		approval := event.OperatorRunApprovedPayload{IncidentID: c.clears, IncidentInForce: c.in}
		err := restart.CheckIncidentInForce(c.entries, approval, "run-1.log")
		switch {
		case c.refusal == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refusal != "" && (err == nil || !strings.Contains(err.Error(), c.refusal)):
			t.Errorf("%s: CheckIncidentInForce = %v, want a refusal saying %q", c.name, err, c.refusal)
		}
	}
}

// TestAnUndecodableRecordRefusesTheIncidentCheck: the approved log's own
// approval and its strategy.engine.state decisions are the evidence of the
// state it ended in, so one that cannot be decoded refuses the approval
// rather than being read as Normal (ADR 0019, amended 2026-09-24, "The
// state survives a restart").
func TestAnUndecodableRecordRefusesTheIncidentCheck(t *testing.T) {
	brokenApproval := approvalEntry(t, event.OperatorRunApprovedPayload{})
	brokenApproval.Envelope.Payload = json.RawMessage(`{}`)
	brokenState := engineStateEntry(t, event.EngineStatePayload{})
	brokenState.Envelope.Payload = json.RawMessage(`not json`)
	for name, entry := range map[string]journal.Entry{"approval": brokenApproval, "engine state": brokenState} {
		err := restart.CheckIncidentInForce([]journal.Entry{entry}, event.OperatorRunApprovedPayload{}, "run-1.log")
		if err == nil || !strings.Contains(err.Error(), entry.Envelope.ID) || !strings.Contains(err.Error(), "run-1.log") {
			t.Errorf("%s: CheckIncidentInForce = %v, want a refusal naming %s in run-1.log", name, err, entry.Envelope.ID)
		}
	}
}
