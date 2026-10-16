package targetweight_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// TestABoostRestartFromItsJournalRecoversTheLatchAndTheEngineState loses
// the first session's mark, so the first order is refused and the account
// Degraded on SPY, then restarts while boosted (session 17). The restart
// reads the latch and the engine state back from the journal; every later
// decision and outcome is the uninterrupted run's (ADR 0026, point 5).
func TestABoostRestartFromItsJournalRecoversTheLatchAndTheEngineState(t *testing.T) {
	noMark := map[int]bool{0: true}
	full := simulate(t, options{noMark: noMark})
	earlier := simulate(t, options{noMark: noMark, closes: syntheticCloses()[:18]})
	inputs, err := targetweight.RestartInputs(entryEnvelopes(earlier.entries), targetweight.Options{DecideFrom: sessionEnd(0)}, sessionEnd(17).Add(3600e9))
	if err != nil {
		t.Fatal(err)
	}
	restarted := simulate(t, options{noMark: noMark, restart: inputs, from: 18, snapshots: full.inputs})

	got := recoveredRecord(t, restarted.decisions)
	if !got.BoostLatched || got.EngineState != event.EngineStateDegraded || !slices.Equal(got.Scope, []string{"SPY"}) {
		t.Errorf("recovered %+v", got)
	}
	if got.LastDecisionID != "targets-decided:joint:"+sessionEnd(17).Format("2006-01-02T15:04:05Z07:00") {
		t.Errorf("last decision %s", got.LastDecisionID)
	}
	var after []event.Envelope
	for _, e := range full.decisions {
		if e.EventTime.After(sessionEnd(17)) {
			after = append(after, e)
		}
	}
	var mine []event.Envelope
	for _, e := range restarted.decisions {
		if e.Type != event.RestartRecoveredEventType {
			mine = append(mine, e)
		}
	}
	if len(mine) != len(after) {
		t.Fatalf("%d decisions after the restart, want %d", len(mine), len(after))
	}
	for i := range mine {
		if mine[i].ID != after[i].ID || !bytes.Equal(mine[i].Payload, after[i].Payload) {
			t.Errorf("decision %d after the restart:\n got %s %s\nwant %s %s", i, mine[i].ID, mine[i].Payload, after[i].ID, after[i].Payload)
		}
	}
}
