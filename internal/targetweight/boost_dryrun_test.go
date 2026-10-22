package targetweight_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// boostDryRunOptions is a dry-run Boost run as a dry-run adapter sees it: no
// order reaches a venue, so none is acknowledged, filled or ended by one
// (ADR 0026, point 3, "Mode"). The account starts holding 58 SPY on margin
// (145% of its $4,000), so the first decision, at 98%, sells, and every
// later exit has shares to sell; nothing executes, so it holds 58 to the end.
func boostDryRunOptions() options {
	unsent := make([]int, len(syntheticCloses()))
	for i := range unsent {
		unsent[i] = i
	}
	return options{mutate: boostDryRun, unsent: unsent, startHeld: 58}
}

// TestADryRunBoostExitIsSizedAsPaperWouldSizeIt: the dry-run's first
// decision sells down to 98% and Boost's exit at session 20 sells again. No
// venue received the first sell, so it sells nothing and can never fill:
// the exit is sized from the account as the snapshot holds it with no sell
// working, the quantity the planner gives a paper run for the same book,
// never net of the first sell (ADR 0026, point 2, step 5, and point 3,
// "Mode"; ADR 0019, amended 2026-10-15, "net of every working sell").
func TestADryRunBoostExitIsSizedAsPaperWouldSizeIt(t *testing.T) {
	r := simulate(t, boostDryRunOptions())
	first := releasedBy(t, r.decisions, 0)
	if len(first) != 1 || first[0].Side != event.OrderSideSell {
		t.Fatalf("setup: session 0 releases %+v, want the sale down to 98%%", first)
	}
	if releasedBy(t, r.decisions, 16) == nil {
		t.Fatal("setup: session 16 releases nothing, want Boost's entry")
	}
	var exit *event.OrderPlannedPayload
	for _, p := range planned(t, r.decisions) {
		if p.Tag == decisionTag(20) {
			exit = &p
		}
	}
	if exit == nil {
		t.Fatal("session 20 plans no exit: the first sell, never sent, still counts as being sold")
	}
	decided := ofType(r.decisions, event.TargetsDecidedEventType)[20]
	var d event.TargetsDecidedPayload
	decode(t, decided, &d)
	var snapshot event.AccountPositionsSnapshotPayload
	for _, in := range r.inputs {
		if in.ID == "snapshot:20" {
			decode(t, in, &snapshot)
		}
	}
	paper, ok, err := orderplan.PlanBoost(orderplan.BoostInput{
		Tag: decided.ID, Decision: d, Snapshot: snapshot, ReferencePrice: exit.ReferencePrice, PriceBand: exit.PriceBand,
	})
	if err != nil || !ok {
		t.Fatalf("the planner sizes no exit from the book: %v %v", ok, err)
	}
	if exit.Side != event.OrderSideSell || exit.Quantity != paper.Quantity || exit.HeldQuantity != 58 {
		t.Errorf("the dry-run exit is %s %d from %d held; paper sizes %s %d from the same book", exit.Side, exit.Quantity, exit.HeldQuantity, paper.Side, paper.Quantity)
	}
	if want := int64(math.Trunc(58 - float64(exit.TargetWeight*exit.NetLiquidation/exit.ReferencePrice))); exit.Quantity != want {
		t.Errorf("the dry-run exit sells %d, want %d: the holding less the target, nothing netted", exit.Quantity, want)
	}
}

// TestADryRunBoostRecordsEachUnsentReleaseCanceled: every order a dry-run
// Boost run releases is followed, in the same decision, by its synthetic
// terminal report: canceled, marked synthetic, naming no venue order, with
// the order's own shares and limit, as a dry-run momentum run records (ADR
// 0026, point 3, "Mode", amended 2026-10-17). The journal is byte-identical
// across runs and replays through a fresh handler.
func TestADryRunBoostRecordsEachUnsentReleaseCanceled(t *testing.T) {
	opts := boostDryRunOptions()
	r := simulate(t, opts)
	orders := released(t, r.decisions)
	if len(orders) < 3 {
		t.Fatalf("setup: the dry-run releases %d orders", len(orders))
	}
	reports := map[string]event.LimitOrderLifecyclePayload{}
	for _, e := range ofType(r.decisions, event.OrderLifecycleEventType) {
		var p event.LimitOrderLifecyclePayload
		decode(t, e, &p)
		if e.SchemaVersion != event.OrderLifecycleSyntheticVersion {
			t.Errorf("lifecycle %s is at version %d, want the synthetic version %d", e.ID, e.SchemaVersion, event.OrderLifecycleSyntheticVersion)
		}
		reports[p.ClientOrderID] = p
	}
	if len(reports) != len(orders) {
		t.Errorf("%d synthetic reports for %d released orders", len(reports), len(orders))
	}
	for _, o := range orders {
		p, ok := reports[o.ClientOrderID]
		switch {
		case o.Send:
			t.Errorf("dry-run released %s to send", o.ClientOrderID)
		case !ok:
			t.Errorf("released %s has no synthetic terminal report", o.ClientOrderID)
		case !p.Synthetic || p.Status != event.OrderLifecycleStatusCanceled || p.OrderID != "" || !p.Describes(o):
			t.Errorf("report on %s is %+v, want a synthetic cancel of the order as released", o.ClientOrderID, p)
		}
	}
	for i, e := range r.decisions {
		if e.Type != event.OrderReleasedEventType {
			continue
		}
		if i+1 >= len(r.decisions) || r.decisions[i+1].Type != event.OrderLifecycleEventType || r.decisions[i+1].CausationID != e.CausationID {
			t.Errorf("release %s is not followed by its synthetic report from the same input", e.ID)
		}
	}
	for n, d := range targets(t, r.decisions) {
		if !d.Boost.Watched.IsZero() {
			t.Errorf("session %d watches %+v; an order no venue received can never execute", n, d.Boost.Watched)
		}
		if d.Boost.ReplanPending {
			t.Errorf("session %d leaves a re-plan pending; a synthetic cancel is not a venue's", n)
		}
	}
	assertByteIdenticalAndReplays(t, opts)
}

// TestADryRunBoostRestartedMidExitDecidesAsTheFullRun: a dry-run run
// restarted from the decision before Boost's exit, from the exit's own and
// from the one after it decides as the full run does, byte for byte (ADR
// 0026, point 5: recovered, never recomputed).
func TestADryRunBoostRestartedMidExitDecidesAsTheFullRun(t *testing.T) {
	opts := boostDryRunOptions()
	for _, n := range []int{0, 16, 19, 20, 21} {
		assertRecoveredAsTheFullRun(t, opts, n)
	}
}
