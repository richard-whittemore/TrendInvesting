package strategy

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds white-box tests for applySymbolChange's own state-movement
// mechanics (ADR 0024) that an end-to-end fixture cannot reach on its own: a
// standing ADR 0020 hold under the old instrument id, and a declared ADR
// 0009 classification record, both present at the moment of a rename. It
// uses this package's own invariant-test fixture builder
// (invariant_test.go), which publishes a ready Campaign under "AAPL" without
// driving a whole bar/fill stream to get there.

// TestApplySymbolChangeRewritesHoldsAndClassifications pins symbol_change.go's
// own state-movement: a standing hold's instrumentID moves to the new id (so
// ADR 0008's per-instrument cap accounting still recognises it), and a
// declared classification record moves with it (ADR 0009).
func TestApplySymbolChangeRewritesHoldsAndClassifications(t *testing.T) {
	r := newConfiguredReducerForInvariantTest(t)
	buildCorruptedCampaignState(t, r, 90)
	r.holds = []hold{{proposalID: "p1", instrumentID: "AAPL", cost: 100}}
	r.classifications = map[string]classificationRecord{"AAPL": {}}

	payload := event.CorporateActionPayload{
		InstrumentID: "AAPL", Kind: event.CorporateActionKindSymbolChange,
		EffectiveAt: day(2), NewInstrumentID: "AAPL2",
	}
	input := event.Envelope{ID: "corporate-action-1"}
	emissions, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
		return tx.applySymbolChange(payload, input)
	})
	if err != nil {
		t.Fatalf("transact() error = %v", err)
	}
	if len(emissions) != 1 {
		t.Fatalf("got %d emission(s), want 1", len(emissions))
	}

	if len(r.holds) != 1 || r.holds[0].instrumentID != "AAPL2" {
		t.Fatalf("holds after rename = %+v, want instrumentID %q", r.holds, "AAPL2")
	}
	if _, stillOld := r.classifications["AAPL"]; stillOld {
		t.Fatal("classification record left under the old instrument id")
	}
	if _, ok := r.classifications["AAPL2"]; !ok {
		t.Fatal("classification record did not move to the new instrument id")
	}
	if _, oldKnown := r.instruments["AAPL"]; oldKnown {
		t.Fatal("old instrument id still published after the rename")
	}
	state, ok := r.instruments["AAPL2"]
	if !ok || state.campaign == nil {
		t.Fatal("new instrument id does not hold the moved Campaign")
	}
	if state.campaign.instrumentID != "AAPL2" {
		t.Fatalf("campaign.instrumentID = %q after the rename, want %q: every later Add proposal and Exit Order reads this field directly", state.campaign.instrumentID, "AAPL2")
	}
}

// TestInstrumentIDsExcludesAnInstrumentRenamedWithinTheTransaction pins
// transition.instrumentIDs' own exclusion of an id renamed away earlier in
// the SAME transaction (ADR 0024): commit has not yet deleted it from the
// published map, but it is no longer this transaction's to see.
func TestInstrumentIDsExcludesAnInstrumentRenamedWithinTheTransaction(t *testing.T) {
	r := newConfiguredReducerForInvariantTest(t)
	buildCorruptedCampaignState(t, r, 90)

	_, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
		state, ok := tx.instrument("AAPL")
		if !ok {
			t.Fatal("fixture instrument AAPL is missing")
		}
		tx.renameInstrument("AAPL", "AAPL2", state)

		ids := tx.instrumentIDs()
		for _, id := range ids {
			if id == "AAPL" {
				t.Fatal("instrumentIDs() still lists the id this transaction renamed away")
			}
		}
		found := false
		for _, id := range ids {
			if id == "AAPL2" {
				found = true
			}
		}
		if !found {
			t.Fatal("instrumentIDs() does not list the new id after a rename within the same transaction")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("transact() error = %v", err)
	}
}
