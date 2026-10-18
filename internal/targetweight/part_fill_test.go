package targetweight_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// decisionTag is the ID of session n's strategy.targets.decided.
func decisionTag(n int) string {
	return "targets-decided:joint:" + sessionEnd(n).Format(time.RFC3339)
}

// releasedBy returns the orders released for session n's decision.
func releasedBy(t *testing.T, es []event.Envelope, n int) []event.OrderReleasedPayload {
	t.Helper()
	var out []event.OrderReleasedPayload
	for _, o := range released(t, es) {
		if o.Tag == decisionTag(n) {
			out = append(out, o)
		}
	}
	return out
}

// TestAPartFilledOrderIsReplannedAtTheNextDecision: the first purchase of 39
// shares expires with 20 filled, so the next decided session plans the
// difference to the target in force, though the target has not changed, as
// after a refusal (ADR 0026, point 2, "Boost", amended 2026-10-18), and
// records why: part-filled, with the order, its filled and released shares.
func TestAPartFilledOrderIsReplannedAtTheNextDecision(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{1: 20}})
	first := releasedBy(t, r.decisions, 0)
	if len(first) != 1 || first[0].Quantity != 39 {
		t.Fatalf("first purchase: %+v, want 39 shares", first)
	}
	decided := targets(t, r.decisions)
	d := decided[1]
	if d.Outcome != event.TargetsOutcomeDecided {
		t.Fatalf("session 1: %s %s", d.Outcome, d.Detail)
	}
	want := event.BoostReplan{Cause: event.ReplanCausePartFilled, ClientOrderID: first[0].ClientOrderID, Filled: 20, Released: 39}
	if d.Boost.Replan != want || d.Boost.ReplanPending {
		t.Errorf("session 1 re-plan: %+v pending %v, want %+v and nothing pending after it", d.Boost.Replan, d.Boost.ReplanPending, want)
	}
	// 98% of 4000 at 104 is 37.69 shares; 20 are held: 17 more.
	got := releasedBy(t, r.decisions, 1)
	if len(got) != 1 || got[0].Side != event.OrderSideBuy || got[0].Quantity != 17 {
		t.Fatalf("session 1 releases %+v, want the remaining 17 shares bought", got)
	}
	if decided[2].Boost.Replan != (event.BoostReplan{}) {
		t.Errorf("session 2 records a re-plan: %+v", decided[2].Boost.Replan)
	}
}

// TestAnExpiredUnfilledOrderIsReplannedInFull: the first purchase of 39
// shares expires with nothing filled, so the next decided session plans the
// whole difference to the target in force, recording the cause unfilled
// with the order and its released shares (ADR 0026, point 2, "Boost",
// amended 2026-10-18, the extension of the part-fill rule to no fill).
func TestAnExpiredUnfilledOrderIsReplannedInFull(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{1: 0}})
	first := releasedBy(t, r.decisions, 0)
	d := targets(t, r.decisions)[1]
	want := event.BoostReplan{Cause: event.ReplanCauseUnfilled, ClientOrderID: first[0].ClientOrderID, Released: 39}
	if d.Boost.Replan != want || d.Boost.ReplanPending {
		t.Errorf("session 1 re-plan: %+v pending %v, want %+v", d.Boost.Replan, d.Boost.ReplanPending, want)
	}
	// 98% of 4000 at 104 is 37.69 shares, none held: 37.
	if got := releasedBy(t, r.decisions, 1); len(got) != 1 || got[0].Side != event.OrderSideBuy || got[0].Quantity != 37 {
		t.Errorf("session 1 releases %+v, want 37 shares bought", got)
	}
}

// TestAFullyFilledOrderIsNotReplanned: an order filled in full, whether the
// venue then reports it canceled or not, leaves nothing to plan.
func TestAFullyFilledOrderIsNotReplanned(t *testing.T) {
	for name, opts := range map[string]options{
		"filled in full":    {},
		"filled at the cap": {partFill: map[int]int64{1: 39}},
	} {
		t.Run(name, func(t *testing.T) {
			r := simulate(t, opts)
			if got := releasedBy(t, r.decisions, 1); len(got) != 0 {
				t.Errorf("session 1 releases %+v, want nothing", got)
			}
			for i, d := range targets(t, r.decisions) {
				if d.Boost.Replan != (event.BoostReplan{}) || d.Boost.ReplanPending {
					t.Errorf("session %d records a re-plan: %+v pending %v", i, d.Boost.Replan, d.Boost.ReplanPending)
				}
			}
		})
	}
}

// TestAWithheldSessionKeepsThePartFillReplan: a withheld session plans
// nothing; it records the re-plan still pending and its cause, and the next
// decided session plans it.
func TestAWithheldSessionKeepsThePartFillReplan(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{1: 20}, noSnap: map[int]bool{1: true}})
	decided := targets(t, r.decisions)
	if d := decided[1]; d.Outcome != event.TargetsOutcomeWithheld || !d.Boost.ReplanPending || d.Boost.Replan.Cause != event.ReplanCausePartFilled {
		t.Fatalf("session 1: %s pending %v %+v, want withheld with the part-fill re-plan pending", d.Outcome, d.Boost.ReplanPending, d.Boost.Replan)
	}
	if d := decided[2]; d.Boost.ReplanPending || d.Boost.Replan.Cause != event.ReplanCausePartFilled || d.Boost.Replan.Filled != 20 {
		t.Errorf("session 2: pending %v %+v, want the part-fill re-planned", d.Boost.ReplanPending, d.Boost.Replan)
	}
	if got := releasedBy(t, r.decisions, 2); len(got) != 1 || got[0].Side != event.OrderSideBuy {
		t.Errorf("session 2 releases %+v, want the remainder bought", got)
	}
}

// TestALaterOrderSupersedesAPartFill: the entry bought at session 16 fills 5
// shares, and the venue reports it expired only after the exit has been
// planned at session 20. The exit was sized from the account as it stood,
// so the late report plans nothing: re-planning then would trade drift.
func TestALaterOrderSupersedesAPartFill(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{17: 5}, cancelAt: map[int]int{17: 21}})
	if exit := releasedBy(t, r.decisions, 20); len(exit) != 1 || exit[0].Side != event.OrderSideSell {
		t.Fatalf("session 20 releases %+v, want the exit", exit)
	}
	if got := releasedBy(t, r.decisions, 21); len(got) != 0 {
		t.Errorf("session 21 releases %+v, want nothing", got)
	}
	for i, d := range targets(t, r.decisions) {
		if d.Boost.Replan != (event.BoostReplan{}) || d.Boost.ReplanPending {
			t.Errorf("session %d records a re-plan: %+v pending %v", i, d.Boost.Replan, d.Boost.ReplanPending)
		}
	}
}

// TestARunRecoveredFromAPendingPartFillReplansIt: the withheld decision
// carries the pending re-plan and its cause, so a run restarted from it
// (ADR 0026, point 5) plans the remainder at its first decided session.
func TestARunRecoveredFromAPendingPartFillReplansIt(t *testing.T) {
	full := simulate(t, options{partFill: map[int]int64{1: 20}, noSnap: map[int]bool{1: true}})
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[1]
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs})
	d := targets(t, r.decisions)
	if len(d) == 0 || d[0].Boost.Replan.Cause != event.ReplanCausePartFilled || d[0].Boost.Replan.Filled != 20 {
		t.Fatalf("first recovered decision does not re-plan the part-fill: %d decisions", len(d))
	}
	want := releasedBy(t, full.decisions, 2)
	got := releasedBy(t, r.decisions, 2)
	if len(got) != 1 || len(want) != 1 || got[0].Quantity != want[0].Quantity {
		t.Errorf("recovered run releases %+v at session 2, the full run %+v", got, want)
	}
}

// TestACancelRecordedAfterItsExpirySessionIsStillReplanned: the first
// purchase of 39 shares fills 20 at session 1, but the venue's cancel is
// recorded only after session 1's close. Session 1 decides, the target
// unchanged, and plans nothing; that does not end the watch on the order,
// which is still working, so when the cancel arrives the next decided
// session plans the remainder (ADR 0026, point 2, "Boost", amended
// 2026-10-18). 20 shares bought at 104 leave 4000 - 2080 = 1920 in cash; at
// session 2's close of 108 net liquidation is 1920 + 20 x 108 = 4080, and
// 98% of it is 3998.4 / 108 = 37.02 shares: 17 more.
func TestACancelRecordedAfterItsExpirySessionIsStillReplanned(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{1: 20}, cancelAt: map[int]int{1: 2}})
	first := releasedBy(t, r.decisions, 0)
	if got := releasedBy(t, r.decisions, 1); len(got) != 0 {
		t.Fatalf("session 1 releases %+v, want nothing: the target is unchanged and the order still works", got)
	}
	d := targets(t, r.decisions)[2]
	want := event.BoostReplan{Cause: event.ReplanCausePartFilled, ClientOrderID: first[0].ClientOrderID, Filled: 20, Released: 39}
	if d.Outcome != event.TargetsOutcomeDecided || d.Boost.Replan != want {
		t.Errorf("session 2: %s re-plan %+v, want decided with %+v", d.Outcome, d.Boost.Replan, want)
	}
	if got := releasedBy(t, r.decisions, 2); len(got) != 1 || got[0].Side != event.OrderSideBuy || got[0].Quantity != 17 {
		t.Errorf("session 2 releases %+v, want the remaining 17 shares bought", got)
	}
}

// TestAPartFillCompletedAfterAWithheldCloseIsNotReplanned: the first
// purchase is cancelled at session 1 with 20 of 39 filled, and session 1 is
// withheld, so the re-plan is pending; the venue then reports the other 19
// executed before session 2's close. The order filled in full, so session 2
// plans nothing: re-planning would trade the price's drift toward an
// unchanged target.
func TestAPartFillCompletedAfterAWithheldCloseIsNotReplanned(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{1: 20}, noSnap: map[int]bool{1: true}, lateFill: map[int]lateFill{1: {at: 2, shares: 19}}})
	decided := targets(t, r.decisions)
	if d := decided[1]; d.Outcome != event.TargetsOutcomeWithheld || !d.Boost.ReplanPending {
		t.Fatalf("session 1: %s pending %v, want withheld with the part-fill re-plan pending", d.Outcome, d.Boost.ReplanPending)
	}
	if d := decided[2]; d.Outcome != event.TargetsOutcomeDecided || d.Boost.ReplanPending || d.Boost.Replan != (event.BoostReplan{}) {
		t.Errorf("session 2: %s pending %v %+v, want decided with no re-plan", d.Outcome, d.Boost.ReplanPending, d.Boost.Replan)
	}
	if got := releasedBy(t, r.decisions, 2); len(got) != 0 {
		t.Errorf("session 2 releases %+v, want nothing: the order filled in full", got)
	}
}

// TestAPendingPartFillReplanReadsTheLatestFills: the first purchase is
// cancelled at session 1 with 20 of 39 filled and 10 more are reported at
// session 2; both sessions are withheld. Session 2 records the re-plan's
// fill as it then stands, 30, and session 3 plans the rest: 20 at 104 and 10
// at 108 leave 4000 - 2080 - 1080 = 840 in cash; at 112 net liquidation is
// 840 + 30 x 112 = 4200, 98% of it 4116 / 112 = 36.75 shares: 6 more.
func TestAPendingPartFillReplanReadsTheLatestFills(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{1: 20}, noSnap: map[int]bool{1: true, 2: true}, lateFill: map[int]lateFill{1: {at: 2, shares: 10}}})
	first := releasedBy(t, r.decisions, 0)
	want := event.BoostReplan{Cause: event.ReplanCausePartFilled, ClientOrderID: first[0].ClientOrderID, Filled: 30, Released: 39}
	decided := targets(t, r.decisions)
	if d := decided[2]; !d.Boost.ReplanPending || d.Boost.Replan != want {
		t.Errorf("session 2: pending %v %+v, want %+v", d.Boost.ReplanPending, d.Boost.Replan, want)
	}
	if d := decided[3]; d.Boost.Replan != want {
		t.Errorf("session 3 re-plans %+v, want %+v", d.Boost.Replan, want)
	}
	if got := releasedBy(t, r.decisions, 3); len(got) != 1 || got[0].Side != event.OrderSideBuy || got[0].Quantity != 6 {
		t.Errorf("session 3 releases %+v, want the remaining 6 shares bought", got)
	}
}

// TestAVenueRejectionIsReplannedOnceThenWithheld: the venue refuses the
// first purchase before it rests (invalid), a reason that may last, so the
// next decided session re-plans it once, recording the cause rejected
// (ADR 0026, point 2, "Boost", amended 2026-10-18): 98% of 4000 at 104 is
// 37.69 shares, none held, so 37. When the venue rejects that order too, no
// session plans it again: session 2 records the re-plan withheld, naming
// the order and its shares, which the reporter pages, and plans nothing,
// as after a refusal under Degraded or Halted.
func TestAVenueRejectionIsReplannedOnceThenWithheld(t *testing.T) {
	r := simulate(t, options{reject: map[int]bool{1: true, 2: true}})
	first := releasedBy(t, r.decisions, 0)
	decided := targets(t, r.decisions)
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: first[0].ClientOrderID, Released: 39}
	if d := decided[1]; d.Boost.Replan != want {
		t.Errorf("session 1 re-plan %+v, want %+v", d.Boost.Replan, want)
	}
	second := releasedBy(t, r.decisions, 1)
	if len(second) != 1 || second[0].Side != event.OrderSideBuy || second[0].Quantity != 37 {
		t.Fatalf("session 1 releases %+v, want the 37 shares re-planned", second)
	}
	d := decided[2]
	withheld := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: second[0].ClientOrderID, Released: 37}
	if d.Boost.ReplanWithheld != withheld || d.Boost.ReplanPending || d.Boost.Replan != (event.BoostReplan{}) {
		t.Errorf("session 2: withheld %+v pending %v re-plan %+v, want %+v withheld and nothing pending", d.Boost.ReplanWithheld, d.Boost.ReplanPending, d.Boost.Replan, withheld)
	}
	for n := 2; n < 16; n++ {
		if got := releasedBy(t, r.decisions, n); len(got) != 0 {
			t.Errorf("session %d releases %+v, want nothing until the target changes", n, got)
		}
		if n > 2 && decided[n].Boost.ReplanWithheld != (event.BoostReplan{}) {
			t.Errorf("session %d records the withheld re-plan again", n)
		}
	}
}

// TestACancelAfterARejectionIsReplannedAsAnyOther: a rejection's one
// re-plan that then expires part-filled is re-planned as any part-fill.
func TestACancelAfterARejectionIsReplannedAsAnyOther(t *testing.T) {
	r := simulate(t, options{reject: map[int]bool{1: true}, partFill: map[int]int64{2: 10}})
	second := releasedBy(t, r.decisions, 1)
	d := targets(t, r.decisions)[2]
	want := event.BoostReplan{Cause: event.ReplanCausePartFilled, ClientOrderID: second[0].ClientOrderID, Filled: 10, Released: 37}
	if d.Boost.Replan != want || d.Boost.ReplanWithheld != (event.BoostReplan{}) {
		t.Errorf("session 2 re-plan %+v withheld %+v, want %+v", d.Boost.Replan, d.Boost.ReplanWithheld, want)
	}
	if got := releasedBy(t, r.decisions, 2); len(got) != 1 || got[0].Side != event.OrderSideBuy {
		t.Errorf("session 2 releases %+v, want the remainder bought", got)
	}
}

// TestARunRecoveredWithAnOrderWorkingStillReplansItsRemainder: the
// earlier run's last decision, session 0's, released the first purchase,
// which is still working when the run restarts from it. The decision
// records the watched order, so the restarted run reads the venue's reports
// on it, 20 of 39 filled and then cancelled at session 1, and plans the
// remainder at session 1 as the full run does (ADR 0026, points 2 and 5).
func TestARunRecoveredWithAnOrderWorkingStillReplansItsRemainder(t *testing.T) {
	full := simulate(t, options{partFill: map[int]int64{1: 20}})
	order := releasedBy(t, full.decisions, 0)
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[0]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	want := event.BoostWatchedOrder{ClientOrderID: order[0].ClientOrderID, Side: event.OrderSideBuy, Released: 39, LimitPrice: order[0].LimitPrice}
	if !p.Boost.Watched.Equal(want) {
		t.Fatalf("session 0 records the watched order %+v, want %+v", p.Boost.Watched, want)
	}
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, partFill: map[int]int64{1: 20},
		working: map[int][]event.OrderReleasedPayload{1: order}})
	got, original := targets(t, r.decisions)[0], targets(t, full.decisions)[1]
	if got.Boost.Replan != original.Boost.Replan || got.Boost.Replan.Cause != event.ReplanCausePartFilled {
		t.Errorf("recovered session 1 re-plans %+v, the full run %+v", got.Boost.Replan, original.Boost.Replan)
	}
	gotOrder, wantOrder := releasedBy(t, r.decisions, 1), releasedBy(t, full.decisions, 1)
	if len(gotOrder) != 1 || len(wantOrder) != 1 || gotOrder[0].Quantity != wantOrder[0].Quantity {
		t.Errorf("recovered run releases %+v at session 1, the full run %+v", gotOrder, wantOrder)
	}
}

// TestARunRecoveredMidPartFillCountsTheFillsBeforeIt: a run restarted from
// the withheld decision after a part-fill keeps the 20 shares filled before
// the restart, so the 19 the venue reports after it complete the order and
// nothing is re-planned.
func TestARunRecoveredMidPartFillCountsTheFillsBeforeIt(t *testing.T) {
	opts := options{partFill: map[int]int64{1: 20}, noSnap: map[int]bool{1: true, 2: true}, lateFill: map[int]lateFill{1: {at: 3, shares: 19}}}
	full := simulate(t, opts)
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[1]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	if p.Boost.Watched.Filled != 20 || p.Boost.Watched.Status != event.OrderLifecycleStatusCanceled {
		t.Fatalf("session 1 records the watched order %+v, want 20 filled and canceled", p.Boost.Watched)
	}
	order := releasedBy(t, full.decisions, 0)[0]
	late := event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: order.ClientOrderID, FillID: "fill-late:" + order.ClientOrderID,
		InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 19, Price: 112, FilledAt: sessionEnd(3).Add(-80 * time.Minute)}
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, noSnap: opts.noSnap,
		fills: map[int][]event.TargetWeightFillPayload{3: {late}}})
	d := targets(t, r.decisions)
	if len(d) < 2 || d[1].Outcome != event.TargetsOutcomeDecided || d[1].Boost.Replan != (event.BoostReplan{}) {
		t.Fatalf("recovered session 3 must decide with no re-plan: %+v", d)
	}
	if got := releasedBy(t, r.decisions, 3); len(got) != 0 {
		t.Errorf("recovered session 3 releases %+v, want nothing: the order filled in full", got)
	}
}

// inputFill returns the execution an earlier run's inputs reported under id.
func inputFill(t *testing.T, inputs []event.Envelope, id string) event.TargetWeightFillPayload {
	t.Helper()
	for _, in := range inputs {
		if in.Type == event.FillEventType && in.ID == id {
			var f event.TargetWeightFillPayload
			decode(t, in, &f)
			return f
		}
	}
	t.Fatalf("no fill %s in the earlier run's inputs", id)
	return event.TargetWeightFillPayload{}
}

// TestARunRecoveredCountsAnExecutionFromBeforeItsDecisionOnce: the earlier
// run's last decision, session 1's, records the watched order cancelled
// with 20 of 39 filled, and the execution itself. The venue re-reports that
// execution after the restart; the restarted run knows it, so it counts once,
// as on an order this run released (ADR 0019, "Explained means"), and
// session 3 re-plans from 20 filled, as the full run does.
func TestARunRecoveredCountsAnExecutionFromBeforeItsDecisionOnce(t *testing.T) {
	opts := options{partFill: map[int]int64{1: 20}, noSnap: map[int]bool{1: true, 2: true}}
	full := simulate(t, opts)
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[1]
	order := releasedBy(t, full.decisions, 0)[0]
	before := inputFill(t, full.inputs, "fill:"+order.ClientOrderID)
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, noSnap: opts.noSnap,
		fills: map[int][]event.TargetWeightFillPayload{2: {before}}})
	got, want := targets(t, r.decisions), targets(t, full.decisions)[3]
	if len(got) < 2 || got[1].Boost.Replan != want.Boost.Replan || want.Boost.Replan.Filled != 20 {
		t.Fatalf("recovered session 3 re-plans %+v, the full run %+v", got, want.Boost.Replan)
	}
	if g, w := releasedBy(t, r.decisions, 3), releasedBy(t, full.decisions, 3); len(g) != 1 || len(w) != 1 || g[0].Quantity != w[0].Quantity {
		t.Errorf("recovered run releases %+v at session 3, the full run %+v", g, w)
	}
}

// TestARunRecoveredReadsAFillRecordedAfterItsDecisionOnce: the earlier run
// recorded a further execution of 10 shares after its last decision,
// session 1's, and stopped. The restarted run's decision does not hold it,
// so when the venue sends it again, twice, beside the execution the
// decision does hold, it counts once: 30 filled, and session 3 buys the 6
// the full run buys (TestAPendingPartFillReplanReadsTheLatestFills).
func TestARunRecoveredReadsAFillRecordedAfterItsDecisionOnce(t *testing.T) {
	opts := options{partFill: map[int]int64{1: 20}, noSnap: map[int]bool{1: true, 2: true}, lateFill: map[int]lateFill{1: {at: 2, shares: 10}}}
	full := simulate(t, opts)
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[1]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	if p.Boost.Watched.Filled != 20 {
		t.Fatalf("session 1 records %d filled, want the 20 before the later execution", p.Boost.Watched.Filled)
	}
	order := releasedBy(t, full.decisions, 0)[0]
	before := inputFill(t, full.inputs, "fill:"+order.ClientOrderID)
	after := inputFill(t, full.inputs, "fill-late:"+order.ClientOrderID)
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, noSnap: opts.noSnap,
		fills: map[int][]event.TargetWeightFillPayload{2: {before, after, after}}})
	got, want := targets(t, r.decisions), targets(t, full.decisions)[3]
	if len(got) < 2 || got[1].Boost.Replan != want.Boost.Replan || want.Boost.Replan.Filled != 30 {
		t.Fatalf("recovered session 3 re-plans %+v, the full run %+v", got, want.Boost.Replan)
	}
	if g := releasedBy(t, r.decisions, 3); len(g) != 1 || g[0].Quantity != 6 {
		t.Errorf("recovered run releases %+v at session 3, want the 6 the full run buys", g)
	}
}

// TestARecoveryKeepsAnOrderReleasedBeforeIt: a broker snapshot read before
// the recovered decision turns the margin alarm urgent, and the sale back to
// 98% is released. The recovered decision adds its watched order to the
// orders this run released and drops none, so the venue's reports on the
// sale are still read.
func TestARecoveryKeepsAnOrderReleasedBeforeIt(t *testing.T) {
	at := sessionEnd(0)
	h, s := configured(t)
	held := &account{cash: -2000, held: 60}
	urgent := held.broker(at.Add(-2*time.Hour), 100)
	urgent.ExcessLiquidity = 0.1 * urgent.NetLiquidation
	sale := released(t, mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k", urgent.AsOf, urgent)))
	if len(sale) != 1 {
		t.Fatalf("the urgent broker snapshot released %+v, want the sale back to 98%%", sale)
	}
	full := simulate(t, options{partFill: map[int]int64{1: 20}})
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[0]
	mustApply(t, h, s.envelope(t, recovery.Type, recovery.SchemaVersion, recovery.ID, at, recovery.Payload))
	for _, o := range []event.OrderReleasedPayload{sale[0], releasedBy(t, full.decisions, 0)[0]} {
		ack := event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "ib-" + o.ClientOrderID, InstrumentID: o.InstrumentID,
			Side: o.Side, Status: event.OrderLifecycleStatusSubmitted, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at.Add(-time.Hour)}
		mustApply(t, h, s.envelope(t, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack:"+o.ClientOrderID, ack.OccurredAt, ack))
	}
}

// TestARefusedRejectionReplanIsStillItsOneReplan: the venue rejects the
// first purchase, and the kill switch refuses its re-plan at session 1. The
// re-plan released at session 2, once the switch is off, is still the
// rejection's one re-plan (ADR 0026, point 2, "Boost", amended 2026-10-18),
// so when the venue rejects it too, session 3 records it withheld and
// plans nothing.
func TestARefusedRejectionReplanIsStillItsOneReplan(t *testing.T) {
	r := simulate(t, options{reject: map[int]bool{1: true, 3: true}, kill: map[int]bool{1: true, 2: false}})
	assertRejectedTwiceIsWithheld(t, r)
}

// TestARunRecoveredFromARefusedRejectionReplanStillBoundsIt: a run
// restarted from session 1's decision, whose rejection re-plan the kill
// switch refused, recovers the re-plan as the rejection's, so the venue's
// rejection of the order it releases at session 2 is withheld at session 3.
func TestARunRecoveredFromARefusedRejectionReplanStillBoundsIt(t *testing.T) {
	opts := options{reject: map[int]bool{1: true, 3: true}, kill: map[int]bool{1: true, 2: false}}
	full := simulate(t, opts)
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[1]
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, reject: opts.reject})
	assertRejectedTwiceIsWithheld(t, r)
}

// assertRejectedTwiceIsWithheld checks that the order released at session 2
// and rejected at session 3 is recorded withheld at session 3, and that
// nothing is released then.
func assertRejectedTwiceIsWithheld(t *testing.T, r run) {
	t.Helper()
	if got := releasedBy(t, r.decisions, 1); len(got) != 0 {
		t.Fatalf("session 1 releases %+v, want the re-plan refused by the kill switch", got)
	}
	second := releasedBy(t, r.decisions, 2)
	if len(second) != 1 || second[0].Side != event.OrderSideBuy {
		t.Fatalf("session 2 releases %+v, want the re-plan once the switch is off", second)
	}
	var d event.TargetsDecidedPayload
	for _, x := range targets(t, r.decisions) {
		if x.SessionPeriodEnd.Equal(sessionEnd(3)) {
			d = x
		}
	}
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: second[0].ClientOrderID, Released: second[0].Quantity}
	if d.Boost.ReplanWithheld != want || d.Boost.ReplanPending {
		t.Errorf("session 3: withheld %+v pending %v, want %+v withheld and nothing pending", d.Boost.ReplanWithheld, d.Boost.ReplanPending, want)
	}
	if got := releasedBy(t, r.decisions, 3); len(got) != 0 {
		t.Errorf("session 3 releases %+v, want nothing: the rejection's one re-plan was rejected too", got)
	}
}

// TestASecondRejectionWithARefusalPendingIsStillWithheld: the venue
// rejects the first purchase, and its one re-plan, released at session 1,
// works until session 17. The entry at session 16 is refused by the kill
// switch, so a refusal's re-plan is pending when the venue rejects the
// re-planned order at session 17: that rejection is still recorded
// withheld, and paged, at session 17's decision.
func TestASecondRejectionWithARefusalPendingIsStillWithheld(t *testing.T) {
	hold := map[int]bool{}
	for i := 2; i <= 16; i++ {
		hold[i] = true
	}
	r := simulate(t, options{reject: map[int]bool{1: true, 17: true}, hold: hold, kill: map[int]bool{16: true, 17: false}})
	second := releasedBy(t, r.decisions, 1)
	if len(second) != 1 {
		t.Fatalf("session 1 releases %+v, want the rejection's one re-plan", second)
	}
	decided := targets(t, r.decisions)
	if d := decided[16]; !d.Boost.ReplanPending || len(releasedBy(t, r.decisions, 16)) != 0 {
		t.Fatalf("session 16: pending %v, released %+v; want the entry refused and re-planned", d.Boost.ReplanPending, releasedBy(t, r.decisions, 16))
	}
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: second[0].ClientOrderID, Released: second[0].Quantity}
	if got := decided[17].Boost.ReplanWithheld; got != want {
		t.Errorf("session 17 withheld %+v, want %+v", got, want)
	}
}

// TestASecondRejectionReadDuringARecoveredRunsWarmUpIsStillRecorded: a run
// restarted from session 1's decision watches the rejection's one re-plan;
// the venue's rejection of it is read at session 1, which only feeds the
// indicators, so the first decision after the warm-up, session 2's, records
// it withheld, as the full run's session 2 does, and the reporter pages it.
func TestASecondRejectionReadDuringARecoveredRunsWarmUpIsStillRecorded(t *testing.T) {
	full := simulate(t, options{reject: map[int]bool{1: true, 2: true}})
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[1]
	second := releasedBy(t, full.decisions, 1)
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, working: map[int][]event.OrderReleasedPayload{1: second}, reject: map[int]bool{1: true}})
	got, want := targets(t, r.decisions), targets(t, full.decisions)[2]
	if len(got) == 0 {
		t.Fatal("the recovered run decides nothing")
	}
	if got[0].Boost.ReplanWithheld != want.Boost.ReplanWithheld || want.Boost.ReplanWithheld.Cause != event.ReplanCauseRejected {
		t.Fatalf("recovered session 2 records withheld %+v, the full run %+v", got[0].Boost.ReplanWithheld, want.Boost.ReplanWithheld)
	}
	if g := releasedBy(t, r.decisions, 2); len(g) != 0 {
		t.Errorf("recovered session 2 releases %+v, want nothing", g)
	}
	for _, d := range got[1:] {
		if d.Boost.ReplanWithheld != (event.BoostReplan{}) {
			t.Errorf("session %s records the withheld re-plan again", d.SessionPeriodEnd)
		}
	}
}

// TestAPartFillRunIsByteIdenticalAndReplays: the re-plan is derived from the
// journal's own reports, so a rerun writes the same bytes and the journal
// replays to the same decisions.
func TestAPartFillRunIsByteIdenticalAndReplays(t *testing.T) {
	opts := options{partFill: map[int]int64{1: 20}}
	assertByteIdenticalAndReplays(t, opts)
}

// assertByteIdenticalAndReplays runs opts twice, compares the journals byte
// for byte and replays the first through a fresh handler.
func assertByteIdenticalAndReplays(t *testing.T, opts options) {
	t.Helper()
	write := func(r run) []byte {
		header, err := r.recorder.Header(r.hash, strategyVersion)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := journal.Write(&buf, header, r.entries); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	first := write(simulate(t, opts))
	if !bytes.Equal(first, write(simulate(t, opts))) {
		t.Fatal("rerunning the same inputs must write a byte-identical journal")
	}
	_, records, err := journal.Read(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	inputs, decisions, err := journal.Split(records)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := targetweight.New(strategyVersion, testConfiguration(), targetweight.Options{DecideFrom: sessionEnd(0)})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := replay.New(handler)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if d := replay.Equivalent(decisions, replayed); d != nil {
		t.Fatalf("replay diverged: %+v", d)
	}
}
