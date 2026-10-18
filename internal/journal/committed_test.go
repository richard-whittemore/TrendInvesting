package journal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
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
