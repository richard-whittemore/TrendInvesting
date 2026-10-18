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

// MaxEmissionsPerInput is the most decisions a Recorder journals for one
// input before it refuses the input entirely. It is what gives the record
// bound a finite size: the bound is checked between inputs, so a run holds
// at most DefaultMaxRecords + MaxEmissionsPerInput records (ADR 0017).
//
// The largest burst one input can cause is the end of the input stream,
// which ends every proposal still outstanding: ADR 0011's three proposal
// kinds, across every instrument the run holds state for. Every other input
// decides for one instrument. This ceiling therefore covers a universe two
// orders of magnitude larger than the hundred instruments ADR 0017 sizes the
// platform's largest run at, while capping the overshoot at about a
// thirtieth of the records the bound itself allows.
const MaxEmissionsPerInput = 65_536

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

// EmissionLimitError reports a run stopped because one input caused more
// decisions than a journal held in memory takes from a single input.
//
// Nothing of that input is recorded. The records taken before it are a
// journal, under a header stating the span they cover.
type EmissionLimitError struct {
	Input   string
	Emitted int
	Limit   int
}

func (e *EmissionLimitError) Error() string {
	return fmt.Sprintf("journal: event %s caused %d decisions, more than the %d one input may contribute to a journal composed in memory; the record bound is checked between inputs, so this ceiling is what keeps a run's overshoot of it finite; nothing of this input is recorded, and what was recorded before it is journalled under the span it covers", e.Input, e.Emitted, e.Limit)
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
// most the decisions of the input that reached it, and MaxEmissionsPerInput
// is what makes that overshoot finite: a run holds at most maxRecords +
// MaxEmissionsPerInput records, and an input emitting more than the ceiling
// is refused whole rather than recorded in part.
//
// A durable Recorder (NewDurableRecorder) also appends each input's group to
// a durable log, flushed, before Apply returns any of its decisions:
// journal-before-send (ADR 0019, amended 2026-09-24; ADR 0026 §3; ADR 0027).
// Apply's return is the only path a decision has towards an adapter, so a
// decision that was not made durable is never returned, and after a
// durability failure every later input is refused.
type Recorder struct {
	handler    replay.Handler
	entries    []Entry
	maxRecords int

	outputSequence uint64

	// log is nil for a Recorder that holds its records in memory only.
	log *LogWriter
	// expected is the recorded group the next input must reproduce while
	// Resume rebuilds a run from its durable log, and nil otherwise.
	expected []Entry
	// failed is set once a durable Recorder cannot keep its log in step with
	// its handler, and refuses every later input.
	failed error
	// committed indexes each committed group by its input's ID, first
	// occurrence kept, so the reply to an input can be served again.
	committed map[string]committedGroup
}

// committedGroup locates one committed group in entries, with the error the
// handler returned for its input, if any.
type committedGroup struct {
	start, end int
	refusal    string
}

// Committed is one input as it was committed, with the decisions recorded
// for it, in order and stamped, and the refusal its application returned
// (empty when there was none).
type Committed struct {
	Input     event.Envelope
	Decisions []event.Envelope
	Refusal   string
}

// Committed returns the group committed for the input with this ID: made
// durable for a durable Recorder, recorded for an in-memory one. An input
// refused before it was recorded (a resume divergence, the emission
// ceiling) or whose group could not be made durable was never committed,
// and reports false; an input the handler refused is committed with its
// refusal. Because the group is the log's own record,
// the reply built from it is the same however often it is asked for and
// whether the run was resumed since (ADR 0027, sections 4 and 6).
func (r *Recorder) Committed(inputID string) (Committed, bool) {
	g, ok := r.committed[inputID]
	if !ok {
		return Committed{}, false
	}
	c := Committed{Input: r.entries[g.start].Envelope, Refusal: g.refusal}
	for _, e := range r.entries[g.start+1 : g.end] {
		c.Decisions = append(c.Decisions, e.Envelope)
	}
	return c, true
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

// NewDurableRecorder returns a Recorder holding up to maxRecords records
// that also streams every input's group to log before releasing any of its
// decisions (ADR 0027).
func NewDurableRecorder(handler replay.Handler, log *LogWriter, maxRecords int) *Recorder {
	return &Recorder{handler: handler, maxRecords: maxRecords, log: log}
}

// DurabilityError reports an input whose records could not be made durable,
// or that a durable Recorder could no longer accept because an earlier one
// could not. None of its decisions is returned (ADR 0027).
type DurabilityError struct {
	Input string
	Err   error
}

func (e *DurabilityError) Error() string {
	return fmt.Sprintf("journal: the records of event %s are not durable, so none of its decisions is released: %v", e.Input, e.Err)
}

func (e *DurabilityError) Unwrap() error { return e.Err }

// Apply implements replay.Handler. An in-memory Recorder returns the
// wrapped handler's decisions and error unchanged, so the driver sees
// exactly what it would without the Recorder in the way. A durable Recorder
// returns them only once they are durable, and returns no decision with any
// error of its own (journal-before-send; ADR 0027).
func (r *Recorder) Apply(ctx context.Context, input event.Envelope) ([]event.Envelope, error) {
	if r.handler == nil {
		return nil, errors.New("journal: a recorder requires a handler to record")
	}
	if r.failed != nil {
		return nil, &DurabilityError{Input: input.ID, Err: r.failed}
	}
	// Before the input is recorded, so nothing of this input reaches the
	// journal without the decisions it caused.
	if len(r.entries) >= r.maxRecords {
		return nil, &RecordLimitError{Recorded: len(r.entries), Limit: r.maxRecords}
	}

	decisions, applyErr := r.handler.Apply(ctx, input)
	// Also before the input is recorded, so a run that trips the ceiling
	// journals whole inputs only, and so the overshoot the input-boundary
	// check permits has a size this package states rather than one the
	// handler chooses.
	if len(decisions) > MaxEmissionsPerInput {
		ceiling := &EmissionLimitError{Input: input.ID, Emitted: len(decisions), Limit: MaxEmissionsPerInput}
		if applyErr != nil {
			// The handler's error first, so errors.Is still finds it: a
			// burst too large to journal must not hide why it was emitted.
			return r.released(decisions), fmt.Errorf("%w; %w", applyErr, ceiling)
		}
		return r.released(decisions), ceiling
	}

	group := []Entry{{Kind: KindInput, Envelope: input}}
	var stampErr error
	for i, decision := range decisions {
		r.outputSequence++
		stamped := replay.Stamp(input, decision, r.outputSequence)
		if err := stamped.Validate(); err != nil {
			stampErr = fmt.Errorf("journal: emission %d of event %s cannot be journalled: %w", i, input.ID, err)
			break
		}
		group = append(group, Entry{Kind: KindDecision, Envelope: stamped})
	}
	refusal := applyErr
	if stampErr != nil {
		refusal = stampErr
	}
	if err := r.record(input, group, refusal); err != nil {
		return nil, err
	}
	if stampErr != nil {
		return r.released(decisions), stampErr
	}
	return decisions, applyErr
}

// released is what Apply may return alongside an error of the Recorder's
// own: everything, for an in-memory Recorder; nothing, for a durable one,
// since some of decisions were never journalled.
func (r *Recorder) released(decisions []event.Envelope) []event.Envelope {
	if r.log != nil {
		return nil
	}
	return decisions
}

// record takes one input's group: checked against the recorded run while
// resuming, then made durable, then held for the end-of-run journal.
//
// A group whose log append failed is still held: the journal written at the
// end of the run keeps it as evidence of what was decided, while the log a
// restart reads does not, and nothing of it was released.
func (r *Recorder) record(input event.Envelope, group []Entry, refusal error) error {
	if r.expected != nil {
		if divergence := compareGroup(input.ID, r.expected, group); divergence != nil {
			r.failed = divergence
			return divergence
		}
		r.expected = nil
	}
	start := len(r.entries)
	r.entries = append(r.entries, group...)
	if r.log != nil {
		if err := r.log.Append(group); err != nil {
			r.failed = err
			return &DurabilityError{Input: input.ID, Err: err}
		}
	}
	if _, seen := r.committed[input.ID]; !seen {
		if r.committed == nil {
			r.committed = map[string]committedGroup{}
		}
		g := committedGroup{start: start, end: len(r.entries)}
		if refusal != nil {
			g.refusal = refusal.Error()
		}
		r.committed[input.ID] = g
	}
	return nil
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
