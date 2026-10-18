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

// decidedEntry is a recorded strategy.targets.decided decision whose latch
// after it is latched.
func decidedEntry(t *testing.T, latched bool) journal.Entry {
	t.Helper()
	raw, err := json.Marshal(event.TargetsDecidedPayload{Boost: event.BoostFigures{LatchedAfter: latched}})
	if err != nil {
		t.Fatal(err)
	}
	return journal.Entry{Kind: journal.KindDecision, Envelope: event.Envelope{
		ID: "targets-decided:joint:1", Type: event.TargetsDecidedEventType, Payload: raw,
	}}
}

// startedFrom is a recorded approval stating latched, the one a run that
// decided nothing started from.
func startedFrom(t *testing.T, latched bool) journal.Entry {
	t.Helper()
	return approvalEntry(t, event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
		Boost: event.BoostStateInForce{Latched: new(latched)},
	})
}

// TestAnApprovedLatchIsTheOneTheApprovedLogEndsWith: the latch is restored,
// never recomputed (ADR 0026, point 5), so an approval states the approved
// log's last strategy.targets.decided latched_after, or, when that run
// decided nothing, the latch its own approval stated; a log with neither
// leaves the approval's latch as the fresh run's start.
func TestAnApprovedLatchIsTheOneTheApprovedLogEndsWith(t *testing.T) {
	brokenDecided := decidedEntry(t, false)
	brokenDecided.Envelope.Payload = json.RawMessage(`not json`)
	brokenApproval := startedFrom(t, false)
	brokenApproval.Envelope.Payload = json.RawMessage(`{}`)
	for _, c := range []struct {
		name    string
		entries []journal.Entry
		stated  bool
		refusal string
	}{
		{"neither, latched", nil, true, ""},
		{"neither, unlatched", nil, false, ""},
		{"decided latched, stated latched", []journal.Entry{decidedEntry(t, false), decidedEntry(t, true)}, true, ""},
		{"decided latched, stated unlatched", []journal.Entry{decidedEntry(t, true)}, false, "latched_after true"},
		{"decided after its approval wins", []journal.Entry{startedFrom(t, true), decidedEntry(t, false)}, false, ""},
		{"decided nothing, the approval's latch kept", []journal.Entry{startedFrom(t, true)}, true, ""},
		{"decided nothing, the approval's latch changed", []journal.Entry{startedFrom(t, true)}, false, "decided nothing"},
		{"undecodable decision", []journal.Entry{brokenDecided}, false, "decode targets-decided:joint:1 in run-1.log"},
		{"undecodable approval", []journal.Entry{brokenApproval}, false, "operator-run-approved:x in run-1.log"},
	} {
		approval := event.OperatorRunApprovedPayload{Boost: event.BoostStateInForce{Latched: new(c.stated)}}
		err := restart.CheckApprovedLatch(c.entries, approval, "run-1.log")
		switch {
		case c.refusal == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refusal != "" && (err == nil || !strings.Contains(err.Error(), c.refusal)):
			t.Errorf("%s: CheckApprovedLatch = %v, want a refusal saying %q", c.name, err, c.refusal)
		}
	}
}

// TestAVersion1ApprovalInTheApprovedLogStaysReadable: the approved log is
// evidence recorded earlier, so its approval is read at the schema version it
// was recorded at (ADR 0015). A version 1 approval, which could not state an
// incident's cash scope, still gives the latch a run that decided nothing
// started from.
func TestAVersion1ApprovalInTheApprovedLogStaysReadable(t *testing.T) {
	recorded := approvalEntry(t, event.OperatorRunApprovedPayload{
		AccountID: "joint", PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 13, 0, 0, 0, time.UTC),
		Boost:           event.BoostStateInForce{Latched: new(true)},
		IncidentInForce: &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateDegraded, ScopeInstrumentIDs: []string{"SPY"}, SaleAttempted: new(false)},
	})
	var keys map[string]any
	if err := json.Unmarshal(recorded.Envelope.Payload, &keys); err != nil {
		t.Fatal(err)
	}
	delete(keys["incident_in_force"].(map[string]any), "scope_cash_components")
	raw, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	recorded.Envelope.Payload, recorded.Envelope.SchemaVersion = raw, 1
	entries := []journal.Entry{recorded}
	if err := restart.CheckApprovedLatch(entries, event.OperatorRunApprovedPayload{Boost: event.BoostStateInForce{Latched: new(true)}}, "run-1.log"); err != nil {
		t.Fatalf("a version 1 approval in the approved log is read at its version: %v", err)
	}
	if err := restart.CheckApprovedLatch(entries, event.OperatorRunApprovedPayload{Boost: event.BoostStateInForce{Latched: new(false)}}, "run-1.log"); err == nil || !strings.Contains(err.Error(), "decided nothing") {
		t.Fatalf("its latch is the one in force: %v", err)
	}
}
