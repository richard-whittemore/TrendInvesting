package targetweight_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// TestARetractedSessionIsNoGap is retraction's replay-equivalence test: a
// session the exchange cancelled, listed on the calendar and then retracted,
// decides exactly as a calendar that never listed it does. Unretracted, the
// same session never closes, every window that reaches it has a gap, and
// January's decision is withheld; retracted, the windows fill from the
// sessions before it and January decides.
func TestARetractedSessionIsNoGap(t *testing.T) {
	const cancelled = "2025-01-27" // inside January's last seven sessions
	gap := simulateMomentum(t, momentumOptions{neverClosed: map[string]bool{cancelled: true}})
	if jan := targets(t, gap.decisions)[0]; jan.Outcome != event.TargetsOutcomeWithheld || jan.WithheldReason != event.WithheldMissingBars {
		t.Fatalf("unretracted, January is %s (%s); the fixture should leave a gap", jan.Outcome, jan.WithheldReason)
	}
	unlisted := simulateMomentum(t, momentumOptions{neverClosed: map[string]bool{cancelled: true}, unlisted: map[string]bool{cancelled: true}})
	retracted := simulateMomentum(t, momentumOptions{neverClosed: map[string]bool{cancelled: true}, retract: []string{cancelled}})
	if jan := targets(t, unlisted.decisions)[0]; jan.Outcome != event.TargetsOutcomeDecided {
		t.Fatalf("with no session on %s, January is %s (%s)", cancelled, jan.Outcome, jan.Detail)
	}
	requireSameDecisions(t, unlisted.decisions, retracted.decisions)
}

func (f historyFixture) retract(session string) event.Envelope {
	return f.s.envelope(f.t, event.SessionRetractedEventType, event.SessionRetractedSchemaVersion, "retracted:"+session, closeOf("2025-01-30"),
		event.SessionRetractedPayload{Source: "fixture", Session: session, Reason: "the exchange closed"})
}

// TestSessionRetractionRules: a retraction names a session the calendar
// lists and that is still to come; a second retraction of it changes
// nothing, and a later calendar that still lists it is read without it.
func TestSessionRetractionRules(t *testing.T) {
	f := newHistoryFixture(t) // 2025-01-30, 01-31, 02-03
	mustRefuse(t, f.h, f.retract("2025-02-01"), "not on the calendar")
	mustRefuse(t, f.h, f.s.envelope(t, event.SessionRetractedEventType, event.SessionRetractedSchemaVersion, "bad", closeOf("2025-01-30"), event.SessionRetractedPayload{}), "invalid")
	mustApply(t, f.h, f.bar("SPY", "2025-01-30", 500))
	mustApply(t, f.h, f.close("2025-01-30", "SPY"))
	mustRefuse(t, f.h, f.retract("2025-01-30"), "on or before the last session closed, 2025-01-30, so it can no longer be retracted")
	mustApply(t, f.h, f.retract("2025-02-03"))
	if out := mustApply(t, f.h, f.retract("2025-02-03")); out != nil {
		t.Errorf("a redelivered retraction emitted %d records", len(out))
	}
	mustApply(t, f.h, calendarInput(t, f.s, "retracted only", "2025-02-03"))
	mustApply(t, f.h, calendarInput(t, f.s, "later", "2025-01-31", "2025-02-03", "2025-02-04"))
	mustApply(t, f.h, f.bar("SPY", "2025-01-31", 500))
	if out := mustApply(t, f.h, f.close("2025-01-31", "SPY")); len(targets(t, out)) != 1 {
		t.Errorf("01-31, the last session before February's 02-04, made %d decisions", len(targets(t, out)))
	}
	mustApply(t, f.h, f.bar("SPY", "2025-02-03", 500))
	mustRefuse(t, f.h, f.close("2025-02-03", "SPY"), "not on the calendar")
}

// TestASessionWithBarsCannotBeRetracted: bars for a session say it traded,
// so a retraction of it contradicts them and is refused.
func TestASessionWithBarsCannotBeRetracted(t *testing.T) {
	f := newHistoryFixture(t)
	mustApply(t, f.h, f.bar("SPY", "2025-01-31", 500))
	mustRefuse(t, f.h, f.retract("2025-01-31"), "bars for it have arrived")
}

// TestASessionWithItsUniverseCannotBeRetracted: a universe snapshot of a
// session, and its close histories, say it traded just as its bars do.
func TestASessionWithItsUniverseCannotBeRetracted(t *testing.T) {
	f := newHistoryFixture(t)
	mustApply(t, f.h, f.universe("2025-01-31"))
	mustRefuse(t, f.h, f.retract("2025-01-31"), "its universe snapshot has arrived, so it traded")
	mustApply(t, f.h, f.history("AAA", "2025-01-31", "2024-12-31", close31(20)))
	mustRefuse(t, f.h, f.retract("2025-01-31"), "so it traded")
}

// TestALateRetractionWaitsForTheLastSessionsHistories: when a retraction
// makes the session already closed the month's last, a compact run decides
// it once that session's close histories arrive, exactly as a run whose
// calendar never listed the retracted session decides it at its close; a
// full-bar run, which holds every member's closes already, decides it at the
// retraction, the same again.
func TestALateRetractionWaitsForTheLastSessionsHistories(t *testing.T) {
	const cancelled = "2025-02-28" // February's last; 02-27 becomes it
	never := simulateMomentum(t, momentumOptions{neverClosed: map[string]bool{cancelled: true}, unlisted: map[string]bool{cancelled: true}})
	feb := targets(t, never.decisions)
	if len(feb) != 2 || feb[1].Momentum.CloseSession != "2025-02-27" || feb[1].Outcome != event.TargetsOutcomeDecided {
		t.Fatalf("with no session on %s, the decisions are %+v", cancelled, feb)
	}
	t.Run("compact", func(t *testing.T) {
		late := simulateMomentum(t, momentumOptions{compact: true, lateRetract: cancelled})
		requireSameDecisions(t, never.decisions, late.decisions)
	})
	t.Run("full bars", func(t *testing.T) {
		late := simulateMomentum(t, momentumOptions{lateRetract: cancelled})
		requireSameDecisions(t, never.decisions, late.decisions)
	})
}

// TestALateRetractionWithoutHistoriesIsWithheldAtTheNextClose: when the
// session after the one awaiting its histories closes first, that decision
// is journalled withheld as histories-missing, at that close, and the run
// goes on.
func TestALateRetractionWithoutHistoriesIsWithheldAtTheNextClose(t *testing.T) {
	r := simulateMomentum(t, momentumOptions{compact: true, lateRetract: "2025-02-28", lateHistoriesMissing: true})
	got := targets(t, r.decisions)
	if len(got) != 2 {
		t.Fatalf("%d decisions, want January's and February's", len(got))
	}
	feb := got[1]
	if feb.Outcome != event.TargetsOutcomeWithheld || feb.WithheldReason != event.WithheldHistoriesMissing || feb.Momentum.CloseSession != "2025-02-27" {
		t.Fatalf("February is %s (%s) at %s: %s", feb.Outcome, feb.WithheldReason, feb.Momentum.CloseSession, feb.Detail)
	}
	if !feb.SessionPeriodEnd.Equal(closeOf("2025-02-27")) || !feb.DecisionTime.Equal(closeOf("2025-03-03")) {
		t.Errorf("session end %s and decision time %s, want 02-27's close found at 03-03's", feb.SessionPeriodEnd, feb.DecisionTime)
	}
	if feb.Glide.RebalanceSession != "2025-03-03" {
		t.Errorf("rebalance session %s", feb.Glide.RebalanceSession)
	}
}

// TestRetractingTheNextSessionDecidesTheLastOneClosed: when the session
// retracted is the one after the last closed and it was the month's last,
// the session already closed becomes the last before the rebalance. Its
// decision is made at the retraction, from that session's closes and
// universe, exactly as a calendar that never listed the retracted session
// decides it at that session's close, so no decision time passes without a
// decision (ADR 0026, point 1).
func TestRetractingTheNextSessionDecidesTheLastOneClosed(t *testing.T) {
	feed := func(f historyFixture) []event.Envelope {
		mustApply(t, f.h, f.bar("AAA", "2025-01-30", 20))
		mustApply(t, f.h, f.bar("SPY", "2025-01-30", 500))
		mustApply(t, f.h, f.universe("2025-01-30"))
		return mustApply(t, f.h, f.close("2025-01-30", "AAA", "SPY"))
	}
	never, _ := momentumHandler(t)
	n := historyFixture{t: t, h: never}
	n.s = &stream{hash: never.ConfigurationHash()}
	mustApply(t, never, calendarInput(t, n.s, "calendar", "2025-01-30", "2025-02-03"))
	want := targets(t, feed(n))

	f := newHistoryFixtureReading(t, event.UniverseInputBars) // 2025-01-30, 01-31, 02-03
	if out := feed(f); len(out) != 0 {
		t.Fatalf("01-30 decided with 01-31 still on the calendar: %d records", len(out))
	}
	got := targets(t, mustApply(t, f.h, f.retract("2025-01-31")))
	if len(want) != 1 || len(got) != 1 || want[0].Momentum.CloseSession != "2025-01-30" {
		t.Fatalf("decisions %v at the retraction, want %v", got, want)
	}
	if !equalJSON(t, want[0], got[0]) {
		t.Errorf("the retraction decided %+v, want %+v", got[0], want[0])
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x, y)
}

// TestARetractionMustLeaveASessionAfterTheLastClosed: with nothing after the
// last session closed, whether it was the last before a rebalance cannot be
// known, so the retraction is refused until a calendar reaches further.
func TestARetractionMustLeaveASessionAfterTheLastClosed(t *testing.T) {
	h, s := momentumHandler(t)
	f := historyFixture{t: t, h: h, s: s}
	mustApply(t, h, calendarInput(t, s, "calendar", "2025-01-30", "2025-01-31"))
	mustApply(t, h, f.bar("SPY", "2025-01-30", 500))
	mustApply(t, h, f.close("2025-01-30", "SPY"))
	mustRefuse(t, h, f.retract("2025-01-31"), "no session after")
}

// TestRetractingTheNextSessionMidMonthDecidesNothing: the session after the
// retracted one is in the same month, so the last session closed is still no
// decision session; and one that already was stays decided once.
func TestRetractingTheNextSessionMidMonthDecidesNothing(t *testing.T) {
	h, s := momentumHandler(t)
	f := historyFixture{t: t, h: h, s: s}
	mustApply(t, h, calendarInput(t, s, "calendar", "2025-01-29", "2025-01-30", "2025-01-31", "2025-02-03", "2025-02-04"))
	mustApply(t, h, f.bar("SPY", "2025-01-29", 500))
	mustApply(t, h, f.close("2025-01-29", "SPY"))
	if out := mustApply(t, h, f.retract("2025-01-30")); out != nil {
		t.Errorf("retracting 01-30 after 01-29 emitted %d records", len(out))
	}
	mustApply(t, h, f.bar("SPY", "2025-01-31", 500))
	if out := mustApply(t, h, f.close("2025-01-31", "SPY")); len(targets(t, out)) != 1 {
		t.Fatalf("01-31 made %d decisions", len(targets(t, out)))
	}
	if out := mustApply(t, h, f.retract("2025-02-03")); out != nil {
		t.Errorf("retracting 02-03 after the decided 01-31 emitted %d records", len(out))
	}
}

// compactRun is a momentum run for an owner born in birth, deciding from
// 2025-01-02, over the calendar sessions, whose first session, 2024-12-31,
// is a decision session read from the compact universe input: its universe
// snapshot and the histories of AAA and BBB, which it ranks.
func compactRun(t *testing.T, birth string, sessions ...string) historyFixture {
	t.Helper()
	cfg := momentumConfiguration()
	cfg.Momentum.UniverseInput = event.UniverseInputCompact
	cfg.Glide.Birth = birth
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	f := historyFixture{t: t, h: h, s: &stream{hash: h.ConfigurationHash()}}
	mustApply(t, h, f.s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", closeOf("2024-12-31"), cfg))
	mustApply(t, h, calendarInput(t, f.s, "calendar", append([]string{"2024-12-31"}, sessions...)...))
	mustApply(t, h, f.universe("2024-12-31"))
	for id, price := range map[string]float64{"AAA": 20, "BBB": 10} {
		mustApply(t, h, f.history(id, "2024-12-31", "2024-12-31", event.SessionClose{Session: "2024-12-31", Close: price}))
	}
	f.closeWithSPY("2024-12-31")
	return f
}

func (f historyFixture) closeWithSPY(session string) []event.Envelope {
	f.t.Helper()
	mustApply(f.t, f.h, f.bar("SPY", session, 500))
	return mustApply(f.t, f.h, f.close(session, "SPY"))
}

// TestAPendingDecisionReadsOnlyItsMembersHistories: the decision a late
// retraction left awaiting its histories reads one history per member
// ranked from its universe snapshot, on the calendar and on the base of the
// member's earlier closes.
func TestAPendingDecisionReadsOnlyItsMembersHistories(t *testing.T) {
	f := compactRun(t, "1995-06", "2025-01-02", "2025-01-30", "2025-01-31", "2025-02-03", "2025-02-04")
	f.closeWithSPY("2025-01-02")
	mustApply(t, f.h, f.universe("2025-01-30"))
	f.closeWithSPY("2025-01-30")
	if out := mustApply(t, f.h, f.retract("2025-01-31")); len(out) != 0 {
		t.Fatalf("the compact run decided at the retraction: %d records", len(out))
	}
	on30 := func(price float64) event.SessionClose { return event.SessionClose{Session: "2025-01-30", Close: price} }
	mustRefuse(t, f.h, f.history("ETF", "2025-01-30", "2024-12-31", on30(50)), "but the universe of 2025-01-30 ranks [AAA BBB]")
	mustRefuse(t, f.h, f.history("AAA", "2025-01-30", "2024-12-31", event.SessionClose{Session: "2025-01-29", Close: 20}), "not on the calendar")
	mustApply(t, f.h, f.history("AAA", "2025-01-30", "2024-12-31", on30(20)))
	mustRefuse(t, f.h, f.history("AAA", "2025-01-30", "2024-12-31", on30(20)), "a second close history for AAA awaiting the decision at 2025-01-30")
	mustRefuse(t, f.h, f.history("BBB", "2025-01-30", "2025-01-01", on30(10)), "levels on two bases")
}

// TestAPendingDecisionThatCompletesDecides: the last member's history
// decides the session awaiting them, at that session's close time, from
// that session's universe snapshot, even when the next session's snapshot
// arrived in between.
func TestAPendingDecisionThatCompletesDecides(t *testing.T) {
	// Enough SPY closes for the trend, so the decision ranks its universe.
	f := compactRun(t, "1995-06", "2025-01-02", "2025-01-03", "2025-01-06", "2025-01-07", "2025-01-30", "2025-01-31", "2025-02-03", "2025-02-04")
	for _, session := range []string{"2025-01-02", "2025-01-03", "2025-01-06", "2025-01-07"} {
		f.closeWithSPY(session)
	}
	mustApply(t, f.h, f.universe("2025-01-30"))
	f.closeWithSPY("2025-01-30")
	mustApply(t, f.h, f.retract("2025-01-31"))
	// 02-03's snapshot lists one row, 01-30's three.
	mustApply(t, f.h, f.s.envelope(t, event.UniverseSnapshotEventType, event.UniverseSnapshotSchemaVersion, "universe:2025-02-03", closeOf("2025-02-03"),
		event.UniverseSnapshotPayload{Session: "2025-02-03", Source: "fixture-universe", Rows: []event.UniverseRow{
			{InstrumentID: "AAA", Type: "CS", Close: 20, Volume: 1e6, Sector: "Tech", SectorSource: "gics"},
		}}))
	on30 := func(price float64) event.SessionClose { return event.SessionClose{Session: "2025-01-30", Close: price} }
	if out := mustApply(t, f.h, f.history("AAA", "2025-01-30", "2024-12-31", on30(20))); len(out) != 0 {
		t.Fatalf("one of two histories decided: %d records", len(out))
	}
	got := targets(t, mustApply(t, f.h, f.history("BBB", "2025-01-30", "2024-12-31", on30(10))))
	if len(got) != 1 || got[0].Momentum.CloseSession != "2025-01-30" || !got[0].DecisionTime.Equal(closeOf("2025-01-30")) || got[0].Momentum.UniverseRows != 3 {
		t.Fatalf("decisions %+v", got)
	}
	if out := f.closeWithSPY("2025-02-03"); len(targets(t, out)) != 0 {
		t.Errorf("02-03 journalled %d decisions after 01-30's was made", len(targets(t, out)))
	}
}

// TestALateRetractionBeforeAnyHistoryWaitsForThem: a compact run that has
// read no history yet still waits for the histories of the decision a late
// retraction makes due, and withholds it as histories-missing when the next
// session closes first.
func TestALateRetractionBeforeAnyHistoryWaitsForThem(t *testing.T) {
	h, s := momentumHandlerReading(t, event.UniverseInputCompact)
	f := historyFixture{t: t, h: h, s: s}
	mustApply(t, h, calendarInput(t, s, "calendar", "2025-01-30", "2025-01-31", "2025-02-03", "2025-02-04"))
	mustApply(t, h, f.universe("2025-01-30"))
	f.closeWithSPY("2025-01-30")
	if out := mustApply(t, h, f.retract("2025-01-31")); len(out) != 0 {
		t.Fatalf("the compact run decided at the retraction: %d records", len(out))
	}
	got := targets(t, f.closeWithSPY("2025-02-03"))
	if len(got) != 1 || got[0].WithheldReason != event.WithheldHistoriesMissing || got[0].Momentum.CloseSession != "2025-01-30" {
		t.Fatalf("decisions %+v", got)
	}
}

// TestALateRetractionInABarsRunWithNoRankedMemberBarIsWithheld: the
// decision a late retraction makes due in a run configured for bars is
// withheld as missing-bars when no member ranked from its universe snapshot
// had a bar in that session.
func TestALateRetractionInABarsRunWithNoRankedMemberBarIsWithheld(t *testing.T) {
	h, s := momentumHandler(t)
	f := historyFixture{t: t, h: h, s: s}
	mustApply(t, h, calendarInput(t, s, "calendar", "2025-01-30", "2025-01-31", "2025-02-03", "2025-02-04"))
	mustApply(t, h, f.universe("2025-01-30"))
	f.closeWithSPY("2025-01-30")
	got := targets(t, mustApply(t, h, f.retract("2025-01-31")))
	if len(got) != 1 || got[0].WithheldReason != event.WithheldMissingBars || got[0].Momentum.CloseSession != "2025-01-30" {
		t.Fatalf("decisions %+v", got)
	}
}

// TestAWithheldHistoriesMissingDecisionChecksTheAge: a decision withheld as
// histories-missing records the glide figures, so an implausible age on its
// rebalance session stops the run, whether it is withheld at its own close
// or at the close after a late retraction made it due.
func TestAWithheldHistoriesMissingDecisionChecksTheAge(t *testing.T) {
	const birth = "1915-01" // 110 years in January 2025, one month more in February
	t.Run("at its close", func(t *testing.T) {
		f := compactRun(t, birth, "2025-01-02", "2025-01-31", "2025-02-03")
		f.closeWithSPY("2025-01-02")
		mustApply(t, f.h, f.universe("2025-01-31"))
		mustApply(t, f.h, f.bar("SPY", "2025-01-31", 500))
		mustRefuse(t, f.h, f.close("2025-01-31", "SPY"), "implies an age of 1321 months")
	})
	t.Run("at the next close", func(t *testing.T) {
		f := compactRun(t, birth, "2025-01-02", "2025-01-30", "2025-01-31", "2025-02-03", "2025-02-04")
		f.closeWithSPY("2025-01-02")
		mustApply(t, f.h, f.universe("2025-01-30"))
		f.closeWithSPY("2025-01-30")
		mustApply(t, f.h, f.retract("2025-01-31"))
		mustApply(t, f.h, f.bar("SPY", "2025-02-03", 500))
		mustRefuse(t, f.h, f.close("2025-02-03", "SPY"), "implies an age of 1321 months")
	})
	t.Run("missing-bars at the retraction in a bars run", func(t *testing.T) {
		cfg := momentumConfiguration()
		cfg.Glide.Birth = birth
		h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
		if err != nil {
			t.Fatal(err)
		}
		f := historyFixture{t: t, h: h, s: &stream{hash: h.ConfigurationHash()}}
		mustApply(t, h, f.s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", closeOf("2025-01-02"), cfg))
		mustApply(t, h, calendarInput(t, f.s, "calendar", "2025-01-02", "2025-01-30", "2025-01-31", "2025-02-03"))
		f.closeWithSPY("2025-01-02")
		mustApply(t, h, f.universe("2025-01-30"))
		f.closeWithSPY("2025-01-30")
		mustRefuse(t, h, f.retract("2025-01-31"), "implies an age of 1321 months")
	})
}
