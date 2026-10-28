package event_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validRunApprovalAccepted() event.RunApprovalAcceptedPayload {
	return event.RunApprovalAcceptedPayload{
		AccountID: "DUIRA", ApprovalID: "operator-run-approved:sha256:" + strings.Repeat("a", 64), ApprovalSequence: 2,
		RebalanceInProgress: sellsWorking(),
	}
}

// TestARunRecordsTheApprovalItAccepted: a momentum run records, as a
// decision, the approval it accepted and the rebalance in progress it
// carries (ADR 0026, point 5; ADR 0027, note of 2026-10-28), so a later
// restart reads that fact from the log rather than recomputing it.
func TestARunRecordsTheApprovalItAccepted(t *testing.T) {
	v := validRunApprovalAccepted()
	if err := v.Validate(); err != nil {
		t.Fatalf("the fixture is invalid: %v", err)
	}
	none := v
	none.RebalanceInProgress = nil
	if err := none.Validate(); err != nil {
		t.Errorf("an acceptance carrying no rebalance: %v", err)
	}
	type P = event.RunApprovalAcceptedPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "approval", v, func(p *P) { p.ApprovalID = "" }, "approval id")
	expectInvalid(t, "sequence", v, func(p *P) { p.ApprovalSequence = 0 }, "approval sequence")
	expectInvalid(t, "rebalance", v, func(p *P) { p.RebalanceInProgress.Phase = "x" }, "phase")
	if event.RunApprovalAcceptedEventType != "strategy.run.approval-accepted" || event.RunApprovalAcceptedSchemaVersion != 1 {
		t.Errorf("type %q version %d", event.RunApprovalAcceptedEventType, event.RunApprovalAcceptedSchemaVersion)
	}
}

// TestARecordedAcceptanceStatesItsRebalanceKey: the rebalance key is
// required, null when none, so a record that lost it is refused, never read
// as no rebalance in progress; an unknown version is refused (ADR 0015).
func TestARecordedAcceptanceStatesItsRebalanceKey(t *testing.T) {
	base := `"account_id":"DUIRA","approval_id":"a","approval_sequence":2`
	for _, raw := range []string{`{` + base + `,"rebalance_in_progress":null}`,
		`{` + base + `,"rebalance_in_progress":{"decision_id":"d","decision_time":"2025-01-31T21:00:00Z","phase":"sells-working","client_order_ids":["o"]}}`} {
		if _, err := event.DecodeRunApprovalAccepted([]byte(raw), event.RunApprovalAcceptedSchemaVersion); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for raw, want := range map[string]string{
		`{` + base + `}`: "rebalance_in_progress",
		`{` + base + `,"rebalance_in_progress":null,"extra":1}`:                                       "extra",
		`{` + base + `,"rebalance_in_progress":{"decision_id":"d"}}`:                                  "phase",
		`{"account_id":"DUIRA","approval_id":"a","approval_sequence":0,"rebalance_in_progress":null}`: "approval sequence",
		`not json`: "decode",
	} {
		if _, err := event.DecodeRunApprovalAccepted([]byte(raw), event.RunApprovalAcceptedSchemaVersion); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want a refusal mentioning %q", raw, err, want)
		}
	}
	if _, err := event.DecodeRunApprovalAccepted([]byte(`{`+base+`,"rebalance_in_progress":null}`), 2); err == nil || !strings.Contains(err.Error(), "schema version 2") {
		t.Errorf("an unknown version: %v", err)
	}
}
