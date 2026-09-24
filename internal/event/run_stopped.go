package event

import (
	"errors"
	"fmt"
)

// AdapterRunStoppedEventType identifies the input event an adapter sends
// when it deliberately stops a run already in progress, distinct from
// reaching the natural end of its input stream (RunCompletedEventType's own
// doc comment: that event states only that the stream ended, never why).
//
// ADR 0012 requires a run's evidence to say whether it is a completed
// result or an abandoned one, and before this event existed a journal's
// final input was always RunCompletedEventType regardless of the reason,
// so a reviewer could not tell a clean end apart from, for example, LEAN's
// untrusted DELISTED (adapter/lean/algorithm.py's handle_delisting).
//
// It is sent immediately BEFORE RunCompletedEventType, and only on a
// deliberate stop: a clean end of the input stream sends no event of this
// type at all. A startup failure — before the adapter's first exchange with
// the engine — cannot send anything either, this event included: there is no
// connection, no sequence, and no run yet for it to belong to
// (adapter/lean/README.md states this gap explicitly).
//
// Namespaced "adapter.", a new prefix: this is neither a fact about the
// market ("market."), a venue's report of an execution ("execution."), an
// account observation ("account."), this system's own decision
// ("strategy."), nor the input-stream-ended fact ("replay."). It is the
// adapter's own report of why it stopped feeding the stream, which none of
// those existing prefixes name.
const AdapterRunStoppedEventType = "adapter.run.stopped"

// AdapterRunStoppedSchemaVersion is the current schema version of
// AdapterRunStoppedPayload.
const AdapterRunStoppedSchemaVersion uint32 = 1

// The closed set of reasons AdapterRunStoppedPayload.Reason may name.
// Validate rejects any other value, so a reason a producer invents today is
// never silently accepted and misread later. Start with exactly the reasons
// an adapter stops for once its stream has already started —
// adapter/lean/algorithm.py's stop() call sites reachable after Initialize —
// so the set states what actually happens rather than what might.
const (
	// AdapterRunStoppedReasonDelisted means LEAN reported the instrument
	// DELISTED and the adapter chose not to trust it: LEAN's own delisting
	// signal carries no reason and also fires for a ticker conversion — it
	// reported GOOAV, a when-issued share that became GOOG, as DELISTED on
	// 2014-04-03 — so the run stops and names the instrument rather than
	// risk recording a Delisting Exit that never happened
	// (adapter/lean/algorithm.py's handle_delisting;
	// adapter/lean/README.md). InstrumentID is required for this reason.
	AdapterRunStoppedReasonDelisted = "delisted"
)

// AdapterRunStoppedPayload records a deliberate stop: Reason names why,
// InstrumentID names the instrument the reason concerns where the reason has
// one, and Detail carries the figures behind it in prose — the same
// requirement EngineStatePayload.Detail and ProposalDeclinedPayload.Detail
// state, that a reason without its figures cannot be checked.
//
// It carries no timestamp of its own: the envelope's own EventTime states
// when, exactly as RunCompletedPayload's own doc comment explains for the
// event this one always precedes.
type AdapterRunStoppedPayload struct {
	// Reason is one of the enumerated AdapterRunStoppedReason* constants.
	Reason string `json:"reason"`
	// InstrumentID is the instrument Reason concerns. Required exactly when
	// Reason needs one — today, always, since AdapterRunStoppedReasonDelisted
	// is the only recognised reason and it always names an instrument.
	InstrumentID string `json:"instrument_id"`
	Detail       string `json:"detail"`
}

// Validate checks that Reason is one of the enumerated constants, with
// whatever InstrumentID requirement that reason carries, and that Detail is
// present.
func (p AdapterRunStoppedPayload) Validate() error {
	var errs []error
	switch p.Reason {
	case AdapterRunStoppedReasonDelisted:
		if p.InstrumentID == "" {
			errs = append(errs, errors.New(`instrument id is required for reason "delisted"`))
		}
	case "":
		errs = append(errs, errors.New("reason is required"))
	default:
		errs = append(errs, fmt.Errorf("reason %q is not a recognised run stop reason", p.Reason))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required: a stop without its figures cannot be checked"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid adapter run stopped payload: %w", err)
	}
	return nil
}
