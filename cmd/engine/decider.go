package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

// decisionsEventType identifies decisionsPayload for the Envelope's Type
// field. It is not a domain event internal/strategy ever emits — it belongs
// to this command's own composition, never to internal/event's contract —
// and exists only because of the mismatch decision 1 (below) resolves.
const decisionsEventType = "engine.decisions"

// decisionsSchemaVersion is decisionsPayload's own schema version. It shares
// nothing with event.ConfigurationSchemaVersion or any payload schema
// internal/strategy defines: this shape belongs to this command alone.
const decisionsSchemaVersion uint32 = 1

// decisionsPayload is decisionsEventType's payload: the decisions
// strategy.Reducer emitted for one accepted input, in emission order,
// exactly as replay.Engine.Apply returned them to this command's Decider —
// stamped (Sequence, CausationID, CorrelationID) exactly as journal.Recorder
// stamps its own copy for the journal, by the same replay.Stamp function,
// because both now go through the one replay.Engine wrapping the same
// recorder (decision 1's own routing; see newDecider).
//
// Decisions is never nil on the wire: bundleDecisions normalises a reducer
// that decided nothing for this input into an empty JSON array, never the
// `null` a bare json.Marshal of a nil slice would write. The two decode to
// the same length in Go, which is exactly why a Go-only test would miss the
// difference — but the LEAN adapter is Python, where `null` and `[]` are not
// the same value and a consumer written against "a list of decisions" is
// entitled to assume it is never handed anything else.
type decisionsPayload struct {
	Decisions []event.Envelope `json:"decisions"`
}

// wireGuard is what makes strategy.Reducer's state safe to read once this
// process is ready to write its journal, given the two facts a round of
// review confirmed against the tree:
//
//   - strategy.Reducer.Apply's own signature discards its context
//     (its first parameter is `_`): once called, it runs to completion
//     however long that takes, and nothing about ctx being cancelled stops
//     it partway through.
//   - transport.Server.decideWithTimeout races a Decider call against
//     ctx.Done() in a goroutine transport's own Server.wg does not track,
//     and gives up waiting the moment ctx.Done() fires — leaving that
//     goroutine to keep running, unobserved by anything transport itself
//     ever waits for again.
//
// So a decision transport has already abandoned (a configured
// DecisionTimeout firing, or this process's own shutdown cancelling the
// server's lifetime while a decision is mid-flight) can still be inside
// engine.Apply — still mutating the journal.Recorder engine wraps — after
// transport.Server has told the connection the decision was abandoned, and
// even after Server.ServeContext has returned control to run. Reading
// recorder.Entries()/Header() at that point, or admitting a further
// decision as though nothing had happened, would race that goroutine or
// finalise a journal it might still be appending to.
//
// wireGuard closes that gap with the smallest mechanism that actually
// prevents it, rather than a second, independent timeout of its own:
//
//   - mu is held for the WHOLE of one call to decide, not merely entered and
//     released around it, so an abandoned call's goroutine still holds mu
//     for as long as it is actually running inside the reducer — serialising
//     it against every OTHER call this composition ever makes into engine,
//     including one on the very next frame the same connection sends after
//     an abandoned decision. ADR 0014's own measured failure table gives
//     "Drop the bar" as the adapter's correct response to a rejected
//     timeout, over the SAME connection — nothing about that response stops
//     a second decide call from starting while the first one's goroutine is
//     still running.
//   - The moment a call's own ctx is found to have been cancelled once
//     engine.Apply returns, wireGuard is permanently poisoned: every decide
//     call after that point — on this connection or a later one — returns
//     the same error immediately, without touching engine again, instead of
//     going on as though the abandoned call had never happened. Once one
//     decision's true effect on the reducer was no longer being waited for
//     by anything, this process can no longer vouch for what state the
//     reducer is in, and applying MORE input against it would compound a
//     state nothing has verified rather than merely reporting it once.
//   - awaitIdle, called once ServeContext has returned and before run reads
//     anything from the recorder, blocks until whichever call currently
//     holds mu — ordinarily none, but possibly a very late, abandoned one —
//     has actually finished, so this process never finalises a journal a
//     goroutine may still be appending decisions to. It does not shorten how
//     long an abandoned call takes to finish (nothing can, since neither the
//     reducer nor journal.Recorder ever look at ctx); it only makes sure
//     this process never reads the recorder before that call is done.
//
// What this costs: a decision transport judges to be stuck stops this
// engine's forward progress rather than being cast adrift while everything
// else carries on — every later decision blocks behind it, and if it never
// returns (the reducer is, by internal/'s own rules, deterministic and does
// no I/O, so "never" means a genuine defect, not an expected outcome) this
// process's shutdown blocks on awaitIdle for ever too, and the only recourse
// is to SIGKILL it, losing every decision since the journal was last
// written — the identical exposure ADR 0017 already states for a run that
// ends without writing at all, reached here through a different door.
type wireGuard struct {
	mu       sync.Mutex
	wg       sync.WaitGroup
	poisoned error
}

// decide applies input through engine while holding g for the call's whole
// duration (see wireGuard's own doc comment), and permanently poisons g the
// moment ctx is found to have been cancelled once engine.Apply returns.
func (g *wireGuard) decide(ctx context.Context, engine *replay.Engine, input event.Envelope) ([]event.Envelope, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.poisoned != nil {
		return nil, g.poisoned
	}
	g.wg.Add(1)
	defer g.wg.Done()

	decisions, err := engine.Apply(ctx, input)
	if ctxErr := ctx.Err(); ctxErr != nil {
		g.poisoned = fmt.Errorf("engine: this decision's deadline passed while it was still running against the reducer, so the wire had already abandoned it; this engine's state can no longer be vouched for, and no further decisions will be made: %w", ctxErr)
		return nil, g.poisoned
	}
	return decisions, err
}

// awaitIdle blocks until no call to decide is in progress. Call it once
// transport.Server.ServeContext has returned and before reading anything
// engine.Apply (and so decide) may have written to — see wireGuard's own
// doc comment for why a call can still be running after ServeContext
// returns, and why waiting rather than acting immediately is the fix.
func (g *wireGuard) awaitIdle() {
	g.wg.Wait()
}

// newDecider composes engine — a *replay.Engine already wrapping a
// journal.Recorder around a configured strategy.Reducer (see run) — into a
// transport.Decider, guarded by guard (see wireGuard).
//
// # Decision 1: a Decider returns one envelope; a reducer returns many
//
// transport.Decider is func(ctx, bar) (event.Envelope, error): the wire
// protocol carries exactly one reply per request (transport/protocol.go's
// own doc comment: "A reply is one JSON-encoded [Response] followed by a
// newline. Requests are answered in order on a connection, so a connection
// is a strictly sequential request/response channel"), a shape ADR 0014
// measured and adopted after comparing it against gRPC. strategy.Reducer
// implements replay.Handler, whose own doc comment says a handler "returns
// the decision envelopes it emits, in emission order" and "that emits
// nothing returns (nil, nil); this is valid, not an error" — zero or more
// decisions per input, never exactly one: a single breakout bar routinely
// produces three, in a fixed order (internal/strategy/reducer.go's Reducer
// doc comment: "Setup-evaluated, Signal, sizing outcome").
//
// Two ways to close that gap were considered.
//
// Changing transport.Decider to return []event.Envelope was rejected here.
// It would touch transport/protocol.go's Response shape, transport/client.go's
// Client.Decide, both existing callers (cmd/transport-spike, transport/spike)
// and the whole of transport/transport_test.go — the wire shape ADR 0014
// actually measured round-trip latency for — to solve a problem this
// composition can avoid creating in the first place: nothing about the
// newline-delimited frame format needs to change for one reply to carry
// several decisions.
//
// So this Decider returns exactly one envelope, of Type decisionsEventType,
// whose Payload is decisionsPayload: the reducer's own decisions for this
// input, in emission order, UNCHANGED. Nothing is picked as "the" decision
// (that would be a rule about which decision TYPE matters to an order —
// cmd/ contains no rules, cmd-is-composition-only, .golangci.yml), nothing
// is summarised, and nothing is dropped. A reducer that decided nothing for
// this input at all (internal/strategy/reducer.go's delisted-instrument
// case, for one) is represented honestly as an empty list — never as a
// fabricated envelope pretending to be one of the reducer's own domain
// events, which is exactly the kind of default docs/development.md's
// principle 4 ("Fail closed on unknown schemas, missing sequences, stale
// data, or uncertain brokerage state") rules out generally.
//
// transport.Client.exchange already treats this outer envelope exactly like
// any other reply: it checks the single reply's CausationID against the bar
// it sent (transport/client.go), which this envelope carries (see
// bundleDecisions), so the existing client-side machinery needs no change
// either.
//
// The journal this command writes is unaffected by this choice either way:
// journal.Recorder records each decision as its own stamped record
// unconditionally, independent of how this Decider chooses to answer the
// wire.
//
// # Decision 1, continued: the input stream must be routed through
// replay.Engine, not journal.Recorder alone
//
// journal.Recorder.Apply performs no sequence-contiguity check of its own —
// nothing does, unless something wraps it in a replay.Engine, whose Apply
// (and Run, built on it) is where that check actually lives. Calling
// journal.Recorder.Apply directly, as an earlier version of this command
// did, let a duplicated or gapped input reach the reducer and be journalled
// as though it were a well-formed stream: a journal that could then fail
// its own replay, in the one artefact this project treats as evidence.
//
// The fix is not a second contiguity check written here: replay.Engine.Apply
// already enforces it — a missing, duplicated, or reordered input fails
// closed there, by Apply's own doc comment, the same way Run already
// enforced it for a whole batch handed to it at once — so this composition
// inherits the one existing implementation of the rule rather than
// restating it. engine, the parameter here, IS that Engine, constructed
// once in run and reused for every call this Decider ever makes, so its own
// persisted cursor spans the whole run, across as many separate connections
// as arrive at it (decision 3, run's own doc comment).
//
// # Decision 4: only one active executor, and what protects the reducer
// even when that holds
//
// docs/architecture.md's safety invariants require that "Only one active
// executor may submit orders." transport.Server, unchanged, accepts more
// than one connection and would invoke a shared Decider concurrently for
// each — Serve spawns one goroutine per accepted connection. The fix for
// that is not inside this Decider at all: a Decider carries no connection
// identity (it is called once per REQUEST, not once per connection), so it
// cannot itself tell "a second call from the same connection I'm already
// serving" apart from "a call from a genuinely different connection". That
// distinction belongs to whatever sees a connection as a connection, which
// is transport.Server alone — so run configures
// transport.ServerConfig.MaxConnections: 1 (a new, additive field: zero
// keeps every existing caller's unlimited behaviour, and Server refuses a
// connection beyond the bound at accept time, before it ever reaches a
// Decider, while continuing to serve whichever connection it already has).
// That is an argued transport change, not a workaround: docs/architecture.md
// already says the single-active-executor requirement exists, and nothing
// before this let the transport layer enforce even the narrow slice of it
// this composition needs (excluding a second connection outright; the
// broader executor-election and fencing policy ADR 0014's access-control
// amendment describes remains, in its own words, "a prerequisite of
// paper/live order submission, together with its existing reconciliation
// and readiness gates" — not attempted here).
//
// MaxConnections: 1 is necessary and NOT sufficient on its own, which is
// exactly wireGuard's other job. Excluding a second CONNECTION does not
// exclude a second, overlapping GOROUTINE from the SAME connection: transport
// abandoning a decision by deadline (or this process's own shutdown doing
// the same) does not stop that decision's goroutine, and the connection
// that sent it — per ADR 0014's own documented behaviour, "the session
// survives" — may send its next bar immediately after, starting a second
// call to this Decider while the first is still running. wireGuard's mu
// serialises exactly that case: internal/strategy.Reducer "holds mutable
// per-instrument state... it is not safe for concurrent use" (its own doc
// comment), and two overlapping calls into it, whatever connection or
// goroutine either one came from, would race that state — memory corruption
// in the component that sizes real positions, not a mere correctness
// nicety.
func newDecider(guard *wireGuard, engine *replay.Engine, strategyVersion, configurationHash string) transport.Decider {
	return func(ctx context.Context, input event.Envelope) (event.Envelope, error) {
		decisions, err := guard.decide(ctx, engine, input)
		if err != nil {
			// Fail closed (docs/development.md principle 4): an envelope the
			// reducer rejects, a bound journal.Recorder itself enforces
			// (journal.RecordLimitError, journal.EmissionLimitError), a
			// non-contiguous sequence (replay.Engine.Apply), or a
			// wireGuard poisoned by an earlier abandoned decision, all reach
			// the adapter as a protocol error (transport/server.go's
			// decisionErrorCode maps any non-nil error here to
			// CodeDeciderFailed, absent a deadline of its own). Nothing is
			// fabricated in its place.
			return event.Envelope{}, err
		}
		return bundleDecisions(input, decisions, strategyVersion, configurationHash)
	}
}

// bundleDecisions builds the wire envelope newDecider returns for one
// accepted input.
func bundleDecisions(input event.Envelope, decisions []event.Envelope, strategyVersion, configurationHash string) (event.Envelope, error) {
	// Normalised to a non-nil, possibly-empty slice before marshalling: Go's
	// encoding/json writes a nil slice as `null`, and decisionsPayload's own
	// doc comment states why that must never reach the wire for "the
	// reducer decided nothing" (this is not a rule about internal/ or
	// domain events; it is what happens when json.Marshal([]T(nil)) is
	// asked to render a nil slice, so no citation applies beyond that fact).
	if decisions == nil {
		decisions = []event.Envelope{}
	}
	payload, err := json.Marshal(decisionsPayload{Decisions: decisions})
	if err != nil {
		return event.Envelope{}, fmt.Errorf("engine: encode the decisions payload for %s: %w", input.ID, err)
	}
	return event.Envelope{
		ID:                "decisions:" + input.ID,
		Type:              decisionsEventType,
		SchemaVersion:     decisionsSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         input.EventTime,
		RecordedAt:        time.Now().UTC(),
		Sequence:          input.Sequence,
		CorrelationID:     input.CorrelationID,
		CausationID:       input.ID,
		Source:            sourceEngine,
		StrategyVersion:   strategyVersion,
		ConfigurationHash: configurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}, nil
}
