package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// These two tests are the point of #48: a journal can fail either of two
// independent checks, and the two failures mean different things.
//
//   - Chain verification answers "was this file edited after it was
//     written". A forger who recomputes the chain defeats it.
//   - Replay equivalence answers "does this engine still produce these
//     decisions". It reads the journal's INPUT stream and compares what the
//     reducer produces from it against the decisions the journal recorded,
//     so it catches a forged decision however well the chain was repaired —
//     and it is undisturbed by an edit to a record the inputs do not depend
//     on, which is what leaves chain verification the only check that sees
//     the careless edit.

// splitJournal separates the journal's input stream from the decisions the
// reducer produced, from the record's own Kind — the claim the chain covers
// (ADR 0017), not an inference from a producer's name.
func splitJournal(records []journal.Record) (inputs, decisions []event.Envelope) {
	for _, record := range records {
		if record.Kind == journal.KindDecision {
			decisions = append(decisions, record.Envelope)
			continue
		}
		inputs = append(inputs, record.Envelope)
	}
	return inputs, decisions
}

// replayInputs is replay equivalence's own question, asked of a journal's
// input stream: run it through a fresh reducer and see what this build
// decides now.
func replayInputs(t *testing.T, inputs []event.Envelope) []event.Envelope {
	t.Helper()

	cfg := fixtureConfiguration(t)
	reducer, err := strategy.NewReducer(event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, testBuild), cfg)
	if err != nil {
		t.Fatalf("strategy.NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	emitted, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Engine.Run() error = %v", err)
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

// alterRecordedQuantity changes what a recorded expiry says was proposed.
// repairPayloadHash is what separates the two editors below: a careful
// forger leaves the envelope internally consistent, a careless one does not.
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

// lastDecision is the index in records of the final decision, which every
// fixture run ends with (the end-of-stream expiry).
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

// TestAForgedDecisionPassesChainVerificationAndIsCaughtByReplayEquivalence:
// the forger edits a recorded decision and recomputes the whole chain, so
// the file verifies. Replaying the journal's own inputs reproduces what the
// reducer actually decided, which is not what the file now says.
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

	// The forger rewrites the file with the chain recomputed over their own
	// version of history.
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

	// Chain verification is satisfied: the forgery is internally consistent.
	forgedRaw, err := os.ReadFile(forgedPath)
	if err != nil {
		t.Fatalf("read the forged journal: %v", err)
	}
	if _, err := journal.Verify(bytes.NewReader(forgedRaw)); err != nil {
		t.Fatalf("journal.Verify() error = %v; a forger who recomputes the chain must defeat it, or this test is not testing what it claims", err)
	}

	// Replay equivalence is not: the inputs still say what really happened.
	_, forgedRecords := readJournalFile(t, forgedPath)
	inputs, recorded := splitJournal(forgedRecords)
	replayed := replayInputs(t, inputs)

	if reflect.DeepEqual(recorded, replayed) {
		t.Fatal("replaying the journal's inputs reproduced the forged decisions; the forgery went undetected by both checks")
	}
}

// TestACarelessEditIsCaughtByChainVerificationWhileReplayOfItsInputsStillReproducesTheOriginalDecisions:
// the careless editor changes a recorded decision and leaves the chain
// alone. The chain names the record they touched. The input stream is
// untouched, so replaying it produces exactly the decisions the run
// originally made — which is why the chain, not replay, is what attributes
// this edit.
func TestACarelessEditIsCaughtByChainVerificationWhileReplayOfItsInputsStillReproducesTheOriginalDecisions(t *testing.T) {
	_, path := runBacktestTo(t)
	header, records := readJournalFile(t, path)

	_, original := splitJournal(records)

	edited := lastDecision(t, records)
	records[edited].Envelope = alterRecordedQuantity(t, records[edited].Envelope, false)

	editedPath := filepath.Join(t.TempDir(), "edited.jsonl")
	writeRecordsVerbatim(t, editedPath, header, records)

	editedRaw, err := os.ReadFile(editedPath)
	if err != nil {
		t.Fatalf("read the edited journal: %v", err)
	}

	// Chain verification catches it, and names the record.
	_, err = journal.Verify(bytes.NewReader(editedRaw))
	var broken *journal.ChainBrokenError
	if !errors.As(err, &broken) {
		t.Fatalf("journal.Verify() error = %v, want a ChainBrokenError", err)
	}
	if broken.Sequence != records[edited].Sequence {
		t.Fatalf("the chain broke at record %d, want %d", broken.Sequence, records[edited].Sequence)
	}

	// And a replay of the journal's inputs still reproduces the decisions
	// the run originally made: the edit damaged the record of history, not
	// the history the inputs describe.
	_, editedRecords, err := journal.Read(bytes.NewReader(editedRaw))
	if err != nil {
		t.Fatalf("journal.Read() error = %v", err)
	}
	inputs, _ := splitJournal(editedRecords)
	if !reflect.DeepEqual(replayInputs(t, inputs), original) {
		t.Fatal("replaying the edited journal's inputs did not reproduce the run's original decisions")
	}
}

// writeRecordsVerbatim writes records exactly as given, chain hashes
// included — what an editor with a text editor and no intent to repair
// anything leaves behind.
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
