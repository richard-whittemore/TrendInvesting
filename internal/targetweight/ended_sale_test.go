package targetweight_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// endedSale is a run whose incident sale back to 98% has ended by session
// 21's decision, and a run restarted from that decision.
type endedSale struct {
	sale      event.OrderReleasedPayload
	full      run
	decision  event.Envelope
	recovered run
	// intrude is what both runs were fed besides the venue's own reports.
	intrude map[int]func(apply func(typ string, version uint32, id string, at time.Time, payload any) error)
}

// endSaleBy21 releases the incident's first sale at session 20 under a
// Halted state, an urgent reading at session 23 finding its attempt spent,
// lets the venue report on it by session 21 as shape says, and restarts a
// run from session 21's decision with the full run's venue reports after
// session 21 (venueReports). shape gets the sale and returns the options
// that make the venue's reports; the same intrusions are fed to both runs.
func endSaleBy21(t *testing.T, shape func(sale event.OrderReleasedPayload, opts *options)) endedSale {
	t.Helper()
	state := haltedState(t)
	first := orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 0)
	urgent := map[int]func(*event.BrokerSnapshotPayload){23: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	base := simulate(t, options{brokers: true})
	probe := restartAt17(t, base, options{prelude: []event.Envelope{state}, brokerMutate: urgent, hold: map[int]bool{20: true, 21: true}})
	rel := released(t, probe.decisions)
	if len(rel) != 1 || rel[0].ClientOrderID != first || rel[0].Quantity < 4 {
		t.Fatalf("setup: released %+v, want the incident's first sale of at least four shares", rel)
	}
	e := endedSale{sale: rel[0]}
	opts := options{prelude: []event.Envelope{state}, brokerMutate: urgent}
	shape(e.sale, &opts)
	e.intrude = opts.intrude
	e.full = restartAt17(t, base, opts)
	if rel := released(t, e.full.decisions); len(rel) != 1 || rel[0].ClientOrderID != first {
		t.Fatalf("setup: the full run released %+v, want the incident's first sale only", rel)
	}
	for _, d := range ofType(e.full.decisions, event.TargetsDecidedEventType) {
		if d.EventTime.Equal(sessionEnd(21)) {
			e.decision = d
		}
	}
	e.recovered = simulate(t, options{recovery: &e.decision, snapshots: e.full.inputs, brokers: true, prelude: []event.Envelope{state},
		reports: venueReports(e.full.inputs, 21), brokerMutate: urgent, intrude: e.intrude})
	return e
}

// part is an execution of shares of o at session n, with a commission.
func part(o event.OrderReleasedPayload, id string, shares int64, n int) event.TargetWeightFillPayload {
	return event.TargetWeightFillPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, FillID: id, InstrumentID: o.InstrumentID,
		Side: o.Side, Quantity: shares, Price: o.LimitPrice, Commission: 1.25, FilledAt: sessionEnd(n).Add(-2 * time.Hour)}
}

// afterSession21 are run's envelopes of type typ stamped after session 21.
func afterSession21(es []event.Envelope, typ string) []event.Envelope {
	var out []event.Envelope
	for _, e := range ofType(es, typ) {
		if e.EventTime.After(sessionEnd(21)) {
			out = append(out, e)
		}
	}
	return out
}

// sameAfterSession21 checks that the restarted run's decisions and
// close-snapshot checks after session 21 are the full run's, and that it
// releases and plans nothing: the attempt is spent.
func sameAfterSession21(t *testing.T, e endedSale) []event.TargetsDecidedPayload {
	t.Helper()
	decided := func(es []event.Envelope) []event.TargetsDecidedPayload {
		var out []event.TargetsDecidedPayload
		for _, d := range afterSession21(es, event.TargetsDecidedEventType) {
			var p event.TargetsDecidedPayload
			decode(t, d, &p)
			out = append(out, p)
		}
		return out
	}
	checks := func(es []event.Envelope) []event.ReconciliationResultPayload {
		var out []event.ReconciliationResultPayload
		for _, d := range afterSession21(es, event.ReconciliationResultEventType) {
			var p event.ReconciliationResultPayload
			decode(t, d, &p)
			out = append(out, p)
		}
		return out
	}
	want, got := decided(e.full.decisions), decided(e.recovered.decisions)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("the restarted run's decisions after session 21 are %+v, want the full run's %+v", got, want)
	}
	if w, g := checks(e.full.decisions), checks(e.recovered.decisions); !reflect.DeepEqual(g, w) {
		t.Errorf("the restarted run's close-snapshot checks after session 21 are %+v, want the full run's %+v", g, w)
	}
	if rel := released(t, e.recovered.decisions); len(rel) != 0 {
		t.Errorf("the restarted run released %+v, want no second attempt", rel)
	}
	if sold := sales(t, e.recovered.decisions); len(sold) != 0 {
		t.Errorf("the restarted run planned the sales %+v, want none: the attempt is spent", sold)
	}
	return got
}

// recordedSale is the incident sale as decision d records it, nil for none.
func recordedSale(t *testing.T, d event.Envelope) *event.BoostRestoredSaleOrder {
	t.Helper()
	var p event.TargetsDecidedPayload
	decode(t, d, &p)
	if p.Boost.RestoredSale == nil {
		return nil
	}
	return p.Boost.RestoredSale.Order
}

// TestALateFillOfAnEndedSaleIsReadAfterARestartAsInTheFullRun: the
// incident's sale, released at session 20, expires at session 21 and the
// venue then reports an execution of it made before the expiry, so session
// 21's decision records the sale ended, canceled, with that execution and
// its venue order ID. A run restarted from that decision reads the
// venue's later reports on it as the run that never restarted: a second
// execution reported after the cancel, the same execution sent again, and
// a further cancel. Each execution and its commission is counted once, so
// each later decision and close-snapshot check is the full run's (ADR
// 0022: a fill after a cancel; ADR 0019, "Explained means": an execution
// sent again is a redelivery; ADR 0026, point 5: recovered, never
// recomputed).
func TestALateFillOfAnEndedSaleIsReadAfterARestartAsInTheFullRun(t *testing.T) {
	var one, two event.TargetWeightFillPayload
	var errs []error
	e := endSaleBy21(t, func(sale event.OrderReleasedPayload, opts *options) {
		one = part(sale, "fill-1:"+sale.ClientOrderID, sale.Quantity/2, 21)
		two = part(sale, "fill-2:"+sale.ClientOrderID, 1, 22)
		opts.partFill = map[int]int64{21: 0}
		opts.fills = map[int][]event.TargetWeightFillPayload{21: {one}, 22: {two}, 23: {two}}
		// A further cancel of the ended sale at session 24.
		again := event.LimitOrderLifecyclePayload{AccountID: sale.AccountID, ClientOrderID: sale.ClientOrderID, OrderID: "ib-" + sale.ClientOrderID,
			InstrumentID: sale.InstrumentID, Side: sale.Side, Status: event.OrderLifecycleStatusCanceled, Quantity: sale.Quantity,
			LimitPrice: sale.LimitPrice, OccurredAt: sessionEnd(24).Add(-3 * time.Hour), Message: "canceled"}
		opts.intrude = map[int]func(apply func(typ string, version uint32, id string, at time.Time, payload any) error){
			24: func(apply func(typ string, version uint32, id string, at time.Time, payload any) error) {
				errs = append(errs, apply(event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "cancel-again:"+sale.ClientOrderID, again.OccurredAt, again))
			},
		}
	})
	o := recordedSale(t, e.decision)
	if o == nil || o.Status != event.OrderLifecycleStatusCanceled || o.EndedAt.IsZero() || o.Filled != one.Quantity || len(o.Fills) != 1 ||
		o.VenueOrderID != "ib-"+e.sale.ClientOrderID || !o.Named || o.Attempt != 1 {
		t.Fatalf("session 21's decision records the sale as %+v, want it canceled under venue order ib-%s with its first execution", o, e.sale.ClientOrderID)
	}
	if len(errs) != 2 || errs[0] != nil || errs[1] != nil {
		t.Errorf("the further cancel of the ended sale was read as %v, want it read by both runs", errs)
	}
	got := sameAfterSession21(t, e)
	last := got[len(got)-1].Boost.RestoredSale
	if last == nil || last.Order == nil || last.Order.Filled != one.Quantity+two.Quantity || len(last.Order.Fills) != 2 ||
		last.Order.Fills[0].Commission+last.Order.Fills[1].Commission != one.Commission+two.Commission {
		t.Errorf("the restarted run's last decision records the sale as %+v, want each execution and its commission once", last)
	}
}

// TestARedeliveredFillOfASaleFilledInFullIsReadAfterARestart: the
// incident's sale fills in full at session 21, so session 21's decision
// records it filled in full, under its venue order ID, with its one
// execution. IBKR sends the execution again after the restart; the
// restarted run reads it as a redelivery, as the full run does, and counts
// nothing twice (ADR 0019, "Explained means"; ADR 0026, point 5).
func TestARedeliveredFillOfASaleFilledInFullIsReadAfterARestart(t *testing.T) {
	var whole event.TargetWeightFillPayload
	e := endSaleBy21(t, func(sale event.OrderReleasedPayload, opts *options) {
		whole = part(sale, "fill-all:"+sale.ClientOrderID, sale.Quantity, 21)
		// Nothing else executes it: the venue reports the one execution,
		// then sends it again.
		opts.hold = map[int]bool{}
		for n := 20; n < len(syntheticCloses()); n++ {
			opts.hold[n] = true
		}
		opts.fills = map[int][]event.TargetWeightFillPayload{21: {whole}, 22: {whole}}
	})
	o := recordedSale(t, e.decision)
	if o == nil || o.Status != "" || o.Filled != e.sale.Quantity || len(o.Fills) != 1 || o.VenueOrderID != "ib-"+e.sale.ClientOrderID || !o.Named {
		t.Fatalf("session 21's decision records the sale as %+v, want it filled in full under venue order ib-%s", o, e.sale.ClientOrderID)
	}
	got := sameAfterSession21(t, e)
	if s := got[0].Boost.RestoredSale; s == nil || s.Order == nil || s.Order.Filled != whole.Quantity || len(s.Order.Fills) != 1 {
		t.Errorf("the restarted run's first decision after the redelivery records the sale as %+v, want its one execution once", s)
	}
}

// TestAFillAnEndedSaleCannotHoldIsRefusedAfterARestartAsInTheFullRun: once
// the ended sale's executions are recorded, an execution taking it past
// its released shares, and another execution under a recorded fill ID, are
// refused by the restarted run exactly as by the full run (ADR 0019,
// "Explained means"; ADR 0026, point 5; fail closed, AGENTS.md).
func TestAFillAnEndedSaleCannotHoldIsRefusedAfterARestartAsInTheFullRun(t *testing.T) {
	var one event.TargetWeightFillPayload
	e := endSaleBy21(t, func(sale event.OrderReleasedPayload, opts *options) {
		one = part(sale, "fill-1:"+sale.ClientOrderID, sale.Quantity-1, 21)
		opts.partFill = map[int]int64{21: 0}
		opts.fills = map[int][]event.TargetWeightFillPayload{21: {one}}
	})
	if o := recordedSale(t, e.decision); o == nil || o.Status != event.OrderLifecycleStatusCanceled || o.Filled != one.Quantity {
		t.Fatalf("session 21's decision records the sale as %+v, want it canceled with %d filled", o, one.Quantity)
	}
	at := sessionEnd(len(syntheticCloses()) - 1).Add(time.Hour)
	past := part(e.sale, "fill-past:"+e.sale.ClientOrderID, 2, len(syntheticCloses())-1)
	reused := one
	reused.Quantity = 1
	for name, f := range map[string]event.TargetWeightFillPayload{"past its released shares": past, "under a recorded fill ID": reused} {
		want := applyAfter(t, e.full, event.FillEventType, event.FillTargetWeightVersion, f.FillID, at, f)
		got := applyAfter(t, e.recovered, event.FillEventType, event.FillTargetWeightVersion, f.FillID, at, f)
		if want == nil || got == nil || got.Error() != want.Error() {
			t.Errorf("an execution %s was read as %v after the restart, want the full run's refusal %v", name, got, want)
		}
	}
	if !strings.Contains(applyAfter(t, e.recovered, event.FillEventType, event.FillTargetWeightVersion, past.FillID, at, past).Error(), "past its") {
		t.Error("an execution past the sale's released shares is refused for another reason")
	}
	if late := slices.DeleteFunc(slices.Clone(e.full.decisions), func(d event.Envelope) bool { return !d.EventTime.After(sessionEnd(21)) }); len(late) == 0 {
		t.Fatal("setup: the full run decides nothing after session 21")
	}
	sameAfterSession21(t, e)
}
