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
//
// A handler that fails closed may emit a final event explaining why —
// alongside the error, in the same Apply call, not instead of it: Apply's
// contract permits returning both a non-nil error and a non-empty emission
// slice on the same call. The engine journals that emission (stamping and
// validating it exactly like any successful one) before returning the
// error, so a capital-safety halt or similar terminal decision reaches the
// journal even though the run itself does not continue. A handler that has
// nothing to explain returns (nil, err) as before.
type Handler interface {
	Apply(context.Context, event.Envelope) ([]event.Envelope, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, event.Envelope) ([]event.Envelope, error)

// Apply implements Handler.
func (f HandlerFunc) Apply(ctx context.Context, envelope event.Envelope) ([]event.Envelope, error) {
	return f(ctx, envelope)
}

// Stamp returns emission with the three fields a handler may not set for
// itself: Sequence from the output stream's own counter, CausationID from
// the input that caused the emission, and CorrelationID from that input's
// own CorrelationID or, absent one, its ID (docs/architecture.md: "a handler
// cannot claim causation or correlation it did not have").
//
// It is exported so that a journal writer recording decisions as they are
// emitted stamps them exactly as Engine.Run does. The two must not drift: a
// journal whose decisions were stamped differently could never be reproduced
// by a replay of its own inputs, which is the property that makes it
// evidence rather than a summary.
func Stamp(input, emission event.Envelope, outputSequence uint64) event.Envelope {
	correlationID := input.CorrelationID
	if correlationID == "" {
		correlationID = input.ID
	}
	emission.Sequence = outputSequence
	emission.CausationID = input.ID
	emission.CorrelationID = correlationID
	return emission
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
//
// When Apply returns an error, its emissions (if any — see Handler's doc
// comment on a handler's final explanatory event) are still stamped,
// validated, and appended exactly like a successful call's: Run returns
// every emission collected up to and including the failing call's own,
// alongside the error. A handler's error always wins the message: if the
// failing call's own emission is ALSO invalid, both failures are named, with
// the handler's original error first in the chain (errors.Is/As still finds
// it) and the emission's invalidity appended, since an emission that cannot
// be journalled is a failure in its own right and must not be silently
// dropped behind the error that happened to arrive alongside it.
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

		decisions, applyErr := e.handler.Apply(ctx, envelope)

		for emissionIndex, decision := range decisions {
			outputSequence++
			decision = Stamp(envelope, decision, outputSequence)
			if err := decision.Validate(); err != nil {
				validationErr := fmt.Errorf("emit at input sequence %d, emission %d: %w", envelope.Sequence, emissionIndex, err)
				if applyErr != nil {
					return emitted, fmt.Errorf("apply event %s at sequence %d: %w; its final emission is also invalid: %w", envelope.ID, envelope.Sequence, applyErr, validationErr)
				}
				return nil, validationErr
			}
			emitted = append(emitted, decision)
		}

		if applyErr != nil {
			return emitted, fmt.Errorf("apply event %s at sequence %d: %w", envelope.ID, envelope.Sequence, applyErr)
		}

		previous = envelope.Sequence
	}
	return emitted, nil
}
