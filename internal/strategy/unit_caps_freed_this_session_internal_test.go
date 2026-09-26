package strategy

// This file is deliberately `package strategy`, not `strategy_test` — the
// same shape unit_caps_internal_test.go's own doc comment explains for the
// industry/sector caps: the property under test here is a boundary
// condition of transition.protectedSessionGeneration and its two callers,
// recordUnitsFreedThisSession and freedThisSessionUnits (unit_caps.go), and
// driving it through the full event seam would need a live/intraday-style
// fill timestamp, several Sessions, and a shared cap just to reach a
// question that is really about Reducer's own session bookkeeping
// (sessionOpen, sessionGeneration, hasClosedSession, lastClosedSession).
// same_session_headroom_test.go proves the SAME mechanism end to end through
// Apply(); this file isolates the mechanism itself so each of its boundary
// cases can be pinned directly, independent of any one fixture's shape.
//
// Key context: a fill's own FilledAt is NOT a reliable proxy for "which
// Session does this belong to". A backtest's open-instant gapped stop is
// stamped at the NEW Session's own period end (fills.go) despite being
// delivered before that Session has even opened; a live venue reports a
// fill at its own execution instant, which is earlier than that Session's
// close by however much of the trading day remained (ADR 0021 §6's
// amendment: "this Session's fills" precede "this Session's bar, and its
// market.session.closed" in a live per-slice order). protectedSessionGeneration
// is the one place that turns "when did this fill happen, relative to what
// this Reducer has already decided" into "which Session's Add and entry
// decisions must still count it as committed" — never by comparing the
// fill's own timestamp to a Session's period end.

import "testing"

func TestProtectedSessionGenerationWhileASessionIsOpen(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	r.sessionOpen = true
	r.sessionGeneration = 5
	r.sessionPeriodEnd = day(10)
	tx := r.begin()

	// Every one of these calls states a fill whose own FilledAt says
	// something different — long before the open Session's close, exactly
	// at it, and (defensively) after it — and every one must protect
	// through the SAME open Session, generation 5, because a Session that is
	// already open is the one every fill delivered while it is open belongs
	// to (ADR 0021's own per-slice order never delivers a fill for a LATER
	// Session before the open one's own close).
	if generation, ok := tx.protectedSessionGeneration(day(10).AddDate(0, 0, -1)); !ok || generation != 5 {
		t.Errorf("protectedSessionGeneration(long before close) = (%d, %v), want (5, true)", generation, ok)
	}
	if generation, ok := tx.protectedSessionGeneration(day(10)); !ok || generation != 5 {
		t.Errorf("protectedSessionGeneration(exactly at close) = (%d, %v), want (5, true)", generation, ok)
	}
	if generation, ok := tx.protectedSessionGeneration(day(10).AddDate(0, 0, 1)); !ok || generation != 5 {
		t.Errorf("protectedSessionGeneration(after close) = (%d, %v), want (5, true)", generation, ok)
	}
}

func TestProtectedSessionGenerationBeforeAnUpcomingSessionOpens(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	r.sessionOpen = false
	r.hasClosedSession = true
	r.lastClosedSession = day(10)
	r.sessionGeneration = 5
	tx := r.begin()

	// A fill dated AFTER the last closed Session, with none yet open: the
	// backtest open-instant case (stamped at the new Session's own period
	// end) and the live case (reported ahead of that Session's own bar)
	// alike. Both must protect through the Session about to open next.
	if generation, ok := tx.protectedSessionGeneration(day(11)); !ok || generation != 6 {
		t.Errorf("protectedSessionGeneration(day after last close) = (%d, %v), want (6, true)", generation, ok)
	}
}

func TestProtectedSessionGenerationForAFillAtOrBeforeTheLastClosedSessionNeedsNoProtection(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	r.sessionOpen = false
	r.hasClosedSession = true
	r.lastClosedSession = day(10)
	r.sessionGeneration = 5
	tx := r.begin()

	// A fill dated AT the last closed Session's own period end — exactly
	// what internal/fills.RunSession's own intrabar fixpoint delivers,
	// strictly after that Session's own close already decided everything
	// using the correct, still-open Campaign. ADR 0010 already frees this
	// Unit for the Session that opens next: protecting it again here would
	// be one Session too conservative.
	if generation, ok := tx.protectedSessionGeneration(day(10)); ok {
		t.Errorf("protectedSessionGeneration(at the last closed Session) = (%d, %v), want ok=false", generation, ok)
	}
	// A fill dated before it needs no protection either.
	if generation, ok := tx.protectedSessionGeneration(day(9)); ok {
		t.Errorf("protectedSessionGeneration(before the last closed Session) = (%d, %v), want ok=false", generation, ok)
	}
}

func TestProtectedSessionGenerationBeforeAnySessionHasEverClosed(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	r.sessionOpen = false
	r.hasClosedSession = false
	r.sessionGeneration = 0
	tx := r.begin()

	// No Session has ever opened or closed: whatever a fill's own timestamp
	// says, it precedes the very first Session, generation 1.
	if generation, ok := tx.protectedSessionGeneration(day(1)); !ok || generation != 1 {
		t.Errorf("protectedSessionGeneration() = (%d, %v), want (1, true)", generation, ok)
	}
}

// TestRecordUnitsFreedThisSessionAccumulatesAcrossDifferentTimestamps is the
// accumulation property CodeRabbit and Greptile's review flagged directly: a
// reset keyed to filledAt EQUALITY would treat two fills for the SAME
// instrument, timestamped even a second apart, as belonging to different
// Sessions — discarding the first fill's Units the moment the second
// arrived. Keyed to the Session's own generation instead, both stay.
func TestRecordUnitsFreedThisSessionAccumulatesAcrossDifferentTimestamps(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	r.sessionOpen = true
	r.sessionGeneration = 5
	r.instruments = map[string]*instrumentState{"AAA": {}}
	tx := r.begin()

	state, ok := tx.instrument("AAA")
	if !ok {
		t.Fatal("instrument(AAA) not found")
	}
	c := classification{Unclassified: true}

	tx.recordUnitsFreedThisSession(state, c, 1, day(1))
	tx.recordUnitsFreedThisSession(state, c, 1, day(2)) // a different timestamp, same open Session

	if got := tx.freedThisSessionUnits(state); got != 2 {
		t.Errorf("freedThisSessionUnits() = %d, want 2 (both fills accumulated)", got)
	}
}

// TestRecordUnitsFreedThisSessionStartsOverForALaterSession proves the other
// side of the same mechanism: once the reducer has genuinely moved on to a
// LATER Session, a fresh recording for that Session does not inherit an
// earlier Session's stale count.
func TestRecordUnitsFreedThisSessionStartsOverForALaterSession(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	r.sessionOpen = true
	r.sessionGeneration = 5
	r.instruments = map[string]*instrumentState{"AAA": {}}
	tx := r.begin()

	state, ok := tx.instrument("AAA")
	if !ok {
		t.Fatal("instrument(AAA) not found")
	}
	c := classification{Unclassified: true}
	tx.recordUnitsFreedThisSession(state, c, 5, day(1))
	if got := tx.freedThisSessionUnits(state); got != 5 {
		t.Fatalf("freedThisSessionUnits() = %d, want 5", got)
	}

	// Session 5 closes, Session 6 opens.
	tx.sessionGeneration = 6
	tx.recordUnitsFreedThisSession(state, c, 1, day(2))

	if got := tx.freedThisSessionUnits(state); got != 1 {
		t.Errorf("freedThisSessionUnits() = %d, want 1: Session 5's own count must not leak into Session 6's", got)
	}
}

// TestFreedThisSessionUnitsReleasesOnceTheGenerationMovesPast is criterion
// (c) at this mechanism's own seam: a Unit recorded against Session 5 is
// still committed while Session 5 is current, and freely available — 0 —
// the moment Session 6 becomes current, with no further recording at all.
func TestFreedThisSessionUnitsReleasesOnceTheGenerationMovesPast(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	r.sessionOpen = true
	r.sessionGeneration = 5
	r.instruments = map[string]*instrumentState{"AAA": {}}
	tx := r.begin()

	state, ok := tx.instrument("AAA")
	if !ok {
		t.Fatal("instrument(AAA) not found")
	}
	tx.recordUnitsFreedThisSession(state, classification{Unclassified: true}, 3, day(1))
	if got := tx.freedThisSessionUnits(state); got != 3 {
		t.Fatalf("freedThisSessionUnits() = %d, want 3 while Session 5 is current", got)
	}

	tx.sessionGeneration = 6
	if got := tx.freedThisSessionUnits(state); got != 0 {
		t.Errorf("freedThisSessionUnits() = %d, want 0 once Session 6 is current (ADR 0010: freed on t, available on t+1)", got)
	}
}

// TestRecordUnitsFreedThisSessionIsANoOpWhenNoProtectionIsOwed proves a fill
// needing no protection (protectedSessionGeneration's own third case) leaves
// state's bookkeeping untouched — the ordinary internal/fills.RunSession
// intrabar-fixpoint fill, already correctly excluded from the live count it
// closed by the time the NEXT Session decides anything.
func TestRecordUnitsFreedThisSessionIsANoOpWhenNoProtectionIsOwed(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	r.sessionOpen = false
	r.hasClosedSession = true
	r.lastClosedSession = day(10)
	r.sessionGeneration = 5
	r.instruments = map[string]*instrumentState{"AAA": {}}
	tx := r.begin()

	state, ok := tx.instrument("AAA")
	if !ok {
		t.Fatal("instrument(AAA) not found")
	}
	tx.recordUnitsFreedThisSession(state, classification{Unclassified: true}, 4, day(10))

	if state.unitsFreedThisSession != 0 || state.unitsFreedThisSessionGeneration != 0 {
		t.Errorf("state = {%d, generation %d}, want untouched (0, 0): this fill needed no protection", state.unitsFreedThisSession, state.unitsFreedThisSessionGeneration)
	}
	// The Session about to open next (6) must not see it as committed either.
	if got := tx.freedThisSessionUnits(state); got != 0 {
		t.Errorf("freedThisSessionUnits() = %d, want 0", got)
	}
}
