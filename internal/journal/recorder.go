package journal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// Recorder is a replay.Handler that wraps another and records what passes
// through it: each input, followed immediately by the decisions that input
// caused. That recording order is the journal's single contiguous sequence
// of inputs and decisions.
//
// It is also where the two are told apart, structurally and once: what
// arrives as Apply's argument is an input, what the handler returns is a
// decision. Every later reader takes that from the record's own Kind rather
// than re-deriving it from a producer's name.
//
// A decision is stamped with replay.Stamp — the same function replay.Engine
// uses — so a replay of the journal's own input stream reproduces the
// recorded decisions byte for byte, which is what makes the journal evidence
// rather than a summary. A decision that is invalid once stamped fails
// closed here exactly as it would in the engine.
//
// The records are held in memory and written by Write when the run ends,
// because the header states the span the run covered and that is not known
// until the last input has arrived.
type Recorder struct {
	handler replay.Handler
	entries []Entry

	outputSequence uint64
	spanStart      time.Time
	spanEnd        time.Time
	hasInput       bool
}

// NewRecorder returns a Recorder wrapping handler, which is required.
func NewRecorder(handler replay.Handler) *Recorder {
	return &Recorder{handler: handler}
}

// Apply implements replay.Handler. It returns the wrapped handler's
// decisions and error unchanged, so the driver sees exactly what it would
// without the Recorder in the way.
func (r *Recorder) Apply(ctx context.Context, input event.Envelope) ([]event.Envelope, error) {
	if r.handler == nil {
		return nil, errors.New("journal: a recorder requires a handler to record")
	}

	r.entries = append(r.entries, Entry{Kind: KindInput, Envelope: input})
	r.observeInputTime(input.EventTime)

	decisions, applyErr := r.handler.Apply(ctx, input)
	for i, decision := range decisions {
		r.outputSequence++
		stamped := replay.Stamp(input, decision, r.outputSequence)
		if err := stamped.Validate(); err != nil {
			return decisions, fmt.Errorf("journal: emission %d of event %s cannot be journalled: %w", i, input.ID, err)
		}
		r.entries = append(r.entries, Entry{Kind: KindDecision, Envelope: stamped})
	}
	return decisions, applyErr
}

// observeInputTime widens the span the journal covers. The span is the first
// and last INPUT event time: a decision is attributed to the input that
// caused it and cannot fall outside it.
func (r *Recorder) observeInputTime(at time.Time) {
	if !r.hasInput {
		r.spanStart, r.spanEnd, r.hasInput = at, at, true
		return
	}
	if at.Before(r.spanStart) {
		r.spanStart = at
	}
	if at.After(r.spanEnd) {
		r.spanEnd = at
	}
}

// Entries returns everything recorded, in recording order.
func (r *Recorder) Entries() []Entry {
	out := make([]Entry, len(r.entries))
	copy(out, r.entries)
	return out
}

// Header returns the journal header for this run, with the span derived from
// the inputs actually applied rather than supplied by a caller.
func (r *Recorder) Header(configurationHash, strategyVersion string) (Header, error) {
	if !r.hasInput {
		return Header{}, errors.New("journal: the run applied no input, so there is no span to state")
	}
	header := NewHeader(configurationHash, strategyVersion, r.spanStart, r.spanEnd)
	if err := header.validate(); err != nil {
		return Header{}, err
	}
	return header, nil
}
