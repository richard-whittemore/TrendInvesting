package journal

import (
	"context"
	"errors"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// DefaultMaxRecords is how many records a Recorder holds before it refuses to
// record more: roughly twice the largest daily-bar run this platform is built
// for, and a size the machines it runs on hold comfortably (ADR 0017).
const DefaultMaxRecords = 2_000_000

// RecordLimitError reports a run stopped because recording another input
// would take its journal past the records a Recorder holds in memory.
//
// It is a stopped run, not a lost one: the records already taken are a
// journal, under a header stating the span they cover.
type RecordLimitError struct {
	Recorded int
	Limit    int
}

func (e *RecordLimitError) Error() string {
	return fmt.Sprintf("journal: this run has recorded %d of the %d records a journal is composed from in memory, and stops here rather than dying on an allocation having written nothing; the records it took are journalled under the span they cover, so run a shorter span or a smaller universe, or raise the bound, which costs memory in proportion", e.Recorded, e.Limit)
}

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
// until the last input has arrived (ADR 0017). That buffer is bounded, and
// the bound is checked at an input boundary: refusing part way through an
// input's emissions would leave a journal claiming the reducer decided
// nothing for its last input, which replay equivalence would then report as a
// divergence in a faithful record. A run therefore overshoots its bound by at
// most the decisions of the input that reached it.
type Recorder struct {
	handler    replay.Handler
	entries    []Entry
	maxRecords int

	outputSequence uint64
}

// NewRecorder returns a Recorder wrapping handler, which is required, holding
// up to DefaultMaxRecords records.
func NewRecorder(handler replay.Handler) *Recorder {
	return NewBoundedRecorder(handler, DefaultMaxRecords)
}

// NewBoundedRecorder returns a Recorder holding up to maxRecords records.
//
// A run that would exceed the bound stops with a *RecordLimitError rather
// than growing: the failure a bound replaces is an allocation that kills the
// process with nothing written at all.
func NewBoundedRecorder(handler replay.Handler, maxRecords int) *Recorder {
	return &Recorder{handler: handler, maxRecords: maxRecords}
}

// Apply implements replay.Handler. It returns the wrapped handler's
// decisions and error unchanged, so the driver sees exactly what it would
// without the Recorder in the way.
func (r *Recorder) Apply(ctx context.Context, input event.Envelope) ([]event.Envelope, error) {
	if r.handler == nil {
		return nil, errors.New("journal: a recorder requires a handler to record")
	}
	// Before the input is recorded, so nothing of this input reaches the
	// journal without the decisions it caused.
	if len(r.entries) >= r.maxRecords {
		return nil, &RecordLimitError{Recorded: len(r.entries), Limit: r.maxRecords}
	}

	r.entries = append(r.entries, Entry{Kind: KindInput, Envelope: input})

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

// Entries returns everything recorded, in recording order.
func (r *Recorder) Entries() []Entry {
	out := make([]Entry, len(r.entries))
	copy(out, r.entries)
	return out
}

// Header returns the journal header for this run, with the span derived from
// the inputs actually applied rather than supplied by a caller.
//
// It derives the span through the same function Write checks a header
// against, so a header composed here cannot be one Write refuses.
func (r *Recorder) Header(configurationHash, strategyVersion string) (Header, error) {
	start, end, ok := inputSpan(r.entries, entrySpanOf)
	if !ok {
		return Header{}, errors.New("journal: the run applied no input, so there is no span to state")
	}
	header := NewHeader(configurationHash, strategyVersion, start, end)
	if err := header.validate(); err != nil {
		return Header{}, err
	}
	return header, nil
}
