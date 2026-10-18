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
