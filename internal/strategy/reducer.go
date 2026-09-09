// Package strategy holds the deterministic reducers that turn a validated
// event stream into decision events (docs/architecture.md: "Replay
// engine"). It implements replay.Handler and depends only on internal/event,
// internal/indicator, internal/sizing, and internal/replay: no LEAN,
// database, transport, or wall-clock access (AGENTS.md rule 7; enforced by
// depguard/forbidigo).
package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// sourceReducer is the value stamped into every envelope this package
// emits, identifying the component that produced it (docs/architecture.md:
// "the source that emitted the event").
const sourceReducer = "reducer"

// Reducer implements replay.Handler for #8/#9/#10: it tracks each
// instrument's True Range, N (CONTEXT.md: "True Range", "N"), and Entry
// Channel (CONTEXT.md: "Entry Channel"; ADR 0002) from completed bars. It
// emits one Setup-evaluated decision event per bar, reporting N, the Entry
// Channel, the Setup's Tier, and its distance to entry in N; when the bar's
// high exceeds the Entry Channel (a Breakout, Tier A), it additionally emits
// a Signal and then the sizing outcome that Signal produced — either a trade
// proposal or a recorded decline (see sizeUnit). The order within one Apply
// return is always Setup-evaluated, Signal, sizing outcome. It requires a
// configuration event before any bar and fails closed if a bar arrives first,
// accepts exactly one configuration event per run (matching its constructor's
// configuration hash — see applyConfiguration), and rejects a duplicate or
// out-of-order bar for any one instrument (see applyCompletedBar).
//
// N and the Entry Channel are computed from the split-adjusted price view
// only (ADR 0004): signal computation must never see raw prices, so a
// Campaign's entries and exits stay consistent with the levels a live
// system would have seen on the day.
//
// Every input to a bar's decision — N, the Entry Channel, and so the Setup's
// Tier, the Signal, the Unit size and the Protective Stop intent — is read
// from the state left by the bars BEFORE it, and the bar is folded into that
// state only afterwards. CONTEXT.md, under "Completed bar": the decision bar
// is never an input to its own decision. See applyCompletedBar's evaluate and
// advance blocks.
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

	// #10's sizing configuration, captured once from the configuration event.
	//
	// Both forms of the Sizing Mode are kept: configuredSizingMode is the
	// value the configuration event declared and is what a proposal is
	// stamped with; sizingMode is the same choice in internal/sizing's own
	// vocabulary and is what the arithmetic is called with. Keeping both
	// means the value written to the journal is the one that was configured,
	// never a string conversion back from the arithmetic package — such a
	// conversion would quietly launder a drift between the two enumerations
	// into an audit record instead of failing on it.
	configuredSizingMode event.SizingMode
	sizingMode           sizing.Mode
	unitVolatilityFrac   float64
	stopMultiple         float64
	riskAtStopFraction   float64
	dollarsPerPoint      float64
	// notionalAccount is the configured starting equity (ADR 0007). The
	// Drawdown Steps that reduce it and the yearly re-basing that restores it
	// are #16/#17: nothing in this reducer changes it yet.
	notionalAccount float64

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

	// #11's two additions, both defined and explained in campaign.go.
	// pendingProposal is a trade proposal emitted and not yet resolved, and is
	// deliberately NOT position state: no Campaign is ever derived from it
	// alone. campaign is the instrument's open Campaign, and is nil unless a
	// recorded fill brought one into being.
	pendingProposal *pendingProposalState
	campaign        *campaignState
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

// Apply implements replay.Handler. It recognises exactly three input event
// types:
//
//   - event.ConfigurationEventType: recorded, and required before any bar.
//   - event.CompletedBarEventType: updates that instrument's True Range/N
//     and emits one event.SetupEvaluatedEventType decision.
//   - event.FillEventType: the only input that may change position state
//     (#11; see campaign.go).
//
// Any other event type fails closed rather than being silently ignored
// (docs/development.md principle 4: "Fail closed on unknown schemas").
func (r *Reducer) Apply(_ context.Context, envelope event.Envelope) ([]event.Envelope, error) {
	switch envelope.Type {
	case event.ConfigurationEventType:
		return r.applyConfiguration(envelope)
	case event.CompletedBarEventType:
		return r.applyCompletedBar(envelope)
	case event.FillEventType:
		return r.applyFill(envelope)
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
	sizingMode, err := sizingModeFor(payload.SizingMode)
	if err != nil {
		return nil, err
	}
	r.entryChannelLength = payload.EntryChannelLength
	r.tierBDistanceInN = payload.TierBDistanceInN
	r.configuredSizingMode = payload.SizingMode
	r.sizingMode = sizingMode
	r.unitVolatilityFrac = payload.UnitVolatilityFraction
	r.stopMultiple = payload.StopMultiple
	r.riskAtStopFraction = payload.RiskAtStopFraction
	r.dollarsPerPoint = payload.DollarsPerPoint
	// ADR 0007's Notional Account, at its configured starting value. The
	// Drawdown Steps that reduce it, and the yearly re-basing that restores
	// it, are #16/#17: nothing in this reducer changes this figure yet, so a
	// proposal today is always sized against the configured equity.
	r.notionalAccount = payload.NotionalAccount.StartingEquity
	r.configured = true
	return nil, nil
}

// sizingModeFor maps the configuration contract's Sizing Mode onto
// internal/sizing's own.
//
// internal/sizing declares its own Mode rather than importing internal/event,
// so that the arithmetic stays as free of the wire contract as
// internal/indicator is. That leaves exactly one place where the two
// enumerations meet — here — and it fails closed rather than defaulting, so a
// mode this build does not know about can never be silently sized as if it
// were the Baseline. The two enumerations' string values are pinned equal by
// TestSizingModeConstantsMatchTheEventContract.
//
// ConfigurationPayload.Validate has already rejected any unrecognised mode by
// the time this is reached, so the default branch is unreachable in practice;
// it exists so that adding a third mode to the contract without extending
// this mapping fails closed instead of falling through.
func sizingModeFor(mode event.SizingMode) (sizing.Mode, error) {
	switch mode {
	case event.SizingModeVolatilityNormalised:
		return sizing.ModeVolatilityNormalised, nil
	case event.SizingModeFixedRiskAtStop:
		return sizing.ModeFixedRiskAtStop, nil
	default:
		return "", fmt.Errorf("strategy: sizing mode %q is not a recognised sizing mode; failing closed rather than sizing a position under an unknown principle (ADR 0003)", mode)
	}
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

	// --- Evaluate. Every input to this bar's decision is read here, from the
	// state left by the bars BEFORE it. Nothing this bar contributes is
	// folded in until the "advance" block below.
	//
	// This is the fix for the prototype's headline look-ahead bug (see
	// indicator.EntryChannel's doc comment), applied uniformly rather than
	// only to the channel — a PR #64 review finding. The rule is
	// CONTEXT.md's, under "Completed bar": the decision bar is never an input
	// to its own decision. For N specifically it is also what ADR 0005
	// requires: the entry is a resting order that fills *inside* the breakout
	// bar, so the Unit size and the Protective Stop have to be computable
	// before that bar opens. A bar that updated N before being decided would
	// shrink its own Unit and tighten its own stop by having been volatile —
	// using information that did not exist when the order was placed.
	//
	// NReady means "N is a usable volatility reading", not merely "bar-count
	// warm-up complete": twenty flat bars (high==low==close) legitimately
	// warm up indicator.WilderAverage with N==0, which a volatility-normalised
	// sizing step could not safely divide by. Such an instrument is reported
	// not ready rather than erroring the run, and becomes ready again the
	// moment True Range is non-zero (see indicator.WilderAverage's doc
	// comment). decisionN is 0 whenever nReady is false, since
	// WilderAverage.Value is 0 until warm-up completes and a not-ready N past
	// warm-up is not-ready precisely because it is 0.
	decisionN := state.n.Value()
	nReady := state.n.Ready() && decisionN > 0

	// #11: the period end of the bar BEFORE this one — the moment this bar
	// opened — captured here because the advance block below overwrites it. It
	// is the earliest instant at which an order proposed on this bar could
	// have executed; see Reducer.applyFill for the window it bounds.
	previousPeriodEnd := state.lastPeriodEnd

	entryChannelHigh, entryChannelReady := state.entryChannel.Extreme()
	// The Turtle Rules p.19: a Breakout "exceeds" the channel, so the
	// comparison is strict; a tie is not a breakout.
	breakout := entryChannelReady && view.High > entryChannelHigh

	// --- Advance. Every read that decides this bar has now happened, so the
	// bar can be folded into the running state for the NEXT bar to see. The
	// per-instrument tracking itself is unchanged: this bar's True Range and
	// high still enter N and the Entry Channel, just after the decision
	// rather than before it.
	state.n.Add(tr)
	state.entryChannel.Add(view.High)
	state.previousClose = view.Close
	state.hasPreviousClose = true
	state.lastPeriodEnd = bar.PeriodEnd

	// --- #11: the previous bar's outstanding business, resolved so that it is
	// EMITTED before any decision this bar produces — the ordering ADR 0010
	// applies within a day, exits before entries. It sits below the advance
	// block rather than above the evaluate block only so that the
	// evaluate/advance pair stays contiguous; it reads and writes none of that
	// state. A trade proposal that no fill arrived for expires with its bar,
	// per ADR 0011; see Reducer.expireProposal for why the expiry is emitted
	// rather than dropped.
	var emissions []event.Envelope
	if state.pendingProposal != nil {
		expired, err := r.expireProposal(state, bar, envelope)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, expired)
	}

	// --- #11: no new entry while a Campaign is open in this instrument. N and
	// the Entry Channel above are still tracked (#12's stop evaluation and
	// #14's Exit Channel need them), but nothing further is emitted: CONTEXT.md
	// defines a Setup as an Eligible instrument NOT in a Campaign, so emitting
	// a Setup-evaluated event here would journal a claim that is false by the
	// project's own vocabulary, and would report a Tier for something that
	// cannot become a Signal. The Campaign's own per-bar decision events are
	// #12 onward.
	if state.campaign != nil {
		return emissions, nil
	}

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
		distanceToEntryInN = (entryChannelHigh - view.High) / decisionN
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
		N:                  decisionN,
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

	// One Setup-evaluated event exists per (instrument, completed bar), so
	// that pair identifies it uniquely — see stamp for why the ID is built
	// this way rather than generated.
	decision := r.stamp(
		decisionID("setup-evaluated", bar.InstrumentID, bar.PeriodEnd),
		event.SetupEvaluatedEventType, event.SetupEvaluatedSchemaVersion,
		bar.PeriodEnd, envelope, payloadBytes,
	)
	emissions = append(emissions, decision)

	// #9: Tier A is a Signal. No Signal while N or the channel is not ready
	// (tier is TierA only when ready is true, above), and never on a tie
	// (a tie is TierB, not TierA — breakout, and therefore tier==TierA,
	// requires a strict >). Emitted after the Setup-evaluated event, per
	// the ticket's ordering.
	if tier == event.TierA {
		signalPayload := event.SignalPayload{
			InstrumentID: bar.InstrumentID,
			PeriodEnd:    bar.PeriodEnd,
			// Rule names the mechanism (a breakout above the preceding
			// EntryChannelLength bars' high), not one of Faith's system
			// labels: System 1 is a 20-day channel and System 2 a 55-day
			// one (ADR 0002), so a name tied to "System 2" would misdescribe
			// a Variant configured with EntryChannelLength 20 — that is
			// System 1's channel, not System 2's. ADR names the rule's
			// defining ADR (0002 defines the breakout rule and selects 55
			// for the Baseline), not "the Baseline" or "the Variant
			// currently running": whether this run IS the Baseline or a
			// declared Variant is what the envelope's ConfigurationHash and
			// StrategyVersion establish (ADR 0012), never this field. The
			// length this Signal was actually computed with, not a
			// hard-coded 55, is EntryChannelLength below.
			Rule:               event.RuleEntryChannelBreakout,
			ADR:                event.ADREntryChannelBreakout,
			Direction:          event.DirectionLong,
			EntryChannelLength: r.entryChannelLength,
			EntryChannelHigh:   entryChannelHigh,
			BreakoutHigh:       view.High,
			N:                  decisionN,
		}
		if err := signalPayload.Validate(); err != nil {
			return nil, fmt.Errorf("strategy: built invalid signal payload: %w", err)
		}
		signalBytes, err := json.Marshal(signalPayload)
		if err != nil {
			return nil, fmt.Errorf("strategy: marshal signal payload: %w", err)
		}
		signalID := decisionID("signal", bar.InstrumentID, bar.PeriodEnd)
		signal := r.stamp(signalID, event.SignalEventType, event.SignalSchemaVersion, bar.PeriodEnd, envelope, signalBytes)
		emissions = append(emissions, signal)

		// #10: a Signal is sized into a trade proposal, emitted third and
		// last of the bar. Exactly one emission always follows the Signal —
		// a proposal, or a decline saying why there is none — so a Signal is
		// never left with nothing after it (see sizeUnit).
		sized, err := r.sizeUnit(bar, envelope, signalID, view.High, decisionN, nReady)
		if err != nil {
			return nil, err
		}
		emissions = append(emissions, sized)

		// #11: a proposal is remembered as outstanding so that a fill can be
		// checked against it — and NOTHING about position state moves here.
		// See Reducer.rememberPendingProposal.
		if err := r.rememberPendingProposal(state, sized, previousPeriodEnd); err != nil {
			return nil, err
		}
	}

	return emissions, nil
}

// sizeUnit turns a Signal into either a trade proposal or a recorded decline,
// and returns exactly one envelope either way (#10).
//
// "Either way" is the point. A Signal that produces no position must leave a
// journal entry saying so, or "no Signal today" and "a Signal whose sizing
// produced nothing" become indistinguishable on replay — and the second is a
// fact about the strategy's capacity that a reviewer needs. So every path
// below that refuses to propose emits a strategy.proposal.declined event with
// an enumerated reason instead; none of them silently returns nothing.
//
// The Notional Account is the configured starting equity: Drawdown Steps and
// yearly re-basing (ADR 0007) are #16/#17. No cap of any kind is checked —
// this is one Unit, and ADR 0008's four caps are #55 and later tickets. The
// entry level is the breakout high, the level the Signal fired at; what fills
// there is #18's decision, and slippage is a fill concern (ADR 0013), so
// nothing is applied to it here.
func (r *Reducer) sizeUnit(bar event.CompletedBarPayload, input event.Envelope, signalID string, entryLevel, n float64, nReady bool) (event.Envelope, error) {
	// Unreachable from this reducer: Tier A requires a ready N, and a
	// Signal is only emitted at Tier A. Guarded anyway — .greptile/rules.md
	// requires a zero, negative or not-yet-warm volatility value to fail
	// closed, and "it cannot happen here" is not a reason to divide by it if
	// the Tier logic above ever changes.
	if !nReady {
		return r.decline(bar, input, signalID, event.DeclineReasonNNotReady,
			fmt.Sprintf("n is not a usable volatility reading (n %v); no unit can be sized from it", n))
	}

	unit, err := sizing.SizeUnit(sizing.Inputs{
		Mode:                   r.sizingMode,
		NotionalAccount:        r.notionalAccount,
		UnitVolatilityFraction: r.unitVolatilityFrac,
		StopMultiple:           r.stopMultiple,
		RiskAtStopFraction:     r.riskAtStopFraction,
		N:                      n,
		DollarsPerPoint:        r.dollarsPerPoint,
	})
	if err != nil {
		// Not a decline: the configuration has already been validated and N
		// is known usable, so an error here means an input this reducer
		// believed sound produced impossible arithmetic. That is a defect,
		// and a defect stops the run rather than being journalled as a
		// routine decline.
		return event.Envelope{}, fmt.Errorf("strategy: instrument %q at %s: %w",
			bar.InstrumentID, bar.PeriodEnd.Format(time.RFC3339), err)
	}

	if unit.Quantity <= 0 {
		// The Turtle Rules p.15 names this outcome directly: small accounts
		// lose diversification because truncation is coarse. It is a fact
		// about the account, not an error.
		return r.decline(bar, input, signalID, event.DeclineReasonQuantityBelowOneUnit,
			fmt.Sprintf("notional account %v under %s sizing, with n %v and dollars per point %v, sizes fewer than one whole unit",
				r.notionalAccount, r.configuredSizingMode, n, r.dollarsPerPoint))
	}

	// The Protective Stop intent, in the expression order
	// event.TradeProposalPayload.Validate re-derives it in, so the two agree
	// bit for bit (CONTEXT.md: "Protective Stop"; The Turtle Rules p.22's 2N
	// stop in the Baseline).
	protectiveStopIntent := entryLevel - r.stopMultiple*n
	if protectiveStopIntent <= 0 {
		// A long equity cannot trade below zero, so this stop is
		// unreachable: the Unit would in fact risk the whole position rather
		// than the derived fraction. Declining is the fail-closed answer, and
		// journalling it is how the condition becomes visible instead of
		// looking like a bar that simply did not signal.
		return r.decline(bar, input, signalID, event.DeclineReasonStopIntentNotPositive,
			fmt.Sprintf("protective stop intent %v (entry level %v - stop multiple %v x n %v) is not a reachable price for a long position",
				protectiveStopIntent, entryLevel, r.stopMultiple, n))
	}

	proposal := event.TradeProposalPayload{
		InstrumentID: bar.InstrumentID,
		PeriodEnd:    bar.PeriodEnd,
		SignalID:     signalID,
		// Rule names what the sizing computes, per Sizing Mode, never the
		// parameter values it ran with — the same reasoning #9 applied to
		// the Signal's rule name. ADR 0003 is the defining decision for both
		// modes: it is what declares that there are two and that choosing
		// between them is a declared experiment.
		Rule:                   sizingRuleFor(r.configuredSizingMode),
		ADR:                    event.ADRUnitSizing,
		Direction:              event.DirectionLong,
		EntryLevel:             entryLevel,
		Quantity:               unit.Quantity,
		N:                      n,
		SizingMode:             r.configuredSizingMode,
		UnitVolatilityFraction: r.unitVolatilityFrac,
		StopMultiple:           r.stopMultiple,
		RiskAtStop:             unit.RiskAtStop,
		RealisedRiskAtStop:     unit.RealisedRiskAtStop,
		DollarsPerPoint:        r.dollarsPerPoint,
		NotionalAccount:        r.notionalAccount,
		ProtectiveStopIntent:   protectiveStopIntent,
	}
	if err := proposal.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: built invalid trade proposal payload: %w", err)
	}
	proposalBytes, err := json.Marshal(proposal)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: marshal trade proposal payload: %w", err)
	}
	return r.stamp(
		decisionID("proposal", bar.InstrumentID, bar.PeriodEnd),
		event.TradeProposalEventType, event.TradeProposalSchemaVersion,
		bar.PeriodEnd, input, proposalBytes,
	), nil
}

// decline builds the strategy.proposal.declined emission for one Signal that
// produced no position.
func (r *Reducer) decline(bar event.CompletedBarPayload, input event.Envelope, signalID, reason, detail string) (event.Envelope, error) {
	payload := event.ProposalDeclinedPayload{
		InstrumentID: bar.InstrumentID,
		PeriodEnd:    bar.PeriodEnd,
		SignalID:     signalID,
		Reason:       reason,
		Detail:       detail,
	}
	if err := payload.Validate(); err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: built invalid proposal declined payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("strategy: marshal proposal declined payload: %w", err)
	}
	return r.stamp(
		decisionID("proposal-declined", bar.InstrumentID, bar.PeriodEnd),
		event.ProposalDeclinedEventType, event.ProposalDeclinedSchemaVersion,
		bar.PeriodEnd, input, payloadBytes,
	), nil
}

// sizingRuleFor names the rule a proposal cites, per Sizing Mode.
//
// The default is deliberately not a fallback to the Baseline's name: a
// proposal is only ever built after sizing.SizeUnit has accepted the mode, so
// reaching it would mean a mode this build cannot size produced a position.
// An empty rule fails event.TradeProposalPayload.Validate, so the run stops
// rather than journalling a proposal that names the wrong principle.
func sizingRuleFor(mode event.SizingMode) string {
	switch mode {
	case event.SizingModeVolatilityNormalised:
		return event.RuleUnitSizingVolatilityNormalised
	case event.SizingModeFixedRiskAtStop:
		return event.RuleUnitSizingFixedRiskAtStop
	default:
		return ""
	}
}

// decisionID builds an emitted envelope's ID from the kind of decision, the
// instrument, and the completed bar it belongs to.
//
// Exactly one decision of each kind exists per (instrument, completed bar),
// so that triple identifies it uniquely — which makes the ID deterministic
// and reproducible on replay without any randomness or wall-clock read (both
// forbidden in internal/; see .golangci.yml's forbidigo rules). The timestamp
// layout is fixed and includes nanoseconds so two bars can never collapse to
// the same ID through formatting.
func decisionID(kind, instrumentID string, periodEnd time.Time) string {
	return fmt.Sprintf("%s:%s:%s", kind, instrumentID, periodEnd.UTC().Format("2006-01-02T15:04:05.000000000Z"))
}

// stamp fills in the envelope fields every emission from this reducer shares.
//
// EventTime is the completed bar's period end and RecordedAt is the input
// envelope's RecordedAt — never time.Now(), which .golangci.yml forbids in
// internal/ precisely because a decision must depend only on the ordered
// event stream. Sequence, CausationID and CorrelationID are deliberately left
// unset: replay.Engine assigns them, overwriting whatever a handler sets, so
// a handler cannot claim causation it did not have (docs/architecture.md).
func (r *Reducer) stamp(id, eventType string, schemaVersion uint32, periodEnd time.Time, input event.Envelope, payload json.RawMessage) event.Envelope {
	return event.Envelope{
		ID:                id,
		Type:              eventType,
		SchemaVersion:     schemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         periodEnd,
		RecordedAt:        input.RecordedAt,
		Source:            sourceReducer,
		StrategyVersion:   r.strategyVersion,
		ConfigurationHash: r.configurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
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
