package journal_test

import (
	"reflect"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// TestSplitSeparatesInputsFromDecisionsByKind confirms the split reads each
// record's own Kind rather than its position, and preserves recording order
// within each pile.
func TestSplitSeparatesInputsFromDecisionsByKind(t *testing.T) {
	t.Parallel()

	entries := testEntries(6) // alternates input, decision, input, decision, ...
	var records []journal.Record
	chain := journal.NewChain(testHeader())
	for i, entry := range entries {
		records = append(records, journal.Record{
			Sequence:   uint64(i + 1),
			Kind:       entry.Kind,
			Envelope:   entry.Envelope,
			RecordHash: chain.Next(entry.Kind, entry.Envelope),
		})
	}

	inputs, decisions, err := journal.Split(records)
	if err != nil {
		t.Fatalf("Split() error = %v", err)
	}
	if len(inputs) != 3 || len(decisions) != 3 {
		t.Fatalf("Split() = %d input(s), %d decision(s), want 3 and 3", len(inputs), len(decisions))
	}
	for i, want := range []int{1, 3, 5} {
		if !reflect.DeepEqual(inputs[i], entries[want-1].Envelope) {
			t.Errorf("input %d = %+v, want the envelope from record %d", i, inputs[i], want)
		}
	}
	for i, want := range []int{2, 4, 6} {
		if !reflect.DeepEqual(decisions[i], entries[want-1].Envelope) {
			t.Errorf("decision %d = %+v, want the envelope from record %d", i, decisions[i], want)
		}
	}
}

// TestSplitFailsClosedOnAnUnrecognisedKind: a record whose Kind is outside
// the closed set must not be silently sorted into either pile.
func TestSplitFailsClosedOnAnUnrecognisedKind(t *testing.T) {
	t.Parallel()

	records := []journal.Record{
		{Sequence: 1, Kind: journal.KindInput, Envelope: testEnvelope(1)},
		{Sequence: 2, Kind: "forged", Envelope: testEnvelope(2)},
	}

	inputs, decisions, err := journal.Split(records)
	if err == nil {
		t.Fatal("Split() error = nil, want a refusal naming the unrecognised kind")
	}
	if inputs != nil || decisions != nil {
		t.Fatalf("Split() = %v, %v on error, want both nil", inputs, decisions)
	}
}
