package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds #16's Notional Account state machine (ADR 0007) and the
// Reducer method that drives it from account.snapshot events. It is placed
// in internal/strategy rather than internal/indicator: an indicator measures
// a price series (True Range, N — see internal/indicator's package
// comment), while a Notional Account tracks capital that a drawdown reduces,
// which is a different kind of fact with a different failure mode — the
// same reasoning #10 used to split internal/sizing out from
// internal/indicator. NotionalAccount itself stays pure (no event, replay,
// or clock dependency; see its own doc comment) despite living alongside the
// reducer that drives it, so it is independently testable exactly like an
// indicator or a sizing function.
//
// Kept in its own file, touching reducer.go only for the Apply switch case,
// the notionalAccount field, and sizeUnit reading Current(): a second agent
// works #11 in the same package in parallel, and both tickets should be able
// to land with a minimal, low-conflict reducer.go diff.

// notionalAccountDrawdownFraction is the fraction the Notional Account is
// multiplied by at each Drawdown Step: a 20% reduction (CONTEXT.md:
// "Drawdown Step"; The Turtle Rules p.17, ADR 0007).
const notionalAccountDrawdownFraction = 0.8

// notionalAccountDrawdownThresholdFraction is the fraction of the CURRENT
// Notional Account that actual equity must fall below the measurement base
// to trigger a Drawdown Step: a 10% fall (The Turtle Rules p.17, ADR 0007).
const notionalAccountDrawdownThresholdFraction = 0.10

// Step is one Drawdown Step NotionalAccount.Observe applied: the Notional
// Account fell From its prior value To 80% of it, because the observed
// Equity crossed Threshold.
type Step struct {
	From      float64
	To        float64
	Threshold float64
	Equity    float64
}

// NotionalAccount implements ADR 0007's Drawdown Step ladder: the equity
// figure position sizing (internal/sizing.SizeUnit, via Reducer.sizeUnit) is
// measured against, which is reduced during a drawdown and is therefore not
// the same as actual account equity (CONTEXT.md: "Notional Account").
//
// It tracks two figures, both initialised to the configured starting equity
// (ADR 0007; this ticket, #16, does not implement the yearly re-basing or
// recovery that would later move them — that is #17):
//
//   - current (A): the Notional Account itself, returned by Current().
//   - base (B): "the figure it was last measured from" (The Turtle Rules
//     p.17) — the equity level a further 10% fall is measured against.
//     Returned by MeasurementBase(). It only ever moves when a Drawdown Step
//     fires, to the threshold just crossed — NEVER upward on a rise in
//     equity. That is the entire difference between this rule and a
//     high-water-mark reset (ADR 0007 explicitly declares the
//     high-water-mark reset a Variant, not the Baseline; CONTEXT.md's
//     "Drawdown Step" entry).
//
// Faith's ladder (The Turtle Rules p.17), transcribed exactly: starting at
// $1,000,000, equity at or below $900,000 (base − 10% of the $1,000,000
// account) steps the account to $800,000 and moves the base to $900,000;
// equity at or below $820,000 (the new $900,000 base, less 10% of the
// now-$800,000 account) steps it again, to $640,000 — the source's own
// "further −$80,000" [T p.17]. A third step, not printed in the source but
// derived by the same rule, moves the base to $820,000 less 10% of $640,000
// ($756,000) and steps the account to $512,000; see notional_test.go's
// TestNotionalAccountFaithsLadder.
//
// Deterministic and side-effect free: Observe takes no clock and no
// randomness, and NotionalAccount has no dependency on internal/event or
// internal/replay, so replaying the same sequence of equity readings through
// a fresh NotionalAccount always produces the same steps.
type NotionalAccount struct {
	base    float64
	current float64
}

// New returns a NotionalAccount starting at starting, which becomes both the
// initial Notional Account and the initial measurement base — ADR 0007: the
// Notional Account equals the configured starting equity before any
// Drawdown Step has been applied. starting must be finite and positive.
func New(starting float64) (*NotionalAccount, error) {
	if err := checkEquity("starting equity", starting); err != nil {
		return nil, fmt.Errorf("strategy: cannot start a notional account: %w", err)
	}
	return &NotionalAccount{base: starting, current: starting}, nil
}

// Current returns the Notional Account's current value: what
// internal/sizing.SizeUnit must be measured against (CONTEXT.md: "Notional
// Account").
func (n *NotionalAccount) Current() float64 {
	return n.current
}

// MeasurementBase returns the figure the next Drawdown Step's threshold is
// measured from (The Turtle Rules p.17: "the figure it was last measured
// from"). It changes only when Observe applies a step, to the threshold
// that step crossed; a rise in equity never moves it.
func (n *NotionalAccount) MeasurementBase() float64 {
	return n.base
}

// Observe reports the effect of one account snapshot's equity reading on the
// Notional Account: it applies zero or more Drawdown Steps and returns each
// one, in the order applied.
//
// A Drawdown Step applies while equity <= base − 10%×current (a 10% fall,
// measured against the CURRENT Notional Account, below the measurement base
// — The Turtle Rules p.17, ADR 0007): the account is multiplied by 0.8 and
// the base moves to the threshold just crossed. The check then repeats
// against the new base and account, so a single large drop can apply
// several steps from one observation — each returned separately, in the
// order they were applied, and each carrying the SAME observed equity.
//
// The boundary is inclusive: The Turtle Rules p.17 says equity "down 10%"
// steps the account, so equity exactly at the threshold triggers, and
// equity one cent above it does not (see
// TestNotionalAccountExactBoundaryTriggersAndOneCentAboveDoesNot).
//
// A rise in equity never moves the base upward and never restores the
// account: this is a measurement-base rule, not a high-water mark (ADR
// 0007's declared Variant; see TestHighWaterMarkResetDivergesFromNotionalAccount).
// Recovery, which requires the yearly starting figure to be regained, is
// #17.
//
// equity must be finite and positive: .greptile/rules.md's fail-closed rule
// applies to every account-affecting figure, not only volatility readings,
// and a raw comparison against a non-finite value would otherwise silently
// apply zero steps rather than reject the reading.
func (n *NotionalAccount) Observe(equity float64) ([]Step, error) {
	if err := checkEquity("equity", equity); err != nil {
		return nil, fmt.Errorf("strategy: cannot observe an account snapshot: %w", err)
	}

	var steps []Step
	for {
		threshold := n.base - notionalAccountDrawdownThresholdFraction*n.current
		if equity > threshold {
			break
		}
		from := n.current
		to := notionalAccountDrawdownFraction * n.current
		steps = append(steps, Step{From: from, To: to, Threshold: threshold, Equity: equity})
		n.current = to
		n.base = threshold
	}
	return steps, nil
}

// checkEquity rejects a non-finite or non-positive figure, named for the
// error message. Shared by New (starting equity) and Observe (an observed
// equity reading): both are account-affecting figures that must fail closed
// under .greptile/rules.md, the same way internal/sizing's checkN and
// checkFraction do for volatility and equity-fraction inputs.
func checkEquity(name string, v float64) error {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return fmt.Errorf("%s must be finite", name)
	case v <= 0:
		return fmt.Errorf("%s must be positive", name)
	}
	return nil
}

// applyAccountSnapshot handles event.AccountSnapshotEventType: ADR 0007's
// Drawdown Step ladder is driven from here rather than from reducer.go
// (see this file's top-of-file comment on why).
//
// Like applyConfiguration and applyCompletedBar, it rejects a schema version
// other than event.AccountSnapshotSchemaVersion before decoding (ADR 0015),
// and requires a configuration event first: a snapshot has nowhere to apply
// its steps to before the Notional Account exists.
func (r *Reducer) applyAccountSnapshot(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received an account snapshot before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.AccountSnapshotSchemaVersion {
		return nil, fmt.Errorf("strategy: account snapshot payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.AccountSnapshotSchemaVersion)
	}

	var snapshot event.AccountSnapshotPayload
	if err := json.Unmarshal(envelope.Payload, &snapshot); err != nil {
		return nil, fmt.Errorf("strategy: decode account snapshot payload: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid account snapshot payload: %w", err)
	}

	// Chronology, mirroring applyCompletedBar's per-instrument rule: a
	// duplicate or out-of-order snapshot would silently re-derive Drawdown
	// Steps from a stale or repeated reading, corrupting the Notional
	// Account for every decision after it. There is one account for the
	// whole run (no per-instrument split, unlike bars), so chronology is
	// tracked globally.
	if r.hasAccountSnapshot && !snapshot.AsOf.After(r.lastAccountSnapshotAt) {
		return nil, fmt.Errorf("strategy: account snapshot as of %s is not strictly after the last recorded snapshot %s; rejecting a duplicate or out-of-order snapshot",
			snapshot.AsOf.Format(time.RFC3339), r.lastAccountSnapshotAt.Format(time.RFC3339))
	}

	steps, err := r.notionalAccount.Observe(snapshot.Equity)
	if err != nil {
		// Unreachable in practice: AccountSnapshotPayload.Validate has
		// already required Equity to be finite and positive, which is
		// everything Observe checks. Guarded anyway, matching this
		// project's fail-closed style (see Reducer.sizeUnit's identical
		// reasoning for sizing.SizeUnit's error path).
		return nil, fmt.Errorf("strategy: %w", err)
	}

	r.lastAccountSnapshotAt = snapshot.AsOf
	r.hasAccountSnapshot = true

	emissions := make([]event.Envelope, 0, len(steps))
	for _, step := range steps {
		r.drawdownStepsSeen++
		payload := event.DrawdownStepAppliedPayload{
			AsOf:           snapshot.AsOf,
			Equity:         step.Equity,
			Threshold:      step.Threshold,
			NotionalBefore: step.From,
			NotionalAfter:  step.To,
			StepNumber:     r.drawdownStepsSeen,
			Rule:           event.RuleNotionalAccountDrawdownStep,
			ADR:            event.ADRNotionalAccountDrawdownStep,
		}
		if err := payload.Validate(); err != nil {
			return nil, fmt.Errorf("strategy: built invalid drawdown step applied payload: %w", err)
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("strategy: marshal drawdown step applied payload: %w", err)
		}
		emissions = append(emissions, r.stamp(
			drawdownStepID(snapshot.AsOf, r.drawdownStepsSeen),
			event.DrawdownStepAppliedEventType, event.DrawdownStepAppliedSchemaVersion,
			snapshot.AsOf, envelope, payloadBytes,
		))
	}
	return emissions, nil
}

// drawdownStepID builds a deterministic, reproducible-on-replay ID for a
// Drawdown-Step-applied emission. Unlike decisionID (reducer.go), which
// identifies an emission by instrument and bar, a Drawdown Step has no
// instrument: the account snapshot's AsOf and this step's 1-based
// StepNumber together identify it uniquely, since a single snapshot can
// apply more than one step (Observe's doc comment).
func drawdownStepID(asOf time.Time, stepNumber int) string {
	return fmt.Sprintf("drawdown-step:%s:%d", asOf.UTC().Format("2006-01-02T15:04:05.000000000Z"), stepNumber)
}
