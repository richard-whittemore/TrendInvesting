package targetweight_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
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
// the order and its shares, which the reporter pages, and no session
// orders again, the entry at session 16 included: two of the account's
// orders rejected in a row wait for the owner's fresh run.
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
	if got := planned(t, r.decisions); len(got) != 2 {
		t.Errorf("the run plans %d orders, want the first purchase and its one re-plan only: nothing, the entry at session 16 included, until the owner starts a fresh run", len(got))
	}
	for n := 2; n < len(decided); n++ {
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

// TestARunRecoveredWatchingASellSizesLaterSellsNetOfIt: the earlier run's
// session-20 decision released Boost's exit, a sell of 17, which is still
// working, unfilled, when the run restarts from that decision. The
// restarted run restores it as a working sell (ADR 0026, point 5; ADR 0019,
// amended 2026-10-15, every sale sized "net of every working sell"), so when
// the margin alarm turns urgent at session 21, the sale back to 98% nets the
// exit's 17 shares and sells what the full run sells, not those shares
// again.
func TestARunRecoveredWatchingASellSizesLaterSellsNetOfIt(t *testing.T) {
	hold := map[int]bool{21: true}
	urgent := map[int]func(*event.BrokerSnapshotPayload){21: func(b *event.BrokerSnapshotPayload) {
		b.NetLiquidation *= 0.8
		b.ExcessLiquidity = 0.1 * b.NetLiquidation
	}}
	full := simulate(t, options{brokers: true, hold: hold, brokerMutate: urgent})
	exit := releasedBy(t, full.decisions, 20)
	if len(exit) != 1 || exit[0].Side != event.OrderSideSell {
		t.Fatalf("session 20 releases %+v, want the exit", exit)
	}
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[20]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	if w := p.Boost.Watched; w.Side != event.OrderSideSell || w.ClientOrderID != exit[0].ClientOrderID || w.Status != "" {
		t.Fatalf("session 20 records the watched order %+v, want the exit still working", w)
	}
	// The earlier run's broker snapshots, the urgent one included, are fed
	// as recorded.
	r := simulate(t, options{brokers: true, recovery: &recovery, snapshots: full.inputs, hold: hold,
		working: map[int][]event.OrderReleasedPayload{21: exit}})
	got, want := sales(t, r.decisions), sales(t, full.decisions)
	if len(want) != 1 || want[0].Sale.WorkingSells != exit[0].Quantity {
		t.Fatalf("the full run's sale back to 98%%: %+v, want one netting the exit's %d shares", want, exit[0].Quantity)
	}
	if len(got) != 1 || got[0].Quantity != want[0].Quantity || got[0].Sale != want[0].Sale {
		t.Errorf("the recovered run's sale back to 98%%: %+v, want the full run's %+v", got, want)
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

// TestTwoRejectionsInARowAwaitTheOwnerAcrossATargetChange: the first
// purchase fills 30 of 39 shares, the venue rejects its re-plan, and the
// kill switch refuses the rejection's one re-plan at every session until
// Boost's entry at session 16 changes the target to 147%. The order to the
// new target is a new order, but the venue rejects it too: two of the
// account's orders in a row rejected, whatever their targets, so session 17
// records it withheld (paged once) and no session buys again until the
// owner starts a fresh run (ADR 0026, point 2, "Boost", amended 2026-10-18:
// the bound of two rejections in a row). The account holds less than 98%,
// so the exit at session 20 plans a buy back toward it, which waits too;
// an exit that sells still goes (TestTwoRejectionsInARowStillSendBoostsExit).
// Every decision from session 17 records the two rejections.
func TestTwoRejectionsInARowAwaitTheOwnerAcrossATargetChange(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{1: 30}, reject: map[int]bool{2: true, 17: true}, kill: map[int]bool{2: true, 16: false}})
	decided := targets(t, r.decisions)
	if d := decided[15]; !d.Boost.ReplanPending || d.Boost.Replan.Cause != event.ReplanCauseRejected || d.Boost.VenueRejections != 1 {
		t.Fatalf("session 15: pending %v re-plan %+v rejections %d, want the rejection's one re-plan still refused, one rejection counted",
			d.Boost.ReplanPending, d.Boost.Replan, d.Boost.VenueRejections)
	}
	entry := releasedBy(t, r.decisions, 16)
	if len(entry) != 1 || entry[0].Side != event.OrderSideBuy {
		t.Fatalf("session 16 releases %+v, want the entry once the switch is off", entry)
	}
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: entry[0].ClientOrderID, Released: entry[0].Quantity}
	if d := decided[17]; d.Boost.ReplanWithheld != want || d.Boost.ReplanPending || d.Boost.Replan != (event.BoostReplan{}) {
		t.Errorf("session 17: withheld %+v pending %v re-plan %+v, want %+v withheld and nothing pending", d.Boost.ReplanWithheld, d.Boost.ReplanPending, d.Boost.Replan, want)
	}
	if d := decided[20]; d.Targets[0].Weight != 0.98 || d.Boost.TargetInForce != 1.47 {
		t.Fatalf("session 20 decides %v from %v, want Boost's exit", d.Targets[0].Weight, d.Boost.TargetInForce)
	}
	for n := 17; n < len(decided); n++ {
		if got := releasedBy(t, r.decisions, n); len(got) != 0 {
			t.Errorf("session %d releases %+v, want nothing until the owner starts a fresh run", n, got)
		}
		if d := decided[n]; d.Boost.VenueRejections != 2 || d.Boost.ReplanPending {
			t.Errorf("session %d: rejections %d pending %v, want the two rejections recorded and nothing pending", n, d.Boost.VenueRejections, d.Boost.ReplanPending)
		}
		if n > 17 && decided[n].Boost.ReplanWithheld != (event.BoostReplan{}) {
			t.Errorf("session %d records the withheld order again", n)
		}
	}
}

// TestTwoRejectionsInARowStillSendBoostsExit: the entry released at
// session 16 fills 5 shares and expires; the venue rejects its re-plan and
// that re-plan's one re-plan, so session 19 records the second rejection in
// a row and Boost's buys stop. The stop never blocks an order that reduces
// exposure (ADR 0026, point 4's exemption; point 2, "Boost", amended
// 2026-10-18): Boost's exit at session 20 is still planned and released, a
// sell down to 98%, and no buy is released from session 19 on. When the
// venue rejects the exit too, session 21 records that rejection as the
// first while buys are stopped (paged as such, not as the stop) and, the
// exit still being due, plans and releases it again; a run restarted from
// session 21's decision counts on from it.
func TestTwoRejectionsInARowStillSendBoostsExit(t *testing.T) {
	for name, exitRejected := range map[string]bool{"the exit accepted": false, "the exit rejected once": true} {
		t.Run(name, func(t *testing.T) {
			reject := map[int]bool{18: true, 19: true}
			if exitRejected {
				reject[21] = true
			}
			r := simulate(t, options{partFill: map[int]int64{17: 5}, reject: reject})
			decided := targets(t, r.decisions)
			if d := decided[19]; d.Boost.VenueRejections != 2 || d.Boost.ReplanWithheld.Cause != event.ReplanCauseRejected {
				t.Fatalf("session 19: rejections %d withheld %+v, want the second rejection in a row recorded", d.Boost.VenueRejections, d.Boost.ReplanWithheld)
			}
			if d := decided[20]; d.Targets[0].Weight != 0.98 || d.Boost.TargetInForce != 1.47 {
				t.Fatalf("session 20 decides %v from %v, want Boost's exit", d.Targets[0].Weight, d.Boost.TargetInForce)
			}
			exit := releasedBy(t, r.decisions, 20)
			if len(exit) != 1 || exit[0].Side != event.OrderSideSell {
				t.Fatalf("session 20 releases %+v, want Boost's exit sold while buys are stopped", exit)
			}
			for n := 19; n < len(decided); n++ {
				for _, o := range releasedBy(t, r.decisions, n) {
					if o.Side == event.OrderSideBuy {
						t.Errorf("session %d releases a buy %+v while buys are stopped", n, o)
					}
				}
			}
			if !exitRejected {
				return
			}
			want := []event.BoostReplan{{Cause: event.ReplanCauseRejected, ClientOrderID: exit[0].ClientOrderID, Released: exit[0].Quantity}}
			if d := decided[21]; !slices.Equal(d.Boost.RejectedWhileStopped, want) || d.Boost.RejectionsWhileStopped != 1 || d.Boost.ReplanWithheld != (event.BoostReplan{}) {
				t.Errorf("session 21 records %+v (%d while stopped), withheld %+v; want the exit's rejection %+v, the first while buys are stopped, and not the stop again",
					d.Boost.RejectedWhileStopped, d.Boost.RejectionsWhileStopped, d.Boost.ReplanWithheld, want)
			}
			for n := 22; n < len(decided); n++ {
				if d := decided[n]; len(d.Boost.RejectedWhileStopped) != 0 || d.Boost.RejectionsWhileStopped != 1 {
					t.Errorf("session %d: rejected %+v, %d while stopped; want it recorded once and counted on", n, d.Boost.RejectedWhileStopped, d.Boost.RejectionsWhileStopped)
				}
			}
			again := releasedBy(t, r.decisions, 21)
			// Re-planned from the account at session 21's close, so sized afresh.
			if len(again) != 1 || again[0].Side != event.OrderSideSell {
				t.Errorf("session 21 releases %+v, want the exit sent again", again)
			}
			assertRecoveredAsTheFullRun(t, options{partFill: map[int]int64{17: 5}, reject: reject}, 21)
		})
	}
}

// TestAnAcceptedOrderReplacedBeforeItsEndStillEndsTheRunOfRejections: the
// entry released at session 16 fills 5 shares and expires, and the venue
// rejects its re-plan (one rejection). That re-plan's one re-plan, released
// at session 18, rests unfilled until it expires at session 20, its cancel
// delivered only at session 21. Boost's exit at session 20 replaces it as
// the watched order before that cancel is read, works through session 21,
// and the venue rejects it at session 22. The order between the two
// rejections rested and was not rejected, and its cancel was read first
// (ADR 0026, point 2, "Boost", amended 2026-10-18: an order the venue
// accepts sets the rejections in a row back to none), so the exit's
// rejection is the first in a row: session 22 re-plans it once and
// withholds nothing.
func TestAnAcceptedOrderReplacedBeforeItsEndStillEndsTheRunOfRejections(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{17: 5, 20: 0}, reject: map[int]bool{18: true},
		hold: map[int]bool{19: true, 21: true}, cancelAt: map[int]int{20: 21}, invalidAt: map[int][]int{22: {20}}})
	exit := releasedBy(t, r.decisions, 20)
	if len(exit) != 1 || exit[0].Side != event.OrderSideSell {
		t.Fatalf("session 20 releases %+v, want Boost's exit", exit)
	}
	decided := targets(t, r.decisions)
	if d := decided[21]; d.Boost.VenueRejections != 0 {
		t.Fatalf("session 21: rejections %d, want none once the cancel is read", d.Boost.VenueRejections)
	}
	d := decided[22]
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: exit[0].ClientOrderID, Released: exit[0].Quantity}
	if d.Boost.ReplanWithheld != (event.BoostReplan{}) || d.Boost.VenueRejections != 1 || d.Boost.Replan != want {
		t.Errorf("session 22: withheld %+v rejections %d re-plan %+v, want nothing withheld, one rejection, %+v re-planned",
			d.Boost.ReplanWithheld, d.Boost.VenueRejections, d.Boost.Replan, want)
	}
	if got := releasedBy(t, r.decisions, 22); len(got) != 1 || got[0].Side != event.OrderSideSell {
		t.Errorf("session 22 releases %+v, want the exit's one re-plan", got)
	}
}

// TestARejectionAfterAnOrderTheVenueAcceptedIsReplannedOnce: the venue
// rejects the first purchase, its re-plan is accepted and expires with 10
// filled, and the venue rejects the re-plan of that remainder. An order the
// venue accepted sets the rejections in a row back to none, so that
// rejection is the first
// in a row: session 3 re-plans it once, cause rejected, and withholds
// nothing.
func TestARejectionAfterAnOrderTheVenueAcceptedIsReplannedOnce(t *testing.T) {
	r := simulate(t, options{reject: map[int]bool{1: true, 3: true}, partFill: map[int]int64{2: 10}})
	third := releasedBy(t, r.decisions, 2)
	if len(third) != 1 {
		t.Fatalf("session 2 releases %+v, want the part-fill's remainder", third)
	}
	d := targets(t, r.decisions)[3]
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: third[0].ClientOrderID, Released: third[0].Quantity}
	if d.Boost.Replan != want || d.Boost.ReplanWithheld != (event.BoostReplan{}) || d.Boost.VenueRejections != 1 {
		t.Errorf("session 3: re-plan %+v withheld %+v rejections %d, want %+v re-planned, nothing withheld, one rejection",
			d.Boost.Replan, d.Boost.ReplanWithheld, d.Boost.VenueRejections, want)
	}
	if got := releasedBy(t, r.decisions, 3); len(got) != 1 || got[0].Side != event.OrderSideBuy {
		t.Errorf("session 3 releases %+v, want the rejection's one re-plan", got)
	}
}

// TestARunRecoveredAwaitingTheOwnerOrdersNothing: a run restarted from the
// decision that recorded the second rejection in a row recovers it, so it
// releases no buy, the exit at session 20 (a buy back toward 98% here)
// included, and pages nothing again, as the full run (ADR 0026, point 5).
func TestARunRecoveredAwaitingTheOwnerOrdersNothing(t *testing.T) {
	opts := options{partFill: map[int]int64{1: 30}, reject: map[int]bool{2: true, 17: true}, kill: map[int]bool{2: true, 16: false}}
	full := simulate(t, opts)
	decisions := ofType(full.decisions, event.TargetsDecidedEventType)
	recovery := decisions[17]
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs})
	if len(released(t, r.decisions)) != 0 {
		t.Errorf("the recovered run releases %+v, want nothing until the owner starts a fresh run", released(t, r.decisions))
	}
	got := ofType(r.decisions, event.TargetsDecidedEventType)
	want := decisions[18:]
	if len(got) != len(want) {
		t.Fatalf("the recovered run records %d decisions, the full run %d after session 17", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Errorf("session %d: the recovered run records\n%s\nthe full run\n%s", 18+i, got[i].Payload, want[i].Payload)
		}
	}
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
// withheld, and paged, at session 17's decision, and the refusal's re-plan
// is not ordered: the account waits for the owner.
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
	if d := decided[17]; d.Boost.ReplanPending || len(releasedBy(t, r.decisions, 17)) != 0 {
		t.Errorf("session 17: pending %v, released %+v; want the refusal's re-plan dropped: two rejections in a row await the owner",
			d.Boost.ReplanPending, releasedBy(t, r.decisions, 17))
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

// TestARunRecoveredFromATargetChangeRefusedAfterARejectionDecidesAsTheFullRun:
// the venue rejects the first purchase's re-plan at session 2, and the kill
// switch refuses the rejection's one re-plan through session 16, whose
// decision changes the target to 147% (Boost's entry) and has its order to
// the new target refused too. That order was a new order, not the
// rejection's re-plan (the bound holds only while the target is
// unchanged), so the full run carries a refusal's re-plan out of session
// 16, though the decision records the rejection it entered with. A run
// restarted from session 16's decision must read it the same way (ADR 0026,
// point 5: recovered, never recomputed, and read as the full run read it):
// every decision it records from session 17 on is the full run's, byte for
// byte.
func TestARunRecoveredFromATargetChangeRefusedAfterARejectionDecidesAsTheFullRun(t *testing.T) {
	opts := options{partFill: map[int]int64{1: 30}, reject: map[int]bool{2: true, 18: true}, kill: map[int]bool{2: true, 17: false}}
	full := simulate(t, opts)
	decisions := ofType(full.decisions, event.TargetsDecidedEventType)
	recovery := decisions[16]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	if p.Boost.Replan.Cause != event.ReplanCauseRejected || !p.Boost.ReplanPending || p.Targets[0].Weight == p.Boost.TargetInForce {
		t.Fatalf("session 16: re-plan %+v pending %v, target %v from %v; want the rejection entered, a re-plan pending and the target changed",
			p.Boost.Replan, p.Boost.ReplanPending, p.Targets[0].Weight, p.Boost.TargetInForce)
	}
	if got := releasedBy(t, full.decisions, 16); len(got) != 0 {
		t.Fatalf("session 16 releases %+v, want the order to the new target refused", got)
	}
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, reject: opts.reject, kill: opts.kill})
	got := ofType(r.decisions, event.TargetsDecidedEventType)
	want := decisions[17:]
	if len(got) != len(want) {
		t.Fatalf("the recovered run records %d decisions, the full run %d after session 16", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Errorf("session %d: the recovered run records\n%s\nthe full run\n%s", 17+i, got[i].Payload, want[i].Payload)
		}
	}
}

// TestADecisionRecordsTheWatchedOrdersVenueOrderID: the venue's lifecycle
// report names the watched order's venue order ID, and every decision after
// it records that ID with the order, so a run restarted from the decision
// can match IBKR's listing of it when no client order ID is echoed (ADR
// 0026, point 5, amended 2026-10-18).
func TestADecisionRecordsTheWatchedOrdersVenueOrderID(t *testing.T) {
	r := simulate(t, options{hold: map[int]bool{1: true}})
	order := releasedBy(t, r.decisions, 0)[0]
	if got := targets(t, r.decisions)[0].Boost.Watched.VenueOrderID; got != "" {
		t.Errorf("session 0 records venue order ID %q before any report on the order", got)
	}
	if got := targets(t, r.decisions)[1].Boost.Watched.VenueOrderID; got != "ib-"+order.ClientOrderID {
		t.Errorf("session 1 records venue order ID %q, want the lifecycle report's %q", got, "ib-"+order.ClientOrderID)
	}
}

// TestARunRecoveredWatchingASellMatchesItsListingByVenueOrderID: Boost's
// exit, a sell of 17 released at session 20, works unfilled through session
// 22, when the margin alarm turns urgent and IBKR lists the exit with no
// client order ID echoed. The full run knows the exit's venue order ID from
// the venue's report, and a run restarted from session 21's decision from
// the decision, so both net the exit's 17 shares once in the sale back to
// 98%, never twice (ADR 0019, amended 2026-10-15, "net of every working
// sell").
func TestARunRecoveredWatchingASellMatchesItsListingByVenueOrderID(t *testing.T) {
	hold := map[int]bool{21: true, 22: true}
	exit := releasedBy(t, simulate(t, options{brokers: true, hold: hold}).decisions, 20)
	if len(exit) != 1 || exit[0].Side != event.OrderSideSell {
		t.Fatalf("session 20 releases %+v, want the exit", exit)
	}
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) {
		b.NetLiquidation *= 0.8
		b.ExcessLiquidity = 0.1 * b.NetLiquidation
		b.OpenOrders = []event.BrokerOrder{{BrokerOrderID: "ib-" + exit[0].ClientOrderID, InstrumentID: "SPY", Side: event.OrderSideSell,
			Status: event.BrokerOrderWorking, RemainingQuantity: exit[0].Quantity}}
	}}
	full := simulate(t, options{brokers: true, hold: hold, brokerMutate: urgent})
	want := sales(t, full.decisions)
	if len(want) != 1 || want[0].Sale.WorkingSells != exit[0].Quantity {
		t.Fatalf("the full run's sale back to 98%%: %+v, want one netting the exit's %d shares once", want, exit[0].Quantity)
	}
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[21]
	r := simulate(t, options{brokers: true, recovery: &recovery, snapshots: full.inputs, hold: hold})
	got := sales(t, r.decisions)
	if len(got) != 1 || got[0].Quantity != want[0].Quantity || got[0].Sale != want[0].Sale {
		t.Errorf("the recovered run's sale back to 98%%: %+v, want the full run's %+v", got, want)
	}
}

// assertRecoveredAsTheFullRun restarts a run from session n's decision of
// the run opts describes and feeds it that run's own venue reports after
// session n: every decision it records, and every order it releases, must
// be the full run's, byte for byte (ADR 0026, point 5: a restart recovers,
// never recomputes).
func assertRecoveredAsTheFullRun(t *testing.T, opts options, n int) {
	t.Helper()
	full := simulate(t, opts)
	decisions := ofType(full.decisions, event.TargetsDecidedEventType)
	recovery := decisions[n]
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, reports: venueReports(full.inputs, n), kill: opts.kill, closes: opts.closes, mutate: opts.mutate})
	got, want := ofType(r.decisions, event.TargetsDecidedEventType), decisions[n+1:]
	if len(got) != len(want) {
		t.Fatalf("the recovered run records %d decisions, the full run %d after session %d", len(got), len(want), n)
	}
	for i := range want {
		if !bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Errorf("session %d: the recovered run records\n%s\nthe full run\n%s", n+1+i, got[i].Payload, want[i].Payload)
		}
	}
	for s := n + 1; s < len(decisions); s++ {
		g, w := releasedBy(t, r.decisions, s), releasedBy(t, full.decisions, s)
		if len(g) != len(w) || len(g) == 1 && g[0] != w[0] {
			t.Errorf("session %d: the recovered run releases %+v, the full run %+v", s, g, w)
		}
	}
}

// TestARunRecoveredBehindAnUndecidedOrderCountsAsTheFullRun: the venue
// rejects the part-fill's re-plan at session 18 (one rejection); its one
// re-plan, released at session 18, rests unfilled, and Boost's exit at
// session 20 replaces it as the watched order before its cancel arrives, at
// session 21, after the venue rejects the exit. The exit's rejection is
// read first and is the second in a row, so buys stop; the cancel read
// after it does not lift the stop (ADR 0026, point 2, "Boost", amended
// 2026-10-18). A run restarted from session 20's decision recovers the
// order still undecided there and reads its cancel as the full run does.
func TestARunRecoveredBehindAnUndecidedOrderCountsAsTheFullRun(t *testing.T) {
	opts := options{partFill: map[int]int64{17: 5, 20: 0}, reject: map[int]bool{18: true, 21: true},
		hold: map[int]bool{19: true}, cancelAt: map[int]int{20: 21}}
	full := simulate(t, opts)
	resting := releasedBy(t, full.decisions, 18)
	undecided := func(d event.BoostFigures) bool {
		return slices.ContainsFunc(d.UndecidedOrders, func(o event.BoostUndecidedOrder) bool { return o.ClientOrderID == resting[0].ClientOrderID })
	}
	if len(resting) != 1 || !undecided(targets(t, full.decisions)[20].Boost) {
		t.Fatalf("setup: session 18 releases %+v, want one order still undecided at session 20", resting)
	}
	if d := targets(t, full.decisions)[21].Boost; d.VenueRejections != 2 || undecided(d) {
		t.Fatalf("session 21: rejections %d, undecided %+v; want the stop and the resting order ruled on", d.VenueRejections, d.UndecidedOrders)
	}
	assertRecoveredAsTheFullRun(t, opts, 20)
}

// TestARunRecoveredWatchingAPartFilledOrderCountsAsTheFullRun: the venue
// rejects the first purchase; its one re-plan fills 10 shares at session 2
// and is still working when session 2 decides, so the venue accepted it and
// the rejections in a row are back to none. The venue then rejects its remainder
// (session 3) and the order planned after it (session 4): the full run
// counts that last rejection as the first in a row. A run restarted from
// session 2's decision counts the same, and does not stop buys.
func TestARunRecoveredWatchingAPartFilledOrderCountsAsTheFullRun(t *testing.T) {
	opts := options{reject: map[int]bool{1: true, 4: true}, partFill: map[int]int64{2: 10}, cancelAt: map[int]int{2: 40},
		invalidAt: map[int][]int{3: {1}}}
	full := simulate(t, opts)
	if d := targets(t, full.decisions)[4]; d.Boost.VenueRejections != 1 || d.Boost.ReplanWithheld != (event.BoostReplan{}) {
		t.Fatalf("session 4: rejections %d withheld %+v, want one rejection counted and nothing withheld", d.Boost.VenueRejections, d.Boost.ReplanWithheld)
	}
	assertRecoveredAsTheFullRun(t, opts, 2)
}

// TestALateRejectionThatStopsBuysIsRecordedAndPaged: Boost's entry,
// released at session 16, works unfilled with no word from the venue but
// submitted. Boost's exit at session 20 replaces it as the watched order,
// and the venue rejects the exit at session 21: one rejection, the first
// read, so the exit, a sell, is planned once more. The venue then rejects the entry at session 22: two of
// the account's orders rejected in a row, so buys stop, though the watched
// order, the exit sent again, fills. The decision of session 22, at whose
// close that rejection is read, records the order that completed the run
// (paged once, ADR 0026, point 2, "Boost", amended 2026-10-18), and no
// session buys again; a run restarted from session 21's decision does the
// same.
func TestALateRejectionThatStopsBuysIsRecordedAndPaged(t *testing.T) {
	opts := options{hold: map[int]bool{17: true, 18: true, 19: true, 20: true, 21: true}, invalidAt: map[int][]int{21: {20}, 22: {16}}}
	r := simulate(t, opts)
	entry, exit, again := releasedBy(t, r.decisions, 16), releasedBy(t, r.decisions, 20), releasedBy(t, r.decisions, 21)
	if len(entry) != 1 || entry[0].Side != event.OrderSideBuy || len(exit) != 1 || exit[0].Side != event.OrderSideSell || len(again) != 1 {
		t.Fatalf("want the entry, the exit and the exit again released; got %+v, %+v, %+v", entry, exit, again)
	}
	decided := targets(t, r.decisions)
	if d := decided[21]; d.Boost.VenueRejections != 1 || d.Boost.ReplanWithheld != (event.BoostReplan{}) {
		t.Fatalf("session 21: rejections %d withheld %+v, want one rejection", d.Boost.VenueRejections, d.Boost.ReplanWithheld)
	}
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: entry[0].ClientOrderID, Released: entry[0].Quantity}
	if d := decided[22]; d.Boost.VenueRejections != 2 || d.Boost.ReplanWithheld != want || !d.Boost.Watched.IsZero() {
		t.Errorf("session 22: rejections %d withheld %+v watched %+v, want two, %+v recorded and the exit filled", d.Boost.VenueRejections, d.Boost.ReplanWithheld, d.Boost.Watched, want)
	}
	for n := 22; n < len(decided); n++ {
		for _, o := range releasedBy(t, r.decisions, n) {
			if o.Side == event.OrderSideBuy {
				t.Errorf("session %d releases a buy %+v while buys are stopped", n, o)
			}
		}
		if n > 22 && decided[n].Boost.ReplanWithheld != (event.BoostReplan{}) {
			t.Errorf("session %d records the stop again", n)
		}
	}
	assertRecoveredAsTheFullRun(t, opts, 21)
}

// TestTheRejectionsInARowStayAtTwo: the venue rejects the part-fill's
// re-plan (one rejection); its one re-plan works through session 20, when
// Boost's exit is released. At session 21 the venue rejects the exit, then
// the order before it: three in a row, but the count stops at two, as the
// stop it marks (ADR 0026, point 2, "Boost", amended 2026-10-18). The
// exit's rejection, read second, made it two and is recorded as the stop;
// the one re-plan's, read after it, is recorded as the first rejection
// while buys are stopped; the exit, a sell, is planned again.
func TestTheRejectionsInARowStayAtTwo(t *testing.T) {
	opts := options{partFill: map[int]int64{17: 5}, reject: map[int]bool{18: true}, hold: map[int]bool{19: true, 20: true},
		invalidAt: map[int][]int{21: {20, 18}}}
	r := simulate(t, opts)
	replan, exit := releasedBy(t, r.decisions, 18), releasedBy(t, r.decisions, 20)
	if len(replan) != 1 || len(exit) != 1 || exit[0].Side != event.OrderSideSell {
		t.Fatalf("want the rejection's one re-plan at session 18 and the exit at 20; got %+v and %+v", replan, exit)
	}
	d := targets(t, r.decisions)[21]
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: exit[0].ClientOrderID, Released: exit[0].Quantity}
	after := []event.BoostReplan{{Cause: event.ReplanCauseRejected, ClientOrderID: replan[0].ClientOrderID, Released: replan[0].Quantity}}
	if d.Boost.VenueRejections != 2 || d.Boost.ReplanWithheld != want || !slices.Equal(d.Boost.RejectedWhileStopped, after) || d.Boost.RejectionsWhileStopped != 1 {
		t.Errorf("session 21: rejections %d withheld %+v, rejected while stopped %+v (%d); want two, %+v recorded, then %+v",
			d.Boost.VenueRejections, d.Boost.ReplanWithheld, d.Boost.RejectedWhileStopped, d.Boost.RejectionsWhileStopped, want, after)
	}
	if got := releasedBy(t, r.decisions, 21); len(got) != 1 || got[0].Side != event.OrderSideSell {
		t.Errorf("session 21 releases %+v, want the exit sent again", got)
	}
}

// TestAnExitPartFilledWhileBuysAreStoppedIsReplannedNotPaged: after two
// rejections in a row stop Boost's buys, the exit released at session 20
// fills one share and expires at session 21. The venue's first word on it
// was an execution, so its cancel is not a rejection: nothing is recorded
// as rejected while buys are stopped, and session 21 re-plans the rest of
// the sale, part-filled (ADR 0026, point 2, "Boost", amended 2026-10-18,
// and point 4's exemption).
func TestAnExitPartFilledWhileBuysAreStoppedIsReplannedNotPaged(t *testing.T) {
	r := simulate(t, options{partFill: map[int]int64{17: 5, 21: 1}, reject: map[int]bool{18: true, 19: true}})
	exit := releasedBy(t, r.decisions, 20)
	if len(exit) != 1 || exit[0].Side != event.OrderSideSell || exit[0].Quantity < 2 {
		t.Fatalf("session 20 releases %+v, want Boost's exit of more than one share", exit)
	}
	d := targets(t, r.decisions)[21]
	want := event.BoostReplan{Cause: event.ReplanCausePartFilled, ClientOrderID: exit[0].ClientOrderID, Filled: 1, Released: exit[0].Quantity}
	if d.Boost.Replan != want || len(d.Boost.RejectedWhileStopped) != 0 || d.Boost.RejectionsWhileStopped != 0 || d.Boost.VenueRejections != 2 {
		t.Errorf("session 21: re-plan %+v, rejected while stopped %+v (%d), rejections %d; want %+v, nothing rejected, the stop kept",
			d.Boost.Replan, d.Boost.RejectedWhileStopped, d.Boost.RejectionsWhileStopped, d.Boost.VenueRejections, want)
	}
	if got := releasedBy(t, r.decisions, 21); len(got) != 1 || got[0].Side != event.OrderSideSell {
		t.Errorf("session 21 releases %+v, want the rest of the exit sold", got)
	}
}

// TestWhatTheRejectionsRecordStaysBoundedWhileBuysAreStopped: the venue
// rejects the re-plan and its one re-plan (the stop, session 19), then
// rejects Boost's exit at every session, which is planned and released
// again each time (ADR 0026, point 2, "Boost"). Each decision records the
// rejections in a row and the orders not yet ruled on (point 5), and
// neither grows with the sessions since the stop: the two rejections that
// made the stop, and at most the one order released since the venue's last
// word.
func TestWhatTheRejectionsRecordStaysBoundedWhileBuysAreStopped(t *testing.T) {
	closes := syntheticCloses()
	for i := range 40 {
		closes = append(closes, 146+float64(float64(i%3)*0.1))
	}
	reject := map[int]bool{18: true, 19: true}
	for i := 21; i < len(closes); i++ {
		reject[i] = true
	}
	opts := options{closes: closes, partFill: map[int]int64{17: 5}, reject: reject}
	r := simulate(t, opts)
	decided := targets(t, r.decisions)
	if len(decided) < 60 {
		t.Fatalf("setup: %d decisions, want a long stop", len(decided))
	}
	for n := 19; n < len(decided); n++ {
		d := decided[n].Boost
		if d.VenueRejections != 2 || len(d.RejectedInARow) != 2 || len(d.UndecidedOrders) > 1 {
			t.Fatalf("session %d: %d rejections naming %v, undecided %v; want the two that stopped buys and at most the latest order",
				n, d.VenueRejections, d.RejectedInARow, d.UndecidedOrders)
		}
	}
	assertRecoveredAsTheFullRun(t, opts, 40)
}

// flippingCloses is an invented series that rises for sixteen sessions and
// then dips and rises in turn, so the latch enters and exits again and
// again. With nothing executing after session 1 (heldFrom), the account
// keeps the first purchase alone, and each entry releases a buy of the
// boost it never gets: at sessions 16, 18, 23, 27, 32, 36, 41 and 45, with
// a sell of one share of drift at 17. Every order stays undecided, the
// venue saying nothing of it but submitted, unless a test rejects it.
func flippingCloses() []float64 {
	closes := make([]float64, 0, 50)
	for i := range 16 {
		closes = append(closes, float64(100+4*i))
	}
	return append(closes, 150.5, 155.5, 149.5, 153, 159, 165, 171, 158, 165, 171, 177, 164, 171, 177, 183, 189,
		175.5, 182.5, 188.5, 194.5, 181.5, 188.5, 194.5, 200.5, 206.5, 193, 200, 206, 212, 199, 206, 212, 218, 224)
}

// heldFrom holds every order working from session n on: nothing executes
// and nothing ends.
func heldFrom(n int) map[int]bool {
	hold := map[int]bool{}
	for i := n; i < len(flippingCloses()); i++ {
		hold[i] = true
	}
	return hold
}

// TestARunWithManyUndecidedOrdersCountsTheRejectionAndRecoversAsTheFullRun:
// the venue rejects the buy released at session 16 (one rejection) and says
// nothing but submitted of the eight orders released after it. No order is
// dropped from what the count needs: every decision from session 17 counts
// that one rejection, and a run restarted from any of them decides as the
// full run, byte for byte (ADR 0026, point 2, "Boost", amended 2026-10-18,
// and point 5: recovered, never recomputed).
func TestARunWithManyUndecidedOrdersCountsTheRejectionAndRecoversAsTheFullRun(t *testing.T) {
	opts := options{closes: flippingCloses(), hold: heldFrom(2), invalidAt: map[int][]int{17: {16}}}
	full := simulate(t, opts)
	decided := targets(t, full.decisions)
	if len(released(t, full.decisions)) < 10 {
		t.Fatalf("setup: %d orders released, want the eight undecided after the rejection", len(released(t, full.decisions)))
	}
	for n := 17; n < len(decided); n++ {
		if d := decided[n].Boost; d.VenueRejections != 1 || d.ReplanWithheld != (event.BoostReplan{}) {
			t.Errorf("session %d: rejections %d withheld %+v, want the one rejection counted", n, d.VenueRejections, d.ReplanWithheld)
		}
	}
	for n := 17; n < len(decided)-1; n++ {
		assertRecoveredAsTheFullRun(t, opts, n)
	}
}

// TestALateRejectionAfterManyUndecidedOrdersStopsBuys: as above, the venue
// rejects session 16's buy, then, at session 46, after eight more orders
// were released, rejects session 17's sell, which it had said nothing of:
// two rejections since the last order it accepted, so buys stop, and the
// decision of session 46 records that sell as the rejection that made it
// two (paged once; ADR 0026, point 2, "Boost", amended 2026-10-18). A run
// restarted from session 45's decision, with the sell still undecided,
// reads its late rejection as the full run does; so does one restarted from
// session 46's, where the count reached two.
func TestALateRejectionAfterManyUndecidedOrdersStopsBuys(t *testing.T) {
	opts := options{closes: flippingCloses(), hold: heldFrom(2), invalidAt: map[int][]int{17: {16}, 46: {17}}}
	r := simulate(t, opts)
	sell := releasedBy(t, r.decisions, 17)
	if len(sell) != 1 || sell[0].Side != event.OrderSideSell {
		t.Fatalf("setup: session 17 releases %+v, want one sell", sell)
	}
	decided := targets(t, r.decisions)
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: sell[0].ClientOrderID, Released: sell[0].Quantity}
	if d := decided[46].Boost; d.VenueRejections != 2 || d.ReplanWithheld != want {
		t.Errorf("session 46: rejections %d withheld %+v, want two and %+v recorded", d.VenueRejections, d.ReplanWithheld, want)
	}
	for n := 46; n < len(decided); n++ {
		for _, o := range releasedBy(t, r.decisions, n) {
			if o.Side == event.OrderSideBuy {
				t.Errorf("session %d releases a buy %+v while buys are stopped", n, o)
			}
		}
	}
	assertRecoveredAsTheFullRun(t, opts, 45)
	assertRecoveredAsTheFullRun(t, opts, 46)
}

// TestALateRejectionOfAnOrderReleasedBeforeTheStopIsRecordedWhileStopped:
// seven orders released from session 16 stay undecided; the venue then
// rejects the buys of sessions 41 and 45, which stops buys at session 46.
// At session 47 it rejects session 16's buy, the oldest of the undecided:
// a rejection read after the stop, recorded once as such and counted
// (ADR 0026, points 2, "Boost", amended 2026-10-18, and 4), never lost. A
// run restarted from session 46's decision, with that buy still undecided,
// records it as the full run does.
func TestALateRejectionOfAnOrderReleasedBeforeTheStopIsRecordedWhileStopped(t *testing.T) {
	opts := options{closes: flippingCloses(), hold: heldFrom(2), invalidAt: map[int][]int{42: {41}, 46: {45}, 47: {16}}}
	r := simulate(t, opts)
	entry := releasedBy(t, r.decisions, 16)
	if len(entry) != 1 || entry[0].Side != event.OrderSideBuy {
		t.Fatalf("setup: session 16 releases %+v, want one buy", entry)
	}
	decided := targets(t, r.decisions)
	if d := decided[46].Boost; d.VenueRejections != 2 || d.ReplanWithheld.ClientOrderID == "" {
		t.Fatalf("setup: session 46 rejections %d withheld %+v, want the stop", d.VenueRejections, d.ReplanWithheld)
	}
	want := []event.BoostReplan{{Cause: event.ReplanCauseRejected, ClientOrderID: entry[0].ClientOrderID, Released: entry[0].Quantity}}
	if d := decided[47].Boost; !slices.Equal(d.RejectedWhileStopped, want) || d.RejectionsWhileStopped != 1 {
		t.Errorf("session 47: rejected while stopped %+v (%d), want %+v, the first since the stop", d.RejectedWhileStopped, d.RejectionsWhileStopped, want)
	}
	assertRecoveredAsTheFullRun(t, opts, 46)
}

// stoppedBy is the client order IDs a decision names as the rejections that
// stopped Boost's buys.
func stoppedBy(d event.BoostFigures) []string {
	var out []string
	for _, o := range d.RejectedInARow {
		out = append(out, o.ClientOrderID)
	}
	return out
}

// TestTheStopNamesTheTwoRejectionsThatMadeIt: the venue rejects the
// part-fill's re-plan and its one re-plan (sessions 18 and 19), which stops
// buys, then Boost's exit at every session from 21, planned and released
// again each time. Every decision from the stop on names the two orders
// whose rejections stopped buys, not the newest rejections, which are each
// recorded once as rejected while stopped (ADR 0026, point 2, "Boost",
// amended 2026-10-18).
func TestTheStopNamesTheTwoRejectionsThatMadeIt(t *testing.T) {
	closes := syntheticCloses()
	for i := range 20 {
		closes = append(closes, 146+float64(float64(i%3)*0.1))
	}
	reject := map[int]bool{17: true, 18: true}
	for i := 21; i < len(closes); i++ {
		reject[i] = true
	}
	r := simulate(t, options{closes: closes, partFill: map[int]int64{16: 5}, reject: reject})
	first, second := releasedBy(t, r.decisions, 16), releasedBy(t, r.decisions, 17)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("setup: sessions 16 and 17 release %+v and %+v, want one order each", first, second)
	}
	decided := targets(t, r.decisions)
	want := []string{first[0].ClientOrderID, second[0].ClientOrderID}
	for n := 18; n < len(decided); n++ {
		if got := stoppedBy(decided[n].Boost); !slices.Equal(got, want) {
			t.Fatalf("session %d names %v as the stop, want %v", n, got, want)
		}
	}
}

// wholeBoostInOneOrder lets one order buy the whole boosted target from
// cash, as the entry does when the first purchase never filled.
func wholeBoostInOneOrder(cfg *event.TargetWeightConfigurationPayload) {
	cfg.Limits.MaxOrderFraction = 1.5
}

// TestALateAcceptanceOfAnOlderOrderDoesNotEndTheRunOfRejections: the first
// purchase, released at session 0, works unfilled; the entry, released at
// session 16, is rejected at session 17 (one rejection) and planned once
// more, and the first purchase's expiry is read at session 18, after that
// rejection; the venue then rejects the entry's one re-plan at session 19.
// An acceptance ends the run of rejections only when the order accepted was
// released after the rejections it would end: the first purchase was
// released before the entry, so its late word changes nothing, the re-plan's
// rejection is the second in a row and buys stop (ADR 0026, point 2,
// "Boost", amended 2026-10-18: a re-planned order rejected again is always
// the second in a row). A run restarted from the decisions of sessions 16,
// 17 and 18 counts as the full run does.
func TestALateAcceptanceOfAnOlderOrderDoesNotEndTheRunOfRejections(t *testing.T) {
	hold := map[int]bool{18: true, 19: true}
	for i := 1; i <= 16; i++ {
		hold[i] = true
	}
	opts := options{hold: hold, invalidAt: map[int][]int{17: {16}, 19: {17}}, partFill: map[int]int64{17: 0}, cancelAt: map[int]int{17: 18},
		mutate: wholeBoostInOneOrder}
	r := simulate(t, opts)
	entry, again := releasedBy(t, r.decisions, 16), releasedBy(t, r.decisions, 17)
	if len(entry) != 1 || entry[0].Side != event.OrderSideBuy || len(again) != 1 || again[0].Side != event.OrderSideBuy {
		t.Fatalf("setup: sessions 16 and 17 release %+v and %+v, want the entry and its one re-plan", entry, again)
	}
	decided := targets(t, r.decisions)
	if d := decided[18].Boost; d.VenueRejections != 1 {
		t.Fatalf("session 18: rejections %d, want the entry's rejection still counted after the older order's expiry", d.VenueRejections)
	}
	want := event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: again[0].ClientOrderID, Released: again[0].Quantity}
	if d := decided[19].Boost; d.VenueRejections != 2 || d.ReplanWithheld != want {
		t.Errorf("session 19: rejections %d withheld %+v, want two and %+v recorded", d.VenueRejections, d.ReplanWithheld, want)
	}
	for n := 19; n < len(decided); n++ {
		for _, o := range releasedBy(t, r.decisions, n) {
			if o.Side == event.OrderSideBuy {
				t.Errorf("session %d releases a buy %+v while buys are stopped", n, o)
			}
		}
	}
	for _, n := range []int{16, 17, 18} {
		assertRecoveredAsTheFullRun(t, opts, n)
	}
}

// TestALaterOrdersAcceptanceStillEndsTheRunOfRejections: as above, but the
// first purchase is rejected at session 17 and the entry, released after
// it, expires unfilled at session 18: an order released after the
// rejection, accepted, ends the run (ADR 0026, point 2, "Boost", amended
// 2026-10-18), and the run restarted from session 17 counts the same.
func TestALaterOrdersAcceptanceStillEndsTheRunOfRejections(t *testing.T) {
	hold := map[int]bool{18: true}
	for i := 1; i <= 16; i++ {
		hold[i] = true
	}
	opts := options{hold: hold, invalidAt: map[int][]int{17: {0}}, partFill: map[int]int64{17: 0}, cancelAt: map[int]int{17: 18},
		mutate: wholeBoostInOneOrder}
	r := simulate(t, opts)
	decided := targets(t, r.decisions)
	if d := decided[17].Boost; d.VenueRejections != 1 {
		t.Fatalf("session 17: rejections %d, want the first purchase's rejection", d.VenueRejections)
	}
	if d := decided[18].Boost; d.VenueRejections != 0 || len(d.RejectedInARow) != 0 {
		t.Errorf("session 18: rejections %d named %+v, want none once the later entry is accepted", d.VenueRejections, d.RejectedInARow)
	}
	assertRecoveredAsTheFullRun(t, opts, 17)
}

// TestADryRunListsNoUndecidedOrder: no venue receives a dry-run's orders,
// so none will ever be ruled on; a dry-run Boost run lists none as
// undecided and records why on every decision (`no_venue`, dry-run), so
// nothing grows with the orders it releases (ADR 0026, point 3, "Mode",
// and point 2, "Boost", amended 2026-10-18). The same run in live lists
// them and records no such mark.
func TestADryRunListsNoUndecidedOrder(t *testing.T) {
	opts := options{closes: flippingCloses(), hold: heldFrom(2)}
	live := targets(t, simulate(t, opts).decisions)
	if d := live[45].Boost; len(d.UndecidedOrders) < 8 || d.NoVenue != "" {
		t.Fatalf("setup: the live run's session 45 lists %d undecided, no venue %q; want the orders the venue said nothing of", len(d.UndecidedOrders), d.NoVenue)
	}
	opts.mutate = boostDryRun
	r := simulate(t, opts)
	if len(released(t, r.decisions)) < 10 {
		t.Fatalf("setup: the dry-run releases %d orders", len(released(t, r.decisions)))
	}
	for n, d := range targets(t, r.decisions) {
		if len(d.Boost.UndecidedOrders) != 0 || d.Boost.NoVenue != event.TradingModeDryRun {
			t.Errorf("session %d: undecided %+v, no venue %q; want none listed and dry-run recorded", n, d.Boost.UndecidedOrders, d.Boost.NoVenue)
		}
	}
}

// TestAClearedNeverSentOrderIsNoLongerUndecided: the venue says nothing of
// the orders released from session 16 on; at session 20 the owner settles
// session 16's buy as never sent (operator.rebalance.cleared, naming its
// decision and the order). No venue will ever rule on it, so it leaves the
// undecided orders when the clearance is read, and a run restarted from
// session 19's decision or session 20's does the same (ADR 0026, point 2,
// "Boost", amended 2026-10-18, and point 5).
func TestAClearedNeverSentOrderIsNoLongerUndecided(t *testing.T) {
	opts := options{closes: flippingCloses(), hold: heldFrom(2), clear: map[int][]int{20: {16}}}
	r := simulate(t, opts)
	entry := releasedBy(t, r.decisions, 16)
	if len(entry) != 1 {
		t.Fatalf("setup: session 16 releases %+v", entry)
	}
	listed := func(d event.BoostFigures) bool {
		return slices.ContainsFunc(d.UndecidedOrders, func(o event.BoostUndecidedOrder) bool { return o.ClientOrderID == entry[0].ClientOrderID })
	}
	decided := targets(t, r.decisions)
	if !listed(decided[19].Boost) {
		t.Fatalf("session 19 lists %+v, want the entry undecided before the clearance", decided[19].Boost.UndecidedOrders)
	}
	for n := 20; n < len(decided); n++ {
		if listed(decided[n].Boost) {
			t.Errorf("session %d still lists the cleared entry %s undecided", n, entry[0].ClientOrderID)
		}
	}
	assertRecoveredAsTheFullRun(t, opts, 19)
	assertRecoveredAsTheFullRun(t, opts, 20)
}

// TestAClearedNeverSentSellIsRecoveredFromALaterDecision: session 17's sell
// is never sent and is settled as such at session 19, after session 18's buy
// replaced it as the watched order. It leaves the undecided orders, but its
// shares stay counted as sold until the owner's fresh run, so a decision
// after the clearance records the order and a run restarted from that
// decision counts them as the full run does (ADR 0026, point 2, "Boost",
// amended 2026-10-18, and point 5: recovered, never recomputed).
func TestAClearedNeverSentSellIsRecoveredFromALaterDecision(t *testing.T) {
	opts := options{closes: flippingCloses(), clear: map[int][]int{19: {17}}}
	r := simulate(t, opts)
	sell := releasedBy(t, r.decisions, 17)
	if len(sell) != 1 || sell[0].Side != event.OrderSideSell {
		t.Fatalf("setup: session 17 releases %+v, want one sell", sell)
	}
	// The orders execute, so Boost sells again after the clearance, sized
	// net of the cleared sell that the gate still counts as working.
	later := 0
	for n := 20; n < len(flippingCloses()); n++ {
		for _, o := range releasedBy(t, r.decisions, n) {
			if o.Side == event.OrderSideSell {
				later++
			}
		}
	}
	if later == 0 {
		t.Fatal("setup: no sell follows the clearance, so a restart could not size one")
	}
	decided := targets(t, r.decisions)
	for _, n := range []int{20, 24, 40} {
		d := decided[n].Boost
		if len(d.ClearedOrders) != 1 || d.ClearedOrders[0].ClientOrderID != sell[0].ClientOrderID || d.ClearedOrders[0].Released != sell[0].Quantity {
			t.Errorf("session %d records cleared orders %+v, want the sell %+v", n, d.ClearedOrders, sell[0])
		}
	}
	for _, n := range []int{19, 20, 24} {
		assertRecoveredAsTheFullRun(t, opts, n)
	}
}

// TestAClearedOlderOrderCountsOneFewerBeforeTheRejection: the venue rejects
// session 18's buy at session 20 (one rejection), when sessions 16's buy
// and 17's sell, released before it, are undecided: the decision counts them
// as older than the rejection. At session 22 the owner settles session 16's
// buy as never sent; it leaves the undecided orders and the count falls by
// one with the rejection unchanged, and a run restarted from the decision
// before or after the clearance counts as the full run does (ADR 0026,
// point 2, "Boost", amended 2026-10-18, and point 5).
func TestAClearedOlderOrderCountsOneFewerBeforeTheRejection(t *testing.T) {
	opts := options{closes: flippingCloses(), hold: heldFrom(2), invalidAt: map[int][]int{20: {18}}, clear: map[int][]int{22: {16}}}
	r := simulate(t, opts)
	decided := targets(t, r.decisions)
	before, after := decided[21].Boost, decided[22].Boost
	if before.VenueRejections != 1 || before.UndecidedBeforeRejection < 1 {
		t.Fatalf("setup: session 21 counts %d rejections and %d undecided before the rejection, want one and some", before.VenueRejections, before.UndecidedBeforeRejection)
	}
	if after.UndecidedBeforeRejection != before.UndecidedBeforeRejection-1 || after.VenueRejections != before.VenueRejections {
		t.Errorf("session 22 counts %d undecided before the rejection and %d rejections; want %d and %d after the clearance",
			after.UndecidedBeforeRejection, after.VenueRejections, before.UndecidedBeforeRejection-1, before.VenueRejections)
	}
	assertRecoveredAsTheFullRun(t, opts, 21)
	assertRecoveredAsTheFullRun(t, opts, 22)
}

// TestAClearanceOfNoUndecidedBoostOrderIsRefused: Boost reads the owner's
// clearance only for an order of its own account that a decided session
// released, under the decision it names, and that the venue has not ruled
// on; anything else is refused, so a clearance never means more than the
// owner said (fail closed, AGENTS.md). A dry-run lists no order undecided,
// so it refuses every clearance.
func TestAClearanceOfNoUndecidedBoostOrderIsRefused(t *testing.T) {
	closes := flippingCloses()[:20]
	opts := options{closes: closes, hold: heldFrom(2), unsent: []int{16}}
	at := sessionEnd(len(closes) - 1).Add(time.Hour)
	first := func(r run, n int) event.OrderReleasedPayload {
		t.Helper()
		o := releasedBy(t, r.decisions, n)
		if len(o) != 1 {
			t.Fatalf("setup: session %d releases %+v", n, o)
		}
		return o[0]
	}
	for name, c := range map[string]struct {
		mode   func(*event.TargetWeightConfigurationPayload)
		change func(r run, p *event.RebalanceClearedPayload)
	}{
		"naming no order":       {change: func(_ run, p *event.RebalanceClearedPayload) { p.ClientOrderIDs = nil }},
		"another account":       {change: func(_ run, p *event.RebalanceClearedPayload) { p.AccountID = "ira" }},
		"another decision":      {change: func(_ run, p *event.RebalanceClearedPayload) { p.DecisionID = decisionTag(17) }},
		"an order not released": {change: func(_ run, p *event.RebalanceClearedPayload) { p.ClientOrderIDs = []string{"tw-nothing"} }},
		"an order the venue ruled on": {change: func(r run, p *event.RebalanceClearedPayload) {
			o := first(r, 0) // the first purchase, filled at session 1
			p.DecisionID, p.ClientOrderIDs = o.Tag, []string{o.ClientOrderID}
		}},
		"an order the venue acknowledged": {change: func(r run, p *event.RebalanceClearedPayload) {
			// Session 17's sell was sent and IBKR named it (submitted), so it
			// was received by the venue and is not one the adapter never sent.
			o := first(r, 17)
			p.DecisionID, p.ClientOrderIDs = o.Tag, []string{o.ClientOrderID}
		}},
		"a dry-run's order": {mode: boostDryRun},
	} {
		t.Run(name, func(t *testing.T) {
			o := opts
			o.mutate = c.mode
			r := simulate(t, o)
			p := event.RebalanceClearedPayload{AccountID: "joint", AsOf: at, DecisionID: decisionTag(16),
				ClientOrderIDs: []string{first(r, 16).ClientOrderID}, Operator: "owner", Detail: "never sent"}
			if c.change != nil {
				c.change(r, &p)
			}
			s := &stream{hash: r.hash, sequence: uint64(len(r.inputs))}
			in := s.envelope(t, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared", at, p)
			if _, err := r.recorder.Apply(context.Background(), in); err == nil {
				t.Errorf("the clearance %+v was read, want it refused", p)
			}
		})
	}
}

// applyAfter feeds the run one more input, stamped at, and returns the
// recorder's error.
func applyAfter(t *testing.T, r run, typ string, version uint32, id string, at time.Time, payload any) error {
	t.Helper()
	s := &stream{hash: r.hash, sequence: uint64(len(r.inputs))}
	_, err := r.recorder.Apply(context.Background(), s.envelope(t, typ, version, id, at, payload))
	return err
}

// TestAClearanceOfAnOrderIBKRListsIsRefused: a broker snapshot that lists
// an open order under its client order ID shows the order reached IBKR,
// whether or not a lifecycle report has named it yet, so the clearance of
// it as never sent is refused (ADR 0026, point 2, "Boost", amended
// 2026-10-18; fail closed, AGENTS.md).
func TestAClearanceOfAnOrderIBKRListsIsRefused(t *testing.T) {
	for _, session := range []int{16, 17, 18} { // 18 is the watched order
		closes := flippingCloses()[:20]
		opts := options{closes: closes, hold: heldFrom(2), unsent: []int{session}}
		r := simulate(t, opts)
		o := releasedBy(t, r.decisions, session)
		if len(o) != 1 {
			t.Fatalf("setup: session %d releases %+v", session, o)
		}
		at := sessionEnd(len(closes) - 1).Add(time.Hour)
		broker := (&account{cash: 4000}).broker(at, closes[len(closes)-1])
		broker.OpenOrders = []event.BrokerOrder{{BrokerOrderID: "ib-" + o[0].ClientOrderID, ClientOrderID: o[0].ClientOrderID, InstrumentID: "SPY",
			Side: o[0].Side, Status: event.BrokerOrderWorking, RemainingQuantity: o[0].Quantity}}
		if err := applyAfter(t, r, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "broker:listing", at, broker); err != nil {
			t.Fatalf("session %d: the broker snapshot: %v", session, err)
		}
		cleared := event.RebalanceClearedPayload{AccountID: "joint", AsOf: at.Add(time.Minute), DecisionID: o[0].Tag,
			ClientOrderIDs: []string{o[0].ClientOrderID}, Operator: "owner", Detail: "never sent"}
		err := applyAfter(t, r, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, "cleared:listed", at.Add(time.Minute), cleared)
		if err == nil || !strings.Contains(err.Error(), "named") {
			t.Errorf("session %d: the clearance of a listed %s was read, err %v; want it refused as an order IBKR named", session, o[0].Side, err)
		}
	}
}

// TestAVenueReportOnAClearedOrderIsRefused: the owner settled an order as
// never sent, so no venue report on it can be true; one that arrives proves
// the clearance wrong and is refused, which stops the run for a person
// (ADR 0026, point 2, "Boost", amended 2026-10-18; fail closed, AGENTS.md),
// as a later report on a cleared IRA order does.
func TestAVenueReportOnAClearedOrderIsRefused(t *testing.T) {
	closes := flippingCloses()[:22]
	r := simulate(t, options{closes: closes, hold: heldFrom(2), clear: map[int][]int{19: {17}}})
	o := releasedBy(t, r.decisions, 17)[0]
	at := sessionEnd(len(closes) - 1).Add(time.Hour)
	for _, status := range []string{event.OrderLifecycleStatusSubmitted, event.OrderLifecycleStatusCanceled, event.OrderLifecycleStatusInvalid} {
		report := event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "ib-" + o.ClientOrderID,
			InstrumentID: o.InstrumentID, Side: o.Side, Status: status, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at}
		err := applyAfter(t, r, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "late:"+status, at, report)
		if err == nil || !strings.Contains(err.Error(), "cleared") {
			t.Errorf("a %s report on the cleared order was read, err %v; want it refused as contradicting the clearance", status, err)
		}
	}
	fill := (&account{}).fill(o, "late-fill", 1, o.LimitPrice, at)
	err := applyAfter(t, r, event.FillEventType, event.FillTargetWeightVersion, "late-fill", at, fill)
	if err == nil || !strings.Contains(err.Error(), "cleared") {
		t.Errorf("a fill of the cleared order was read, err %v; want it refused as contradicting the clearance", err)
	}
}

// TestARunRecoveredBehindAWorkingOrderTheVenueAcceptedCountsAsTheFullRun:
// the first entry, released at session 16, part-fills 5 shares, so the
// venue accepted it, and works on until its cancel at session 30; the exit
// at session 17 and the entry at session 18 replace it as the watched
// order. A decision records an accepted order that is still working
// besides the watched one, so a run restarted from it reads the cancel as
// the full run does, instead of refusing it as the report of an order it
// never released (ADR 0026, point 5: recovered, never recomputed).
func TestARunRecoveredBehindAWorkingOrderTheVenueAcceptedCountsAsTheFullRun(t *testing.T) {
	opts := options{closes: flippingCloses(), hold: heldFrom(18), partFill: map[int]int64{17: 5}, cancelAt: map[int]int{17: 30}}
	entry := releasedBy(t, simulate(t, opts).decisions, 16)
	if len(entry) != 1 {
		t.Fatalf("setup: session 16 releases %+v", entry)
	}
	for _, n := range []int{17, 18, 24} {
		assertRecoveredAsTheFullRun(t, opts, n)
	}
}

// TestARunRecoveredWatchingAnOrderTheVenueAcceptedCountsAsTheFullRun: the
// entry released at session 16 part-fills 5 shares, so the venue accepted
// it, and works on until its cancel at session 21; Boost's exit at session
// 20 replaces it as the watched order. A run restarted from a decision taken
// while the entry was still the watched order restores it as accepted and
// still working, so once the exit replaces it the decision records it among
// the working orders, as the full run's does (ADR 0026, point 5: recovered,
// never recomputed).
func TestARunRecoveredWatchingAnOrderTheVenueAcceptedCountsAsTheFullRun(t *testing.T) {
	opts := options{partFill: map[int]int64{17: 5}, cancelAt: map[int]int{17: 21}}
	full := simulate(t, opts)
	if w := targets(t, full.decisions)[20].Boost.WorkingOrders; len(w) != 1 || w[0].Filled != 5 {
		t.Fatalf("setup: session 20 records working orders %+v, want the entry with its 5 filled", w)
	}
	for _, n := range []int{17, 18, 19} {
		assertRecoveredAsTheFullRun(t, opts, n)
	}
}

// TestARunRecoveredWithTheWatchedOrderAmongOthersListsItLastOnceReplaced:
// the order released at session 18 is part-filled at session 19, so the venue
// accepted it, and the re-plan released at session 19, the watched order,
// part-fills at session 20; the entry released at session 16, older than
// both, is accepted at session 21 by a late execution. The full run lists the
// re-plan before it among the working orders while it is watched, and after
// the older ones once session 23's release replaces it as the watched order. A
// run restarted from a decision taken while the re-plan was still the watched
// one restores the older orders first and the watched one last, so it lists
// them as the full run does (ADR 0026, point 5: recovered, never recomputed;
// an order replaced as the watched one is listed after the other working
// orders).
func TestARunRecoveredWithTheWatchedOrderAmongOthersListsItLastOnceReplaced(t *testing.T) {
	hold := heldFrom(18)
	delete(hold, 18)
	delete(hold, 19)
	delete(hold, 20)
	opts := options{
		closes: flippingCloses(), hold: hold,
		partFill: map[int]int64{17: 0, 19: 2, 20: 1}, cancelAt: map[int]int{17: 30, 19: 30, 20: 30},
		lateFill: map[int]lateFill{17: {at: 21, shares: 3}},
	}
	full := simulate(t, opts)
	decided := targets(t, full.decisions)
	watched := decided[22].Boost.Watched.ClientOrderID
	listed := func(n int) []string {
		var ids []string
		for _, w := range decided[n].Boost.WorkingOrders {
			ids = append(ids, w.ClientOrderID)
		}
		return ids
	}
	if got := listed(22); len(got) != 2 || slices.Contains(got, watched) {
		t.Fatalf("setup: session 22 lists %v beside the watched %s, want two older orders", got, watched)
	}
	if got := listed(23); len(got) != 3 || got[2] != watched {
		t.Fatalf("setup: session 23 lists %v, want the replaced %s last", got, watched)
	}
	for _, n := range []int{21, 22} {
		assertRecoveredAsTheFullRun(t, opts, n)
	}
}

// TestARunRecoveredBehindAWorkingSellMatchesItsListingByVenueOrderID:
// Boost's exit, a sell released at session 17, executes 5 shares at session
// 18 before any lifecycle report names it, so the venue accepted it with no
// venue order ID known; the entry released at session 18 replaces it as
// the watched order. IBKR's listing at session 19 names the exit under its
// client order ID, so its venue order ID is known from then on, and the
// decision records it with the working order. At session 21 the margin
// alarm turns urgent and IBKR lists the exit with no client order ID
// echoed: the full run, and a run restarted from session 20's decision,
// both match the listing to the exit by that ID and net the exit once in
// the sale back to 98%, never twice (ADR 0019, amended
// 2026-10-15, "net of every working sell"; ADR 0026, point 5).
func TestARunRecoveredBehindAWorkingSellMatchesItsListingByVenueOrderID(t *testing.T) {
	base := options{closes: flippingCloses()[:23], brokers: true, hold: heldFrom(18), unsent: []int{17}}
	exit := releasedBy(t, simulate(t, base).decisions, 17)
	if len(exit) != 1 || exit[0].Side != event.OrderSideSell {
		t.Fatalf("setup: session 17 releases %+v, want the exit", exit)
	}
	o := exit[0]
	venueID := "ib-" + o.ClientOrderID
	opts := base
	opts.fills = map[int][]event.TargetWeightFillPayload{18: {{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, FillID: "fill:early", InstrumentID: o.InstrumentID,
		Side: o.Side, Quantity: 5, Price: o.LimitPrice, FilledAt: sessionEnd(18).Add(-2 * time.Hour)}}}
	opts.brokerMutate = map[int]func(*event.BrokerSnapshotPayload){
		19: func(b *event.BrokerSnapshotPayload) {
			b.OpenOrders = []event.BrokerOrder{{BrokerOrderID: venueID, ClientOrderID: o.ClientOrderID, InstrumentID: "SPY", Side: event.OrderSideSell,
				Status: event.BrokerOrderWorking, RemainingQuantity: o.Quantity - 5}}
		},
		21: func(b *event.BrokerSnapshotPayload) {
			b.NetLiquidation *= 0.8
			b.ExcessLiquidity = 0.1 * b.NetLiquidation
			b.OpenOrders = []event.BrokerOrder{{BrokerOrderID: venueID, InstrumentID: "SPY", Side: event.OrderSideSell,
				Status: event.BrokerOrderWorking, RemainingQuantity: o.Quantity - 5}}
		},
	}
	full := simulate(t, opts)
	if w := targets(t, full.decisions)[20].Boost.WorkingOrders; len(w) != 1 || w[0].ClientOrderID != o.ClientOrderID || w[0].VenueOrderID != venueID {
		t.Fatalf("session 20 records working orders %+v, want the exit with the venue order ID %s its listing named", w, venueID)
	}
	want := sales(t, full.decisions)
	if len(want) != 1 || want[0].Sale.WorkingSells == 0 || want[0].Sale.WorkingSells > o.Quantity {
		t.Fatalf("the full run's sale back to 98%%: %+v, want one netting the exit's %d shares once", want, o.Quantity)
	}
	// The restarted run knows the session-19 listing only from the decision.
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[20]
	inputs := slices.DeleteFunc(slices.Clone(full.inputs), func(e event.Envelope) bool { return e.ID == "broker:19" })
	r := simulate(t, options{closes: opts.closes, brokers: true, recovery: &recovery, snapshots: inputs, hold: opts.hold})
	got := sales(t, r.decisions)
	if len(got) != 1 || got[0].Quantity != want[0].Quantity || got[0].Sale != want[0].Sale {
		t.Errorf("the recovered run's sale back to 98%%: %+v, want the full run's %+v", got, want)
	}
}

// TestARecoveredOrderStatingAnExecutionAlreadyReadIsRefused: a broker
// snapshot read before the recovered decision releases the sale back to 98%,
// and its execution is read. A decision whose working order, or whose watched
// order, states that execution as its own is refused, and the run stops: an
// execution is of one order, and counting it on two would net a sell twice
// (ADR 0019, "Explained means"; fail closed, AGENTS.md).
func TestARecoveredOrderStatingAnExecutionAlreadyReadIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		opts  options
		n     int
		state func(*event.TargetsDecidedPayload) *event.BoostWatchedFill
	}{
		{"a working order", options{closes: flippingCloses(), hold: heldFrom(18), partFill: map[int]int64{17: 5}, cancelAt: map[int]int{17: 30}}, 17,
			func(p *event.TargetsDecidedPayload) *event.BoostWatchedFill {
				if len(p.Boost.WorkingOrders) != 1 || len(p.Boost.WorkingOrders[0].Fills) != 1 {
					return nil
				}
				return &p.Boost.WorkingOrders[0].Fills[0]
			}},
		{"the watched order", options{partFill: map[int]int64{17: 5}, cancelAt: map[int]int{17: 21}}, 17,
			func(p *event.TargetsDecidedPayload) *event.BoostWatchedFill {
				if len(p.Boost.Watched.Fills) != 1 {
					return nil
				}
				return &p.Boost.Watched.Fills[0]
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at := sessionEnd(0)
			h, s := configured(t)
			held := &account{cash: -2000, held: 60}
			urgent := held.broker(at.Add(-2*time.Hour), 100)
			urgent.ExcessLiquidity = 0.1 * urgent.NetLiquidation
			sale := released(t, mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k", urgent.AsOf, urgent)))
			if len(sale) != 1 {
				t.Fatalf("the urgent broker snapshot released %+v, want the sale back to 98%%", sale)
			}
			fill := held.fill(sale[0], "fill:sale", 1, sale[0].LimitPrice, at.Add(-time.Hour))
			mustApply(t, h, s.envelope(t, event.FillEventType, event.FillTargetWeightVersion, fill.FillID, fill.FilledAt, fill))
			recovery := ofType(simulate(t, c.opts).decisions, event.TargetsDecidedEventType)[c.n]
			var p event.TargetsDecidedPayload
			decode(t, recovery, &p)
			stated := c.state(&p)
			if stated == nil {
				t.Fatalf("setup: session %d records %+v, want %s with one execution", c.n, p.Boost, c.name)
			}
			stated.FillID = fill.FillID
			_, err := h.Apply(context.Background(), s.envelope(t, recovery.Type, recovery.SchemaVersion, recovery.ID, at, p))
			if err == nil || !strings.Contains(err.Error(), "already applied") {
				t.Errorf("the recovered decision restoring the sale's execution on %s was read, err %v; want it refused", c.name, err)
			}
		})
	}
}
