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
//
// Run and Apply (below) are two independent entry points onto the same
// Handler, each scoped to what its own caller actually has in hand, and
// neither shares state with the other on one Engine value:
//
//   - Run checks one BATCH's own sequence for internal contiguity, starting
//     fresh every call — this is deliberate, pre-existing behaviour a
//     caller may rely on: internal/strategy's own test suite constructs one
//     Reducer/Engine and calls Run several times in stages, including
//     retrying an identical, already-numbered batch after a failure, and
//     documents that "replay.Engine.Run only requires each CALL's own input
//     sequence to be internally contiguous... not contiguous against a
//     PRIOR call" (internal/strategy/stop_ladder_test.go's own comment on
//     TestPartialStopWithAnInvalidExpiryLeavesCampaignStateCompletelyUnchanged).
//     That is the right contract for a caller replaying a whole recorded
//     journal, or retrying one rejected batch, where "the next call" is a
//     new, independently-numbered attempt, not a continuation of the same
//     producer's stream.
//   - Apply checks ONE envelope at a time against a cursor that persists on
//     the Engine value itself, across as many separate calls as it is ever
//     asked to make. That is the contract a genuinely continuous stream
//     needs — an adapter's own bar-by-bar numbering, arriving one envelope
//     per call over an indefinite lifetime, where "the next call" IS the
//     same producer's very next message, and a gap between two SEPARATE
//     calls is exactly as real a defect as a gap within one batch. Run's
//     own per-batch semantics cannot serve this: calling Run with a
//     one-element slice, repeatedly, would never check anything against the
//     PREVIOUS call at all (each Run call is deliberately independent, per
//     the point above), which is precisely how an earlier version of
//     cmd/engine came to accept a duplicated or gapped bar over the socket.
//
// A caller uses one or the other, never both on one Engine value: nothing
// here forbids constructing an Engine and calling both, but the two checks
// would then be answering different questions about two differently-scoped
// notions of "the stream", and mixing them has no tested meaning.
type Engine struct {
	handler Handler

	// hasPrevious and previous are Apply's own contiguity state, entirely
	// separate from Run's (each Run call uses its own local variables, as
	// it always has): false, and so unconsulted, before this Engine's first
	// call to Apply, and thereafter the Sequence of the last input Apply
	// accepted.
	hasPrevious bool
	previous    uint64
	// outputSequence numbers Apply's own emitted stream, independent of the
	// input stream and shared across every call to Apply for the life of
	// this Engine, for the identical reason previous is: two separate calls
	// to Apply form one contiguous output stream, not two. Run keeps its
	// own, separate output counter, exactly as it always has.
	outputSequence uint64
}

// New returns an Engine using handler.
func New(handler Handler) (*Engine, error) {
	if handler == nil {
		return nil, errors.New("replay handler is required")
	}
	return &Engine{handler: handler}, nil
}

// Apply validates envelope, checks it continues this Engine's own,
// persistent input sequence (see Engine's own doc comment for how this
// differs from, and does not share state with, Run), applies it to the
// wrapped Handler, and returns every decision envelope the handler emitted
// from it, in emission order.
//
// The first call an Engine ever receives accepts any starting Sequence;
// every call after it — however many separate calls this Engine is ever
// asked to make — must name the immediately preceding call's own Sequence +
// 1, so a missing, duplicated, or reordered event fails closed across the
// Engine's whole lifetime, not merely within whichever single call
// encountered it.
//
// Emitted envelopes form their own contiguous output stream, independent of
// the input stream: Apply assigns each one's Sequence from a counter shared
// across every call this Engine makes, starting at 1, in emission order,
// overwriting whatever the handler set. For each emission Apply also sets
// CausationID to the input envelope's ID, and CorrelationID to the input's
// CorrelationID if set, else the input's ID; a handler cannot override
// either. Each emitted envelope is validated after stamping, so an invalid
// emission fails closed, naming the input sequence and the emission index.
//
// When the handler's own Apply returns an error, its emissions (if any — see
// Handler's doc comment on a handler's final explanatory event) are still
// stamped, validated, and returned exactly like a successful call's,
// alongside the error. A handler's error always wins the message: if the
// call's own emission is ALSO invalid, both failures are named, with the
// handler's original error first in the chain (errors.Is/As still finds it)
// and the emission's invalidity appended, since an emission that cannot be
// journalled is a failure in its own right and must not be silently dropped
// behind the error that happened to arrive alongside it.
//
// # What advances together, and what stays put together
//
// previous (the input cursor) and outputSequence (the output counter) move
// together, as one fact: whether this call is keeping anything at all. They
// advance exactly when Apply is about to return a decisions slice that is
// not nil, however that slice came to be — a fully successful call, a plain
// handler error whose emissions all validated, or a handler error whose
// emission was ALSO invalid but had valid siblings before it, kept under
// Handler's own "may emit a final event explaining why" contract. In every
// one of those, this envelope's Sequence is spent — a retry of the identical
// envelope is a duplicate, not a retry — and outputSequence is left exactly
// where the returned slice's own stamps end, never one further, so the very
// next call's own first emission continues immediately after it with no gap.
//
// They stay exactly where they were, together, only when Apply is about to
// return nil outright: a return before the handler is ever reached (ctx,
// envelope shape, or contiguity itself), where this envelope was never
// accepted as part of the stream at all, or a post-handler invalid emission
// with NO accompanying handler error, which — unlike the case above — has
// nothing worth keeping and discards the whole call. Only THAT case is a
// genuine retry: this envelope's own Sequence is checked again, unchanged,
// against unchanged output positions, exactly as if the call had never
// happened, because nothing of it was kept anywhere.
//
// outputSequence is therefore never incremented by an emission that ends up
// discarded: a naive implementation that bumped it inside the validation
// loop before knowing whether THIS call's result survives would burn output
// positions on a call whose own decisions never reach a caller, leaving the
// next kept decision to start after a gap — exactly the "contiguous output
// stream" promise above would then not hold for a caller reading Apply's
// return values across calls, however faithfully it held within one.
func (e *Engine) Apply(ctx context.Context, envelope event.Envelope) ([]event.Envelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := envelope.Validate(); err != nil {
		return nil, fmt.Errorf("event %s at sequence %d: %w", envelope.ID, envelope.Sequence, err)
	}
	if e.hasPrevious && envelope.Sequence != e.previous+1 {
		return nil, fmt.Errorf("non-contiguous sequence: got %d after %d", envelope.Sequence, e.previous)
	}

	decisions, applyErr := e.handler.Apply(ctx, envelope)

	// entryOutputSequence is read once and never written until this call
	// knows what it is keeping: every stamp below is computed from it plus
	// how many decisions are in emitted so far, so emitted's own length is
	// the single source of truth for both the next stamp to hand out and,
	// at each return, how much of outputSequence's advance to commit.
	entryOutputSequence := e.outputSequence
	var emitted []event.Envelope
	for emissionIndex, decision := range decisions {
		decision = Stamp(envelope, decision, entryOutputSequence+uint64(len(emitted))+1)
		if err := decision.Validate(); err != nil {
			validationErr := fmt.Errorf("emit at input sequence %d, emission %d: %w", envelope.Sequence, emissionIndex, err)
			if applyErr != nil {
				e.outputSequence = entryOutputSequence + uint64(len(emitted))
				e.hasPrevious = true
				e.previous = envelope.Sequence
				return emitted, fmt.Errorf("apply event %s at sequence %d: %w; its final emission is also invalid: %w", envelope.ID, envelope.Sequence, applyErr, validationErr)
			}
			// Nothing from this call is kept anywhere: neither cursor
			// moves, so a retry of the identical envelope, at the
			// identical Sequence, is checked exactly as this call was.
			return nil, validationErr
		}
		emitted = append(emitted, decision)
	}

	e.outputSequence = entryOutputSequence + uint64(len(emitted))
	e.hasPrevious = true
	e.previous = envelope.Sequence

	if applyErr != nil {
		return emitted, fmt.Errorf("apply event %s at sequence %d: %w", envelope.ID, envelope.Sequence, applyErr)
	}
	return emitted, nil
}

// Run applies events in the supplied order and returns every decision
// envelope the handler emitted, in emission order. Input sequences must be
// contiguous WITHIN THIS CALL so a missing, duplicated, or reordered event
// fails closed; a later, separate call to Run (or to Apply) on the same
// Engine checks its own sequence independently, starting fresh — see
// Engine's own doc comment for why that is deliberate and tested, not an
// oversight Apply needed to inherit.
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
