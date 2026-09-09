package strategy_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

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

	account, err := strategy.NewNotionalAccount(1_000_000)
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

	triggering, err := strategy.NewNotionalAccount(1_000_000)
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

	notTriggering, err := strategy.NewNotionalAccount(1_000_000)
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

	account, err := strategy.NewNotionalAccount(1_000_000)
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

	account, err := strategy.NewNotionalAccount(1_000_000)
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
		if _, err := strategy.NewNotionalAccount(starting); err == nil {
			t.Errorf("New(%v) error = nil, want an error", starting)
		}
	}
}

func TestNotionalAccountObserveRejectsNonFiniteOrNonPositiveEquity(t *testing.T) {
	t.Parallel()

	for _, equity := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		account, err := strategy.NewNotionalAccount(1_000_000)
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

	account, err := strategy.NewNotionalAccount(1_000_000)
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

	account, err := strategy.NewNotionalAccount(1_000_000)
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

	account, err := strategy.NewNotionalAccount(1_000_000)
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

	account, err := strategy.NewNotionalAccount(1_000_000)
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
