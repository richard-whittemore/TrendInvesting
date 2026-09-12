package event

import (
	"errors"
	"fmt"
	"time"
)

// SetupEvaluatedEventType identifies the Setup-evaluated decision payload
// for the Envelope's Type field.
const SetupEvaluatedEventType = "strategy.setup.evaluated"

// SetupEvaluatedSchemaVersion is the current schema version of
// SetupEvaluatedPayload, for the Envelope's SchemaVersion field.
//
// Version 2 added EntryChannelHigh, EntryChannelReady, Tier, and
// DistanceToEntryInN. A schema change is explicit in this project
// (docs/development.md), never a silent field addition.
const SetupEvaluatedSchemaVersion uint32 = 2

// The three Tier values (CONTEXT.md: "Tier"). TierNone means neither: the
// Setup is not currently approaching or meeting its entry condition, or the
// inputs needed to judge that (N, the Entry Channel) are not yet ready.
const (
	TierA    = "A"
	TierB    = "B"
	TierNone = ""
)

// SetupEvaluatedPayload carries the outcome of evaluating one instrument's
// Setup (CONTEXT.md: "Setup") on one completed bar: the N in force for
// deciding this bar (CONTEXT.md: "N") and whether it is a usable volatility
// reading, the current Entry Channel high (CONTEXT.md: "Entry Channel"; The
// Turtle Rules p.19, ADR 0002) and whether it is warmed up, the Setup's
// Tier, and its distance to the Entry Channel in N.
//
// N here is "the N in force for deciding this bar": the Wilder average of
// the True Ranges of the completed bars PRECEDING it, never including this
// bar's own. That matches the Entry Channel beside it and CONTEXT.md's
// "Completed bar" rule — the decision bar is never an input to its own
// decision — so the bar that completes N's twenty-bar warm-up is not itself
// decided against the seed; the bar after it is the first that can be.
//
// NReady means "N is a usable volatility reading", which is stronger than
// "bar-count warm-up (CONTEXT.md: 'Completed bar') is complete": twenty flat
// bars (high==low==close) legitimately complete warm-up with N==0, and a
// volatility-normalised sizing step could not safely divide by that. NReady
// is therefore false in both cases — warm-up incomplete, or warm-up complete
// but N==0 — and Validate rejects the contradictory combination NReady==true
// with N==0 outright, so it can never reach a sizing calculation.
//
// EntryChannelReady means at least EntryChannelLength completed bars
// (configured, 55 in the Baseline) have been added to the Entry Channel
// window. EntryChannelHigh is the channel's high computed from the bars
// BEFORE this one — see indicator.EntryChannel's doc comment for why that
// exclusion matters (the prototype's headline look-ahead bug) — and is 0
// while not ready, the convention N already uses.
//
// Tier and DistanceToEntryInN are only meaningful when both NReady and
// EntryChannelReady are true; Validate enforces that DistanceToEntryInN is
// exactly 0 and Tier is TierNone whenever either input is not ready, so a
// consumer reading either field without checking readiness first sees an
// unambiguous "not evaluable" value rather than a stale or fabricated one.
// When both are ready: DistanceToEntryInN is (EntryChannelHigh-High)/N —
// positive while price sits below the channel, negative once price has
// broken out above it, and exactly 0 on a tie (high exactly equal to the
// channel high). The three cases: DistanceToEntryInN < 0 is TierA (The
// Turtle Rules p.19's "exceeds" — a strict breakout); 0 <=
// DistanceToEntryInN <= ConfigurationPayload.TierBDistanceInN is TierB
// (since TierBDistanceInN is always non-negative, a tie is always at least
// TierB — ADR 0011's Watchlist exists to surface exactly that closest
// possible approach); otherwise TierNone.
//
// This payload is deliberately not a Signal (CONTEXT.md: "Signal" is a Tier
// A event, SignalPayload): this event evaluates and reports the Setup's
// state whether or not it reaches Tier A, so a Setup's approach to its entry
// condition is observable even when no Signal fires.
type SetupEvaluatedPayload struct {
	InstrumentID       string    `json:"instrument_id"`
	PeriodEnd          time.Time `json:"period_end"`
	N                  float64   `json:"n"`
	NReady             bool      `json:"n_ready"`
	EntryChannelHigh   float64   `json:"entry_channel_high"`
	EntryChannelReady  bool      `json:"entry_channel_ready"`
	Tier               string    `json:"tier"`
	DistanceToEntryInN float64   `json:"distance_to_entry_in_n"`
}

// Validate checks that the payload identifies an instrument and period,
// that N and EntryChannelHigh are finite numbers never combined with their
// own readiness flag while zero [N] or non-positive [EntryChannelHigh] (nor
// nonzero while not ready, for EntryChannelHigh), that Tier is one of the
// three declared values, and that Tier and DistanceToEntryInN are
// internally consistent with each other and with the two readiness flags —
// see the type's doc comment for what each combination means.
//
// True Range, and therefore its Wilder average, is never negative (bar.go's
// PriceView.validate already enforces high >= low), so a negative N always
// indicates a defect upstream rather than a legitimate reading. NReady==true
// with N==0 is rejected for the reason on the type's doc comment: N is not a
// usable volatility reading at zero, so a producer must never claim it is
// ready — the reducer that builds this payload is expected to report
// NReady==false for a flat instrument instead of hitting this check, but
// Validate enforces the invariant regardless of which producer built the
// payload.
func (p SetupEvaluatedPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if p.PeriodEnd.IsZero() {
		errs = append(errs, errors.New("period end is required"))
	}
	switch {
	case !isFinite(p.N):
		errs = append(errs, errors.New("n must be finite"))
	case p.N < 0:
		errs = append(errs, errors.New("n must not be negative"))
	case p.NReady && p.N == 0:
		errs = append(errs, errors.New("n must be positive when ready"))
	}

	switch {
	case !isFinite(p.EntryChannelHigh):
		errs = append(errs, errors.New("entry channel high must be finite"))
	case p.EntryChannelHigh < 0:
		errs = append(errs, errors.New("entry channel high must not be negative"))
	case p.EntryChannelReady && p.EntryChannelHigh <= 0:
		errs = append(errs, errors.New("entry channel high must be positive when ready"))
	case !p.EntryChannelReady && p.EntryChannelHigh != 0:
		errs = append(errs, errors.New("entry channel high must be zero while not ready"))
	}

	switch p.Tier {
	case TierA, TierB, TierNone:
		// recognised
	default:
		errs = append(errs, fmt.Errorf("tier %q is not a recognised tier", p.Tier))
	}

	ready := p.NReady && p.EntryChannelReady
	distanceFinite := isFinite(p.DistanceToEntryInN)
	switch {
	case !distanceFinite:
		errs = append(errs, errors.New("distance to entry in n must be finite"))
	case !ready && p.DistanceToEntryInN != 0:
		errs = append(errs, errors.New("distance to entry in n must be zero while n or the entry channel is not ready"))
	}
	if !ready && p.Tier != TierNone {
		errs = append(errs, errors.New("tier must be empty while n or the entry channel is not ready"))
	}
	if ready && distanceFinite {
		switch {
		case p.Tier == TierA && !(p.DistanceToEntryInN < 0):
			errs = append(errs, errors.New("tier a requires a negative distance to entry in n (a breakout strictly exceeds the entry channel)"))
		case p.Tier == TierB && !(p.DistanceToEntryInN >= 0):
			errs = append(errs, errors.New("tier b requires a non-negative distance to entry in n"))
		case p.Tier == TierNone && p.DistanceToEntryInN < 0:
			errs = append(errs, errors.New("a negative distance to entry in n implies a breakout and must be tier a"))
		case p.Tier == TierNone && p.DistanceToEntryInN == 0:
			errs = append(errs, errors.New("a zero distance to entry in n (a tie) implies at least tier b, not tier none"))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid setup evaluated payload: %w", err)
	}
	return nil
}
