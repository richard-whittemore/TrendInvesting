package targetweight_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func approval(latched bool) event.OperatorRunApprovedPayload {
	return event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-1.log",
		PreviousLogFinalHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ApprovedBy:           "owner", ApprovedAt: time.Date(2025, time.January, 2, 14, 0, 0, 0, time.UTC),
		Boost: event.BoostStateInForce{Latched: latched},
	}
}

// TestAnApprovedFreshRunStartsFromTheApprovedLatch: a fresh run started
// after the owner's approval takes Boost's latch from it, never from the
// bars (ADR 0026, point 5), so a run approved while boosted targets 1.47
// (0.98 x BOOST_SIZE 1.5, plan.md section 2) on its first decision.
func TestAnApprovedFreshRunStartsFromTheApprovedLatch(t *testing.T) {
	at := sessionEnd(0)
	h, s := configured(t)
	mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", at, approval(true)))
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b", at, bar(100, at)))
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, (&account{cash: 4000}).snapshot(at, 100)))
	mustApply(t, h, s.envelope(t, event.MarkRecordedEventType, event.MarkRecordedSchemaVersion, "m", at, event.MarkRecordedPayload{InstrumentID: "SPY", AsOf: at, Price: 100, Source: event.AdapterIBKR}))
	out := mustApply(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}))
	decided := targets(t, out)
	if len(decided) != 1 || !decided[0].Boost.LatchedBefore || decided[0].Targets[0].Weight != 1.47 {
		t.Fatalf("decided %+v, want the approved latch in force and a target of 1.47", decided)
	}
}

func TestApprovalInputRules(t *testing.T) {
	at := sessionEnd(0)
	h, s := configured(t)
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, 2, "x", at, approval(false)), "schema version")
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, event.OperatorRunApprovedPayload{}), "invalid")
	other := approval(false)
	other.AccountID = "ira"
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, other), "another account")
	foreign := s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, approval(false))
	foreign.ConfigurationHash = "sha256:other"
	mustRefuse(t, h, foreign, "another configuration")
	mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, approval(false)))
	mustRefuse(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "y", at, approval(false)), "comes once")

	late, ls := configured(t)
	mustApply(t, late, ls.envelope(t, event.CompletedBarEventType, event.CompletedBarSchemaVersion, "b", at, bar(100, at)))
	mustRefuse(t, late, ls.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "x", at, approval(false)), "comes once")
}
