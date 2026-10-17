package targetweight_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
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
	mustRefuse(t, f.h, f.retract("2025-01-30"), "already closed")
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

// TestRetractingTheNextSessionDecidesTheLastOneClosed: when the session
// retracted is the one after the last closed and it was the month's last,
// the session already closed becomes the last before the rebalance. Its
// decision is made at the retraction, from that session's closes and
// universe, exactly as a calendar that never listed the retracted session
// decides it at that session's close, so no decision time passes without a
// decision (ADR 0026, point 1).
func TestRetractingTheNextSessionDecidesTheLastOneClosed(t *testing.T) {
	feed := func(f historyFixture) []event.Envelope {
		mustApply(t, f.h, f.bar("SPY", "2025-01-30", 500))
		mustApply(t, f.h, f.universe("2025-01-30"))
		return mustApply(t, f.h, f.close("2025-01-30", "SPY"))
	}
	never, _ := momentumHandler(t)
	n := historyFixture{t: t, h: never}
	n.s = &stream{hash: never.ConfigurationHash()}
	mustApply(t, never, calendarInput(t, n.s, "calendar", "2025-01-30", "2025-02-03"))
	want := targets(t, feed(n))

	f := newHistoryFixture(t) // 2025-01-30, 01-31, 02-03
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
