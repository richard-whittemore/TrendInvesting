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
	p.IncidentInForce = &event.IncidentInForce{IncidentID: "engine-state:joint:1", SaleAttempted: new(true), SaleClientOrderID: "joint-sale-1"}
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
		"not attempted":       {IncidentID: "engine-state:joint:1", SaleAttempted: new(false)},
		"attempted, withheld": {IncidentID: "engine-state:joint:1", SaleAttempted: new(true)},
		"attempted, sent":     {IncidentID: "engine-state:joint:1", SaleAttempted: new(true), SaleClientOrderID: "joint-sale-1"},
	} {
		ok := v
		ok.IncidentInForce = &in
		if err := ok.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	in := func(mutate func(*event.IncidentInForce)) func(*P) {
		return func(p *P) {
			i := event.IncidentInForce{IncidentID: "engine-state:joint:1", SaleAttempted: new(true), SaleClientOrderID: "joint-sale-1"}
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

// TestDecodeOperatorRunApprovedRequiresEveryKey: the approval file is
// written outside the engine, so a key it leaves out, or one this build does
// not know, is refused rather than read as a zero value: no Boost state, no
// latch, or no statement of the incident in force (null states that there is
// none) is never a default (ADR 0026, point 5; ADR 0019, amended 2026-10-15).
func TestDecodeOperatorRunApprovedRequiresEveryKey(t *testing.T) {
	base := `"account_id":"joint","previous_log":"run-1.log","previous_log_final_hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","incident_id":"","approved_by":"owner","approved_at":"2026-10-17T13:00:00Z"`
	accept := []string{
		`{` + base + `,"boost":{"latched":true},"incident_in_force":null}`,
		`{` + base + `,"boost":{"latched":false},"incident_in_force":{"incident_id":"engine-state:joint:1","sale_attempted":false}}`,
	}
	for _, raw := range accept {
		if _, err := event.DecodeOperatorRunApproved([]byte(raw)); err != nil {
			t.Errorf("DecodeOperatorRunApproved(%s) = %v", raw, err)
		}
	}
	refuse := map[string]string{
		`{` + base + `,"incident_in_force":null}`:                                         "boost",
		`{` + base + `,"boost":{},"incident_in_force":null}`:                              "latched",
		`{` + base + `,"boost":{"latched":null},"incident_in_force":null}`:                "latched",
		`{` + base + `,"boost":null,"incident_in_force":null}`:                            "boost",
		`{` + base + `,"boost":{"latched":true}}`:                                         "incident_in_force",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":{"incident_id":"x"}}`: "sale attempted",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":null,"extra":1}`:      "extra",
		`{` + base + `,"boost":{"latched":true},"incident_in_force":null} {}`:             "one JSON value",
	}
	for raw, want := range refuse {
		if _, err := event.DecodeOperatorRunApproved([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeOperatorRunApproved(%s) = %v, want a refusal mentioning %q", raw, err, want)
		}
	}
}
