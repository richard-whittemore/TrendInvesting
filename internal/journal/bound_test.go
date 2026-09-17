package journal_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// A run's journal is composed in memory, so a run large enough either fits or
// dies on an allocation having written nothing at all. The bound is the
// tripwire that turns the second outcome into a run that stops, says why, and
// journals what it took.

func TestABoundedRecorderStopsAtItsBound(t *testing.T) {
	t.Parallel()

	// The handler emits two decisions per input, so each input costs three
	// entries: one input fits under a bound of three and a second does not.
	recorder := journal.NewBoundedRecorder(emittingHandler(), 3)
	if _, err := recorder.Apply(context.Background(), testEnvelope(1)); err != nil {
		t.Fatalf("Recorder.Apply() error = %v, want the first input recorded", err)
	}

	_, err := recorder.Apply(context.Background(), testEnvelope(3))

	var limit *journal.RecordLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("Recorder.Apply() error = %v, want a *journal.RecordLimitError", err)
	}
	if limit.Limit != 3 {
		t.Errorf("Limit = %d, want the 3 the recorder was bounded at", limit.Limit)
	}
	if limit.Recorded != 3 {
		t.Errorf("Recorded = %d, want the 3 entries the first input cost", limit.Recorded)
	}
}

// TestTheBoundIsCheckedBeforeAnInputIsRecorded is what keeps the truncated
// journal honest. Refusing part way through an input's emissions would leave a
// journal claiming the reducer decided nothing for its last input, and replay
// equivalence would report that as a divergence in a faithful record.
func TestTheBoundIsCheckedBeforeAnInputIsRecorded(t *testing.T) {
	t.Parallel()

	recorder := journal.NewBoundedRecorder(emittingHandler(), 3)
	if _, err := recorder.Apply(context.Background(), testEnvelope(1)); err != nil {
		t.Fatalf("Recorder.Apply() error = %v", err)
	}
	if _, err := recorder.Apply(context.Background(), testEnvelope(3)); err == nil {
		t.Fatal("Recorder.Apply() error = nil, want the second input refused")
	}

	entries := recorder.Entries()
	if len(entries) != 3 {
		t.Fatalf("recorded %d entries, want the 3 the first input cost and nothing of the input refused", len(entries))
	}
	if entries[0].Kind != journal.KindInput {
		t.Fatalf("entry 0 is a %s, want the input", entries[0].Kind)
	}
	for i, entry := range entries[1:] {
		if entry.Kind != journal.KindDecision {
			t.Errorf("entry %d is a %s, want the decision the recorded input caused", i+1, entry.Kind)
		}
	}
}

// TestARunStoppedAtTheBoundStillJournalsWhatItRecorded is the whole point of
// the tripwire: an OOM leaves nothing, and this leaves a journal that verifies
// under a header stating the span the run actually reached.
func TestARunStoppedAtTheBoundStillJournalsWhatItRecorded(t *testing.T) {
	t.Parallel()

	recorder := journal.NewBoundedRecorder(emittingHandler(), 3)
	if _, err := recorder.Apply(context.Background(), testEnvelope(1)); err != nil {
		t.Fatalf("Recorder.Apply() error = %v", err)
	}
	if _, err := recorder.Apply(context.Background(), testEnvelope(3)); err == nil {
		t.Fatal("Recorder.Apply() error = nil, want the second input refused")
	}

	header, err := recorder.Header(testConfigurationHash, testStrategyVersion)
	if err != nil {
		t.Fatalf("Recorder.Header() error = %v", err)
	}
	if !header.SpanStart.Equal(at(1)) || !header.SpanEnd.Equal(at(1)) {
		t.Fatalf("span = [%s, %s], want the day-1 input it applied and not the day-3 input it refused", header.SpanStart, header.SpanEnd)
	}

	var buf bytes.Buffer
	if err := journal.Write(&buf, header, recorder.Entries()); err != nil {
		t.Fatalf("journal.Write() error = %v, want the truncated run journalled", err)
	}
	verification, err := journal.Verify(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("journal.Verify() error = %v, want the truncated journal to verify", err)
	}
	if verification.RecordCount != 3 {
		t.Errorf("RecordCount = %d, want 3", verification.RecordCount)
	}
}

// TestTheRecordBoundErrorNamesWhatToDoAboutIt: the operator reads this
// instead of an OOM, so it has to say what was reached, that the cost is
// memory, and what they can do.
func TestTheRecordBoundErrorNamesWhatToDoAboutIt(t *testing.T) {
	t.Parallel()

	recorder := journal.NewBoundedRecorder(emittingHandler(), 1)
	if _, err := recorder.Apply(context.Background(), testEnvelope(1)); err != nil {
		t.Fatalf("Recorder.Apply() error = %v", err)
	}

	_, err := recorder.Apply(context.Background(), testEnvelope(3))
	if err == nil {
		t.Fatal("Recorder.Apply() error = nil, want the bound reached")
	}

	for _, want := range []string{"in memory", "shorter", "raise"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

// TestARecorderBoundedAtNothingRecordsNothing: the bound is checked before
// the memory is committed, so a bound of zero refuses the first input rather
// than admitting one for free.
func TestARecorderBoundedAtNothingRecordsNothing(t *testing.T) {
	t.Parallel()

	recorder := journal.NewBoundedRecorder(emittingHandler(), 0)

	_, err := recorder.Apply(context.Background(), testEnvelope(1))
	var limit *journal.RecordLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("Recorder.Apply() error = %v, want a *journal.RecordLimitError", err)
	}
	if entries := recorder.Entries(); len(entries) != 0 {
		t.Errorf("recorded %d entries under a bound of nothing, want none", len(entries))
	}
}

func TestTheDefaultBoundIsAtLeastOneRecord(t *testing.T) {
	t.Parallel()

	if journal.DefaultMaxRecords < 1 {
		t.Fatalf("DefaultMaxRecords = %d; a recorder that can record nothing journals nothing", journal.DefaultMaxRecords)
	}
}

// burstHandler emits count decisions for every input, and fails with err.
// One input emitting a burst is the shape the record bound does not catch:
// the bound is checked between inputs, so the burst lands whole.
func burstHandler(count int, err error) replay.Handler {
	return replay.HandlerFunc(func(_ context.Context, input event.Envelope) ([]event.Envelope, error) {
		decisions := make([]event.Envelope, count)
		for i := range decisions {
			decisions[i] = decisionFor(input.Sequence, i)
		}
		return decisions, err
	})
}

// TestARunStopsWhenOneInputEmitsMoreThanTheCeiling is what makes the record
// bound's overshoot finite. The bound is checked at an input boundary, so a
// run exceeds it by whatever the admitted input emits, and a reducer ending
// the input stream emits one expiry per outstanding proposal across the
// whole universe (ADR 0011). Without a ceiling on that burst the bound has
// no stated size at all, against the exact failure it exists to prevent.
func TestARunStopsWhenOneInputEmitsMoreThanTheCeiling(t *testing.T) {
	t.Parallel()

	recorder := journal.NewRecorder(burstHandler(journal.MaxEmissionsPerInput+1, nil))

	_, err := recorder.Apply(context.Background(), testEnvelope(1))

	var ceiling *journal.EmissionLimitError
	if !errors.As(err, &ceiling) {
		t.Fatalf("Recorder.Apply() error = %v, want a *journal.EmissionLimitError", err)
	}
	if ceiling.Limit != journal.MaxEmissionsPerInput {
		t.Errorf("Limit = %d, want the %d one input may contribute", ceiling.Limit, journal.MaxEmissionsPerInput)
	}
	if ceiling.Emitted != journal.MaxEmissionsPerInput+1 {
		t.Errorf("Emitted = %d, want the %d the handler emitted", ceiling.Emitted, journal.MaxEmissionsPerInput+1)
	}
	if entries := recorder.Entries(); len(entries) != 0 {
		t.Errorf("recorded %d entries, want nothing of the input whose burst was refused", len(entries))
	}
}

// TestAnInputEmittingExactlyTheCeilingIsRecorded pins the ceiling as the
// largest burst a journal takes rather than the smallest it refuses, so
// MaxEmissionsPerInput is the number the overshoot is stated in.
func TestAnInputEmittingExactlyTheCeilingIsRecorded(t *testing.T) {
	t.Parallel()

	recorder := journal.NewRecorder(burstHandler(journal.MaxEmissionsPerInput, nil))

	if _, err := recorder.Apply(context.Background(), testEnvelope(1)); err != nil {
		t.Fatalf("Recorder.Apply() error = %v, want the ceiling itself admitted", err)
	}
	if got, want := len(recorder.Entries()), journal.MaxEmissionsPerInput+1; got != want {
		t.Errorf("recorded %d entries, want the input and the %d decisions it caused", got, journal.MaxEmissionsPerInput)
	}
}

// TestAnOverEmittingHandlerThatAlsoFailedClosedNamesBothFailures: a handler
// may emit a final event explaining why it stopped, alongside its error
// rather than instead of it (replay.Handler's contract). A burst too large
// to journal must not swallow the error it arrived with.
func TestAnOverEmittingHandlerThatAlsoFailedClosedNamesBothFailures(t *testing.T) {
	t.Parallel()

	handlerErr := errors.New("the reducer failed closed")
	recorder := journal.NewRecorder(burstHandler(journal.MaxEmissionsPerInput+1, handlerErr))

	_, err := recorder.Apply(context.Background(), testEnvelope(1))

	if !errors.Is(err, handlerErr) {
		t.Errorf("Recorder.Apply() error = %v, want the handler's own failure still in the chain", err)
	}
	var ceiling *journal.EmissionLimitError
	if !errors.As(err, &ceiling) {
		t.Errorf("Recorder.Apply() error = %v, want the emission ceiling named as well", err)
	}
}

// TestTheEmissionCeilingErrorNamesWhatItRefused: the operator reads this
// instead of an OOM, so it has to name the input, say the cost is memory,
// and say that the input it refused left nothing behind.
func TestTheEmissionCeilingErrorNamesWhatItRefused(t *testing.T) {
	t.Parallel()

	recorder := journal.NewRecorder(burstHandler(journal.MaxEmissionsPerInput+1, nil))

	_, err := recorder.Apply(context.Background(), testEnvelope(1))
	if err == nil {
		t.Fatal("Recorder.Apply() error = nil, want the ceiling reached")
	}

	for _, want := range []string{"evt-1", "in memory", "nothing of this input is recorded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}
