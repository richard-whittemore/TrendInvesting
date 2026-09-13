package replay_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// TestStampSetsTheThreeFieldsAHandlerMayNotSet: Sequence from the output
// stream's own counter, CausationID from the input, CorrelationID from the
// input's own or, absent one, the input's ID.
func TestStampSetsTheThreeFieldsAHandlerMayNotSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		inputID           string
		inputCorrelation  string
		wantCorrelationID string
	}{
		{
			name:              "correlation inherited from the input",
			inputID:           "bar:AAPL:1",
			inputCorrelation:  "run-7",
			wantCorrelationID: "run-7",
		},
		{
			name:              "correlation falls back to the input id",
			inputID:           "bar:AAPL:1",
			wantCorrelationID: "bar:AAPL:1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			input := event.Envelope{ID: tt.inputID, CorrelationID: tt.inputCorrelation, Sequence: 12}
			emission := event.Envelope{
				ID: "decision-1",
				// Everything a handler might have set for itself, and may not.
				Sequence:      99,
				CausationID:   "claimed",
				CorrelationID: "claimed",
			}

			stamped := replay.Stamp(input, emission, 4)

			if stamped.Sequence != 4 {
				t.Errorf("Sequence = %d, want 4", stamped.Sequence)
			}
			if stamped.CausationID != tt.inputID {
				t.Errorf("CausationID = %q, want %q", stamped.CausationID, tt.inputID)
			}
			if stamped.CorrelationID != tt.wantCorrelationID {
				t.Errorf("CorrelationID = %q, want %q", stamped.CorrelationID, tt.wantCorrelationID)
			}
			if stamped.ID != "decision-1" {
				t.Errorf("Stamp altered the emission's own id: %q", stamped.ID)
			}
			if emission.Sequence != 99 {
				t.Error("Stamp mutated its argument; it must return a stamped copy")
			}
		})
	}
}

// TestEngineRunStampsExactlyAsStampDoes is what makes it safe for a journal
// writer to stamp a decision itself: the two must not drift, or a journal
// would record decisions a replay of its own inputs could never reproduce.
func TestEngineRunStampsExactlyAsStampDoes(t *testing.T) {
	t.Parallel()

	input := envelope(1)
	emission := decision("decision-1")

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return []event.Envelope{emission}, nil
	}))
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	emitted, err := engine.Run(context.Background(), []event.Envelope{input})
	if err != nil {
		t.Fatalf("Engine.Run() error = %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("Engine.Run() emitted %d envelopes, want 1", len(emitted))
	}

	want := replay.Stamp(input, emission, 1)
	if !reflect.DeepEqual(emitted[0], want) {
		t.Fatalf("Engine.Run() stamped\n %+v\nStamp produced\n %+v", emitted[0], want)
	}
}
