package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// stoppedJournal reads the journal at path and inserts an
// event.AdapterRunStoppedEventType input immediately before its final
// replay.run.completed input, renumbering every record from the insertion
// point on and recomputing the chain. cmd/backtest never produces
// this event itself — it drives the reducer from a bar fixture directly
// rather than through an adapter — so this models, at the journal seam, what
// a LEAN adapter's own journal looks like when it deliberately stops.
func stoppedJournal(t *testing.T, path, reason, instrumentID, detail string) string {
	t.Helper()
	return stoppedJournalEndingAt(t, path, reason, instrumentID, detail, true)
}

// stoppedJournalEndingAt is stoppedJournal, but with keepCompletion false it
// drops replay.run.completed and everything after it, modelling an adapter
// whose completion exchange failed after the engine accepted its stop.
func stoppedJournalEndingAt(t *testing.T, path, reason, instrumentID, detail string, keepCompletion bool) string {
	t.Helper()

	header, records := readJournalFile(t, path)

	insertAt := -1
	for i, r := range records {
		if r.Kind == journal.KindInput && r.Envelope.Type == event.RunCompletedEventType {
			insertAt = i
			break
		}
	}
	if insertAt < 0 {
		t.Fatal("the source journal records no replay.run.completed input to stop before")
	}
	completedAt := records[insertAt].Envelope.EventTime

	stopPayload, err := json.Marshal(event.AdapterRunStoppedPayload{
		Reason: reason, InstrumentID: instrumentID, Detail: detail,
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := event.Envelope{
		ID:                "run-stopped",
		Type:              event.AdapterRunStoppedEventType,
		SchemaVersion:     event.AdapterRunStoppedSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         completedAt,
		RecordedAt:        completedAt,
		Source:            sourceFixture,
		StrategyVersion:   header.StrategyVersion,
		ConfigurationHash: header.ConfigurationHash,
		PayloadHash:       event.HashPayload(stopPayload),
		Payload:           stopPayload,
	}

	entries := make([]journal.Entry, 0, len(records)+1)
	for _, r := range records[:insertAt] {
		entries = append(entries, journal.Entry{Kind: r.Kind, Envelope: r.Envelope})
	}
	entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: stop})
	if keepCompletion {
		for _, r := range records[insertAt:] {
			entries = append(entries, journal.Entry{Kind: r.Kind, Envelope: r.Envelope})
		}
	}
	// journal.Write derives each record's own Sequence from position; the
	// envelope's OWN Sequence field is renumbered here too, so the written
	// file is internally consistent for any check that reads it instead
	// (ADR 0017's contiguity rule).
	for i := range entries {
		entries[i].Envelope.Sequence = uint64(i + 1)
	}

	out := filepath.Join(t.TempDir(), "stopped.jsonl")
	file, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Write(file, header, entries); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestVerifyReportsAStoppedRunDistinctlyFromComplete is the claim on
// cmd/backtest's -verify: a run an adapter deliberately stopped is reported
// with its reason and instrument, never as plain "complete" — the shape
// TestVerifyReportsRunCompleteness already pins for the ordinary two cases
// (complete, incomplete).
func TestVerifyReportsAStoppedRunDistinctlyFromComplete(t *testing.T) {
	_, path := runBacktestTo(t)
	stopped := stoppedJournal(t, path, event.AdapterRunStoppedReasonDelisted, "AAPL", "LEAN reports AAPL DELISTED")

	verification := verifyJournal(t, stopped)
	if !verification.Stopped {
		t.Fatal("Stopped = false, want true")
	}
	if !verification.Complete {
		t.Error("Complete = false, want true: the stop is followed by replay.run.completed")
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"-verify", stopped}, &out); err != nil {
		t.Fatalf("run(-verify): %v", err)
	}
	want := "run                stopped: delisted AAPL\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("verify report = %q, want it to contain %q", out.String(), want)
	}
	if strings.Contains(out.String(), "run                complete\n") {
		t.Error("a stopped run must never be reported as plain \"complete\"")
	}
}

// TestVerifyReportsAStopWithoutCompletionAsIncomplete: when the engine
// accepted a stop but the completion exchange then failed, the journal ends
// at the stop and outstanding proposals were never expired. The report must
// keep INCOMPLETE, which is the fact ADR 0012 requires to stay visible, as
// well as naming the stop.
func TestVerifyReportsAStopWithoutCompletionAsIncomplete(t *testing.T) {
	_, path := runBacktestTo(t)
	stopped := stoppedJournalEndingAt(t, path, event.AdapterRunStoppedReasonDelisted, "AAPL", "LEAN reports AAPL DELISTED", false)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"-verify", stopped}, &out); err != nil {
		t.Fatalf("run(-verify): %v", err)
	}
	for _, want := range []string{"INCOMPLETE", "stopped: delisted AAPL"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("verify report = %q, want it to contain %q", out.String(), want)
		}
	}
}
