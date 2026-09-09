package replay_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// envelope builds a valid envelope at the given sequence, so each test states
// only what it is actually about.
func envelope(sequence uint64) event.Envelope {
	now := time.Date(2026, time.August, 29, 20, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{}`)
	return event.Envelope{
		ID:                "evt",
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

func TestEngineRunAppliesEventsInOrder(t *testing.T) {
	t.Parallel()

	var got []uint64
	engine, err := replay.New(replay.HandlerFunc(func(_ context.Context, item event.Envelope) error {
		got = append(got, item.Sequence)
		return nil
	}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := engine.Run(context.Background(), []event.Envelope{envelope(4), envelope(5), envelope(6)}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(got) != 3 || got[0] != 4 || got[1] != 5 || got[2] != 6 {
		t.Fatalf("applied sequences = %v, want [4 5 6]", got)
	}
}

func TestEngineRunRejectsSequenceGap(t *testing.T) {
	t.Parallel()

	engine, err := replay.New(replay.HandlerFunc(func(context.Context, event.Envelope) error { return nil }))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	err = engine.Run(context.Background(), []event.Envelope{envelope(1), envelope(3)})
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
