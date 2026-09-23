package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// stubEnvelope is a minimal, individually-valid event.Envelope for a test
// handler to hand back as one of "the decisions" — its exact content is
// never inspected by newDecider, only carried through.
func stubEnvelope(id string) event.Envelope {
	payload := []byte(`{}`)
	return event.Envelope{
		ID:                id,
		Type:              "stub.decision",
		SchemaVersion:     1,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		RecordedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Sequence:          1,
		Source:            "stub",
		StrategyVersion:   "stub/1.0.0+test",
		ConfigurationHash: "sha256:stub",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

func stubInput(t *testing.T, id string) event.Envelope {
	t.Helper()
	payload := []byte(`{}`)
	return event.Envelope{
		ID:                id,
		Type:              "stub.input",
		SchemaVersion:     1,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		RecordedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Sequence:          1,
		Source:            "test-client",
		StrategyVersion:   "stub/1.0.0+test",
		ConfigurationHash: "sha256:stub",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// TestNewDeciderBundlesEveryDecisionIntoOneEnvelope is decision 1's own
// test: a handler emitting several decisions for one input must have every
// one of them reach the wire, in order, inside the single envelope
// transport.Decider's contract allows — none of them picked as "the"
// decision, none summarised, none dropped.
func TestNewDeciderBundlesEveryDecisionIntoOneEnvelope(t *testing.T) {
	want := []event.Envelope{stubEnvelope("a"), stubEnvelope("b"), stubEnvelope("c")}
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return want, nil
	})
	decide := newDecider(journal.NewRecorder(handler), "stub/1.0.0+test", "sha256:stub")

	input := stubInput(t, "input-1")
	reply, err := decide(context.Background(), input)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if reply.Type != decisionsEventType {
		t.Fatalf("reply type = %q, want %q", reply.Type, decisionsEventType)
	}
	if reply.CausationID != input.ID {
		t.Fatalf("reply causation id = %q, want %q", reply.CausationID, input.ID)
	}

	var payload decisionsPayload
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		t.Fatalf("decode reply payload: %v", err)
	}
	if len(payload.Decisions) != len(want) {
		t.Fatalf("got %d decisions, want %d", len(payload.Decisions), len(want))
	}
	for i, decision := range payload.Decisions {
		if decision.ID != want[i].ID {
			t.Errorf("decision %d id = %q, want %q", i, decision.ID, want[i].ID)
		}
	}
}

// TestNewDeciderRepresentsZeroDecisionsHonestly is the other half of
// decision 1: a handler that legitimately decides nothing for an input
// (internal/strategy/reducer.go's delisted-instrument case, for one) must
// come back as an empty list, never as a fabricated envelope invented to
// fill the one-reply-per-request slot.
func TestNewDeciderRepresentsZeroDecisionsHonestly(t *testing.T) {
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return nil, nil
	})
	decide := newDecider(journal.NewRecorder(handler), "stub/1.0.0+test", "sha256:stub")

	reply, err := decide(context.Background(), stubInput(t, "input-1"))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	var payload decisionsPayload
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		t.Fatalf("decode reply payload: %v", err)
	}
	if len(payload.Decisions) != 0 {
		t.Fatalf("got %d decisions, want 0", len(payload.Decisions))
	}
}

// TestNewDeciderFailsClosedOnAReducerError is this ticket's "an envelope the
// reducer rejects returns a protocol error, never a fabricated decision",
// exercised directly against the Decider this command builds: a handler
// error must reach the caller as an error, with the returned envelope left
// at its zero value, never populated with a guessed or partial decision.
func TestNewDeciderFailsClosedOnAReducerError(t *testing.T) {
	wantErr := errors.New("risk controller is not ready")
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		return nil, wantErr
	})
	decide := newDecider(journal.NewRecorder(handler), "stub/1.0.0+test", "sha256:stub")

	reply, err := decide(context.Background(), stubInput(t, "input-1"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("decide error = %v, want it to wrap %v", err, wantErr)
	}
	if reply.ID != "" || reply.Type != "" || reply.Payload != nil {
		t.Fatalf("decide returned a non-empty envelope alongside an error: %+v", reply)
	}
}

// TestNewDeciderRefusesASecondConcurrentConnection is decision 4's test:
// while one call is in flight, a second, concurrent call must be refused
// immediately (errAnotherConnectionActive) rather than either corrupting the
// shared reducer state or silently queuing behind the first. A blocking
// handler makes the overlap deterministic instead of relying on timing.
func TestNewDeciderRefusesASecondConcurrentConnection(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	handler := replay.HandlerFunc(func(_ context.Context, _ event.Envelope) ([]event.Envelope, error) {
		// Only the first call blocks; the later "third" call below must not
		// hang forever behind a channel that already closed.
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil, nil
	})
	decide := newDecider(journal.NewRecorder(handler), "stub/1.0.0+test", "sha256:stub")

	firstErr := make(chan error, 1)
	go func() {
		_, err := decide(context.Background(), stubInput(t, "first"))
		firstErr <- err
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first call to enter the handler")
	}

	if _, err := decide(context.Background(), stubInput(t, "second")); !errors.Is(err, errAnotherConnectionActive) {
		t.Fatalf("second concurrent call returned %v, want errAnotherConnectionActive", err)
	}

	close(release)
	select {
	case err := <-firstErr:
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first call to finish")
	}

	// The lock is released once the first call returns: a later,
	// non-overlapping call from what could be a reconnected adapter is not
	// "another active executor" and must succeed.
	if _, err := decide(context.Background(), stubInput(t, "third")); err != nil {
		t.Fatalf("a later, non-overlapping call was refused: %v", err)
	}
}

// TestDeciderRefusesABarBeforeConfiguration exercises "an unconfigured
// engine refuses inputs rather than sizing against nothing" at the level
// internal/strategy.Reducer itself enforces it: a real Reducer that has
// never been given a configuration input rejects a bar outright, and this
// command's Decider must carry that refusal through as an error rather than
// absorb it. run (engine.go) makes this unreachable in ordinary operation by
// always applying the configuration input before transport.Listen is ever
// called (decision 2); this test pins the guard directly, independent of
// that ordering.
func TestDeciderRefusesABarBeforeConfiguration(t *testing.T) {
	cfg := testConfiguration(t)
	strategyVersion := event.ComposeStrategyVersion(cfg.StrategyID, strategy.RulesVersion, "test-build")
	reducer, err := strategy.NewReducer(strategyVersion, cfg)
	if err != nil {
		t.Fatalf("construct the reducer: %v", err)
	}
	decide := newDecider(journal.NewRecorder(reducer), strategyVersion, event.ConfigurationHash(cfg))

	bar := flatBar("TEST", 0)
	envelope := barEnvelope(t, bar, 1, strategyVersion, event.ConfigurationHash(cfg))

	reply, err := decide(context.Background(), envelope)
	if err == nil {
		t.Fatal("decide succeeded against an unconfigured reducer; want an error")
	}
	if !strings.Contains(err.Error(), "configuration event") {
		t.Errorf("error = %v, want it to name the missing configuration event", err)
	}
	if reply.ID != "" || reply.Type != "" || reply.Payload != nil {
		t.Fatalf("decide returned a non-empty envelope alongside an error: %+v", reply)
	}
}
