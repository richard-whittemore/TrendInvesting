package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// splitJournal separates inputs and decisions by the record's chain-covered
// Kind (ADR 0017). It uses journal.Split so unknown kinds fail exactly as they
// do in production; a producer's name must never determine the split.
func splitJournal(t *testing.T, records []journal.Record) (inputs, decisions []event.Envelope) {
	t.Helper()
	inputs, decisions, err := journal.Split(records)
	if err != nil {
		t.Fatalf("journal.Split() error = %v", err)
	}
	return inputs, decisions
}

// replayInputs runs a journal's inputs through a fresh reducer built from
// its header, using the production replay path (ADR 0017).
func replayInputs(t *testing.T, header journal.Header, inputs []event.Envelope) []event.Envelope {
	t.Helper()

	emitted, err := replayJournalInputs(header, inputs)
	if err != nil {
		t.Fatalf("replayJournalInputs() error = %v", err)
	}
	return emitted
}

func readJournalFile(t *testing.T, path string) (journal.Header, []journal.Record) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	header, records, err := journal.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	return header, records
}

// alterRecordedQuantity changes a recorded expiry's proposed quantity.
// repairPayloadHash preserves envelope integrity for a forgery; leaving it
// false models a careless edit (ADR 0017).
func alterRecordedQuantity(t *testing.T, envelope event.Envelope, repairPayloadHash bool) event.Envelope {
	t.Helper()

	var payload event.ProposalExpiredPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("decode the recorded expiry: %v", err)
	}
	payload.Quantity += 1000
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	envelope.Payload = encoded
	if repairPayloadHash {
		envelope.PayloadHash = event.HashPayload(encoded)
	}
	return envelope
}

// lastDecision finds the final decision; this fixture ends with an
// end-of-stream proposal expiry (ADR 0011; event.RunCompletedEventType).
func lastDecision(t *testing.T, records []journal.Record) int {
	t.Helper()
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Kind == journal.KindDecision {
			return i
		}
	}
	t.Fatal("the journal records no decision at all")
	return 0
}

// TestAForgedDecisionPassesChainVerificationAndIsCaughtByReplayEquivalence
// checks ADR 0017's independent checks: repaired payload and chain hashes
// pass verification, but unchanged inputs cannot reproduce a forged decision.
func TestAForgedDecisionPassesChainVerificationAndIsCaughtByReplayEquivalence(t *testing.T) {
	_, path := runBacktestTo(t)
	header, records := readJournalFile(t, path)

	forged := lastDecision(t, records)
	entries := make([]journal.Entry, 0, len(records))
	for i, record := range records {
		entry := journal.Entry{Kind: record.Kind, Envelope: record.Envelope}
		if i == forged {
			entry.Envelope = alterRecordedQuantity(t, record.Envelope, true)
		}
		entries = append(entries, entry)
	}

	forgedPath := filepath.Join(t.TempDir(), "forged.jsonl")
	file, err := os.Create(forgedPath)
	if err != nil {
		t.Fatalf("create the forged journal: %v", err)
	}
	if err := journal.Write(file, header, entries); err != nil {
		t.Fatalf("journal.Write() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close the forged journal: %v", err)
	}

	forgedRaw, err := os.ReadFile(forgedPath)
	if err != nil {
		t.Fatalf("read the forged journal: %v", err)
	}
	if _, err := journal.Verify(bytes.NewReader(forgedRaw)); err != nil {
		t.Fatalf("journal.Verify() error = %v; a forger who recomputes the chain must defeat it, or this test is not testing what it claims", err)
	}

	forgedHeader, forgedRecords := readJournalFile(t, forgedPath)
	inputs, recorded := splitJournal(t, forgedRecords)
	replayed := replayInputs(t, forgedHeader, inputs)

	if replay.Equivalent(recorded, replayed) == nil {
		t.Fatal("replaying the journal's inputs reproduced the forged decisions; the forgery went undetected by both checks")
	}
}

// TestACarelessEditIsCaughtByChainVerificationWhileReplayOfItsInputsStillReproducesTheOriginalDecisions
// checks that chain verification identifies an edited decision (ADR 0017).
// The unchanged input stream must still reproduce the original decisions;
// this comparison uses the originals, not the edited decisions.
func TestACarelessEditIsCaughtByChainVerificationWhileReplayOfItsInputsStillReproducesTheOriginalDecisions(t *testing.T) {
	_, path := runBacktestTo(t)
	header, records := readJournalFile(t, path)

	_, original := splitJournal(t, records)

	edited := lastDecision(t, records)
	records[edited].Envelope = alterRecordedQuantity(t, records[edited].Envelope, false)

	editedPath := filepath.Join(t.TempDir(), "edited.jsonl")
	writeRecordsVerbatim(t, editedPath, header, records)

	editedRaw, err := os.ReadFile(editedPath)
	if err != nil {
		t.Fatalf("read the edited journal: %v", err)
	}

	_, err = journal.Verify(bytes.NewReader(editedRaw))
	var broken *journal.ChainBrokenError
	if !errors.As(err, &broken) {
		t.Fatalf("journal.Verify() error = %v, want a ChainBrokenError", err)
	}
	if broken.Sequence != records[edited].Sequence {
		t.Fatalf("the chain broke at record %d, want %d", broken.Sequence, records[edited].Sequence)
	}

	editedHeader, editedRecords, err := journal.Read(bytes.NewReader(editedRaw))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	inputs, _ := splitJournal(t, editedRecords)
	if d := replay.Equivalent(original, replayInputs(t, editedHeader, inputs)); d != nil {
		t.Fatalf("replaying the edited journal's inputs did not reproduce the run's original decisions; first divergence at %d:\n want %+v\n got  %+v", d.Index, d.Want, d.Got)
	}
}

// writeRecordsVerbatim preserves supplied chain hashes when writing edited
// records, modelling a careless edit without hash repair (ADR 0017).
func writeRecordsVerbatim(t *testing.T, path string, header journal.Header, records []journal.Record) {
	t.Helper()

	var buf bytes.Buffer
	lines := []any{header}
	for _, record := range records {
		lines = append(lines, record)
	}
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		buf.Write(append(encoded, '\n'))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
