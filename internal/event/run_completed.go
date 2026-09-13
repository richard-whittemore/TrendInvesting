package event

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

// RunCompletedPayload records that a run's input stream has ended. It is
// deliberately empty: the event type states the fact and the envelope's
// EventTime states when, which is what every expiry the event causes is
// stamped with.
//
// It carried a CompletedAt of its own until a review observed the obvious
// hazard: two instants that must always be equal are two instants that will
// eventually disagree, and a disagreement here would stamp the expiries at
// one of them while the journal's span — derived from input event times —
// came from the other, placing decisions outside the span the journal
// reports. One instant makes that unrepresentable rather than merely
// invalid.
type RunCompletedPayload struct{}

// Validate reports the payload valid: it states nothing, so there is
// nothing to check. It exists because every payload in this package has a
// Validate, and a producer should not have to know which ones are empty.
func (p RunCompletedPayload) Validate() error {
	return nil
}
