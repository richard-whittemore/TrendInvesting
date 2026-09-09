// Package strategy holds the deterministic reducers that turn a validated
// event stream into decision events (docs/architecture.md: "Replay
// engine"). It implements replay.Handler and depends only on internal/event,
// internal/indicator, and internal/replay: no LEAN, database, transport, or
// wall-clock access (AGENTS.md rule 7; enforced by depguard/forbidigo).
package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
)

// sourceReducer is the value stamped into every envelope this package
// emits, identifying the component that produced it (docs/architecture.md:
// "the source that emitted the event").
const sourceReducer = "reducer"

// Reducer implements replay.Handler for #8: it tracks each instrument's
// True Range and N (CONTEXT.md: "True Range", "N") from completed bars, and
// emits one Setup-evaluated decision event per bar, reporting N and whether
// it is a usable volatility reading. It requires a configuration event
// before any bar and fails closed if a bar arrives first, accepts exactly
// one configuration event per run (matching its constructor's configuration
// hash — see applyConfiguration), and rejects a duplicate or out-of-order
// bar for any one instrument (see applyCompletedBar).
//
// N is computed from the split-adjusted price view only (ADR 0004): signal
// computation must never see raw prices, so a Campaign's entries and exits
// stay consistent with the levels a live system would have seen on the day.
//
// This reducer holds mutable per-instrument state (docs/architecture.md
// notes a Handler applies events to "deterministic domain state"); it is not
// safe for concurrent use, matching replay.Engine's sequential Run.
type Reducer struct {
	strategyVersion   string
	configurationHash string

	configured  bool
	instruments map[string]*instrumentState
}

// instrumentState is one instrument's running True Range/N state.
// previousClose and hasPreviousClose together let TrueRange compute the gap
// terms once a prior bar exists, and are left at their zero values for the
// first bar of a given instrument, per the choice documented on
// indicator.TrueRange. lastPeriodEnd is the PeriodEnd of the last bar
// accepted for this instrument, and is the zero time.Time before any bar has
// been seen (CompletedBarPayload.Validate already rejects a zero PeriodEnd,
// so the zero value is unambiguous as "no bar yet").
type instrumentState struct {
	previousClose    float64
	hasPreviousClose bool
	lastPeriodEnd    time.Time
	n                *indicator.WilderAverage
}

// NewReducer returns a Reducer that stamps every decision it emits with
// Source "reducer", the given strategyVersion, and the given
// configurationHash. Both are opaque strings supplied by the caller; their
// derivation from a declared Baseline or Variant configuration is #50, out
// of scope here. Both are required, matching the provenance fields
// event.Envelope.Validate() requires on every emission.
func NewReducer(strategyVersion, configurationHash string) (*Reducer, error) {
	if strategyVersion == "" {
		return nil, errors.New("strategy: strategy version is required")
	}
	if configurationHash == "" {
		return nil, errors.New("strategy: configuration hash is required")
	}
	return &Reducer{
		strategyVersion:   strategyVersion,
		configurationHash: configurationHash,
		instruments:       make(map[string]*instrumentState),
	}, nil
}

// Apply implements replay.Handler. It recognises exactly two input event
// types:
//
//   - event.ConfigurationEventType: recorded, and required before any bar.
//   - event.CompletedBarEventType: updates that instrument's True Range/N
//     and emits one event.SetupEvaluatedEventType decision.
//
// Any other event type fails closed rather than being silently ignored
// (docs/development.md principle 4: "Fail closed on unknown schemas").
func (r *Reducer) Apply(_ context.Context, envelope event.Envelope) ([]event.Envelope, error) {
	switch envelope.Type {
	case event.ConfigurationEventType:
		return r.applyConfiguration(envelope)
	case event.CompletedBarEventType:
		return r.applyCompletedBar(envelope)
	default:
		return nil, fmt.Errorf("strategy: unrecognized event type %q", envelope.Type)
	}
}

// applyConfiguration handles event.ConfigurationEventType.
//
// Two checks guard the provenance the reducer stamps on every later
// decision (Source, StrategyVersion, ConfigurationHash):
//
//   - The event's ConfigurationHash must match the hash this Reducer was
//     constructed with. Without this check a valid configuration envelope
//     carrying a *different* hash would still be accepted, and every
//     decision emitted afterwards would be stamped with the constructor's
//     hash rather than the one the configuration event actually declared —
//     an audit record that attributes a decision to the wrong configuration.
//   - A reducer accepts exactly one configuration event per run. ADR 0006
//     freezes a Campaign's N and Unit size at first entry precisely because
//     mid-stream reconfiguration is not a supported concept here; a second
//     configuration event (even one with a matching hash) is rejected
//     rather than silently re-applied.
func (r *Reducer) applyConfiguration(envelope event.Envelope) ([]event.Envelope, error) {
	if r.configured {
		return nil, fmt.Errorf("strategy: reducer is already configured (configuration hash %q); mid-stream reconfiguration is not supported (ADR 0006 freezes configuration at entry)", r.configurationHash)
	}
	if envelope.ConfigurationHash != r.configurationHash {
		return nil, fmt.Errorf("strategy: configuration event's configuration hash %q does not match the reducer's configuration hash %q", envelope.ConfigurationHash, r.configurationHash)
	}
	var payload event.ConfigurationPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return nil, fmt.Errorf("strategy: decode configuration payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid configuration payload: %w", err)
	}
	r.configured = true
	return nil, nil
}

func (r *Reducer) applyCompletedBar(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received a completed bar before a configuration event; failing closed")
	}

	var bar event.CompletedBarPayload
	if err := json.Unmarshal(envelope.Payload, &bar); err != nil {
		return nil, fmt.Errorf("strategy: decode completed bar payload: %w", err)
	}
	if err := bar.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid completed bar payload: %w", err)
	}

	state, err := r.stateFor(bar.InstrumentID)
	if err != nil {
		return nil, err
	}

	// Bar chronology, per instrument. A duplicate or out-of-order bar would
	// silently advance the True Range/N accumulator and overwrite
	// previousClose, corrupting every later gap calculation for this
	// instrument — this is not only the upstream journal's job to prevent:
	// the reducer is the last line before the arithmetic that sizes real
	// positions, so it must not trust that what it was handed is
	// chronological. Chronology is per instrument, not global: the engine's
	// input Sequence, not PeriodEnd, is what orders the stream across
	// different instruments (docs/architecture.md).
	if !state.lastPeriodEnd.IsZero() && !bar.PeriodEnd.After(state.lastPeriodEnd) {
		return nil, fmt.Errorf("strategy: instrument %q: bar period end %s is not strictly after the last recorded period end %s; rejecting a duplicate or out-of-order bar",
			bar.InstrumentID, bar.PeriodEnd.Format(time.RFC3339), state.lastPeriodEnd.Format(time.RFC3339))
	}

	// ADR 0004: signal computation, including N, runs on the split-adjusted
	// view only.
	view := bar.SplitAdjusted
	tr := indicator.TrueRange(view.High, view.Low, state.previousClose, state.hasPreviousClose)
	state.n.Add(tr)
	state.previousClose = view.Close
	state.hasPreviousClose = true
	state.lastPeriodEnd = bar.PeriodEnd

	// NReady means "N is a usable volatility reading", not merely "bar-count
	// warm-up complete": twenty flat bars (high==low==close) legitimately
	// warm up indicator.WilderAverage with N==0, which a volatility-normalised
	// sizing step could not safely divide by. Such an instrument is reported
	// not ready rather than erroring the run, and becomes ready again the
	// moment True Range is non-zero (see indicator.WilderAverage's doc
	// comment).
	nReady := state.n.Ready() && state.n.Value() > 0

	decisionPayload := event.SetupEvaluatedPayload{
		InstrumentID: bar.InstrumentID,
		PeriodEnd:    bar.PeriodEnd,
		N:            state.n.Value(),
		NReady:       nReady,
	}
	if err := decisionPayload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: built invalid setup evaluated payload: %w", err)
	}
	payloadBytes, err := json.Marshal(decisionPayload)
	if err != nil {
		return nil, fmt.Errorf("strategy: marshal setup evaluated payload: %w", err)
	}

	decision := event.Envelope{
		// Deterministic and reproducible on replay: one Setup-evaluated
		// event exists per (instrument, completed bar), so the pair
		// identifies it uniquely without any randomness or wall-clock read
		// (forbidden in internal/ — see .golangci.yml forbidigo rules).
		ID:              fmt.Sprintf("setup-evaluated:%s:%s", bar.InstrumentID, bar.PeriodEnd.UTC().Format("2006-01-02T15:04:05.000000000Z")),
		Type:            event.SetupEvaluatedEventType,
		SchemaVersion:   event.SetupEvaluatedSchemaVersion,
		EnvelopeVersion: event.CurrentEnvelopeVersion,
		EventTime:       bar.PeriodEnd,
		// Never time.Now(): RecordedAt on an emission is the input's
		// RecordedAt, not the wall clock at processing time.
		RecordedAt:        envelope.RecordedAt,
		Source:            sourceReducer,
		StrategyVersion:   r.strategyVersion,
		ConfigurationHash: r.configurationHash,
		PayloadHash:       event.HashPayload(payloadBytes),
		Payload:           payloadBytes,
	}
	return []event.Envelope{decision}, nil
}

func (r *Reducer) stateFor(instrumentID string) (*instrumentState, error) {
	if state, ok := r.instruments[instrumentID]; ok {
		return state, nil
	}
	n, err := indicator.NewWilderAverage(indicator.DefaultPeriod)
	if err != nil {
		// Unreachable: indicator.DefaultPeriod is a positive compile-time
		// constant, so NewWilderAverage never rejects it. Failing closed
		// anyway rather than panicking, in case that ever changes.
		return nil, fmt.Errorf("strategy: %w", err)
	}
	state := &instrumentState{n: n}
	r.instruments[instrumentID] = state
	return state, nil
}
