package journal_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// decisionFor is what the test handler emits for input n: two decisions, so
// the tests can tell emission order from input order.
func decisionFor(n uint64, index int) event.Envelope {
	payload := json.RawMessage(fmt.Sprintf(`{"for":%d}`, n))
	return event.Envelope{
		ID:                fmt.Sprintf("decision-%d-%d", n, index),
		Type:              "test.decision",
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         at(int(n)),
		RecordedAt:        at(int(n)),
		Source:            "reducer",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// emittingHandler emits two decisions for every input.
func emittingHandler() replay.Handler {
	return replay.HandlerFunc(func(_ context.Context, input event.Envelope) ([]event.Envelope, error) {
		return []event.Envelope{decisionFor(input.Sequence, 0), decisionFor(input.Sequence, 1)}, nil
	})
}

func driveRecorder(t *testing.T, recorder *journal.Recorder, inputs []event.Envelope) {
	t.Helper()
	for _, input := range inputs {
		if _, err := recorder.Apply(context.Background(), input); err != nil {
			t.Fatalf("Recorder.Apply(%s) error = %v", input.ID, err)
		}
	}
}

// TestRecorderRecordsEachInputThenTheDecisionsItCaused: #19's "every input
// and decision event in a single contiguous sequence", in recording order.
func TestRecorderRecordsEachInputThenTheDecisionsItCaused(t *testing.T) {
	t.Parallel()

	recorder := journal.NewRecorder(emittingHandler())
	inputs := testEnvelopes(2)
	driveRecorder(t, recorder, inputs)

	got := recorder.Envelopes()
	want := []string{"evt-1", "decision-1-0", "decision-1-1", "evt-2", "decision-2-0", "decision-2-1"}
	if len(got) != len(want) {
		t.Fatalf("recorded %d envelopes, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("record %d is %q, want %q", i, got[i].ID, id)
		}
	}
}

// TestRecorderStampsDecisionsExactlyAsTheReplayEngineWould is what makes the
// journal evidence: a replay of the journal's own input stream must
// reproduce the recorded decisions byte for byte, stamping included.
func TestRecorderStampsDecisionsExactlyAsTheReplayEngineWould(t *testing.T) {
	t.Parallel()

	inputs := testEnvelopes(3)

	recorder := journal.NewRecorder(emittingHandler())
	driveRecorder(t, recorder, inputs)

	engine, err := replay.New(emittingHandler())
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	replayed, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Engine.Run() error = %v", err)
	}

	var recorded []event.Envelope
	for _, envelope := range recorder.Envelopes() {
		if envelope.Source == "reducer" {
			recorded = append(recorded, envelope)
		}
	}
	if !reflect.DeepEqual(recorded, replayed) {
		t.Fatalf("the recorded decisions differ from a replay of the same inputs:\n recorded %+v\n replayed %+v", recorded, replayed)
	}
}

// TestRecorderRecordsTheFinalEmissionOfAFailingHandler: a handler that fails
// closed may emit a final event explaining why, and that event must reach
// the journal (replay.Handler's contract).
func TestRecorderRecordsTheFinalEmissionOfAFailingHandler(t *testing.T) {
	t.Parallel()

	halted := errors.New("capital-safety halt")
	recorder := journal.NewRecorder(replay.HandlerFunc(func(_ context.Context, input event.Envelope) ([]event.Envelope, error) {
		return []event.Envelope{decisionFor(input.Sequence, 0)}, halted
	}))

	_, err := recorder.Apply(context.Background(), testEnvelope(1))
	if !errors.Is(err, halted) {
		t.Fatalf("Recorder.Apply() error = %v, want the handler's own", err)
	}

	got := recorder.Envelopes()
	if len(got) != 2 || got[1].ID != "decision-1-0" {
		t.Fatalf("the failing handler's final emission did not reach the journal: %+v", got)
	}
}

// TestRecorderFailsClosedOnADecisionThatCannotBeJournalled: an emission that
// is invalid once stamped is a failure in its own right, exactly as
// replay.Engine treats it.
func TestRecorderFailsClosedOnADecisionThatCannotBeJournalled(t *testing.T) {
	t.Parallel()

	recorder := journal.NewRecorder(replay.HandlerFunc(func(_ context.Context, input event.Envelope) ([]event.Envelope, error) {
		broken := decisionFor(input.Sequence, 0)
		broken.PayloadHash = "not-the-payload-hash"
		return []event.Envelope{broken}, nil
	}))

	_, err := recorder.Apply(context.Background(), testEnvelope(1))
	if err == nil {
		t.Fatal("Recorder.Apply() error = nil, want one naming the unjournallable emission")
	}
	if !strings.Contains(err.Error(), "payload hash") {
		t.Fatalf("Recorder.Apply() error = %v, want one naming the unjournallable emission", err)
	}
}

// TestRecorderHeaderSpansTheFirstAndLastInputEventTime: the span is derived
// from the run, never passed in — and it covers the INPUT stream, which is
// what the run was over.
func TestRecorderHeaderSpansTheFirstAndLastInputEventTime(t *testing.T) {
	t.Parallel()

	recorder := journal.NewRecorder(emittingHandler())
	driveRecorder(t, recorder, testEnvelopes(3))

	header, err := recorder.Header(testConfigurationHash, testStrategyVersion)
	if err != nil {
		t.Fatalf("Recorder.Header() error = %v", err)
	}
	if !header.SpanStart.Equal(at(1)) {
		t.Errorf("SpanStart = %s, want %s", header.SpanStart, at(1))
	}
	if !header.SpanEnd.Equal(at(3)) {
		t.Errorf("SpanEnd = %s, want %s", header.SpanEnd, at(3))
	}
	if header.JournalVersion != journal.FormatVersion {
		t.Errorf("JournalVersion = %d, want %d", header.JournalVersion, journal.FormatVersion)
	}
	if header.ChainAlgorithm == "" {
		t.Error("ChainAlgorithm is empty; a verifier must be told how the chain was computed")
	}
}

func TestRecorderHeaderRefusesARunWithNoInputs(t *testing.T) {
	t.Parallel()

	recorder := journal.NewRecorder(emittingHandler())
	if _, err := recorder.Header(testConfigurationHash, testStrategyVersion); err == nil {
		t.Fatal("Recorder.Header() error = nil, want one refusing to state a span for a run with no inputs")
	}
}
