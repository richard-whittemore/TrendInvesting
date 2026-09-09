// Package replay applies recorded events to deterministic handlers without a
// LEAN or brokerage connection.
package replay

import (
	"context"
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// Handler applies a validated event to deterministic domain state and
// returns the decision envelopes it emits, in emission order. A handler that
// emits nothing returns (nil, nil); this is valid, not an error.
//
// A handler must set Source, StrategyVersion, ConfigurationHash, Payload,
// and PayloadHash (via event.HashPayload) on what it emits. It must not rely
// on Sequence, CausationID, or CorrelationID it sets on an emitted envelope:
// Engine.Run stamps all three, overwriting whatever the handler provided.
type Handler interface {
	Apply(context.Context, event.Envelope) ([]event.Envelope, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, event.Envelope) ([]event.Envelope, error)

// Apply implements Handler.
func (f HandlerFunc) Apply(ctx context.Context, envelope event.Envelope) ([]event.Envelope, error) {
	return f(ctx, envelope)
}

// Engine enforces event validation and contiguous processing order.
type Engine struct {
	handler Handler
}

// New returns an Engine using handler.
func New(handler Handler) (*Engine, error) {
	if handler == nil {
		return nil, errors.New("replay handler is required")
	}
	return &Engine{handler: handler}, nil
}

// Run applies events in the supplied order and returns every decision
// envelope the handler emitted, in emission order. Input sequences must be
// contiguous so a missing, duplicated, or reordered event fails closed.
//
// Emitted envelopes form their own contiguous output stream, independent of
// the input stream: Engine assigns each one's Sequence from a counter
// starting at 1, in emission order, overwriting whatever the handler set.
// Input sequences are left exactly as the producer set them — a later
// journal writer interleaves the two streams by recording order, and replay
// equivalence compares output streams. For each emission the engine also
// sets CausationID to the input envelope's ID, and CorrelationID to the
// input's CorrelationID if set, else the input's ID; a handler cannot
// override either. Each emitted envelope is validated after stamping, so an
// invalid emission fails closed, naming the input sequence and the emission
// index.
func (e *Engine) Run(ctx context.Context, events []event.Envelope) ([]event.Envelope, error) {
	var previous uint64
	var outputSequence uint64
	var emitted []event.Envelope
	for index, envelope := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := envelope.Validate(); err != nil {
			return nil, fmt.Errorf("event %d: %w", index, err)
		}
		if index > 0 && envelope.Sequence != previous+1 {
			return nil, fmt.Errorf("event %d: non-contiguous sequence: got %d after %d", index, envelope.Sequence, previous)
		}

		decisions, err := e.handler.Apply(ctx, envelope)
		if err != nil {
			return nil, fmt.Errorf("apply event %s at sequence %d: %w", envelope.ID, envelope.Sequence, err)
		}

		correlationID := envelope.CorrelationID
		if correlationID == "" {
			correlationID = envelope.ID
		}
		for emissionIndex, decision := range decisions {
			outputSequence++
			decision.Sequence = outputSequence
			decision.CausationID = envelope.ID
			decision.CorrelationID = correlationID
			if err := decision.Validate(); err != nil {
				return nil, fmt.Errorf("emit at input sequence %d, emission %d: %w", envelope.Sequence, emissionIndex, err)
			}
			emitted = append(emitted, decision)
		}

		previous = envelope.Sequence
	}
	return emitted, nil
}
