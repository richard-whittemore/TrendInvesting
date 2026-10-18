package event_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validOperatorRunApproved() event.OperatorRunApprovedPayload {
	return event.OperatorRunApprovedPayload{
		AccountID:            "joint",
		PreviousLog:          "run-2026-10-16.log",
		PreviousLogFinalHash: "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		IncidentID:           "",
		ApprovedBy:           "owner",
		ApprovedAt:           time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
		Boost:                event.BoostStateInForce{Latched: new(true)},
	}
}

func validOperatorRunApprovedInIncident() event.OperatorRunApprovedPayload {
	p := validOperatorRunApproved()
	p.IncidentInForce = &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(true), SaleClientOrderID: "joint-sale-1"}
	return p
}

// TestOperatorRunApprovedValidate: an approval names the account, the
// previous run's durable log by file name and the hash of its last committed
// record, who approved and when; the incident is optional.
func TestOperatorRunApprovedValidate(t *testing.T) {
	v := validOperatorRunApproved()
	if err := v.Validate(); err != nil {
		t.Fatalf("the fixture is invalid: %v", err)
	}
	withIncident := v
	withIncident.IncidentID = "incident-2026-10-16-reconciliation"
	if err := withIncident.Validate(); err != nil {
		t.Errorf("an approval naming an incident: %v", err)
	}
	type P = event.OperatorRunApprovedPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "previous log", v, func(p *P) { p.PreviousLog = "" }, "previous log")
	expectInvalid(t, "previous log path", v, func(p *P) { p.PreviousLog = "../engines/run.log" }, "file name")
	expectInvalid(t, "previous log dot", v, func(p *P) { p.PreviousLog = ".." }, "file name")
	expectInvalid(t, "hash", v, func(p *P) { p.PreviousLogFinalHash = "" }, "final hash")
	expectInvalid(t, "hash prefix", v, func(p *P) {
		p.PreviousLogFinalHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	}, "final hash")
	expectInvalid(t, "hash short", v, func(p *P) { p.PreviousLogFinalHash = "sha256:0123" }, "final hash")
	expectInvalid(t, "hash upper", v, func(p *P) {
		p.PreviousLogFinalHash = "sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef"
	}, "final hash")
	expectInvalid(t, "approved by", v, func(p *P) { p.ApprovedBy = "" }, "approved by")
	expectInvalid(t, "approved at", v, func(p *P) { p.ApprovedAt = time.Time{} }, "approved at")
}

// TestAnApprovalStatesBoostsLatchExplicitly: the latch is state a fresh run
// cannot recompute from bars (ADR 0026, point 5), so an approval that does
// not state it is refused; a missing latch is never read as "not latched",
// which would take a boosted account from 1.47 to 0.98 unasked.
func TestAnApprovalStatesBoostsLatchExplicitly(t *testing.T) {
	type P = event.OperatorRunApprovedPayload
	expectInvalid(t, "latch", validOperatorRunApproved(), func(p *P) { p.Boost.Latched = nil }, "latched")
	unlatched := validOperatorRunApproved()
	unlatched.Boost.Latched = new(false)
	if err := unlatched.Validate(); err != nil {
		t.Errorf("an approval stating an unlatched boost: %v", err)
	}
}

// TestAnApprovalCarriesTheIncidentSaleAttempt: a fresh run that continues an
// incident carries the incident's ID and whether its one sale back to 98%
// was attempted (ADR 0019, amended 2026-10-15: one attempt per incident),
// and the attempt's client order ID when one was sent, so the fresh run never
// attempts that sale twice.
func TestAnApprovalCarriesTheIncidentSaleAttempt(t *testing.T) {
	type P = event.OperatorRunApprovedPayload
	v := validOperatorRunApproved()
	for name, in := range map[string]event.IncidentInForce{
		"not attempted":       {IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(false)},
		"attempted, withheld": {IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(true)},
		"attempted, sent":     {IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(true), SaleClientOrderID: "joint-sale-1"},
	} {
		ok := v
		ok.IncidentInForce = &in
		if err := ok.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	in := func(mutate func(*event.IncidentInForce)) func(*P) {
		return func(p *P) {
			i := event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(true), SaleClientOrderID: "joint-sale-1"}
			mutate(&i)
			p.IncidentInForce = &i
		}
	}
	expectInvalid(t, "incident id", v, in(func(i *event.IncidentInForce) { i.IncidentID = "" }), "incident in force")
	expectInvalid(t, "sale attempted", v, in(func(i *event.IncidentInForce) { i.SaleAttempted = nil }), "sale attempted")
	expectInvalid(t, "order without attempt", v, in(func(i *event.IncidentInForce) { i.SaleAttempted = new(false) }), "client order")
	expectInvalid(t, "cleared and in force", v, func(p *P) {
		in(func(*event.IncidentInForce) {})(p)
		p.IncidentID = p.IncidentInForce.IncidentID
	}, "clears")
}

// TestAnApprovalStatesTheIncidentsRestrictions: an incident in force carries
// the engine state and scope the previous run ended in, so the fresh run
// resumes in them: "a crash or restart while Degraded or Halted resumes in
// that same state and scope" and leaving either is never automatic (ADR
// 0019, amended 2026-09-24, "The state survives a restart"). Degraded names
// its instruments, strictly ascending; Halted covers the whole account and
// names none; a missing state is refused, never read as Normal.
func TestAnApprovalStatesTheIncidentsRestrictions(t *testing.T) {
	type P = event.OperatorRunApprovedPayload
	v := validOperatorRunApproved()
	for name, in := range map[string]event.IncidentInForce{
		"halted":   {IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(false)},
		"degraded": {IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeInstrumentIDs: []string{"SPY"}, SaleAttempted: new(false)},
	} {
		ok := v
		ok.IncidentInForce = &in
		if err := ok.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	in := func(mutate func(*event.IncidentInForce)) func(*P) {
		return func(p *P) {
			i := event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeInstrumentIDs: []string{"SPY"}, SaleAttempted: new(false)}
			mutate(&i)
			p.IncidentInForce = &i
		}
	}
	expectInvalid(t, "no state", v, in(func(i *event.IncidentInForce) { i.State, i.ScopeInstrumentIDs = "", nil }), "state")
	expectInvalid(t, "normal", v, in(func(i *event.IncidentInForce) { i.State, i.ScopeInstrumentIDs = "normal", nil }), "state")
	expectInvalid(t, "degraded without scope", v, in(func(i *event.IncidentInForce) { i.ScopeInstrumentIDs = nil }), "scope")
	expectInvalid(t, "halted with scope", v, in(func(i *event.IncidentInForce) { i.State = event.EngineStateHalted }), "scope")
	expectInvalid(t, "scope out of order", v, in(func(i *event.IncidentInForce) { i.ScopeInstrumentIDs = []string{"TLT", "SPY"} }), "ascending")
}

// TestDecodeOperatorRunApprovedRequiresEveryKey: the approval file is
// written outside the engine, so a key it leaves out, or one this build does
// not know, is refused rather than read as a zero value: no Boost state, no
// latch, or no statement of the incident in force (null states that there is
// none) is never a default (ADR 0026, point 5; ADR 0019, amended 2026-10-15).
func TestDecodeOperatorRunApprovedRequiresEveryKey(t *testing.T) {
	base := `"account_id":"joint","previous_log":"run-1.log","previous_log_final_hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","incident_id":"","approved_by":"owner","approved_at":"2026-10-17T13:00:00Z"`
	accept := []string{
		`{` + base + `,"boost":{"latched":true},"incident_in_force":null}`,
		`{` + base + `,"boost":{"latched":false},"incident_in_force":{"incident_id":"engine-state:joint:1","state":"halted","scope_instrument_ids":[],"scope_cash_components":[],"sale_attempted":false}}`,
		`{` + base + `,"boost":{"latched":false},"incident_in_force":{"incident_id":"engine-state:joint:1","state":"degraded","scope_instrument_ids":["SPY"],"scope_cash_components":[],"sale_attempted":false}}`,
	}
	for _, raw := range accept {
		if _, err := event.DecodeOperatorRunApproved([]byte(raw)); err != nil {
			t.Errorf("DecodeOperatorRunApproved(%s) = %v", raw, err)
		}
	}
	refuse := map[string]string{
		`{` + base + `,"incident_in_force":null}`:                                                                                          "boost",
		`{` + base + `,"boost":{},"incident_in_force":null}`:                                                                               "latched",
		`{` + base + `,"boost":{"latched":null},"incident_in_force":null}`:                                                                 "latched",
		`{` + base + `,"boost":null,"incident_in_force":null}`:                                                                             "boost",
		`{` + base + `,"boost":{"latched":true}}`:                                                                                          "incident_in_force",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":{"incident_id":"x"}}`:                                                  "incident_in_force.sale_attempted",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":{"incident_id":"x","scope_instrument_ids":[],"sale_attempted":false}}`: "incident_in_force.state",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":{"incident_id":"x","state":"degraded","sale_attempted":false}}`:        "incident_in_force.scope_instrument_ids",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":null,"extra":1}`:                                                       "extra",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":5}`:                                                                    "incident_in_force",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":null} {}`:                                                              "one JSON value",
	}
	for raw, want := range refuse {
		if _, err := event.DecodeOperatorRunApproved([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeOperatorRunApproved(%s) = %v, want a refusal mentioning %q", raw, err, want)
		}
	}
}

// TestAnApprovalCarriesTheIncidentsCashScope: a Degraded incident's scope can
// cover cash (a reconciliation's unexplained cash difference, ADR 0019,
// amended 2026-09-24), and a fresh run resumes in that same scope, so the
// approval states the cash components beside the instruments, at schema
// version 2: Degraded names instruments or cash components or both, strictly
// ascending; Halted names neither.
func TestAnApprovalCarriesTheIncidentsCashScope(t *testing.T) {
	if event.OperatorRunApprovedSchemaVersion != 2 {
		t.Errorf("OperatorRunApprovedSchemaVersion = %d, want 2 once an incident in force states its cash scope", event.OperatorRunApprovedSchemaVersion)
	}
	type P = event.OperatorRunApprovedPayload
	v := validOperatorRunApproved()
	cash := []string{event.ReconciliationComponentCash}
	for name, in := range map[string]event.IncidentInForce{
		"cash alone":          {IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeCashComponents: cash, SaleAttempted: new(false)},
		"instrument and cash": {IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeInstrumentIDs: []string{"SPY"}, ScopeCashComponents: cash, SaleAttempted: new(false)},
	} {
		ok := v
		ok.IncidentInForce = &in
		if err := ok.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	in := func(mutate func(*event.IncidentInForce)) func(*P) {
		return func(p *P) {
			i := event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeCashComponents: cash, SaleAttempted: new(false)}
			mutate(&i)
			p.IncidentInForce = &i
		}
	}
	expectInvalid(t, "halted with cash", v, in(func(i *event.IncidentInForce) { i.State = event.EngineStateHalted }), "scope")
	expectInvalid(t, "cash out of order", v, in(func(i *event.IncidentInForce) { i.ScopeCashComponents = []string{"total-cash", "dividends"} }), "ascending")

	base := `"account_id":"joint","previous_log":"run-1.log","previous_log_final_hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","incident_id":"","approved_by":"owner","approved_at":"2026-10-17T13:00:00Z","boost":{"latched":false}`
	cashOnly := `{` + base + `,"incident_in_force":{"incident_id":"engine-state:joint:1","state":"degraded","scope_instrument_ids":[],"scope_cash_components":["total-cash"],"sale_attempted":false}}`
	p, err := event.DecodeOperatorRunApproved([]byte(cashOnly))
	if err != nil || p.IncidentInForce == nil || len(p.IncidentInForce.ScopeCashComponents) != 1 {
		t.Errorf("DecodeOperatorRunApproved(cash alone) = %+v, %v", p.IncidentInForce, err)
	}
	unstated := `{` + base + `,"incident_in_force":{"incident_id":"engine-state:joint:1","state":"degraded","scope_instrument_ids":["SPY"],"sale_attempted":false}}`
	if _, err := event.DecodeOperatorRunApproved([]byte(unstated)); err == nil || !strings.Contains(err.Error(), "incident_in_force.scope_cash_components") {
		t.Errorf("an incident in force that does not state its cash scope: %v", err)
	}

	// A version 1 approval recorded by an earlier build could not state a
	// cash scope, so it is read with none; it may not name one.
	if r, err := event.DecodeRecordedOperatorRunApproved([]byte(unstated), 1); err != nil || len(r.IncidentInForce.ScopeCashComponents) != 0 {
		t.Errorf("a version 1 approval: %+v, %v", r.IncidentInForce, err)
	}
	if _, err := event.DecodeRecordedOperatorRunApproved([]byte(cashOnly), 1); err == nil {
		t.Error("a version 1 approval naming a cash scope was read")
	}
	if _, err := event.DecodeRecordedOperatorRunApproved([]byte(cashOnly), 2); err != nil {
		t.Errorf("a version 2 approval: %v", err)
	}
	if _, err := event.DecodeRecordedOperatorRunApproved([]byte(cashOnly), 3); err == nil || !strings.Contains(err.Error(), "schema version 3") {
		t.Errorf("an approval at an unknown schema version: %v", err)
	}
}
