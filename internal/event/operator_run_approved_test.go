package event_test

import (
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
		Boost:                event.BoostStateInForce{Latched: true},
	}
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
	expectInvalid(t, "hash prefix", v, func(p *P) { p.PreviousLogFinalHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" }, "final hash")
	expectInvalid(t, "hash short", v, func(p *P) { p.PreviousLogFinalHash = "sha256:0123" }, "final hash")
	expectInvalid(t, "hash upper", v, func(p *P) { p.PreviousLogFinalHash = "sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef" }, "final hash")
	expectInvalid(t, "approved by", v, func(p *P) { p.ApprovedBy = "" }, "approved by")
	expectInvalid(t, "approved at", v, func(p *P) { p.ApprovedAt = time.Time{} }, "approved at")
}
