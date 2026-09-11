package strategy_test

import (
	"math"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// Every strategy.NewNotionalAccount call in this file that only exercises
// the Drawdown Step ladder (#16, unchanged by #17) passes rebasing month/day
// 1/1 (January 1st) as an arbitrary but valid re-basing date: none of that
// coverage calls ObserveSnapshot with an AsOf near it, so the re-basing date
// is inert for those tests. #17's own re-basing/recovery/cash-movement tests
// are below, starting at TestNotionalAccountRebasesAcrossAYearBoundary.

// TestNotionalAccountFaithsLadder transcribes The Turtle Rules p.17's worked
// example exactly: $1,000,000 -> down 10% -> $800,000 -> down a further 10%
// of the REDUCED figure -> $640,000 (ADR 0007). It then derives, and tests, a
// third step the source does not print: base 820,000 less 10% of the
// now-640,000 account is 756,000, which steps the account to 512,000.
//
// docs/methodology/Methodology_Analysis.md §2.5 says "20% each time equity
// falls 10% of the original account", but its own worked example
// ($640k after a further -$80k) contradicts that: $80k is 10% of the
// REDUCED $800k account, not of the original $1M. ADR 0007 and the worked
// example govern (see this PR's one-line correction to §2.5); this test
// transcribes the worked example's numbers, not the looser prose.
func TestNotionalAccountFaithsLadder(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("New(1000000) error = %v", err)
	}
	if account.Current() != 1_000_000 {
		t.Fatalf("Current() = %v, want 1,000,000 before any observation", account.Current())
	}
	if account.MeasurementBase() != 1_000_000 {
		t.Fatalf("MeasurementBase() = %v, want 1,000,000 before any observation", account.MeasurementBase())
	}

	// Step 1: equity at exactly the first threshold, 900,000 (base 1,000,000
	// less 10% of the 1,000,000 account) [T p.17].
	steps, err := account.Observe(900_000)
	if err != nil {
		t.Fatalf("Observe(900000) error = %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("len(steps) = %d, want 1", len(steps))
	}
	want := strategy.Step{From: 1_000_000, To: 800_000, Threshold: 900_000, Equity: 900_000}
	if steps[0] != want {
		t.Fatalf("steps[0] = %+v, want %+v", steps[0], want)
	}
	if account.Current() != 800_000 {
		t.Fatalf("Current() = %v, want 800,000", account.Current())
	}
	if account.MeasurementBase() != 900_000 {
		t.Fatalf("MeasurementBase() = %v, want 900,000", account.MeasurementBase())
	}

	// Step 2: equity at exactly the second threshold, 820,000 (base 900,000
	// less 10% of the now-800,000 account) -> $640,000, the source's own
	// "further -$80k" [T p.17].
	steps, err = account.Observe(820_000)
	if err != nil {
		t.Fatalf("Observe(820000) error = %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("len(steps) = %d, want 1", len(steps))
	}
	want = strategy.Step{From: 800_000, To: 640_000, Threshold: 820_000, Equity: 820_000}
	if steps[0] != want {
		t.Fatalf("steps[0] = %+v, want %+v", steps[0], want)
	}
	if account.Current() != 640_000 {
		t.Fatalf("Current() = %v, want 640,000", account.Current())
	}

	// Step 3: derived, not printed in the source. Base 820,000 less 10% of
	// the now-640,000 account is 756,000; stepping there gives 512,000.
	steps, err = account.Observe(756_000)
	if err != nil {
		t.Fatalf("Observe(756000) error = %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("len(steps) = %d, want 1", len(steps))
	}
	want = strategy.Step{From: 640_000, To: 512_000, Threshold: 756_000, Equity: 756_000}
	if steps[0] != want {
		t.Fatalf("steps[0] = %+v, want %+v (derived, not printed in the source)", steps[0], want)
	}
	if account.Current() != 512_000 {
		t.Fatalf("Current() = %v, want 512,000 (derived)", account.Current())
	}
	if account.MeasurementBase() != 756_000 {
		t.Fatalf("MeasurementBase() = %v, want 756,000 (derived)", account.MeasurementBase())
	}
}

// TestNotionalAccountExactBoundaryTriggersAndOneCentAboveDoesNot: The Turtle
// Rules p.17 says equity "down 10%" steps the account, so the boundary is
// inclusive.
func TestNotionalAccountExactBoundaryTriggersAndOneCentAboveDoesNot(t *testing.T) {
	t.Parallel()

	triggering, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	steps, err := triggering.Observe(900_000)
	if err != nil {
		t.Fatalf("Observe(900000) error = %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("Observe(900000): len(steps) = %d, want 1 (exact boundary triggers)", len(steps))
	}

	notTriggering, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	steps, err = notTriggering.Observe(900_000.01)
	if err != nil {
		t.Fatalf("Observe(900000.01) error = %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("Observe(900000.01): len(steps) = %d, want 0 (one cent above the threshold must not trigger)", len(steps))
	}
	if notTriggering.Current() != 1_000_000 {
		t.Fatalf("Current() = %v, want 1,000,000 unchanged", notTriggering.Current())
	}
}

// TestNotionalAccountSingleObservationAppliesSeveralStepsInOrder is the
// "a single large drop can trigger several steps in one snapshot" case.
//
// It uses 750,000, not the 700,000 the orchestrating session's brief
// sketched for this fixture. Applying ADR 0007's rule exactly, 700,000 in a
// single observation from a 1,000,000 start crosses a FOURTH threshold too:
// after the third step the account is 512,000 and the base is 756,000, so
// the fourth threshold is 756,000 - 10%*512,000 = 704,800, and
// 700,000 <= 704,800 — a fourth step, not a third. 750,000 sits strictly
// between the third threshold (756,000, which it must cross) and the fourth
// (704,800, which it must not), so it is the corrected fixture for "exactly
// three steps in one observation, at thresholds 900,000/820,000/756,000" —
// reported as a discrepancy in the brief rather than invented silently or
// followed into a wrong test (see the PR's Findings and this ticket's
// comment thread).
func TestNotionalAccountSingleObservationAppliesSeveralStepsInOrder(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const equity = 750_000
	steps, err := account.Observe(equity)
	if err != nil {
		t.Fatalf("Observe(%v) error = %v", equity, err)
	}

	want := []strategy.Step{
		{From: 1_000_000, To: 800_000, Threshold: 900_000, Equity: equity},
		{From: 800_000, To: 640_000, Threshold: 820_000, Equity: equity},
		{From: 640_000, To: 512_000, Threshold: 756_000, Equity: equity},
	}
	if len(steps) != len(want) {
		t.Fatalf("len(steps) = %d, want %d: %+v", len(steps), len(want), steps)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Errorf("steps[%d] = %+v, want %+v", i, steps[i], want[i])
		}
	}
	if account.Current() != 512_000 {
		t.Fatalf("Current() = %v, want 512,000", account.Current())
	}
	if account.MeasurementBase() != 756_000 {
		t.Fatalf("MeasurementBase() = %v, want 756,000", account.MeasurementBase())
	}

	// Confirms the fixture is exactly the boundary case it claims to be: the
	// next threshold (704,800) is NOT crossed by 750,000, so the loop
	// correctly stopped at three steps rather than continuing to a fourth.
	if next := account.MeasurementBase() - 0.10*account.Current(); equity <= next {
		t.Fatalf("test fixture invalid: %v must be above the next threshold %v, or a fourth step was missed", float64(equity), next)
	}
}

// TestNotionalAccountPartialRecoveryDoesNotRestore: a rise in equity, even
// one that recovers most of the way back to the starting figure, must never
// move the measurement base upward or restore the account. Recovery, which
// requires regaining the full yearly starting figure, is #17.
func TestNotionalAccountPartialRecoveryDoesNotRestore(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := account.Observe(900_000); err != nil {
		t.Fatalf("Observe(900000) error = %v", err)
	}

	// 950,000 is well above the yearly starting figure #17 would require for
	// full recovery, and above the current threshold too (900,000 - 10% of
	// 800,000 = 820,000): a rise must never itself trigger a step, and must
	// never restore the account (ADR 0007: not a high-water mark).
	steps, err := account.Observe(950_000)
	if err != nil {
		t.Fatalf("Observe(950000) error = %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("len(steps) = %d, want 0 (a rise must never itself trigger a step)", len(steps))
	}
	if account.Current() != 800_000 {
		t.Fatalf("Current() = %v, want 800,000 unchanged (no recovery within this ticket; #17)", account.Current())
	}
	if account.MeasurementBase() != 900_000 {
		t.Fatalf("MeasurementBase() = %v, want 900,000 unchanged", account.MeasurementBase())
	}
}

func TestNewNotionalAccountRejectsNonFiniteOrNonPositiveStartingEquity(t *testing.T) {
	t.Parallel()

	for _, starting := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := strategy.NewNotionalAccount(starting, 1, 1); err == nil {
			t.Errorf("New(%v) error = nil, want an error", starting)
		}
	}
}

func TestNotionalAccountObserveRejectsNonFiniteOrNonPositiveEquity(t *testing.T) {
	t.Parallel()

	for _, equity := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if _, err := account.Observe(equity); err == nil {
			t.Errorf("Observe(%v) error = nil, want an error", equity)
		}
	}
}

// TestHighWaterMarkResetDivergesFromNotionalAccount is the ticket's named
// negative test: a high-water-mark reset (the earlier prototype's rule, and
// ADR 0007's declared Variant, not the Baseline) is reimplemented from
// scratch here, independent of strategy.NotionalAccount, and must diverge
// from it on a rise-then-fall fixture.
//
// Sequence: equity rises to a new high (1,200,000), then falls to exactly
// 10% below that NEW high (1,080,000). strategy.NotionalAccount's
// measurement base never moved off the original 1,000,000 (a rise is not
// itself a trigger), so its real threshold is still 900,000, and 1,080,000
// does not cross it: zero steps. A high-water-mark implementation, whose
// base tracks the running maximum, computes its threshold from the new
// 1,200,000 high (1,100,000) and DOES step on 1,080,000 — the divergence
// this test asserts.
func TestHighWaterMarkResetDivergesFromNotionalAccount(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if steps, err := account.Observe(1_200_000); err != nil || len(steps) != 0 {
		t.Fatalf("Observe(1200000) = %v, %v; want 0 steps, nil error (a rise never triggers)", steps, err)
	}
	steps, err := account.Observe(1_080_000)
	if err != nil {
		t.Fatalf("Observe(1080000) error = %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("strategy.NotionalAccount stepped on a 10%% fall from a NEW high: len(steps) = %d, want 0 (the measurement base does not move on a rise)", len(steps))
	}

	hwm := newHighWaterMarkAccount(1_000_000)
	if hwm.observe(1_200_000) {
		t.Fatalf("high-water-mark fixture stepped on a rise, want no step")
	}
	if !hwm.observe(1_080_000) {
		t.Fatalf("high-water-mark reimplementation did not step on 1,080,000 (10%% below the new 1,200,000 high); the negative fixture requires it to, so it can diverge from strategy.NotionalAccount")
	}
}

// highWaterMarkAccount is the rejected prototype behaviour: its base tracks
// the running MAXIMUM equity observed, rather than only the figure last
// measured from at a Drawdown Step (ADR 0007's declared Variant, not the
// Baseline). It is reimplemented here from scratch, independently of
// strategy.NotionalAccount, purely to prove the two diverge.
type highWaterMarkAccount struct {
	base    float64
	current float64
}

func newHighWaterMarkAccount(starting float64) *highWaterMarkAccount {
	return &highWaterMarkAccount{base: starting, current: starting}
}

// observe reports whether this one observation applied a step. Unlike
// strategy.NotionalAccount.Observe, the base is reset to equity whenever
// equity exceeds it, BEFORE the threshold is computed — the high-water-mark
// behaviour this fixture exists to reject.
func (h *highWaterMarkAccount) observe(equity float64) bool {
	if equity > h.base {
		h.base = equity // high-water-mark reset: the rejected behaviour
	}
	threshold := h.base - 0.10*h.current
	if equity > threshold {
		return false
	}
	h.current = 0.8 * h.current
	h.base = threshold
	return true
}

// TestNotionalAccountEquityAtTheAsymptoteErrors is Greptile PR #66's finding
// on this file: the Drawdown Step ladder's thresholds are a geometric
// series (see notionalAccountUndefinedDrawdownFraction's doc comment) that
// converges to, but never reaches, base - 50%*current — 500,000 for a
// 1,000,000 account. Equity at or below that figure would leave every
// future threshold still above it, so a literal application of the rule
// never terminates. ADR 0007 (and Faith's source) do not address a drawdown
// this deep; Observe fails closed instead.
func TestNotionalAccountEquityAtTheAsymptoteErrors(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	if _, err := account.Observe(500_000); err == nil { // base(1,000,000) - 50%*current(1,000,000)
		t.Fatal("Observe(500000) error = nil, want an error: the rule is undefined at the 50% drawdown asymptote")
	}
}

// TestNotionalAccountOneCentAboveTheAsymptoteTerminates confirms Observe
// DOES terminate for equity strictly above the asymptote, even one cent
// above it — in a large but finite number of steps. The step count and the
// final account are empirically pinned (found by running the code, not
// hand-derived): a constructed fact, in the same spirit as
// internal/sizing's float truncation boundary test
// (TestUnitQuantityTruncatesToTheTrueFloorAtAFloatBoundary), so the fixture
// cannot go stale and pass vacuously.
func TestNotionalAccountOneCentAboveTheAsymptoteTerminates(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	steps, err := account.Observe(500_000.01)
	if err != nil {
		t.Fatalf("Observe(500000.01) error = %v, want it to terminate (one cent above the asymptote)", err)
	}

	const wantSteps = 79
	if len(steps) != wantSteps {
		t.Fatalf("len(steps) = %d, want %d", len(steps), wantSteps)
	}
	const wantCurrent = 0.022085588309729898
	if account.Current() != wantCurrent {
		t.Fatalf("Current() = %v, want %v", account.Current(), wantCurrent)
	}
}

// TestNotionalAccountAsymptoteDoesNotAffectTheExistingMultiStepFixtures
// confirms the asymptote check does not disturb any equity comfortably
// above it: every existing golden and multi-step fixture in this file uses
// equity well above 500,000 on a 1,000,000 account (750,000 at the tightest;
// see TestNotionalAccountSingleObservationAppliesSeveralStepsInOrder), so
// they are unaffected by construction — this test states that fact
// explicitly rather than leaving it implicit.
func TestNotionalAccountAsymptoteDoesNotAffectTheExistingMultiStepFixtures(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	steps, err := account.Observe(750_000) // well above the 500,000 asymptote
	if err != nil {
		t.Fatalf("Observe(750000) error = %v, want nil", err)
	}
	if len(steps) != 3 {
		t.Fatalf("len(steps) = %d, want 3", len(steps))
	}
}

// --- #17: yearly re-basing, recovery, and cash movements (ADR 0007) ---

// jan is a UTC midnight time.Time, named for readability in the tests below.
func jan(day, year int) time.Time {
	return time.Date(year, time.January, day, 0, 0, 0, 0, time.UTC)
}

// TestNotionalAccountRebasesAcrossAYearBoundary is the ticket's headline
// case: a snapshot before the configured re-basing date establishes the
// account's first period without re-basing (see
// TestNotionalAccountFirstSnapshotAfterRebasingDateDoesNotRebase for why),
// and the first snapshot ON OR AFTER 1 January of the following year
// re-bases the yearly starting figure, the measurement base, and the
// account itself to actual equity, resets the step count, and reports the
// re-basing. A further step is then measured against the NEW figure, not
// the one re-basing replaced.
func TestNotionalAccountRebasesAcrossAYearBoundary(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}

	// Establishes the account's first period (2026) without re-basing.
	rebase, steps, recovery, err := account.ObserveSnapshot(jan(2, 2026), 970_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(first) error = %v", err)
	}
	if rebase != nil || len(steps) != 0 || recovery != nil {
		t.Fatalf("ObserveSnapshot(first) = %+v, %v, %+v, want no rebase/steps/recovery on the first-ever snapshot", rebase, steps, recovery)
	}
	if account.StartingFigure() != 1_000_000 {
		t.Fatalf("StartingFigure() = %v, want the configured 1,000,000 unchanged", account.StartingFigure())
	}

	// The first snapshot on or after 1 January 2027 re-bases to actual
	// equity, 900,000 — which happens to equal the OLD figure's first
	// threshold (1,000,000 - 10%), so if re-basing did not happen strictly
	// before step evaluation, this same call would incorrectly also fire a
	// Drawdown Step against the old, pre-re-basing figure.
	rebase, steps, recovery, err = account.ObserveSnapshot(jan(1, 2027), 900_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(rebase) error = %v", err)
	}
	if rebase == nil {
		t.Fatal("ObserveSnapshot(rebase): rebase = nil, want a re-basing on the first snapshot on or after the new year's re-basing date")
	}
	wantRebase := strategy.Rebase{PreviousStartingFigure: 1_000_000, NewStartingFigure: 900_000, Equity: 900_000}
	if *rebase != wantRebase {
		t.Errorf("rebase = %+v, want %+v", *rebase, wantRebase)
	}
	if len(steps) != 0 {
		t.Fatalf("len(steps) = %d, want 0: re-basing must happen strictly before step evaluation, so this snapshot cannot ALSO step against the figure re-basing just replaced", len(steps))
	}
	if recovery != nil {
		t.Fatalf("recovery = %+v, want nil on a re-basing snapshot", recovery)
	}
	if account.StartingFigure() != 900_000 || account.Current() != 900_000 || account.MeasurementBase() != 900_000 {
		t.Fatalf("StartingFigure()/Current()/MeasurementBase() = %v/%v/%v, want 900,000/900,000/900,000", account.StartingFigure(), account.Current(), account.MeasurementBase())
	}

	// A further step is now measured against the NEW figure: 900,000 - 10%
	// of 900,000 = 810,000.
	rebase, steps, recovery, err = account.ObserveSnapshot(jan(2, 2027), 810_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(step after rebase) error = %v", err)
	}
	if rebase != nil {
		t.Fatalf("rebase = %+v, want nil (re-basing happens at most once per calendar year)", rebase)
	}
	if len(steps) != 1 {
		t.Fatalf("len(steps) = %d, want 1", len(steps))
	}
	wantStep := strategy.Step{From: 900_000, To: 720_000, Threshold: 810_000, Equity: 810_000}
	if steps[0] != wantStep {
		t.Errorf("steps[0] = %+v, want %+v", steps[0], wantStep)
	}
	if recovery != nil {
		t.Fatalf("recovery = %+v, want nil", recovery)
	}
}

// TestNotionalAccountFirstSnapshotAfterRebasingDateDoesNotRebase names #17's
// documented decision directly: the very first snapshot of a run, even one
// whose AsOf falls on or after the year's re-basing date, does not re-base.
// The configured StartingEquity stands in as the yearly starting figure
// until a LATER snapshot's AsOf crosses into a new re-basing year. A
// backtest starting mid-year has no record of what actual equity was at
// that year's own 1 January — only the configured figure — and re-basing to
// the first observed reading would silently manufacture a "yearly starting
// figure" the run never actually started the year with.
func TestNotionalAccountFirstSnapshotAfterRebasingDateDoesNotRebase(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}

	// June 2026 is well after the 1 January re-basing date, and this is the
	// very first snapshot the account has ever seen.
	rebase, steps, recovery, err := account.ObserveSnapshot(jan(1, 2026).AddDate(0, 5, 0), 950_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot() error = %v", err)
	}
	if rebase != nil {
		t.Fatalf("rebase = %+v, want nil: the first snapshot of a run never re-bases", rebase)
	}
	if len(steps) != 0 || recovery != nil {
		t.Fatalf("steps/recovery = %v/%+v, want none: 950,000 is above the configured account's first threshold (900,000) and below its starting figure", steps, recovery)
	}
	if account.StartingFigure() != 1_000_000 {
		t.Fatalf("StartingFigure() = %v, want the configured 1,000,000 unchanged", account.StartingFigure())
	}
	if account.Current() != 1_000_000 || account.MeasurementBase() != 1_000_000 {
		t.Fatalf("Current()/MeasurementBase() = %v/%v, want 1,000,000/1,000,000 unchanged", account.Current(), account.MeasurementBase())
	}
}

// TestNotionalAccountSameYearSecondRebasingDateDoesNothing confirms
// re-basing happens at MOST once per calendar year: a second snapshot later
// in the same re-basing year must not re-base again, even though its AsOf
// is also on or after the re-basing date.
func TestNotionalAccountSameYearSecondRebasingDateDoesNothing(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}

	if _, _, _, err := account.ObserveSnapshot(jan(1, 2026), 970_000); err != nil {
		t.Fatalf("ObserveSnapshot(2026-01-01) error = %v", err)
	}
	if _, _, _, err := account.ObserveSnapshot(jan(1, 2027), 900_000); err != nil {
		t.Fatalf("ObserveSnapshot(2027-01-01, rebase) error = %v", err)
	}
	if account.StartingFigure() != 900_000 {
		t.Fatalf("StartingFigure() = %v, want 900,000 after the 2027 re-basing", account.StartingFigure())
	}

	// Later in the same re-basing year (2027), well after 1 January: must
	// not re-base a second time.
	rebase, _, _, err := account.ObserveSnapshot(jan(1, 2027).AddDate(0, 6, 0), 895_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(mid-2027) error = %v", err)
	}
	if rebase != nil {
		t.Fatalf("rebase = %+v, want nil: re-basing happens at most once per calendar year", rebase)
	}
	if account.StartingFigure() != 900_000 {
		t.Fatalf("StartingFigure() = %v, want 900,000 unchanged (no second re-basing)", account.StartingFigure())
	}
}

// TestNotionalAccountObserveSnapshotPartialRecoveryDoesNotRestore is #16's
// TestNotionalAccountPartialRecoveryDoesNotRestore repeated through
// ObserveSnapshot, confirming Recovery is nil while equity has not yet
// regained the yearly starting figure.
func TestNotionalAccountObserveSnapshotPartialRecoveryDoesNotRestore(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	if _, _, _, err := account.ObserveSnapshot(jan(2, 2026), 900_000); err != nil {
		t.Fatalf("ObserveSnapshot(900000) error = %v", err)
	}

	rebase, steps, recovery, err := account.ObserveSnapshot(jan(3, 2026), 950_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(950000) error = %v", err)
	}
	if rebase != nil || len(steps) != 0 {
		t.Fatalf("rebase/steps = %+v/%v, want none", rebase, steps)
	}
	if recovery != nil {
		t.Fatalf("recovery = %+v, want nil: 950,000 has not regained the 1,000,000 yearly starting figure", recovery)
	}
	if account.Current() != 800_000 || account.MeasurementBase() != 900_000 {
		t.Fatalf("Current()/MeasurementBase() = %v/%v, want 800,000/900,000 unchanged", account.Current(), account.MeasurementBase())
	}
}

// TestNotionalAccountObserveSnapshotFullRecoveryRestores: equity regaining
// the yearly starting figure after one step restores the account and the
// measurement base to it, resets the step count, and reports the recovery.
func TestNotionalAccountObserveSnapshotFullRecoveryRestores(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	if _, _, _, err := account.ObserveSnapshot(jan(2, 2026), 900_000); err != nil { // step: 1,000,000 -> 800,000
		t.Fatalf("ObserveSnapshot(900000) error = %v", err)
	}

	rebase, steps, recovery, err := account.ObserveSnapshot(jan(3, 2026), 1_000_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(1000000) error = %v", err)
	}
	if rebase != nil || len(steps) != 0 {
		t.Fatalf("rebase/steps = %+v/%v, want none", rebase, steps)
	}
	if recovery == nil {
		t.Fatal("recovery = nil, want a recovery: equity regained the yearly starting figure")
	}
	want := strategy.Recovery{Equity: 1_000_000, StartingFigure: 1_000_000, NotionalBefore: 800_000, StepsCleared: 1}
	if *recovery != want {
		t.Errorf("recovery = %+v, want %+v", *recovery, want)
	}
	if account.Current() != 1_000_000 || account.MeasurementBase() != 1_000_000 {
		t.Fatalf("Current()/MeasurementBase() = %v/%v, want 1,000,000/1,000,000 restored", account.Current(), account.MeasurementBase())
	}

	// The next Drawdown Step after a full recovery starts a fresh episode:
	// the ladder is measured against the restored figure, exactly as if
	// this were a brand-new account at 1,000,000.
	_, steps, _, err = account.ObserveSnapshot(jan(4, 2026), 900_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(900000, post-recovery) error = %v", err)
	}
	if len(steps) != 1 || steps[0] != (strategy.Step{From: 1_000_000, To: 800_000, Threshold: 900_000, Equity: 900_000}) {
		t.Fatalf("steps = %+v, want a fresh single step 1,000,000 -> 800,000", steps)
	}
}

// TestNotionalAccountObserveSnapshotTwoStepsThenRecoveryClearsBoth:
// StepsCleared reports every step applied since the ladder was last reset
// (by a re-basing or a prior recovery), not merely the most recent one.
func TestNotionalAccountObserveSnapshotTwoStepsThenRecoveryClearsBoth(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	if _, _, _, err := account.ObserveSnapshot(jan(2, 2026), 900_000); err != nil { // step 1: -> 800,000
		t.Fatalf("ObserveSnapshot(900000) error = %v", err)
	}
	if _, _, _, err := account.ObserveSnapshot(jan(3, 2026), 820_000); err != nil { // step 2: -> 640,000
		t.Fatalf("ObserveSnapshot(820000) error = %v", err)
	}

	_, _, recovery, err := account.ObserveSnapshot(jan(4, 2026), 1_050_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(1050000) error = %v", err)
	}
	if recovery == nil {
		t.Fatal("recovery = nil, want a recovery")
	}
	want := strategy.Recovery{Equity: 1_050_000, StartingFigure: 1_000_000, NotionalBefore: 640_000, StepsCleared: 2}
	if *recovery != want {
		t.Errorf("recovery = %+v, want %+v (both steps cleared)", *recovery, want)
	}
}

// TestNotionalAccountApplyCashMovementDuringADrawdownDeposit is the
// ticket's named deposit fixture: after one Drawdown Step at equity 890,000
// (S 1,000,000, B 900,000, A 800,000), a deposit of 200,000 scales every
// figure by (890,000+200,000)/890,000 = 1,224,719.10.../1,000,000 ... —
// see sizing_test.go's TestCashMovementScaledFigureDepositExample for the
// bit-pinned derivation this test's expectations are taken from. The
// subsequent snapshot at the post-deposit equity (1,090,000) must neither
// step nor recover: the deposit moved every figure by the identical factor,
// so equity's position relative to every threshold, and relative to the
// starting figure, is unchanged (ADR 0007: "neither triggers nor masks").
func TestNotionalAccountApplyCashMovementDuringADrawdownDeposit(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	if _, _, _, err := account.ObserveSnapshot(jan(2, 2026), 900_000); err != nil { // step: -> 800,000, base 900,000
		t.Fatalf("ObserveSnapshot(900000) error = %v", err)
	}

	const equityBefore = 890_000.0
	movement, err := account.ApplyCashMovement(equityBefore, 200_000)
	if err != nil {
		t.Fatalf("ApplyCashMovement() error = %v", err)
	}
	want := strategy.CashMovement{
		Amount:               200_000,
		EquityBefore:         890_000,
		EquityAfter:          1_090_000,
		StartingFigureBefore: 1_000_000,
		StartingFigureAfter:  1_224_719.1011235956,
		NotionalBefore:       800_000,
		NotionalAfter:        979_775.2808988765,
	}
	if *movement != want {
		t.Errorf("movement = %+v, want %+v", *movement, want)
	}
	if account.StartingFigure() != want.StartingFigureAfter {
		t.Errorf("StartingFigure() = %v, want %v", account.StartingFigure(), want.StartingFigureAfter)
	}
	if account.Current() != want.NotionalAfter {
		t.Errorf("Current() = %v, want %v", account.Current(), want.NotionalAfter)
	}

	// The measurement base scales too (not part of the reported
	// CashMovement, but a subsequent Observe reads it): confirmed
	// indirectly below by observing the post-deposit equity and requiring
	// no step.
	rebase, steps, recovery, err := account.ObserveSnapshot(jan(3, 2026), 1_090_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(post-deposit equity) error = %v", err)
	}
	if rebase != nil || len(steps) != 0 || recovery != nil {
		t.Fatalf("rebase/steps/recovery = %+v/%v/%+v, want none: a deposit must neither trigger nor mask a Drawdown Step", rebase, steps, recovery)
	}
}

// TestNotionalAccountApplyCashMovementDuringADrawdownWithdrawal is the
// symmetric withdrawal fixture, from the same starting state: a withdrawal
// of 100,000 scales every figure by 790,000/890,000. See
// sizing_test.go's TestCashMovementScaledFigureWithdrawalExample.
func TestNotionalAccountApplyCashMovementDuringADrawdownWithdrawal(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	if _, _, _, err := account.ObserveSnapshot(jan(2, 2026), 900_000); err != nil {
		t.Fatalf("ObserveSnapshot(900000) error = %v", err)
	}

	const equityBefore = 890_000.0
	movement, err := account.ApplyCashMovement(equityBefore, -100_000)
	if err != nil {
		t.Fatalf("ApplyCashMovement() error = %v", err)
	}
	want := strategy.CashMovement{
		Amount:               -100_000,
		EquityBefore:         890_000,
		EquityAfter:          790_000,
		StartingFigureBefore: 1_000_000,
		StartingFigureAfter:  887_640.4494382022,
		NotionalBefore:       800_000,
		NotionalAfter:        710_112.3595505618,
	}
	if *movement != want {
		t.Errorf("movement = %+v, want %+v", *movement, want)
	}

	rebase, steps, recovery, err := account.ObserveSnapshot(jan(3, 2026), 790_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(post-withdrawal equity) error = %v", err)
	}
	if rebase != nil || len(steps) != 0 || recovery != nil {
		t.Fatalf("rebase/steps/recovery = %+v/%v/%+v, want none: a withdrawal must neither trigger nor mask a Drawdown Step", rebase, steps, recovery)
	}
}

// TestNotionalAccountApplyCashMovementRejectsWithdrawalToZeroOrBelow: ADR
// 0007 says nothing about a withdrawal that empties or overdraws the
// account, so this fails closed rather than scaling every figure to zero or
// a meaningless negative number.
func TestNotionalAccountApplyCashMovementRejectsWithdrawalToZeroOrBelow(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name         string
		equityBefore float64
		amount       float64
	}{
		{name: "exactly zero", equityBefore: 100_000, amount: -100_000},
		{name: "below zero", equityBefore: 100_000, amount: -150_000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
			if err != nil {
				t.Fatalf("NewNotionalAccount() error = %v", err)
			}
			if _, err := account.ApplyCashMovement(tt.equityBefore, tt.amount); err == nil {
				t.Fatalf("ApplyCashMovement(%v, %v) error = nil, want an error", tt.equityBefore, tt.amount)
			}
		})
	}
}

func TestNotionalAccountApplyCashMovementRejectsNonFiniteOrZeroAmount(t *testing.T) {
	t.Parallel()

	for _, amount := range []float64{0, math.NaN(), math.Inf(1), math.Inf(-1)} {
		account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
		if err != nil {
			t.Fatalf("NewNotionalAccount() error = %v", err)
		}
		if _, err := account.ApplyCashMovement(1_000_000, amount); err == nil {
			t.Errorf("ApplyCashMovement(1000000, %v) error = nil, want an error", amount)
		}
	}
}

// TestNotionalAccountApplyCashMovementRejectsOverflowingEquityAfter is
// Greptile PR #71's finding: equityBefore and amount can both be finite
// while equityBefore+amount overflows to +Inf, which the original "<= 0"
// check let through silently (+Inf is not <= 0). The account must be left
// completely unchanged by a rejected cash movement — no partial scaling.
func TestNotionalAccountApplyCashMovementRejectsOverflowingEquityAfter(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	wantCurrent, wantBase, wantStarting := account.Current(), account.MeasurementBase(), account.StartingFigure()

	const equityBefore = math.MaxFloat64
	const amount = math.MaxFloat64 // equityBefore + amount overflows to +Inf

	if _, err := account.ApplyCashMovement(equityBefore, amount); err == nil {
		t.Fatal("ApplyCashMovement() error = nil, want an error: equity before plus amount overflows to +Inf")
	}
	if account.Current() != wantCurrent || account.MeasurementBase() != wantBase || account.StartingFigure() != wantStarting {
		t.Fatalf("account state changed despite the rejected cash movement: Current()/MeasurementBase()/StartingFigure() = %v/%v/%v, want %v/%v/%v unchanged",
			account.Current(), account.MeasurementBase(), account.StartingFigure(), wantCurrent, wantBase, wantStarting)
	}
}

// TestNotionalAccountApplyCashMovementRejectsAnOverflowingScaledFigure is
// the OTHER half of Greptile PR #71's finding: even when equityBefore+amount
// is itself finite, the ratio it forms can still overflow a figure that was
// already extreme when multiplied by it. This is what the fix's "compute
// every scaled figure into a local and validate before mutating" ordering
// exists to catch — reachable only if the account itself starts at an
// astronomical figure, which a real account never does, but the ladder
// makes no such assumption and must fail closed rather than silently commit
// an infinite Notional Account.
func TestNotionalAccountApplyCashMovementRejectsAnOverflowingScaledFigure(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(math.MaxFloat64/1.5, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	wantCurrent, wantBase, wantStarting := account.Current(), account.MeasurementBase(), account.StartingFigure()

	// The ratio (100+100)/100 = 2 applied to the already-astronomical
	// starting figure (MaxFloat64/1.5) overflows to +Inf.
	if _, err := account.ApplyCashMovement(100, 100); err == nil {
		t.Fatal("ApplyCashMovement() error = nil, want an error: the scaled starting figure overflows to +Inf")
	}
	if account.Current() != wantCurrent || account.MeasurementBase() != wantBase || account.StartingFigure() != wantStarting {
		t.Fatalf("account state changed despite the rejected cash movement: Current()/MeasurementBase()/StartingFigure() = %v/%v/%v, want %v/%v/%v unchanged",
			account.Current(), account.MeasurementBase(), account.StartingFigure(), wantCurrent, wantBase, wantStarting)
	}
}

func TestNewNotionalAccountRejectsAnInvalidRebasingDate(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ month, day int }{
		{month: 0, day: 1},
		{month: 13, day: 1},
		{month: 1, day: 0},
		{month: 1, day: 32},
		{month: 2, day: 29}, // does not recur every year (ConfigurationPayload.Validate's own reasoning)
	} {
		if _, err := strategy.NewNotionalAccount(1_000_000, tt.month, tt.day); err == nil {
			t.Errorf("NewNotionalAccount(1000000, %d, %d) error = nil, want an error", tt.month, tt.day)
		}
	}
}

// TestHighWaterMarkRecoveryDivergesFromNotionalAccountRecovery is #17's
// named negative test: a high-water-mark RECOVERY rule (restore whenever
// equity exceeds the maximum equity observed so far, regardless of the
// yearly starting figure) is reimplemented from scratch here, independent
// of strategy.NotionalAccount, and must diverge from ADR 0007's rule — which
// restores only on regaining the yearly starting figure, never on a new
// high.
//
// Fixture: starting figure 1,000,000; one step drops the account to 800,000
// (base 900,000). Equity then reads 850,000 (a fall, no step: above the
// second threshold of 820,000) and then 950,000 — a NEW HIGH above every
// prior equity reading (900,000, 850,000) but still below the 1,000,000
// starting figure. The Baseline (strategy.NotionalAccount) must not
// recover; the high-water-mark reimplementation must.
func TestHighWaterMarkRecoveryDivergesFromNotionalAccountRecovery(t *testing.T) {
	t.Parallel()

	account, err := strategy.NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	if _, _, _, err := account.ObserveSnapshot(jan(2, 2026), 900_000); err != nil { // step -> 800,000
		t.Fatalf("ObserveSnapshot(900000) error = %v", err)
	}
	if _, steps, _, err := account.ObserveSnapshot(jan(3, 2026), 850_000); err != nil || len(steps) != 0 {
		t.Fatalf("ObserveSnapshot(850000) = %v, %v, want 0 steps, nil error", steps, err)
	}
	_, steps, recovery, err := account.ObserveSnapshot(jan(4, 2026), 950_000)
	if err != nil {
		t.Fatalf("ObserveSnapshot(950000) error = %v", err)
	}
	if len(steps) != 0 || recovery != nil {
		t.Fatalf("steps/recovery = %v/%+v, want none: 950,000 is a new high but has not regained the 1,000,000 starting figure", steps, recovery)
	}

	hwm := newHighWaterMarkRecoveryAccount(1_000_000)
	if hwm.observe(900_000) != "step" {
		t.Fatal("high-water-mark fixture: observe(900000), want a step")
	}
	if hwm.observe(850_000) != "" {
		t.Fatal("high-water-mark fixture: observe(850000), want no effect")
	}
	if hwm.observe(950_000) != "recovered" {
		t.Fatalf("high-water-mark reimplementation did not restore on a new high (950,000 above every prior equity, 900,000/850,000); the negative fixture requires it to, so it can diverge from strategy.NotionalAccount")
	}
}

// highWaterMarkRecoveryAccount is the rejected recovery rule: it restores
// the account whenever equity sets a NEW HIGH in the observed sequence —
// however small, and however far below the yearly starting figure — rather
// than only when equity regains the YEARLY STARTING figure (ADR 0007's
// rule; CONTEXT.md's "Notional Account" and the ADR's declared
// high-water-mark Variant). That is exactly the "quietly re-expand position
// size on a partial recovery" behaviour ADR 0007 exists to reject.
// Reimplemented here from scratch, independently of strategy.NotionalAccount,
// purely to prove the two diverge.
type highWaterMarkRecoveryAccount struct {
	startingFigure float64
	base           float64
	current        float64
	maxEquitySeen  float64 // zero value: no equity observed yet
}

func newHighWaterMarkRecoveryAccount(starting float64) *highWaterMarkRecoveryAccount {
	return &highWaterMarkRecoveryAccount{startingFigure: starting, base: starting, current: starting}
}

// observe reports "step", "recovered", or "" (no effect).
func (h *highWaterMarkRecoveryAccount) observe(equity float64) string {
	isNewHigh := equity > h.maxEquitySeen
	if isNewHigh {
		h.maxEquitySeen = equity // the rejected behaviour: any new high, not regaining the starting figure
	}
	if h.current < h.startingFigure && isNewHigh {
		h.current = equity
		h.base = equity
		return "recovered"
	}
	threshold := h.base - 0.10*h.current
	if equity > threshold {
		return ""
	}
	h.current = 0.8 * h.current
	h.base = threshold
	return "step"
}
