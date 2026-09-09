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

// Reducer implements replay.Handler for #8/#9: it tracks each instrument's
// True Range, N (CONTEXT.md: "True Range", "N"), and Entry Channel
// (CONTEXT.md: "Entry Channel"; ADR 0002) from completed bars. It emits one
// Setup-evaluated decision event per bar, reporting N, the Entry Channel,
// the Setup's Tier, and its distance to entry in N; when the bar's high
// exceeds the Entry Channel (a Breakout, Tier A), it additionally emits a
// Signal, Setup-evaluated first (#9's ordering). It requires a configuration
// event before any bar and fails closed if a bar arrives first, accepts
// exactly one configuration event per run (matching its constructor's
// configuration hash — see applyConfiguration), and rejects a duplicate or
// out-of-order bar for any one instrument (see applyCompletedBar).
//
// N and the Entry Channel are computed from the split-adjusted price view
// only (ADR 0004): signal computation must never see raw prices, so a
// Campaign's entries and exits stay consistent with the levels a live
// system would have seen on the day.
//
// This reducer holds mutable per-instrument state (docs/architecture.md
// notes a Handler applies events to "deterministic domain state"); it is not
// safe for concurrent use, matching replay.Engine's sequential Run.
type Reducer struct {
	strategyVersion   string
	configurationHash string

	configured         bool
	entryChannelLength int
	tierBDistanceInN   float64

	instruments map[string]*instrumentState
}

// instrumentState is one instrument's running True Range/N/Entry Channel
// state. previousClose and hasPreviousClose together let TrueRange compute
// the gap terms once a prior bar exists, and are left at their zero values
// for the first bar of a given instrument, per the choice documented on
// indicator.TrueRange. lastPeriodEnd is the PeriodEnd of the last bar
// accepted for this instrument, and is the zero time.Time before any bar has
// been seen (CompletedBarPayload.Validate already rejects a zero PeriodEnd,
// so the zero value is unambiguous as "no bar yet").
type instrumentState struct {
	previousClose    float64
	hasPreviousClose bool
	lastPeriodEnd    time.Time
	n                *indicator.WilderAverage
	entryChannel     *indicator.EntryChannel
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
// Three checks guard the provenance the reducer stamps on every later
// decision (Source, StrategyVersion, ConfigurationHash), and the shape of
// the payload itself:
//
//   - The envelope's SchemaVersion must equal event.ConfigurationSchemaVersion.
//     This is the payload-level counterpart of ADR 0015's envelope-level
//     rule: an older schema is rejected, never silently upgraded, until an
//     explicit upcaster exists. Without this check, a schema-1 configuration
//     (recorded before #9 added TierBDistanceInN) would still decode: the
//     missing field unmarshals as the float64 zero value, ConfigurationPayload.Validate
//     accepts a zero TierBDistanceInN as legitimately "no Tier B window", and
//     Tier B silently changes meaning for that run rather than the run being
//     rejected as incompatible.
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
	if envelope.SchemaVersion != event.ConfigurationSchemaVersion {
		return nil, fmt.Errorf("strategy: configuration payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.ConfigurationSchemaVersion)
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
	r.entryChannelLength = payload.EntryChannelLength
	r.tierBDistanceInN = payload.TierBDistanceInN
	r.configured = true
	return nil, nil
}

// applyCompletedBar handles event.CompletedBarEventType. Like
// applyConfiguration, it rejects a schema version other than
// event.CompletedBarSchemaVersion before decoding, for the same reason (ADR
// 0015's envelope-level rule, applied here at the payload level): a schema
// this build was not written against must never be silently interpreted as
// the current one.
func (r *Reducer) applyCompletedBar(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received a completed bar before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.CompletedBarSchemaVersion {
		return nil, fmt.Errorf("strategy: completed bar payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.CompletedBarSchemaVersion)
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

	// ADR 0004: signal computation, including N and the Entry Channel, runs
	// on the split-adjusted view only.
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

	// #9's ordering, the fix for the prototype's headline look-ahead bug
	// (see indicator.EntryChannel's doc comment): Extreme is read BEFORE
	// this bar's own high is Added, so the channel this bar is decided
	// against never includes the bar itself.
	entryChannelHigh, entryChannelReady := state.entryChannel.Extreme()
	// The Turtle Rules p.19: a Breakout "exceeds" the channel, so the
	// comparison is strict; a tie is not a breakout.
	breakout := entryChannelReady && view.High > entryChannelHigh
	state.entryChannel.Add(view.High)

	// Tier follows three cases, per DistanceToEntryInN's sign and magnitude
	// (see SetupEvaluatedPayload's doc comment): a strict breakout
	// (distance < 0) is TierA; a tie or an approach within the configured
	// distance (0 <= distance <= TierBDistanceInN) is TierB — since
	// TierBDistanceInN is always non-negative (ConfigurationPayload.Validate),
	// a tie (distance exactly 0) is always at least TierB, never TierNone,
	// so ADR 0011's Watchlist surfaces it as the closest possible approach
	// without a breakout; anything farther is TierNone.
	ready := nReady && entryChannelReady
	tier := event.TierNone
	var distanceToEntryInN float64
	if ready {
		distanceToEntryInN = (entryChannelHigh - view.High) / state.n.Value()
		switch {
		case breakout:
			tier = event.TierA
		case distanceToEntryInN >= 0 && distanceToEntryInN <= r.tierBDistanceInN:
			tier = event.TierB
		}
	}

	reportedEntryChannelHigh := entryChannelHigh
	if !entryChannelReady {
		// Matches N's convention (indicator.WilderAverage: zero while not
		// ready): a channel high computed from fewer than
		// EntryChannelLength bars is not a real Entry Channel level and
		// must never be read as one.
		reportedEntryChannelHigh = 0
	}

	decisionPayload := event.SetupEvaluatedPayload{
		InstrumentID:       bar.InstrumentID,
		PeriodEnd:          bar.PeriodEnd,
		N:                  state.n.Value(),
		NReady:             nReady,
		EntryChannelHigh:   reportedEntryChannelHigh,
		EntryChannelReady:  entryChannelReady,
		Tier:               tier,
		DistanceToEntryInN: distanceToEntryInN,
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
	emissions := []event.Envelope{decision}

	// #9: Tier A is a Signal. No Signal while N or the channel is not ready
	// (tier is TierA only when ready is true, above), and never on a tie
	// (a tie is TierB, not TierA — breakout, and therefore tier==TierA,
	// requires a strict >). Emitted after the Setup-evaluated event, per
	// the ticket's ordering.
	if tier == event.TierA {
		signalPayload := event.SignalPayload{
			InstrumentID: bar.InstrumentID,
			PeriodEnd:    bar.PeriodEnd,
			Rule:         event.RuleSystem2Entry,
			ADR:          event.ADRSystem2Baseline,
			Direction:    event.DirectionLong,
			// The length this Signal was actually computed with, not a
			// hard-coded 55: a Variant configured with a different
			// EntryChannelLength must not have its Signals mislabelled with
			// the Baseline's length.
			EntryChannelLength: r.entryChannelLength,
			EntryChannelHigh:   entryChannelHigh,
			BreakoutHigh:       view.High,
			N:                  state.n.Value(),
		}
		if err := signalPayload.Validate(); err != nil {
			return nil, fmt.Errorf("strategy: built invalid signal payload: %w", err)
		}
		signalBytes, err := json.Marshal(signalPayload)
		if err != nil {
			return nil, fmt.Errorf("strategy: marshal signal payload: %w", err)
		}
		signal := event.Envelope{
			// Deterministic and reproducible on replay, same rationale as
			// the Setup-evaluated event's ID above.
			ID:                fmt.Sprintf("signal:%s:%s", bar.InstrumentID, bar.PeriodEnd.UTC().Format("2006-01-02T15:04:05.000000000Z")),
			Type:              event.SignalEventType,
			SchemaVersion:     event.SignalSchemaVersion,
			EnvelopeVersion:   event.CurrentEnvelopeVersion,
			EventTime:         bar.PeriodEnd,
			RecordedAt:        envelope.RecordedAt,
			Source:            sourceReducer,
			StrategyVersion:   r.strategyVersion,
			ConfigurationHash: r.configurationHash,
			PayloadHash:       event.HashPayload(signalBytes),
			Payload:           signalBytes,
		}
		emissions = append(emissions, signal)
	}

	return emissions, nil
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
	entryChannel, err := indicator.NewEntryChannel(r.entryChannelLength)
	if err != nil {
		// Unreachable in practice: applyConfiguration only sets
		// entryChannelLength from a payload that ConfigurationPayload.Validate
		// has already required to be positive. Failing closed anyway rather
		// than panicking, in case that ever changes.
		return nil, fmt.Errorf("strategy: %w", err)
	}
	state := &instrumentState{n: n, entryChannel: entryChannel}
	r.instruments[instrumentID] = state
	return state, nil
}
