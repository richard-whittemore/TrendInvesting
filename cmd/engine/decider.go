package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
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
// exactly as journal.Recorder.Apply returned them to this command's Decider
// — UNSTAMPED, matching replay.Handler's own documented contract: a handler
// "must not rely on Sequence, CausationID, or CorrelationID it sets on an
// emitted envelope", because "Engine.Run stamps all three, overwriting
// whatever the handler provided", and one that emits nothing at all
// "returns (nil, nil); this is valid, not an error". The stamped,
// causally-linked copy of these same decisions is what journal.Recorder
// records internally (via replay.Stamp) into the journal this command
// writes; this wire payload is a separate, unstamped view of the same
// values, and a reader who needs Sequence/CausationID/CorrelationID reads
// the journal, not the wire.
type decisionsPayload struct {
	Decisions []event.Envelope `json:"decisions"`
}

// errAnotherConnectionActive reports that this engine is already exchanging
// with one connection when a second one attempts to. See newDecider's "#4"
// section for the full reasoning; the transport layer surfaces this exactly
// like any other Decider error — a transport.CodeDeciderFailed protocol
// error, per transport/server.go's decisionErrorCode — so the connection
// that loses the race is told plainly and the session survives for the one
// that is already active.
var errAnotherConnectionActive = errors.New("engine: another connection is already exchanging with this engine; only one active executor may submit orders (docs/architecture.md), and the reducer this command wraps is not safe for concurrent use (internal/strategy.Reducer's own doc comment)")

// newDecider composes recorder — already wrapping a configured
// strategy.Reducer (see run) — into a transport.Decider.
//
// # Decision 1: a Decider returns one envelope; a reducer returns many
//
// transport.Decider is func(ctx, bar) (event.Envelope, error): the wire
// protocol carries exactly one reply per request (transport/protocol.go's
// own doc comment: "A reply is one JSON-encoded [Response]... Requests are
// answered in order on a connection"), a shape ADR 0014 measured and
// adopted after comparing it against gRPC. strategy.Reducer.Apply, by
// contrast, returns zero or more decisions per input
// (internal/replay.Handler's own doc comment: "A handler that emits
// nothing returns (nil, nil); this is valid, not an error") — a single
// breakout bar routinely produces three, in a fixed order
// (internal/strategy/reducer.go's Reducer doc comment: "Setup-evaluated,
// Signal, sizing outcome").
//
// Two ways to close that gap were considered.
//
// Changing transport.Decider to return []event.Envelope was rejected for
// this ticket. It would touch transport/protocol.go's Response shape,
// transport/client.go's Client.Decide, both existing callers
// (cmd/transport-spike, transport/spike) and the whole of
// transport/transport_test.go — the wire shape ADR 0014 actually measured
// round-trip latency for — to solve a problem this composition can avoid
// creating in the first place: nothing about the newline-delimited frame
// format needs to change for one reply to carry several decisions.
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
// events, which is exactly the "unknown schemas... uncertain brokerage
// state" style of fail-closed default docs/development.md's principle 4
// requires generally.
//
// transport.Client.exchange already treats this outer envelope exactly like
// any other reply: it checks the single reply's CausationID against the bar
// it sent (transport/client.go), which this envelope carries (see
// bundleDecisions), so the existing client-side machinery needs no change
// either.
//
// The journal this command writes is unaffected by this choice either way:
// recorder.Apply records each decision as its own stamped record
// (journal.Recorder's own job, unconditionally), independent of how this
// Decider chooses to answer the wire.
//
// # Decision 4: only one active executor
//
// transport.Server accepts more than one connection and would invoke this
// Decider concurrently for each — Serve spawns one goroutine per accepted
// connection, all sharing the one Decider passed to transport.Listen.
// internal/strategy.Reducer "holds mutable per-instrument state... it is
// not safe for concurrent use" (its own doc comment), so two overlapping
// calls into recorder.Apply (which forwards straight to it) would race on
// that state — memory corruption in the component that sizes real
// positions, not a mere correctness nicety. mu below exists first for that
// reason, unconditionally.
//
// TryLock, rather than Lock, is what also enforces
// docs/architecture.md's "Only one active executor may submit orders" as an
// observable outcome rather than a hidden queue: a second, CONCURRENT
// connection's request fails immediately with errAnotherConnectionActive
// instead of blocking silently behind the first — that statement is about
// who may act, not about fair scheduling between two adapters both trying
// to. A connection already holding the lock is never refused by its own
// next request: a connection is "a strictly sequential request/response
// channel" (transport/protocol.go's own doc comment), so this command never
// calls decide again for one connection until its previous call has
// already returned and released mu. A LATER, non-overlapping connection —
// the first one disconnected, and a second (or the same adapter,
// reconnected) connects afterwards — is not "another active executor" and
// is admitted normally once mu is free: see ADR 0014's own reconnection
// scenario, where a dropped adapter connection reconnects to the same
// engine and continues the run this command's journal already started
// recording (decision 3, in run's own doc comment).
//
// docs/architecture.md says plainly that the socket layer itself "does not
// elect or fence the single active executor", and ADR 0014's access-control
// amendment adds that the requirement "remains a prerequisite of paper/live
// order submission, together with its existing reconciliation and readiness
// gates" — a broader policy this ticket does not implement. What mu
// enforces here is narrower and unconditional: this REDUCER, in this
// process, decides for at most one connection at a time, which is required
// for correctness regardless of
// whatever broader executor-election policy a later ticket adds.
func newDecider(recorder *journal.Recorder, strategyVersion, configurationHash string) transport.Decider {
	var mu sync.Mutex
	return func(ctx context.Context, input event.Envelope) (event.Envelope, error) {
		if !mu.TryLock() {
			return event.Envelope{}, errAnotherConnectionActive
		}
		defer mu.Unlock()

		decisions, err := recorder.Apply(ctx, input)
		if err != nil {
			// Fail closed (docs/development.md principle 4: "Fail closed on
			// unknown schemas, missing sequences, stale data, or uncertain
			// brokerage state"): an envelope the reducer rejects, or a bound
			// journal.Recorder itself enforces (journal.RecordLimitError,
			// journal.EmissionLimitError), reaches the adapter as a
			// protocol error (transport/server.go's decisionErrorCode maps
			// any non-nil error here to CodeDeciderFailed, absent a
			// deadline). Nothing is fabricated in its place.
			return event.Envelope{}, err
		}
		return bundleDecisions(input, decisions, strategyVersion, configurationHash)
	}
}

// bundleDecisions builds the wire envelope newDecider returns for one
// accepted input.
func bundleDecisions(input event.Envelope, decisions []event.Envelope, strategyVersion, configurationHash string) (event.Envelope, error) {
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
