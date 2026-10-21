package targetweight_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestARestartWhileDegradedStaysDegraded: "a crash or restart while Degraded
// or Halted resumes in that same state and scope, read back from the run's
// durable record" (ADR 0019, amended 2026-09-24, "The state survives a
// restart"); a restart reads "the engine state and its scope" from the
// journal (ADR 0026, point 5, line 242). The uninterrupted run degrades on
// the entry's missing mark and then freezes the exit; the restarted run,
// given the recorded state, refuses the exit with the same bytes and
// releases nothing.
func TestARestartWhileDegradedStaysDegraded(t *testing.T) {
	full := simulate(t, options{noMark: map[int]bool{16: true}})
	states := ofType(full.decisions, event.EngineStateEventType)
	if len(states) != 1 {
		t.Fatalf("want one recorded state change, got %d", len(states))
	}
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[16]
	restarted := simulate(t, options{recorded: states, recovery: &recovery, snapshots: full.inputs, noMark: map[int]bool{16: true}})
	if n := len(ofType(restarted.decisions, event.OrderReleasedEventType)); n != 0 {
		t.Fatalf("the restarted run released %d orders; Degraded is never left automatically", n)
	}
	got := ofType(restarted.decisions, event.OrderRefusedEventType)
	want := ofType(full.decisions, event.OrderRefusedEventType)[1:]
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("restarted run refused %d orders, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].ID != want[i].ID || !bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Errorf("refusal %d differs after restart:\n got %s\nwant %s", i, got[i].Payload, want[i].Payload)
		}
	}
}

func TestRecordedStateInputRules(t *testing.T) {
	full := simulate(t, options{noMark: map[int]bool{16: true}})
	state := ofType(full.decisions, event.EngineStateEventType)[0]
	at := sessionEnd(0)

	h, s := configured(t)
	foreign := state
	foreign.ConfigurationHash = "other"
	mustRefuse(t, h, foreign, "another configuration")
	mustRefuse(t, h, s.envelope(t, event.EngineStateEventType, 3, "st", at, state.Payload), "schema version")
	bad := s.envelope(t, event.EngineStateEventType, event.EngineStateScopedVersion, "st", at, state.Payload)
	bad.Payload = json.RawMessage(`{"state":1}`)
	mustRefuse(t, h, bad, "decode")
	mustRefuse(t, h, s.envelope(t, event.EngineStateEventType, event.EngineStateScopedVersion, "st", at, event.EngineStatePayload{State: "dazed"}), "engine state")
	mustApply(t, h, s.envelope(t, state.Type, state.SchemaVersion, state.ID, at, state.Payload))
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b", at, bar(100, at)))
	mustRefuse(t, h, s.envelope(t, state.Type, state.SchemaVersion, state.ID, at, state.Payload), "after the run began")
}

// TestAnExitWithALateMarkIsReleased: Boost's exit back to 98% is
// risk-reducing (ADR 0026, point 4, lines 195-200), so it is exempt from the
// price band (point 3, lines 188-191), whose only input is the mark. One late
// mark on the exit's session neither refuses it nor degrades the account.
func TestAnExitWithALateMarkIsReleased(t *testing.T) {
	base := simulate(t, options{})
	exitSession := -1
	for i, d := range targets(t, base.decisions) {
		if d.Boost.LatchedBefore && !d.Boost.LatchedAfter {
			exitSession = i
		}
	}
	if exitSession < 0 {
		t.Fatal("the synthetic series exits")
	}
	r := simulate(t, options{noMark: map[int]bool{exitSession: true}})
	if n := len(ofType(r.decisions, event.OrderRefusedEventType)); n != 0 {
		t.Fatalf("%d orders refused", n)
	}
	if n := len(ofType(r.decisions, event.EngineStateEventType)); n != 0 {
		t.Fatalf("%d state changes", n)
	}
	var exit event.OrderReleasedPayload
	for _, e := range ofType(r.decisions, event.OrderReleasedEventType) {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		if strings.HasSuffix(p.Tag, sessionEnd(exitSession).Format("2006-01-02T15:04:05Z")) {
			exit = p
		}
	}
	if exit.Side != event.OrderSideSell || exit.Figures.MarkSet {
		t.Fatalf("the exit is released without a mark: %+v", exit)
	}
}

// TestASnapshotAfterTheCloseWithholds: orders are valued at the previous
// close (ADR 0026, point 2), so a snapshot taken after the session's close
// does not value its order; the session withholds.
func TestASnapshotAfterTheCloseWithholds(t *testing.T) {
	h, s := configured(t)
	at := sessionEnd(0)
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b", at, bar(100, at)))
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, (&account{cash: 4000}).snapshot(at.Add(time.Hour), 100)))
	out := mustApply(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}))
	if len(out) != 1 {
		t.Fatalf("want only the withheld decision, got %d emissions", len(out))
	}
	var p event.TargetsDecidedPayload
	decode(t, out[0], &p)
	if p.Outcome != event.TargetsOutcomeWithheld || p.WithheldReason != event.WithheldStaleAccountSnapshot || !strings.Contains(p.Detail, "after") {
		t.Fatalf("a later snapshot: %+v", p)
	}
}

// TestAShortHoldingIsAnError: the planner never plans against a short
// (internal/orderplan), and the joint account never holds one, so a snapshot
// reporting one stops the run rather than deciding from it.
func TestAShortHoldingIsAnError(t *testing.T) {
	h, s := configured(t)
	at := sessionEnd(0)
	snap := (&account{cash: 4000}).snapshot(at, 100)
	snap.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: -10}}
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b", at, bar(100, at)))
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotPositionsVersion, "a", at, snap))
	mustRefuse(t, h, s.envelope(t, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "s", at, event.SessionClosedPayload{PeriodEnd: at, InstrumentIDs: []string{"SPY"}}), "short")
}
