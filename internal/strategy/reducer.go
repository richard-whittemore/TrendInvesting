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
// its bar-count warm-up is complete. It requires a configuration event
// before any bar and fails closed if a bar arrives first.
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

// instrumentState is one instrument's running True Range/N state. previousClose
// and hasPreviousClose together let TrueRange compute the gap terms once a
// prior bar exists, and are left at their zero values for the first bar of a
// given instrument, per the choice documented on indicator.TrueRange.
type instrumentState struct {
	previousClose    float64
	hasPreviousClose bool
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

func (r *Reducer) applyConfiguration(envelope event.Envelope) ([]event.Envelope, error) {
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

	// ADR 0004: signal computation, including N, runs on the split-adjusted
	// view only.
	view := bar.SplitAdjusted
	tr := indicator.TrueRange(view.High, view.Low, state.previousClose, state.hasPreviousClose)
	state.n.Add(tr)
	state.previousClose = view.Close
	state.hasPreviousClose = true

	decisionPayload := event.SetupEvaluatedPayload{
		InstrumentID: bar.InstrumentID,
		PeriodEnd:    bar.PeriodEnd,
		N:            state.n.Value(),
		NReady:       state.n.Ready(),
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
