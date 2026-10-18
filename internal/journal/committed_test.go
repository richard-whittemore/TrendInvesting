package journal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// TestCommittedIsTheGroupTheLogHoldsForAnInput: the committed reply to an
// input is built from the group committed for it, its decisions stamped as
// recorded, with the refusal the handler returned, if any. An input never
// committed, because its group could not be made durable, has none (ADR
// 0027, amendment "re-delivery on request").
func TestCommittedIsTheGroupTheLogHoldsForAnInput(t *testing.T) {
	t.Parallel()

	refused := errors.New("refused")
	inner := emittingHandler()
	handler := replay.HandlerFunc(func(ctx context.Context, in event.Envelope) ([]event.Envelope, error) {
		out, err := inner.Apply(ctx, in)
		if in.Sequence == 2 {
			return out, refused
		}
		return out, err
	})
	d := &disk{}
	recorder := durableRecorder(t, handler, d)
	inputs := testInputs(3)
	for _, in := range inputs[:2] {
		_, _ = recorder.Apply(context.Background(), in)
	}
	entries := recorder.Entries()

	first, ok := recorder.Committed(inputs[0].ID)
	if !ok || first.Input.ID != inputs[0].ID || first.Refusal != "" || len(first.Decisions) != 2 || first.Decisions[0].ID != entries[1].Envelope.ID || first.Decisions[0].Sequence != entries[1].Envelope.Sequence {
		t.Fatalf("Committed(%s) = %+v, %v; want its group as recorded", inputs[0].ID, first, ok)
	}
	if second, ok := recorder.Committed(inputs[1].ID); !ok || second.Refusal != "refused" {
		t.Fatalf("Committed(%s) = %+v, %v; want its refusal", inputs[1].ID, second, ok)
	}
	d.failSync = true
	_, _ = recorder.Apply(context.Background(), inputs[2])
	if _, ok := recorder.Committed(inputs[2].ID); ok {
		t.Fatalf("Committed(%s) reports a group that was never made durable", inputs[2].ID)
	}
	if _, ok := recorder.Committed("never-sent"); ok {
		t.Fatal("Committed reports an input never applied")
	}
}

// assertCommitted fails unless the recorder committed in, at its own
// sequence, exactly when want says it did.
func assertCommitted(t *testing.T, name string, recorder *journal.Recorder, in event.Envelope, want bool) {
	t.Helper()
	got, ok := recorder.Committed(in.ID)
	if ok != want || ok && got.Input.Sequence != in.Sequence {
		t.Errorf("%s: Committed(%s) = %+v, %t; want committed %t", name, in.ID, got.Input, ok, want)
	}
}

// TestARecorderStatesWhichInputsItCommitted: a caller that must tell an
// input the recorder took, refused by the handler or not, from one it never
// appended asks the recorder, which commits an input once its group is held
// and, for a durable recorder, made durable (ADR 0027). An input refused by
// the record bound, the emission ceiling or a failed write was not
// committed, though the handler's own refusal was.
func TestARecorderStatesWhichInputsItCommitted(t *testing.T) {
	t.Parallel()

	refused := errors.New("refused by the handler")
	refusing := replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, refused })
	inputs := testInputs(3)

	memory := journal.NewRecorder(refusing)
	assertCommitted(t, "fresh", memory, inputs[0], false)
	if _, err := memory.Apply(context.Background(), inputs[0]); !errors.Is(err, refused) {
		t.Fatalf("Apply() = %v, want the handler's refusal", err)
	}
	assertCommitted(t, "the handler's refusal", memory, inputs[0], true)

	bounded := journal.NewBoundedRecorder(emittingHandler(), 1)
	if _, err := bounded.Apply(context.Background(), inputs[0]); err != nil {
		t.Fatal(err)
	}
	var limit *journal.RecordLimitError
	if _, err := bounded.Apply(context.Background(), inputs[1]); !errors.As(err, &limit) {
		t.Fatalf("Apply() = %v, want the record bound", err)
	}
	assertCommitted(t, "under the record bound", bounded, inputs[0], true)
	assertCommitted(t, "the record bound", bounded, inputs[1], false)

	d := &disk{}
	durable := journal.NewDurableRecorder(burstHandler(journal.MaxEmissionsPerInput+1, nil), createLog(t, d), journal.DefaultMaxRecords)
	var ceiling *journal.EmissionLimitError
	if _, err := durable.Apply(context.Background(), inputs[0]); !errors.As(err, &ceiling) {
		t.Fatalf("Apply() = %v, want the emission ceiling", err)
	}
	assertCommitted(t, "the emission ceiling", durable, inputs[0], false)

	d = &disk{}
	durable = durableRecorder(t, emittingHandler(), d)
	if _, err := durable.Apply(context.Background(), inputs[0]); err != nil {
		t.Fatal(err)
	}
	assertCommitted(t, "a durable input", durable, inputs[0], true)
	d.failSync = true
	var durability *journal.DurabilityError
	if _, err := durable.Apply(context.Background(), inputs[1]); !errors.As(err, &durability) {
		t.Fatalf("Apply() = %v, want a durability failure", err)
	}
	assertCommitted(t, "a failed flush", durable, inputs[1], false)
}

// TestInputsRecordedBeforeCountsBackToTheFirstThatDoesNotMatch: the inputs
// recorded immediately before a committed input, decisions skipped, are
// counted back to the nearest input that does not match, so a caller can
// tell how far inputs of its own moved an input from the sequence it was
// sent with (ADR 0027, amendment "re-delivery on request"). An input never
// committed has none before it.
func TestInputsRecordedBeforeCountsBackToTheFirstThatDoesNotMatch(t *testing.T) {
	t.Parallel()

	recorder := journal.NewRecorder(emittingHandler())
	inputs := testInputs(5)
	for _, i := range []int{1, 2} {
		inputs[i].Source = "own"
	}
	driveRecorder(t, recorder, inputs)
	own := func(e event.Envelope) bool { return e.Source == "own" }
	for _, c := range []struct {
		id   string
		want uint64
	}{
		{inputs[0].ID, 0}, {inputs[1].ID, 0}, {inputs[2].ID, 1}, {inputs[3].ID, 2}, {inputs[4].ID, 0}, {"never-sent", 0},
	} {
		if got := recorder.InputsRecordedBefore(c.id, own); got != c.want {
			t.Errorf("InputsRecordedBefore(%s) = %d, want %d", c.id, got, c.want)
		}
	}
}
