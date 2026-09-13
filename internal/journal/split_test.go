package journal_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
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

// TestCheckIdentityAcceptsAJournalWhoseRecordsAllNameItsHeadersRun is the
// accepting direction: the writer stamps every record with the run's
// identity, so an unaltered journal passes.
func TestCheckIdentityAcceptsAJournalWhoseRecordsAllNameItsHeadersRun(t *testing.T) {
	t.Parallel()

	var records []journal.Record
	for i, entry := range testEntries(4) {
		records = append(records, journal.Record{Sequence: uint64(i + 1), Kind: entry.Kind, Envelope: entry.Envelope})
	}

	if err := journal.CheckIdentity(testHeader(), records); err != nil {
		t.Fatalf("CheckIdentity() error = %v, want nil for a journal whose records all name its own run", err)
	}
}

// TestCheckIdentityNamesTheFirstRecordFromAnotherRun: each identity field is
// checked, and the refusal names the record so a reader is told which one
// rather than only that something is wrong.
func TestCheckIdentityNamesTheFirstRecordFromAnotherRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		alter   func(*event.Envelope)
		wantsIn string
	}{
		{
			name:    "a record from another strategy version",
			alter:   func(e *event.Envelope) { e.StrategyVersion = "other-strategy/9.9.9+elsewhere" },
			wantsIn: "other-strategy/9.9.9+elsewhere",
		},
		{
			name:    "a record from another configuration",
			alter:   func(e *event.Envelope) { e.ConfigurationHash = "sha256:another" },
			wantsIn: "sha256:another",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var records []journal.Record
			for i, entry := range testEntries(4) {
				records = append(records, journal.Record{Sequence: uint64(i + 1), Kind: entry.Kind, Envelope: entry.Envelope})
			}
			tt.alter(&records[2].Envelope)

			err := journal.CheckIdentity(testHeader(), records)
			if err == nil {
				t.Fatal("CheckIdentity() error = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), "record 3") {
				t.Errorf("CheckIdentity() error = %v, want it to name record 3", err)
			}
			if !strings.Contains(err.Error(), tt.wantsIn) {
				t.Errorf("CheckIdentity() error = %v, want it to quote %q", err, tt.wantsIn)
			}
		})
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
