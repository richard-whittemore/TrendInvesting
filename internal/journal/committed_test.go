package journal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// assertCommitted fails unless the recorder's last committed input is want,
// or, for want zero, unless it has committed none.
func assertCommitted(t *testing.T, name string, recorder *journal.Recorder, want uint64) {
	t.Helper()
	got, ok := recorder.LastCommitted()
	if want == 0 && ok || want != 0 && (!ok || got != want) {
		t.Errorf("%s: LastCommitted() = %d, %t; want %d", name, got, ok, want)
	}
}

// TestARecorderStatesTheLastInputItCommitted: a caller that must tell an
// input the recorder took, refused by the handler or not, from one it never
// appended asks the recorder, which commits an input once its group is held
// and, for a durable recorder, made durable (ADR 0027). An input refused by
// the record bound, the emission ceiling or a failed write was not
// committed, though the handler's own refusal was.
func TestARecorderStatesTheLastInputItCommitted(t *testing.T) {
	t.Parallel()

	refused := errors.New("refused by the handler")
	refusing := replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, refused })
	inputs := testInputs(3)

	memory := journal.NewRecorder(refusing)
	assertCommitted(t, "fresh", memory, 0)
	if _, err := memory.Apply(context.Background(), inputs[0]); !errors.Is(err, refused) {
		t.Fatalf("Apply() = %v, want the handler's refusal", err)
	}
	assertCommitted(t, "the handler's refusal", memory, 1)

	bounded := journal.NewBoundedRecorder(emittingHandler(), 1)
	if _, err := bounded.Apply(context.Background(), inputs[0]); err != nil {
		t.Fatal(err)
	}
	var limit *journal.RecordLimitError
	if _, err := bounded.Apply(context.Background(), inputs[1]); !errors.As(err, &limit) {
		t.Fatalf("Apply() = %v, want the record bound", err)
	}
	assertCommitted(t, "the record bound", bounded, 1)

	d := &disk{}
	durable := journal.NewDurableRecorder(burstHandler(journal.MaxEmissionsPerInput+1, nil), createLog(t, d), journal.DefaultMaxRecords)
	var ceiling *journal.EmissionLimitError
	if _, err := durable.Apply(context.Background(), inputs[0]); !errors.As(err, &ceiling) {
		t.Fatalf("Apply() = %v, want the emission ceiling", err)
	}
	assertCommitted(t, "the emission ceiling", durable, 0)

	d = &disk{}
	durable = durableRecorder(t, emittingHandler(), d)
	if _, err := durable.Apply(context.Background(), inputs[0]); err != nil {
		t.Fatal(err)
	}
	assertCommitted(t, "a durable input", durable, 1)
	d.failSync = true
	var durability *journal.DurabilityError
	if _, err := durable.Apply(context.Background(), inputs[1]); !errors.As(err, &durability) {
		t.Fatalf("Apply() = %v, want a durability failure", err)
	}
	assertCommitted(t, "a failed flush", durable, 1)
}
