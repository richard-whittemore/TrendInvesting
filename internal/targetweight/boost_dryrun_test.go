package targetweight_test

import (
	"bytes"
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// boostDryRunOptions is a dry-run Boost run as a dry-run adapter sees it: no
// order reaches a venue, so none is acknowledged, filled or ended by one
// (ADR 0026, point 3, "Mode"; the simulated venue receives no order
// released with send false). The account starts holding 58 SPY on margin
// (145% of its $4,000), so the first decision, at 98%, sells, and every
// later exit has shares to sell; nothing executes, so it holds 58 to the end.
func boostDryRunOptions() options {
	return options{mutate: boostDryRun, startHeld: 58}
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
// 0026, point 3, "Mode", amended 2026-10-17 and 2026-10-22). The journal is byte-identical
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

// TestADryRunBoostRefusesAVenueReportOnAnOrderItCanceled: the entry
// released at session 16 was never sent and is ended by the run's own
// synthetic cancel, so no venue can report on it. An acknowledgement, a
// cancel or a fill naming it at session 17 can only be a defect, as is a
// fill naming an order the run never released: each is
// refused, journalled with the refusal naming the order, and changes
// nothing, so every decision after them is the one the run makes without
// them (ADR 0026, point 3, "Mode", amended 2026-10-22; fail closed,
// AGENTS.md).
func TestADryRunBoostRefusesAVenueReportOnAnOrderItCanceled(t *testing.T) {
	opts := boostDryRunOptions()
	base := simulate(t, opts)
	entry := releasedBy(t, base.decisions, 16)
	if len(entry) != 1 {
		t.Fatalf("setup: session 16 releases %+v, want Boost's entry", entry)
	}
	o, at := entry[0], sessionEnd(17).Add(-4*time.Hour)
	report := func(status string) event.LimitOrderLifecyclePayload {
		return event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "ib-" + o.ClientOrderID,
			InstrumentID: o.InstrumentID, Side: o.Side, Status: status, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at}
	}
	fill := func(id, client string) event.TargetWeightFillPayload {
		return event.TargetWeightFillPayload{AccountID: o.AccountID, ClientOrderID: client, FillID: id, InstrumentID: o.InstrumentID,
			Side: o.Side, Quantity: o.Quantity, Price: o.LimitPrice, FilledAt: at}
	}
	inputs := []struct {
		typ     string
		version uint32
		id      string
		order   string
		payload any
	}{
		{event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "venue-ack", o.ClientOrderID, report(event.OrderLifecycleStatusSubmitted)},
		{event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "venue-cancel", o.ClientOrderID, report(event.OrderLifecycleStatusCanceled)},
		{event.FillEventType, event.FillTargetWeightVersion, "venue-fill", o.ClientOrderID, fill("venue-fill", o.ClientOrderID)},
		// An order the run never released is no more a venue's to report.
		{event.FillEventType, event.FillTargetWeightVersion, "venue-fill-other", "tw-nothing", fill("venue-fill-other", "tw-nothing")},
	}
	errs := map[string]error{}
	opts.intrude = map[int]func(func(string, uint32, string, time.Time, any) error){17: func(apply func(string, uint32, string, time.Time, any) error) {
		for _, in := range inputs {
			errs[in.id] = apply(in.typ, in.version, in.id, at, in.payload)
		}
	}}
	got := simulate(t, opts)
	for _, in := range inputs {
		err := errs[in.id]
		if err == nil || !strings.Contains(err.Error(), in.order) || !strings.Contains(err.Error(), "dry-run") {
			t.Errorf("%s on the dry-run's order %s: err %v, want it refused naming the order and the dry-run", in.id, in.order, err)
			continue
		}
		if c, ok := got.recorder.Committed(in.id); !ok || c.Refusal != err.Error() || len(c.Decisions) != 0 {
			t.Errorf("%s is journalled as %+v (committed %v), want its refusal and no decision", in.id, c, ok)
		}
	}
	if want, have := targets(t, base.decisions)[17:], targets(t, got.decisions)[17:]; !reflect.DeepEqual(want, have) {
		t.Errorf("the decisions after the refused reports are %+v, want the run's without them, %+v", have, want)
	}
	if want, have := released(t, base.decisions), released(t, got.decisions); !reflect.DeepEqual(want, have) {
		t.Errorf("the orders released with the refused reports are %+v, want the run's without them, %+v", have, want)
	}
	// Each refused input holds its own sequence number: the journalled
	// inputs are contiguous across the refusals and the real input after.
	var last uint64
	seen := map[string]bool{}
	for _, e := range got.entries {
		if e.Kind != journal.KindInput {
			continue
		}
		if last != 0 && e.Envelope.Sequence != last+1 {
			t.Fatalf("input %s has sequence %d after %d, want them contiguous", e.Envelope.ID, e.Envelope.Sequence, last)
		}
		last = e.Envelope.Sequence
		seen[e.Envelope.ID] = true
	}
	for _, in := range inputs {
		if !seen[in.id] {
			t.Errorf("refused input %s is not in the journal's inputs", in.id)
		}
	}
	// The journal holding the refusals replays, refusals included, to the
	// same decision log, and a rerun writes the same bytes.
	var logged, inputs2 []event.Envelope
	for _, e := range got.entries {
		if e.Kind == journal.KindInput {
			inputs2 = append(inputs2, e.Envelope)
		} else {
			logged = append(logged, e.Envelope)
		}
	}
	cfg := testConfiguration()
	opts.mutate(&cfg)
	handler, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: sessionEnd(0)})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := replay.New(handler)
	if err != nil {
		t.Fatal(err)
	}
	var replayed []event.Envelope
	for _, in := range inputs2 {
		// A refusal is part of the run: the engine spends its sequence and
		// goes on, as the recorder committed it.
		out, _ := engine.Apply(context.Background(), in)
		replayed = append(replayed, out...)
	}
	if d := replay.Equivalent(logged, replayed); d != nil {
		t.Errorf("replaying the journal with the refusals diverged: %+v", d)
	}
	if !bytes.Equal(journalBytes(t, got), journalBytes(t, simulate(t, opts))) {
		t.Error("rerunning the same inputs, refusals included, must write a byte-identical journal")
	}
}

func journalBytes(t *testing.T, r run) []byte {
	t.Helper()
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

// TestADryRunBoostSaleBackTo98IsCanceledAndSizedWithNothingNetted: Boost's
// exit at session 20 is never sent and ends with its synthetic cancel; the
// margin alarm turns urgent at session 21 and stays urgent at 22, since
// nothing executes. The sale back to 98% is released at 21 with send false,
// followed by its own synthetic cancel from the same input, and sized from
// that broker snapshot with no working sell, the exit's shares not netted,
// as orderplan.PlanSale sizes it; it consumes the alarm's one attempt, so
// session 22's snapshot releases no second sale (ADR 0026, point 3, "Mode",
// amended 2026-10-22, and point 5; ADR 0019, amended 2026-10-15).
func TestADryRunBoostSaleBackTo98IsCanceledAndSizedWithNothingNetted(t *testing.T) {
	opts := boostDryRunOptions()
	urgent := func(b *event.BrokerSnapshotPayload) {
		b.NetLiquidation *= 0.8
		b.ExcessLiquidity = 0.1 * b.NetLiquidation
	}
	opts.brokers = true
	opts.brokerMutate = map[int]func(*event.BrokerSnapshotPayload){21: urgent, 22: urgent}
	r := simulate(t, opts)
	exit := releasedBy(t, r.decisions, 20)
	if len(exit) != 1 || exit[0].Side != event.OrderSideSell {
		t.Fatalf("setup: session 20 releases %+v, want Boost's exit", exit)
	}
	got := sales(t, r.decisions)
	if len(got) != 1 {
		t.Fatalf("the dry-run plans %d sales back to 98%%, want one for the urgent alarm: %+v", len(got), got)
	}
	sale := got[0]
	if sale.Side != event.OrderSideSell || sale.Sale.WorkingSells != 0 || sale.HeldQuantity != 58 {
		t.Errorf("the sale back to 98%% is %s %d from %d held net of %d working; want a sell from the 58 held, nothing netted",
			sale.Side, sale.Quantity, sale.HeldQuantity, sale.Sale.WorkingSells)
	}
	var broker event.BrokerSnapshotPayload
	for _, in := range r.inputs {
		if in.ID == "broker:21" {
			decode(t, in, &broker)
		}
	}
	keep := int64(math.Ceil(0.98 * broker.NetLiquidation / broker.Positions[0].Mark))
	if want := 58 - keep; sale.Quantity != want {
		t.Errorf("the sale back to 98%% sells %d, want %d: the broker snapshot's 58 less ceil(0.98 x %v / %v) = %d kept, nothing netted",
			sale.Quantity, want, broker.NetLiquidation, broker.Positions[0].Mark, keep)
	}
	releaseAt := -1
	for i, e := range r.decisions {
		if e.Type == event.OrderReleasedEventType && e.ID == "risk-order-released:"+sale.ClientOrderID {
			releaseAt = i
		}
	}
	if releaseAt < 0 {
		t.Fatalf("the sale %s is never released", sale.ClientOrderID)
	}
	var o event.OrderReleasedPayload
	decode(t, r.decisions[releaseAt], &o)
	if o.Send {
		t.Errorf("the dry-run released the sale %s to send", o.ClientOrderID)
	}
	next := releaseAt + 1
	if next >= len(r.decisions) || r.decisions[next].Type != event.OrderLifecycleEventType || r.decisions[next].CausationID != r.decisions[releaseAt].CausationID {
		t.Fatalf("the sale's release is not followed by its synthetic report from the same input")
	}
	var p event.LimitOrderLifecyclePayload
	decode(t, r.decisions[next], &p)
	if !p.Synthetic || p.Status != event.OrderLifecycleStatusCanceled || !p.Describes(o) {
		t.Errorf("the sale's report is %+v, want a synthetic cancel of the sale as released", p)
	}
	assertByteIdenticalAndReplays(t, opts)
}
