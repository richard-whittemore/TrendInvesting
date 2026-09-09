package event

import (
	"errors"
	"fmt"
	"time"
)

// SignalEventType identifies the Signal decision payload for the Envelope's
// Type field. CONTEXT.md: "Signal" — the event of a Setup reaching Tier A on
// a completed bar, the strategy recognising that its entry condition is
// met. A Signal is detection only: no position is taken here (#9); sizing
// and order proposal are out of scope.
const SignalEventType = "strategy.signal"

// SignalSchemaVersion is the current schema version of SignalPayload, for
// the Envelope's SchemaVersion field.
const SignalSchemaVersion uint32 = 1

// RuleSystem2Entry names System 2's Entry Channel breakout rule for
// SignalPayload.Rule (The Turtle Rules p.19; ADR 0002). The channel length
// is not baked into this name — a Variant may configure a different
// EntryChannelLength than the Baseline's 55, and a rule name that hard-coded
// "55" would misname the rule that actually produced a Variant's Signal.
// SignalPayload.EntryChannelLength carries the length actually used.
const RuleSystem2Entry = "system2.entry"

// ADRSystem2Baseline is the ADR SignalPayload.ADR cites for a Baseline
// Signal: ADR 0002, which selects System 2 as the Baseline.
const ADRSystem2Baseline = "0002"

// DirectionLong is the only Direction value SignalPayload accepts today.
// The Baseline is long-only (CONTEXT.md: "Universe" describes a stock-first
// platform); a short Direction is not implemented and must never be
// emitted.
const DirectionLong = "long"

// SignalPayload carries a Signal: the strategy recognising that an
// instrument's entry condition was met on one completed bar (CONTEXT.md:
// "Signal", "Breakout"). It names the rule and ADR that produced it so an
// auditor never has to infer which strategy fired.
//
// A Signal belongs to exactly one bar and is never carried forward (ADR
// 0011): the reducer emits at most one Signal per instrument per completed
// bar, and the next bar is evaluated afresh against the updated Entry
// Channel — a genuinely trending instrument re-qualifies on its own by
// making a new high; nothing here remembers a prior Signal.
type SignalPayload struct {
	InstrumentID string    `json:"instrument_id"`
	PeriodEnd    time.Time `json:"period_end"`
	// Rule and ADR name the strategy rule this Signal came from, so a
	// journal reader never has to infer it (docs/development.md principle
	// 3: cite the source of every strategy rule).
	Rule      string `json:"rule"`
	ADR       string `json:"adr"`
	Direction string `json:"direction"`
	// EntryChannelLength is the Entry Channel length (in completed bars)
	// this Signal was actually computed with — 55 in the Baseline, but a
	// Variant may configure a different value (ConfigurationPayload,
	// EntryChannelLength). Carried as its own field, not folded into Rule
	// as a suffix: a Signal is an audit record, and naming the wrong length
	// in it is a defect this field exists to prevent regardless of Rule's
	// text.
	EntryChannelLength int `json:"entry_channel_length"`
	// EntryChannelHigh is the Entry Channel high the breakout exceeded —
	// computed from the preceding bars only, excluding this one (ADR 0002;
	// see indicator.EntryChannel's doc comment). BreakoutHigh is this bar's
	// own (split-adjusted, ADR 0004) high, which strictly exceeds it.
	EntryChannelHigh float64 `json:"entry_channel_high"`
	BreakoutHigh     float64 `json:"breakout_high"`
	// N is the volatility reading (CONTEXT.md: "N") in effect on the
	// breakout bar, retained so a consumer sizing a Campaign from this
	// Signal does not have to look it up separately.
	N float64 `json:"n"`
}

// Validate checks that the payload identifies an instrument, period, rule,
// and ADR, that Direction is the one recognised value, that
// EntryChannelLength is a positive integer, that both price fields are
// finite and positive with BreakoutHigh strictly exceeding EntryChannelHigh
// (The Turtle Rules p.19: a Breakout "exceeds" the channel, so a Signal
// whose own fields contradict that is invalid by construction), and that N
// is finite and positive.
func (p SignalPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.PeriodEnd.IsZero() {
		errs = append(errs, errors.New("period end is required"))
	}
	if p.Rule == "" {
		errs = append(errs, errors.New("rule is required"))
	}
	if p.ADR == "" {
		errs = append(errs, errors.New("adr is required"))
	}
	switch p.Direction {
	case DirectionLong:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("direction %q is not a recognised direction", p.Direction))
	}
	if p.EntryChannelLength <= 0 {
		errs = append(errs, errors.New("entry channel length must be a positive integer"))
	}

	entryChannelHighFinite := isFinite(p.EntryChannelHigh)
	switch {
	case !entryChannelHighFinite:
		errs = append(errs, errors.New("entry channel high must be finite"))
	case p.EntryChannelHigh <= 0:
		errs = append(errs, errors.New("entry channel high must be positive"))
	}

	breakoutHighFinite := isFinite(p.BreakoutHigh)
	switch {
	case !breakoutHighFinite:
		errs = append(errs, errors.New("breakout high must be finite"))
	case p.BreakoutHigh <= 0:
		errs = append(errs, errors.New("breakout high must be positive"))
	}

	if entryChannelHighFinite && breakoutHighFinite && p.BreakoutHigh <= p.EntryChannelHigh {
		errs = append(errs, errors.New("breakout high must exceed the entry channel high"))
	}

	switch {
	case !isFinite(p.N):
		errs = append(errs, errors.New("n must be finite"))
	case p.N <= 0:
		errs = append(errs, errors.New("n must be positive"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid signal payload: %w", err)
	}
	return nil
}
