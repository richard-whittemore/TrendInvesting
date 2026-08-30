// Package replay applies recorded events to deterministic handlers without a
// LEAN or brokerage connection.
package replay

import (
	"context"
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// Handler applies a validated event to deterministic domain state.
type Handler interface {
	Apply(context.Context, event.Envelope) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, event.Envelope) error

// Apply implements Handler.
func (f HandlerFunc) Apply(ctx context.Context, envelope event.Envelope) error {
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

// Run applies events in the supplied order. Sequences must be contiguous so a
// missing, duplicated, or reordered event fails closed.
func (e *Engine) Run(ctx context.Context, events []event.Envelope) error {
	var previous uint64
	for index, envelope := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := envelope.Validate(); err != nil {
			return fmt.Errorf("event %d: %w", index, err)
		}
		if index > 0 && envelope.Sequence != previous+1 {
			return fmt.Errorf("event %d: non-contiguous sequence: got %d after %d", index, envelope.Sequence, previous)
		}
		if err := e.handler.Apply(ctx, envelope); err != nil {
			return fmt.Errorf("apply event %s at sequence %d: %w", envelope.ID, envelope.Sequence, err)
		}
		previous = envelope.Sequence
	}
	return nil
}
