package restart_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// recorded is a journal entry of kind holding p as eventType, under id.
func recorded(t *testing.T, kind, id, eventType string, version uint32, p any) journal.Entry {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return journal.Entry{Kind: kind, Envelope: event.Envelope{ID: id, Type: eventType, SchemaVersion: version, Payload: raw}}
}

func killSwitchEntry(t *testing.T, id string, engaged bool) journal.Entry {
	return recorded(t, journal.KindInput, id, event.KillSwitchEventType, event.KillSwitchSchemaVersion, event.KillSwitchPayload{
		AccountID: "joint", AsOf: time.Date(2026, time.October, 17, 14, 0, 0, 0, time.UTC), Engaged: engaged, Operator: "owner", Detail: "x",
	})
}

// otherKillSwitchEntry is a kill switch for account, which may be another
// account than the log's.
func otherKillSwitchEntry(t *testing.T, id, account string, engaged bool) journal.Entry {
	return recorded(t, journal.KindInput, id, event.KillSwitchEventType, event.KillSwitchSchemaVersion, event.KillSwitchPayload{
		AccountID: account, AsOf: time.Date(2026, time.October, 17, 14, 0, 0, 0, time.UTC), Engaged: engaged, Operator: "owner", Detail: "x",
	})
}

// backdatedKillSwitchEntry is a kill switch for the log's account stating a
// time before killSwitchEntry's.
func backdatedKillSwitchEntry(t *testing.T, id string, engaged bool) journal.Entry {
	return recorded(t, journal.KindInput, id, event.KillSwitchEventType, event.KillSwitchSchemaVersion, event.KillSwitchPayload{
		AccountID: "joint", AsOf: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC), Engaged: engaged, Operator: "owner", Detail: "x",
	})
}

// incidentSale is a planned sale back to 98% under the incident's tag.
func incidentSale(t *testing.T, tag string) journal.Entry {
	return recorded(t, journal.KindDecision, "order-planned:sale", event.OrderPlannedEventType, event.OrderPlannedSafetyVersion, event.OrderPlannedPayload{
		AccountID: "joint", Tag: tag, ClientOrderID: "joint-sale-1", InstrumentID: "SPY", Side: event.OrderSideSell,
		Sale: event.SaleFigures{Kind: event.SaleKindIncident, Trigger: event.SaleTriggerExit},
	})
}

func saleWithheldEntry(t *testing.T, tag, reason string) journal.Entry {
	return recorded(t, journal.KindDecision, "sale-withheld:"+reason, event.SaleWithheldEventType, event.SaleWithheldSchemaVersion, event.SaleWithheldPayload{
		AccountID: "joint", Tag: tag, ClientOrderID: "joint-sale-1", InstrumentID: "SPY", Reason: reason,
	})
}

const incidentID = "engine-state:joint:1"

// TestReadEndStateFindsWhatAFreshRunMustBeToldOf: the state a fresh run
// cannot recompute from bars is read from the approved log's own records:
// Boost's latch (ADR 0026, point 5), the incident it ended in with its
// state and scope (ADR 0019, amended 2026-09-24, "The state survives a
// restart"), whether that incident's one sale back to 98% was attempted, a
// sale planned or withheld for a person (ADR 0019, amended 2026-10-15), and
// whether the kill switch was left engaged (ADR 0026, point 3).
func TestReadEndStateFindsWhatAFreshRunMustBeToldOf(t *testing.T) {
	halted := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x"})
	degraded := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x", ScopeInstrumentIDs: []string{"SPY"}})
	inherited := approvalEntry(t, event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC), Boost: event.BoostStateInForce{Latched: new(true)},
		IncidentInForce: &event.IncidentInForce{IncidentID: incidentID, State: event.EngineStateHalted, SaleAttempted: new(true), SaleClientOrderID: "joint-sale-0"},
	})
	for _, c := range []struct {
		name    string
		entries []journal.Entry
		want    restart.EndState
	}{
		{"nothing", nil, restart.EndState{}},
		{"latched, switch left on", []journal.Entry{decidedEntry(t, true), killSwitchEntry(t, "k1", true)}, restart.EndState{Latched: true, KillSwitchEngaged: true}},
		{"switch turned off again", []journal.Entry{killSwitchEntry(t, "k1", true), killSwitchEntry(t, "k2", false)}, restart.EndState{}},
		{"another account's release, which the gate refused", []journal.Entry{killSwitchEntry(t, "k1", true), otherKillSwitchEntry(t, "k2", "ira", false)}, restart.EndState{KillSwitchEngaged: true}},
		{"an engagement the gate refused", []journal.Entry{otherKillSwitchEntry(t, "k1", "ira", true)}, restart.EndState{}},
		{"a backdated release, which the gate refused", []journal.Entry{killSwitchEntry(t, "k1", true), backdatedKillSwitchEntry(t, "k2", false)}, restart.EndState{KillSwitchEngaged: true}},
		{"degraded, no sale", []journal.Entry{degraded}, restart.EndState{IncidentID: incidentID, State: event.EngineStateDegraded, Scope: []string{"SPY"}}},
		{"halted, sale sent", []journal.Entry{halted, incidentSale(t, incidentID)},
			restart.EndState{IncidentID: incidentID, State: event.EngineStateHalted, SaleAttempted: true, SaleClientOrderID: "joint-sale-1"}},
		{"halted, sale withheld for a person", []journal.Entry{halted, saleWithheldEntry(t, incidentID, event.SaleWithheldUnreadable)},
			restart.EndState{IncidentID: incidentID, State: event.EngineStateHalted, SaleAttempted: true}},
		{"halted, sale withheld from a stale snapshot", []journal.Entry{halted, saleWithheldEntry(t, incidentID, event.SaleWithheldStaleBrokerSnapshot)},
			restart.EndState{IncidentID: incidentID, State: event.EngineStateHalted}},
		{"halted, the engagement's sale", []journal.Entry{halted, incidentSale(t, "k1")},
			restart.EndState{IncidentID: incidentID, State: event.EngineStateHalted}},
		{"an inherited incident whose sale was attempted", []journal.Entry{inherited},
			restart.EndState{Latched: true, IncidentID: incidentID, State: event.EngineStateHalted, SaleAttempted: true, SaleClientOrderID: "joint-sale-0"}},
	} {
		got, err := restart.ReadEndState(c.entries, "joint", "run-1.log")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Latched != c.want.Latched || got.IncidentID != c.want.IncidentID || got.State != c.want.State || !slices.Equal(got.Scope, c.want.Scope) ||
			got.SaleAttempted != c.want.SaleAttempted || got.SaleClientOrderID != c.want.SaleClientOrderID || got.KillSwitchEngaged != c.want.KillSwitchEngaged {
			t.Errorf("%s: ReadEndState = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestReadEndStateRefusesAnUndecodableRecord: a record that is evidence of
// the end state and cannot be decoded refuses, naming it, rather than being
// read as nothing.
func TestReadEndStateRefusesAnUndecodableRecord(t *testing.T) {
	halted := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x"})
	for _, broken := range []journal.Entry{decidedEntry(t, true), killSwitchEntry(t, "k1", true), incidentSale(t, incidentID), saleWithheldEntry(t, incidentID, event.SaleWithheldUnreadable)} {
		broken.Envelope.Payload = json.RawMessage(`not json`)
		if _, err := restart.ReadEndState([]journal.Entry{halted, broken}, "joint", "run-1.log"); err == nil || !strings.Contains(err.Error(), broken.Envelope.ID) || !strings.Contains(err.Error(), "run-1.log") {
			t.Errorf("%s: ReadEndState = %v, want a refusal naming it in run-1.log", broken.Envelope.ID, err)
		}
	}
	state := halted
	state.Envelope.Payload = json.RawMessage(`not json`)
	if _, err := restart.ReadEndState([]journal.Entry{state}, "joint", "run-1.log"); err == nil || !strings.Contains(err.Error(), state.Envelope.ID) {
		t.Errorf("ReadEndState = %v, want the undecodable state refused", err)
	}
}

// TestAnApprovalNeverUnderstatesTheSaleAttempt: a fresh run learns of an
// earlier attempt of the incident's one sale only from the approval, and
// never attempts it again when told (ADR 0019, amended 2026-10-15: one
// attempt per incident, never retried automatically). So an approval
// carrying the incident in force with the sale not attempted is refused
// when the approved log records an attempt; stating it attempted is always
// accepted, since the owner may have sold by hand.
func TestAnApprovalNeverUnderstatesTheSaleAttempt(t *testing.T) {
	halted := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x"})
	carried := func(attempted bool) event.OperatorRunApprovedPayload {
		return event.OperatorRunApprovedPayload{IncidentInForce: &event.IncidentInForce{IncidentID: incidentID, State: event.EngineStateHalted, SaleAttempted: new(attempted)}}
	}
	for _, c := range []struct {
		name     string
		entries  []journal.Entry
		approval event.OperatorRunApprovedPayload
		refusal  string
	}{
		{"cleared", []journal.Entry{halted, incidentSale(t, incidentID)}, event.OperatorRunApprovedPayload{IncidentID: incidentID}, ""},
		{"attempted, stated attempted", []journal.Entry{halted, incidentSale(t, incidentID)}, carried(true), ""},
		{"attempted, stated not attempted", []journal.Entry{halted, incidentSale(t, incidentID)}, carried(false), "joint-sale-1"},
		{"withheld for a person, stated not attempted", []journal.Entry{halted, saleWithheldEntry(t, incidentID, event.SaleWithheldUnknownWorkingSell)}, carried(false), "attempted"},
		{"not attempted, stated attempted", []journal.Entry{halted}, carried(true), ""},
		{"not attempted, stated not attempted", []journal.Entry{halted}, carried(false), ""},
	} {
		err := restart.CheckSaleAttempt(c.entries, c.approval, "run-1.log")
		switch {
		case c.refusal == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refusal != "" && (err == nil || !strings.Contains(err.Error(), c.refusal) || !strings.Contains(err.Error(), "run-1.log")):
			t.Errorf("%s: CheckSaleAttempt = %v, want a refusal saying %q", c.name, err, c.refusal)
		}
	}
	broken := incidentSale(t, incidentID)
	broken.Envelope.Payload = json.RawMessage(`not json`)
	if err := restart.CheckSaleAttempt([]journal.Entry{halted, broken}, carried(false), "run-1.log"); err == nil || !strings.Contains(err.Error(), broken.Envelope.ID) {
		t.Errorf("CheckSaleAttempt = %v, want the undecodable record refused", err)
	}
}

// TestProposeApprovalStatesTheEndStateOrClearsTheIncident: the operator
// tool's approval states what the log ends in and nothing else: the latch,
// and the incident in force with its state, scope and sale attempt, or,
// when the owner clears the incident by its ID (ADR 0019's recovery
// procedure), that incident cleared and none in force. Clearing an incident
// the log did not end in is refused.
func TestProposeApprovalStatesTheEndStateOrClearsTheIncident(t *testing.T) {
	request := restart.ApprovalRequest{
		AccountID: "joint", PreviousLog: "run-1.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("a", 64),
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 15, 0, 0, 0, time.UTC),
	}
	halted := restart.EndState{Latched: true, IncidentID: incidentID, State: event.EngineStateHalted, SaleAttempted: true, SaleClientOrderID: "joint-sale-1"}

	p, err := restart.ProposeApproval(halted, request)
	if err != nil {
		t.Fatal(err)
	}
	in := p.IncidentInForce
	if !*p.Boost.Latched || p.IncidentID != "" || in == nil || in.IncidentID != incidentID || in.State != event.EngineStateHalted ||
		!*in.SaleAttempted || in.SaleClientOrderID != "joint-sale-1" || p.PreviousLog != "run-1.log" || p.ApprovedBy != "owner" {
		t.Errorf("carried: %+v %+v", p, in)
	}

	cleared := request
	cleared.ClearIncident = incidentID
	if p, err = restart.ProposeApproval(halted, cleared); err != nil || p.IncidentID != incidentID || p.IncidentInForce != nil {
		t.Errorf("cleared: %+v %v", p, err)
	}
	if p, err = restart.ProposeApproval(restart.EndState{}, request); err != nil || p.IncidentInForce != nil || *p.Boost.Latched {
		t.Errorf("normal: %+v %v", p, err)
	}

	other := request
	other.ClearIncident = "engine-state:joint:9"
	if _, err := restart.ProposeApproval(halted, other); err == nil || !strings.Contains(err.Error(), incidentID) {
		t.Errorf("clearing another incident: %v", err)
	}
	if _, err := restart.ProposeApproval(restart.EndState{}, other); err == nil || !strings.Contains(err.Error(), "no incident") {
		t.Errorf("clearing an incident in a normal log: %v", err)
	}
	invalid := request
	invalid.ApprovedBy = ""
	if _, err := restart.ProposeApproval(halted, invalid); err == nil || !strings.Contains(err.Error(), "approved by") {
		t.Errorf("an approval without its approver: %v", err)
	}
}

// TestAnIncidentDegradedOnCashIsCarriedInForceWithItsCashScope: a
// reconciliation's unexplained cash degrades the account on a cash component
// (ADR 0019, amended 2026-09-24), and a restart never narrows a Degraded
// scope, so the approval the operator tool proposes carries that cash scope
// in force, and the engine accepts it against the log it was read from.
func TestAnIncidentDegradedOnCashIsCarriedInForceWithItsCashScope(t *testing.T) {
	cash := []string{event.ReconciliationComponentCash}
	degraded := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateDegraded, Reason: event.EngineStateReasonReconciliationUnexplained, Detail: "x", ScopeCashComponents: cash})
	entries := []journal.Entry{degraded}

	end, err := restart.ReadEndState(entries, "joint", "run-1.log")
	if err != nil {
		t.Fatal(err)
	}
	if end.State != event.EngineStateDegraded || len(end.Scope) != 0 || !slices.Equal(end.ScopeCash, cash) {
		t.Fatalf("ReadEndState = %+v, want Degraded on the cash component %v", end, cash)
	}
	p, err := restart.ProposeApproval(end, restart.ApprovalRequest{
		AccountID: "joint", PreviousLog: "run-1.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("a", 64),
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 15, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ProposeApproval = %v, want the cash scope carried in force", err)
	}
	if in := p.IncidentInForce; in == nil || !slices.Equal(in.ScopeCashComponents, cash) {
		t.Fatalf("incident in force %+v, want scope cash components %v", p.IncidentInForce, cash)
	}
	if err := restart.CheckIncidentInForce(entries, p, "run-1.log"); err != nil {
		t.Errorf("CheckIncidentInForce = %v, want the proposed approval accepted", err)
	}
}
