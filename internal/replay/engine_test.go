package replay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// envelope builds a valid input envelope at the given sequence, so each test
// states only what it is actually about. Each envelope's ID is derived from
// its sequence so tests can tell which input caused which emission.
func envelope(sequence uint64) event.Envelope {
	now := time.Date(2026, time.August, 29, 20, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{}`)
	return event.Envelope{
		ID:                fmt.Sprintf("evt-%d", sequence),
		Type:              "test.event",
		SchemaVersion:     1,
		EventTime:         now,
		RecordedAt:        now,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   "test-strategy-1.0.0",
		ConfigurationHash: "cfg-test",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// decision builds a valid envelope a handler can emit, identified by id. It
// deliberately leaves Sequence, CausationID, and CorrelationID at their zero
// values (or, when asked, set to deliberately wrong values) so tests can
// assert the engine — not the handler — is what sets them.
func decision(id string) event.Envelope {
	now := time.Date(2026, time.August, 29, 20, 5, 0, 0, time.UTC)
	payload := json.RawMessage(`{"id":"` + id + `"}`)
	return event.Envelope{
		ID:                id,
		Type:              "test.decision",
		SchemaVersion:     1,
		EventTime:         now,
		RecordedAt:        now,
		Source:            "handler",
		StrategyVersion:   "test-strategy-1.0.0",
		ConfigurationHash: "cfg-test",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

func TestEngineRunAppliesEventsInOrder(t *testing.T) {
	t.Parallel()

	var got []uint64
	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		got = append(got, item.Sequence)
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5), envelope(6)}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(got) != 3 || got[0] != 4 || got[1] != 5 || got[2] != 6 {
		t.Fatalf("applied sequences = %v, want [4 5 6]", got)
	}
}

func TestEngineRunRejectsSequenceGap(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{envelope(1), envelope(3)})
	if err == nil || !strings.Contains(err.Error(), "non-contiguous sequence") {
		t.Fatalf("Run() error = %v, want non-contiguous sequence", err)
	}
}

func TestNewRequiresHandler(t *testing.T) {
	t.Parallel()

	if _, err := replay.New(nil); err == nil {
		t.Fatal("New(nil) error = nil, want error")
	}
}

func TestEngineRunCollectsEmittedEnvelopesInOrder(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 4:
			return []event.Envelope{decision("a")}, nil
		case 5:
			return nil, nil
		case 6:
			return []event.Envelope{decision("b"), decision("c")}, nil
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5), envelope(6)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var ids []string
	for _, item := range emitted {
		ids = append(ids, item.ID)
	}
	want := []string{"a", "b", "c"}
	if len(ids) != len(want) {
		t.Fatalf("emitted ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("emitted ids = %v, want %v", ids, want)
		}
	}
}

func TestEngineRunEmptyEmissionIsValid(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Run(context.Background(), []event.Envelope{envelope(1), envelope(2)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 0 {
		t.Fatalf("emitted = %v, want empty", emitted)
	}
}

func TestEngineRunAssignsContiguousOutputSequenceRegardlessOfHandlerInput(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 4:
			wrong := decision("a")
			wrong.Sequence = 999
			return []event.Envelope{wrong}, nil
		case 5:
			first := decision("b")
			first.Sequence = 0
			second := decision("c")
			second.Sequence = 42
			return []event.Envelope{first, second}, nil
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5)})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(emitted) != 3 {
		t.Fatalf("len(emitted) = %d, want 3", len(emitted))
	}
	for i, item := range emitted {
		want := uint64(i + 1)
		if item.Sequence != want {
			t.Fatalf("emitted[%d].Sequence = %d, want %d", i, item.Sequence, want)
		}
	}
}

func TestEngineRunStampsCausationAndCorrelationFromInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		correlationID     string
		wantCorrelationID string
	}{
		{
			name:              "no correlation id set on input falls back to input id",
			correlationID:     "",
			wantCorrelationID: "evt-4",
		},
		{
			name:              "correlation id set on input is propagated",
			correlationID:     "corr-1",
			wantCorrelationID: "corr-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			input := envelope(4)
			input.CorrelationID = tt.correlationID

			engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
				return []event.Envelope{decision("a")}, nil
			}))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			emitted, err := engine.Run(context.Background(), []event.Envelope{input})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(emitted) != 1 {
				t.Fatalf("len(emitted) = %d, want 1", len(emitted))
			}
			if emitted[0].CausationID != "evt-4" {
				t.Fatalf("CausationID = %q, want %q", emitted[0].CausationID, "evt-4")
			}
			if emitted[0].CorrelationID != tt.wantCorrelationID {
				t.Fatalf("CorrelationID = %q, want %q", emitted[0].CorrelationID, tt.wantCorrelationID)
			}
		})
	}
}

func TestEngineRunOverridesHandlerSetCausationAndCorrelation(t *testing.T) {
	t.Parallel()

	input := envelope(4)
	input.CorrelationID = "corr-1"

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
		tampered := decision("a")
		tampered.CausationID = "not-the-input"
		tampered.CorrelationID = "not-the-correlation"
		return []event.Envelope{tampered}, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	emitted, err := engine.Run(context.Background(), []event.Envelope{input})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("len(emitted) = %d, want 1", len(emitted))
	}
	if emitted[0].CausationID != "evt-4" {
		t.Fatalf("CausationID = %q, want %q (engine must override handler value)", emitted[0].CausationID, "evt-4")
	}
	if emitted[0].CorrelationID != "corr-1" {
		t.Fatalf("CorrelationID = %q, want %q (engine must override handler value)", emitted[0].CorrelationID, "corr-1")
	}
}

func TestEngineRunFailsClosedOnInvalidEmission(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) ([]event.Envelope, error) {
		switch item.Sequence {
		case 4:
			return []event.Envelope{decision("a")}, nil
		case 5:
			valid := decision("b")
			invalid := decision("c")
			invalid.Source = "" // missing a required provenance field
			return []event.Envelope{valid, invalid}, nil
		default:
			t.Fatalf("unexpected input sequence %d", item.Sequence)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5)})
	if err == nil {
		t.Fatal("Run() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "sequence 5") {
		t.Fatalf("Run() error = %v, want it to name the input sequence (5)", err)
	}
	if !strings.Contains(err.Error(), "emission 1") {
		t.Fatalf("Run() error = %v, want it to name the emission index (1)", err)
	}
}

func TestEngineRunHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) ([]event.Envelope, error) {
		t.Fatal("handler must not be called once the context is cancelled")
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := engine.Run(ctx, []event.Envelope{envelope(1)}); err == nil {
		t.Fatal("Run() error = nil, want context error")
	}
}
