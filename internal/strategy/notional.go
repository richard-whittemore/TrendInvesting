package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
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

// notionalAccountDrawdownThresholdFraction is the fraction of the CURRENT
// Notional Account that actual equity must fall below the measurement base
// to trigger a Drawdown Step: a 10% fall (The Turtle Rules p.17, ADR 0007).
// The corresponding 20% reduction itself is sizing.DrawdownSteppedNotional,
// shared with event.DrawdownStepAppliedPayload.Validate so producer and
// validator compute the identical figure (see that function's doc comment).
const notionalAccountDrawdownThresholdFraction = 0.10

// notionalAccountUndefinedDrawdownFraction is the fraction of the account,
// below the measurement base, that the Drawdown Step ladder's thresholds
// approach but can never reach.
//
// Each step's threshold falls by notionalAccountDrawdownThresholdFraction
// (0.10) times the CURRENT account, and the account itself shrinks by
// sizing.DrawdownSteppedNotional's 0.8 at every step. Summed over
// infinitely many steps, the total fall from the base is a geometric
// series:
//
//	0.10 x A0 x (1 + 0.8 + 0.8^2 + ...) = 0.10 x A0 / (1 - 0.8) = 0.5 x A0
//
// (A0 being the account standing at the start of the observation), so the
// threshold sequence converges to base - 0.5 x current but never crosses
// it. Equity at or below that point would leave every future threshold
// still above it, so a literal application of the rule would never
// terminate — see the asymptote check at the top of Observe, which fails
// closed instead (Greptile PR #66 finding: "deep drawdowns never
// terminate").
const notionalAccountUndefinedDrawdownFraction = notionalAccountDrawdownThresholdFraction / (1 - sizing.DrawdownStepRetainedFraction)

// maxDrawdownStepsPerObservation bounds the number of Drawdown Steps Observe
// will apply from one account snapshot.
//
// The asymptote check before the loop already makes an unbounded loop
// unreachable from a valid equity reading; this is a belt-and-braces guard
// so a future change to the drawdown fractions (or a defect in the
// asymptote arithmetic) cannot silently reintroduce one. The tightest
// legitimate case — equity one float64 ULP above the asymptote on a
// $1,000,000 account — takes on the order of 165 iterations to resolve
// (empirically confirmed by
// TestNotionalAccountOneCentAboveTheAsymptoteTerminates, which pins the
// exact count for one cent above); 1024 is many times that margin while
// 0.8^1024 has long since underflowed to exactly zero, so a genuine defect
// (a threshold that never advances) is still caught quickly rather than
// consuming unbounded memory appending Steps forever.
const maxDrawdownStepsPerObservation = 1024

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

	// startingFigure is S, ADR 0007's "yearly starting figure": the equity
	// level a full recovery must regain. It begins at the constructor's
	// starting value, moves to actual equity at a re-basing (#17), and
	// scales proportionally on a cash movement (#17) — but NEVER on a mere
	// rise in equity: that is the entire difference between this rule and a
	// high-water-mark reset (see TestHighWaterMarkRecoveryDivergesFromNotionalAccountRecovery).
	startingFigure float64
	// rebasingMonth/rebasingDay are ADR 0007's configured re-basing date
	// (event.NotionalAccountConfig.RebasingMonth/RebasingDay): the
	// month/day, recurring every year, on or after which the first snapshot
	// re-bases the account to actual equity.
	rebasingMonth int
	rebasingDay   int
	// periodLabel identifies which re-basing year's regime currently
	// governs startingFigure: the calendar year of the most recently-passed
	// re-basing anniversary (rebasingLabelFor). hasPeriodLabel is false
	// until the first snapshot is ever observed; see ObserveSnapshot's doc
	// comment for why the first snapshot of a run establishes this WITHOUT
	// re-basing.
	periodLabel    int
	hasPeriodLabel bool
	// stepsInEpisode counts every Drawdown Step applied since the ladder was
	// last reset — by construction, a re-basing, or a full recovery — so
	// that a recovery can report exactly how many steps it cleared
	// (NotionalAccountRecoveredPayload.StepsCleared).
	stepsInEpisode int
}

// NewNotionalAccount returns a NotionalAccount starting at starting, which
// becomes the initial Notional Account, the initial measurement base, AND
// the initial yearly starting figure — ADR 0007: all three coincide before
// any Drawdown Step, re-basing, or cash movement has been applied.
// rebasingMonth/rebasingDay are ADR 0007's configured re-basing date (a
// month and day that recur every year); NotionalAccountConfig.Validate
// already enforces this in the wire contract, and the identical check is
// repeated here (a non-leap reference year, 2027, exactly as
// event.validRebasingDate uses) so the arithmetic seam fails closed on its
// own, independent of whether a caller validated the payload first. starting
// must be finite and positive.
func NewNotionalAccount(starting float64, rebasingMonth, rebasingDay int) (*NotionalAccount, error) {
	if err := checkEquity("starting equity", starting); err != nil {
		return nil, fmt.Errorf("strategy: cannot start a notional account: %w", err)
	}
	if !validRebasingDate(rebasingMonth, rebasingDay) {
		return nil, fmt.Errorf("strategy: cannot start a notional account: rebasing month %d / day %d is not a valid date that recurs every year", rebasingMonth, rebasingDay)
	}
	return &NotionalAccount{
		base:           starting,
		current:        starting,
		startingFigure: starting,
		rebasingMonth:  rebasingMonth,
		rebasingDay:    rebasingDay,
	}, nil
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
// that step crossed, or when ObserveSnapshot re-bases or recovers the
// account (#17); a rise in equity alone never moves it.
func (n *NotionalAccount) MeasurementBase() float64 {
	return n.base
}

// StartingFigure returns S, ADR 0007's yearly starting figure: the equity
// level a full recovery must regain (see startingFigure's own doc comment
// for what moves it, and what deliberately never does).
func (n *NotionalAccount) StartingFigure() float64 {
	return n.startingFigure
}

// validRebasingDate reports whether month/day form a date that recurs
// identically every year. Duplicated, deliberately, from
// event.validRebasingDate: NotionalAccount is documented to have no
// dependency on internal/event, and this is a small enough check that
// importing event to share it would cost more (a dependency the type's own
// doc comment disclaims) than it saves. 2027 is used as the reference year
// for the same reason event.validRebasingDate does: it is not a leap year,
// so a 29 February re-basing date is rejected rather than silently rolled
// over to 1 March by time.Date.
func validRebasingDate(month, day int) bool {
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return false
	}
	t := time.Date(2027, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return int(t.Month()) == month && t.Day() == day
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
//
// Undefined below a 50% drawdown of the figure standing at the start of this
// call: equity at or below that asymptote (notionalAccountUndefinedDrawdownFraction's
// doc comment derives it) is rejected with an error rather than looped over
// forever — ADR 0007 and Faith's source do not address a drawdown this deep,
// so this is a fail-closed design choice, not a transcribed rule (Greptile
// PR #66 finding).
func (n *NotionalAccount) Observe(equity float64) ([]Step, error) {
	if err := checkEquity("equity", equity); err != nil {
		return nil, fmt.Errorf("strategy: cannot observe an account snapshot: %w", err)
	}

	// The Drawdown Step ladder's thresholds approach, but mathematically
	// never reach, this asymptote (see notionalAccountUndefinedDrawdownFraction's
	// doc comment for the geometric series it comes from). Equity at or
	// below it would leave every future threshold still above it, so the
	// loop below would never break: checking once, up front, against the
	// figures standing at the start of this call catches that without
	// applying a single step first.
	limit := n.base - notionalAccountUndefinedDrawdownFraction*n.current
	if equity <= limit {
		return nil, fmt.Errorf(
			"strategy: equity %v is at or below %v, the asymptote of a %.0f%% drawdown from the yearly starting figure (measurement base %v, account %v): the Notional Account rule (ADR 0007) is undefined this deep — Faith's source does not address it — so trading must halt rather than apply an unbounded number of Drawdown Steps",
			equity, limit, notionalAccountUndefinedDrawdownFraction*100, n.base, n.current)
	}

	var steps []Step
	for i := 0; ; i++ {
		if i >= maxDrawdownStepsPerObservation {
			// Unreachable given the asymptote check above and the fractions
			// as declared: kept as a belt-and-braces guard (Greptile PR #66
			// finding) so a future change to either cannot silently
			// reintroduce an unbounded loop that consumes memory forever
			// appending Steps.
			return nil, fmt.Errorf(
				"strategy: applied %d drawdown steps in one account snapshot observation without terminating; this indicates a defect in the drawdown ladder, not a legitimate market condition",
				i)
		}
		threshold := n.base - notionalAccountDrawdownThresholdFraction*n.current
		if equity > threshold {
			break
		}
		from := n.current
		to := sizing.DrawdownSteppedNotional(n.current)
		steps = append(steps, Step{From: from, To: to, Threshold: threshold, Equity: equity})
		n.current = to
		n.base = threshold
	}
	return steps, nil
}

// Rebase is the result of ObserveSnapshot re-basing the account (ADR 0007):
// the yearly starting figure moved from PreviousStartingFigure to
// NewStartingFigure, which always equals Equity exactly.
type Rebase struct {
	PreviousStartingFigure float64
	NewStartingFigure      float64
	Equity                 float64
}

// Recovery is the result of ObserveSnapshot fully restoring the account
// (ADR 0007): equity regained the yearly starting figure, clearing
// StepsCleared Drawdown Steps applied since the ladder was last reset.
type Recovery struct {
	Equity         float64
	StartingFigure float64
	NotionalBefore float64
	StepsCleared   int
}

// ObserveSnapshot processes one account snapshot's equity reading in full,
// per ADR 0007, in this order:
//
//  1. Re-basing. If asOf's re-basing-year label (rebasingLabelFor) has
//     advanced past the account's current one, the account re-bases: the
//     yearly starting figure, the measurement base, and the account itself
//     all become equity, and the step count resets — BEFORE step 2 runs, so
//     a snapshot that both crosses the re-basing date and is itself far
//     enough down to have stepped against the OLD figure cannot do so
//     (see TestNotionalAccountRebasesAcrossAYearBoundary). Re-basing
//     happens at most once per calendar year (a second snapshot later in
//     an already-rebased year finds its label unchanged and does nothing).
//
//     The very FIRST snapshot a NotionalAccount ever observes establishes
//     its re-basing-year label without re-basing, however far past the
//     re-basing date its own AsOf falls: the constructor's starting figure
//     stands in as the yearly starting figure until a LATER snapshot's
//     label advances past this first one. A backtest (or a live account)
//     starting mid-year has no record of actual equity at that year's own
//     re-basing date — only the configured starting figure — and re-basing
//     to the first observed reading would silently manufacture a "yearly
//     starting figure" the run never actually started the year with (see
//     TestNotionalAccountFirstSnapshotAfterRebasingDateDoesNotRebase).
//
//  2. The Drawdown Step ladder (Observe, unchanged from #16), against
//     whatever the measurement base and account are AFTER step 1.
//
//  3. Recovery. If the account is below the yearly starting figure (i.e.
//     one or more Drawdown Steps are outstanding since the last reset) and
//     equity has regained it, the account and measurement base are
//     restored to it and the step count resets. A partial recovery — equity
//     above the last threshold crossed but still below the starting figure
//     — changes nothing (The Turtle Rules p.17; ADR 0007: never a
//     high-water mark, so a rise that does not regain the starting figure
//     is not itself a trigger for anything).
//
// Steps 2 and 3 are mutually exclusive with each other, and step 1 is
// mutually exclusive with both: a re-basing snapshot leaves the account
// exactly at its (new) starting figure, so it can neither cross a threshold
// below that figure nor already sit below it awaiting recovery. That is not
// asserted defensively here; it follows from the arithmetic, and is pinned
// by TestNotionalAccountRebasesAcrossAYearBoundary.
//
// rebase and recovery are nil when this snapshot did not re-base or
// recover; steps is nil (not merely empty) when no Drawdown Step applied.
func (n *NotionalAccount) ObserveSnapshot(asOf time.Time, equity float64) (rebase *Rebase, steps []Step, recovery *Recovery, err error) {
	if err := checkEquity("equity", equity); err != nil {
		return nil, nil, nil, fmt.Errorf("strategy: cannot observe an account snapshot: %w", err)
	}

	label := n.rebasingLabelFor(asOf)
	switch {
	case !n.hasPeriodLabel:
		// The very first snapshot ever: establish the label without
		// re-basing (see this method's doc comment).
		n.periodLabel = label
		n.hasPeriodLabel = true
	case label > n.periodLabel:
		previous := n.startingFigure
		n.startingFigure = equity
		n.base = equity
		n.current = equity
		n.stepsInEpisode = 0
		n.periodLabel = label
		rebase = &Rebase{PreviousStartingFigure: previous, NewStartingFigure: equity, Equity: equity}
	}

	steps, err = n.Observe(equity)
	if err != nil {
		return rebase, nil, nil, err
	}
	n.stepsInEpisode += len(steps)

	if n.current < n.startingFigure && equity >= n.startingFigure {
		before := n.current
		cleared := n.stepsInEpisode
		n.current = n.startingFigure
		n.base = n.startingFigure
		n.stepsInEpisode = 0
		recovery = &Recovery{Equity: equity, StartingFigure: n.startingFigure, NotionalBefore: before, StepsCleared: cleared}
	}

	return rebase, steps, recovery, nil
}

// rebasingLabelFor returns the calendar year of the most recently-passed (or
// current, if asOf falls exactly on it) re-basing anniversary at or before
// asOf: the year of asOf itself if asOf's month/day is on or after the
// configured re-basing date, else the year before. Two snapshots whose AsOf
// values fall within the same re-basing year (between one re-basing date
// and the next) always return the same label, which is what lets
// ObserveSnapshot detect "a NEW re-basing year has begun" as simply "the
// label advanced" without storing the date of the last re-basing itself.
func (n *NotionalAccount) rebasingLabelFor(asOf time.Time) int {
	asOf = asOf.UTC()
	rebasingDateThisYear := time.Date(asOf.Year(), time.Month(n.rebasingMonth), n.rebasingDay, 0, 0, 0, 0, time.UTC)
	if asOf.Before(rebasingDateThisYear) {
		return asOf.Year() - 1
	}
	return asOf.Year()
}

// CashMovement is the result of ApplyCashMovement: a deposit or withdrawal
// of Amount at EquityBefore scaled the yearly starting figure, the
// measurement base, and the Notional Account, taking equity to EquityAfter
// (ADR 0007).
type CashMovement struct {
	Amount               float64
	EquityBefore         float64
	EquityAfter          float64
	StartingFigureBefore float64
	StartingFigureAfter  float64
	NotionalBefore       float64
	NotionalAfter        float64
}

// ApplyCashMovement scales the yearly starting figure, the measurement base,
// and the Notional Account by the identical factor (equityBefore+amount) /
// equityBefore (ADR 0007), via the shared sizing.CashMovementScaledFigure —
// see that function's doc comment for why every figure must be scaled by
// the SAME derivation. Because every figure moves by the same ratio,
// equity's position relative to any of them — in particular, relative to a
// Drawdown Step threshold or to the starting figure itself — is unchanged
// by the movement: a deposit can neither trigger a step it would not
// otherwise have crossed nor mask one, and a withdrawal can neither trigger
// nor mask one either. The step count is left unchanged: a cash movement
// neither applies nor clears a Drawdown Step.
//
// equityBefore must be finite and positive, and amount must be finite and
// non-zero (a "movement" of nothing is not a movement). equityBefore+amount
// must itself be finite (two finite inputs can still overflow to +/-Inf —
// Greptile PR #71 finding) and strictly positive: a withdrawal that would
// take equity to zero or below fails closed, since ADR 0007 does not
// address an account emptied or overdrawn by a withdrawal. Every check,
// including that each of the three scaled figures itself comes out finite
// and positive (the ratio applied to an already-extreme figure can overflow
// even when equityBefore+amount does not), runs BEFORE any field of n is
// mutated: a rejected cash movement leaves the account exactly as it found
// it, never partially scaled.
func (n *NotionalAccount) ApplyCashMovement(equityBefore, amount float64) (*CashMovement, error) {
	if err := checkEquity("equity before", equityBefore); err != nil {
		return nil, fmt.Errorf("strategy: cannot apply a cash movement: %w", err)
	}
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return nil, errors.New("strategy: cannot apply a cash movement: amount must be finite")
	}
	if amount == 0 {
		return nil, errors.New("strategy: cannot apply a cash movement: amount must be non-zero")
	}
	equityAfter := equityBefore + amount
	switch {
	case math.IsNaN(equityAfter) || math.IsInf(equityAfter, 0):
		return nil, fmt.Errorf("strategy: cannot apply a cash movement: equity before %v plus amount %v is not finite (%v); failing closed rather than scaling the Notional Account by a non-finite factor", equityBefore, amount, equityAfter)
	case equityAfter <= 0:
		return nil, fmt.Errorf("strategy: cannot apply a cash movement: a withdrawal of %v from equity %v would take equity to %v, at or below zero; failing closed rather than scaling the Notional Account by a non-positive factor", amount, equityBefore, equityAfter)
	}

	// Computed into locals, and validated, before anything on n is mutated
	// (see this method's own doc comment): the ratio equityAfter/equityBefore
	// applied to an already-extreme figure can itself overflow even though
	// equityAfter is finite.
	scaledStartingFigure := sizing.CashMovementScaledFigure(n.startingFigure, equityBefore, equityAfter)
	scaledBase := sizing.CashMovementScaledFigure(n.base, equityBefore, equityAfter)
	scaledCurrent := sizing.CashMovementScaledFigure(n.current, equityBefore, equityAfter)
	if err := checkEquity("scaled yearly starting figure", scaledStartingFigure); err != nil {
		return nil, fmt.Errorf("strategy: cannot apply a cash movement: %w", err)
	}
	if err := checkEquity("scaled measurement base", scaledBase); err != nil {
		return nil, fmt.Errorf("strategy: cannot apply a cash movement: %w", err)
	}
	if err := checkEquity("scaled notional account", scaledCurrent); err != nil {
		return nil, fmt.Errorf("strategy: cannot apply a cash movement: %w", err)
	}

	startingFigureBefore := n.startingFigure
	notionalBefore := n.current

	n.startingFigure = scaledStartingFigure
	n.base = scaledBase
	n.current = scaledCurrent

	return &CashMovement{
		Amount:               amount,
		EquityBefore:         equityBefore,
		EquityAfter:          equityAfter,
		StartingFigureBefore: startingFigureBefore,
		StartingFigureAfter:  n.startingFigure,
		NotionalBefore:       notionalBefore,
		NotionalAfter:        n.current,
	}, nil
}

// checkEquity rejects a non-finite or non-positive figure, named for the
// error message. Shared by NewNotionalAccount (starting equity) and Observe (an observed
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
// re-basing, Drawdown Step ladder, and recovery are all driven from here
// rather than from reducer.go (see this file's top-of-file comment on why).
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
	if err := r.pinAccountCurrency(snapshot.Currency); err != nil {
		return nil, err
	}

	// Chronology, mirroring applyCompletedBar's per-instrument rule: a
	// duplicate or out-of-order snapshot would silently re-derive Drawdown
	// Steps from a stale or repeated reading, corrupting the Notional
	// Account for every decision after it. There is one account for the
	// whole run, and #17 puts cash movements on the SAME timeline (ADR
	// 0007 rule 4), so chronology is tracked across both event types
	// together, not per-instrument and not per event type.
	if r.hasAccountEvent && !snapshot.AsOf.After(r.lastAccountEventAt) {
		return nil, fmt.Errorf("strategy: account snapshot as of %s is not strictly after the last recorded account event %s; rejecting a duplicate or out-of-order snapshot",
			snapshot.AsOf.Format(time.RFC3339), r.lastAccountEventAt.Format(time.RFC3339))
	}

	rebase, steps, recovery, err := r.notionalAccount.ObserveSnapshot(snapshot.AsOf, snapshot.Equity)
	if err != nil {
		// Reachable: AccountSnapshotPayload.Validate has already required
		// Equity to be finite and positive, but Observe (called inside
		// ObserveSnapshot) can still fail closed here for a reason no
		// payload-level check can see — equity at or below the Drawdown
		// Step ladder's 50%-drawdown asymptote (NotionalAccount.Observe's
		// doc comment; Greptile PR #66 finding — note that re-basing moves
		// what the asymptote is measured from, since it moves the
		// measurement base and account themselves). Wrapped, not swallowed:
		// the run stops rather than sizing anything further from an
		// undefined Notional Account.
		return nil, fmt.Errorf("strategy: %w", err)
	}

	r.lastAccountEventAt = snapshot.AsOf
	r.hasAccountEvent = true

	var emissions []event.Envelope

	if rebase != nil {
		// A re-basing starts a fresh episode: the next Drawdown Step is
		// step 1 again (#16's Concerns flagged this as #17's job).
		r.drawdownStepsSeen = 0
		payload := event.NotionalAccountRebasedPayload{
			AsOf:                   snapshot.AsOf,
			PreviousStartingFigure: rebase.PreviousStartingFigure,
			NewStartingFigure:      rebase.NewStartingFigure,
			Equity:                 rebase.Equity,
			Rule:                   event.RuleNotionalAccountRebase,
			ADR:                    event.ADRNotionalAccountRebase,
		}
		if err := payload.Validate(); err != nil {
			return nil, fmt.Errorf("strategy: built invalid notional account rebased payload: %w", err)
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("strategy: marshal notional account rebased payload: %w", err)
		}
		emissions = append(emissions, r.stamp(
			notionalAccountEventID("notional-account-rebased", snapshot.AsOf),
			event.NotionalAccountRebasedEventType, event.NotionalAccountRebasedSchemaVersion,
			snapshot.AsOf, envelope, payloadBytes,
		))
	}

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

	if recovery != nil {
		// A recovery likewise starts a fresh episode: the same reset
		// r.drawdownStepsSeen gets on a re-basing.
		r.drawdownStepsSeen = 0
		payload := event.NotionalAccountRecoveredPayload{
			AsOf:           snapshot.AsOf,
			Equity:         recovery.Equity,
			StartingFigure: recovery.StartingFigure,
			NotionalBefore: recovery.NotionalBefore,
			StepsCleared:   recovery.StepsCleared,
			Rule:           event.RuleNotionalAccountRecovery,
			ADR:            event.ADRNotionalAccountRecovery,
		}
		if err := payload.Validate(); err != nil {
			return nil, fmt.Errorf("strategy: built invalid notional account recovered payload: %w", err)
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("strategy: marshal notional account recovered payload: %w", err)
		}
		emissions = append(emissions, r.stamp(
			notionalAccountEventID("notional-account-recovered", snapshot.AsOf),
			event.NotionalAccountRecoveredEventType, event.NotionalAccountRecoveredSchemaVersion,
			snapshot.AsOf, envelope, payloadBytes,
		))
	}

	return emissions, nil
}

// applyCashMovement handles event.CashMovementEventType (#17): a deposit or
// withdrawal scales the Notional Account per ADR 0007 (see
// NotionalAccount.ApplyCashMovement) and is journalled as a
// strategy.notional-account.cash-adjusted decision.
//
// Like applyAccountSnapshot, it requires a configuration event first and
// rejects a schema version other than event.CashMovementSchemaVersion before
// decoding (ADR 0015). Chronology is checked against the SAME
// r.lastAccountEventAt/r.hasAccountEvent state as applyAccountSnapshot: ADR
// 0007 rule 4 puts snapshots and cash movements on one shared per-account
// timeline, and this ticket's decision is that they may not share an AsOf —
// a cash movement carries its own EquityBefore precisely so the rule is
// checkable from the event alone, without needing a same-instant snapshot.
func (r *Reducer) applyCashMovement(envelope event.Envelope) ([]event.Envelope, error) {
	if !r.configured {
		return nil, errors.New("strategy: received a cash movement before a configuration event; failing closed")
	}
	if envelope.SchemaVersion != event.CashMovementSchemaVersion {
		return nil, fmt.Errorf("strategy: cash movement payload schema version %d does not match the version %d this build requires; an older or newer schema is rejected, never silently upgraded, until an explicit upcaster exists (ADR 0015)", envelope.SchemaVersion, event.CashMovementSchemaVersion)
	}

	var movement event.CashMovementPayload
	if err := json.Unmarshal(envelope.Payload, &movement); err != nil {
		return nil, fmt.Errorf("strategy: decode cash movement payload: %w", err)
	}
	if err := movement.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: invalid cash movement payload: %w", err)
	}
	if err := r.pinAccountCurrency(movement.Currency); err != nil {
		return nil, err
	}

	if r.hasAccountEvent && !movement.AsOf.After(r.lastAccountEventAt) {
		return nil, fmt.Errorf("strategy: cash movement as of %s is not strictly after the last recorded account event %s; rejecting a duplicate or out-of-order cash movement",
			movement.AsOf.Format(time.RFC3339), r.lastAccountEventAt.Format(time.RFC3339))
	}

	adjustment, err := r.notionalAccount.ApplyCashMovement(movement.EquityBefore, movement.Amount)
	if err != nil {
		// Reachable: CashMovementPayload.Validate has already rejected a
		// withdrawal to zero or below using the same arithmetic, but
		// ApplyCashMovement is the arithmetic seam's own authority on it —
		// wrapped, not swallowed, matching applyAccountSnapshot's asymptote
		// handling above.
		return nil, fmt.Errorf("strategy: %w", err)
	}

	r.lastAccountEventAt = movement.AsOf
	r.hasAccountEvent = true

	payload := event.NotionalAccountCashAdjustedPayload{
		AsOf:                 movement.AsOf,
		Amount:               adjustment.Amount,
		EquityBefore:         adjustment.EquityBefore,
		EquityAfter:          adjustment.EquityAfter,
		StartingFigureBefore: adjustment.StartingFigureBefore,
		StartingFigureAfter:  adjustment.StartingFigureAfter,
		NotionalBefore:       adjustment.NotionalBefore,
		NotionalAfter:        adjustment.NotionalAfter,
		Rule:                 event.RuleNotionalAccountCashAdjustment,
		ADR:                  event.ADRNotionalAccountCashAdjustment,
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("strategy: built invalid notional account cash adjusted payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("strategy: marshal notional account cash adjusted payload: %w", err)
	}
	return []event.Envelope{r.stamp(
		notionalAccountEventID("notional-account-cash-adjusted", movement.AsOf),
		event.NotionalAccountCashAdjustedEventType, event.NotionalAccountCashAdjustedSchemaVersion,
		movement.AsOf, envelope, payloadBytes,
	)}, nil
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

// notionalAccountEventID builds a deterministic, reproducible-on-replay ID
// for a re-basing, recovery, or cash-adjustment emission (#17). Unlike a
// Drawdown Step, each of these can occur at most once per AsOf — ADR 0007
// rule 4's shared, strictly-increasing account timeline already guarantees
// that — so kind and AsOf together identify it uniquely without a counter.
func notionalAccountEventID(kind string, asOf time.Time) string {
	return fmt.Sprintf("%s:%s", kind, asOf.UTC().Format("2006-01-02T15:04:05.000000000Z"))
}

// pinAccountCurrency pins r.accountCurrency from the first account snapshot
// or cash movement accepted, and rejects any later one of either type whose
// Currency differs from the pin (Greptile PR #71 finding: Currency was
// validated for presence only by AccountSnapshotPayload/CashMovementPayload
// and then discarded, so nothing stopped a later event stated in a
// different currency from being silently scaled and compared against
// figures stated in the first one). Multi-currency accounts are out of
// scope for this project (issue #17 Findings); this makes that explicit at
// the reducer rather than leaving it silently unenforced. The pin survives
// everything else the Notional Account does — re-basing, a Drawdown Step,
// a recovery — since it lives on the Reducer, not on NotionalAccount.
func (r *Reducer) pinAccountCurrency(currency string) error {
	if r.accountCurrency == "" {
		r.accountCurrency = currency
		return nil
	}
	if currency != r.accountCurrency {
		return fmt.Errorf("strategy: account event currency %q does not match the account's pinned currency %q; multi-currency accounts are out of scope", currency, r.accountCurrency)
	}
	return nil
}
