package event

import (
	"errors"
	"fmt"
	"time"
)

// RunCompletedEventType identifies the fact that a run's input stream has
// ended: no further input for this run exists.
//
// It is an INPUT event, produced by whatever drives the run, not a decision.
// The reducer handles it by expiring every proposal still outstanding (ADR
// 0011 gives a proposal the lifetime of one bar, and the bar that would have
// superseded it never arrives), so every proposal in a completed run reaches
// exactly one terminal event.
//
// Carrying the fact in an envelope, rather than in a method the engine calls
// after the last input, is what keeps two invariants intact: every emission
// has a causing input to name in CausationID (docs/architecture.md), and a
// replay of a journal's own input stream reproduces the run's decisions —
// including the expiries, because the event that caused them is itself in
// that stream.
const RunCompletedEventType = "replay.run.completed"

// RunCompletedSchemaVersion is the current schema version of
// RunCompletedPayload.
const RunCompletedSchemaVersion uint32 = 1

// RunCompletedPayload records when a run's input stream ended.
//
// CompletedAt is the instant the stream ran to, and is what every expiry the
// event causes is stamped with. It is stated in the payload rather than read
// from the envelope's EventTime because it is the fact the event asserts,
// and a reducer reads facts from payloads.
type RunCompletedPayload struct {
	CompletedAt time.Time `json:"completed_at"`
}

// Validate checks that the payload states the instant the stream ended.
func (p RunCompletedPayload) Validate() error {
	var errs []error
	if p.CompletedAt.IsZero() {
		errs = append(errs, errors.New("completed at is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid run completed payload: %w", err)
	}
	return nil
}
