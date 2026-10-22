package targetweight_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// TestARetiredOrderIsReplannedAtTheNextDecision: the first purchase of 39
// shares, released at session 0's close, is never sent, and the adapter
// retires it at session 1 as an order whose session is over
// (execution.order.retired). No venue will ever rule on it, so it leaves the
// orders awaiting the venue's word, counted neither accepted nor rejected,
// and ends with nothing filled: the next decided session plans the whole
// difference to the target in force, recording the cause unfilled with the
// order and its released shares, as an order that expired unfilled is
// (the owner's decision of 2026-10-22; ADR 0026, point 2, "Boost", amended
// 2026-10-22). The owner's retirement of an order the adapter can never
// send is read alike.
func TestARetiredOrderIsReplannedAtTheNextDecision(t *testing.T) {
	for name, by := range map[string]string{"the session over": "", "the owner's retirement": "richard"} {
		t.Run(name, func(t *testing.T) {
			r := simulate(t, options{retire: map[int][]int{1: {0}}, retireBy: by})
			first := releasedBy(t, r.decisions, 0)
			if len(first) != 1 || first[0].Quantity != 39 {
				t.Fatalf("first purchase: %+v, want 39 shares", first)
			}
			decided := targets(t, r.decisions)
			undecided := func(d event.BoostFigures) bool {
				return slices.ContainsFunc(d.UndecidedOrders, func(o event.BoostUndecidedOrder) bool { return o.ClientOrderID == first[0].ClientOrderID })
			}
			if !undecided(decided[0].Boost) {
				t.Fatalf("session 0 lists %+v undecided, want the purchase", decided[0].Boost.UndecidedOrders)
			}
			d := decided[1]
			want := event.BoostReplan{Cause: event.ReplanCauseUnfilled, ClientOrderID: first[0].ClientOrderID, Released: 39}
			if d.Outcome != event.TargetsOutcomeDecided || d.Boost.Replan != want || d.Boost.ReplanPending {
				t.Errorf("session 1: %s re-plan %+v pending %v, want decided with %+v", d.Outcome, d.Boost.Replan, d.Boost.ReplanPending, want)
			}
			if undecided(d.Boost) || d.Boost.VenueRejections != 0 {
				t.Errorf("session 1 lists %+v undecided and %d rejections, want the retired order in neither", d.Boost.UndecidedOrders, d.Boost.VenueRejections)
			}
			// 98% of 4000 at 104 is 37.69 shares, none held: 37.
			if got := releasedBy(t, r.decisions, 1); len(got) != 1 || got[0].Side != event.OrderSideBuy || got[0].Quantity != 37 {
				t.Errorf("session 1 releases %+v, want 37 shares bought", got)
			}
		})
	}
}

// TestARetiredSellIsNoLongerCountedAsBeingSold: session 17's sell of 18
// shares is never sent, and the adapter retires it at session 18. It ends
// there with nothing filled, so session 18 records its re-plan as unfilled
// and the risk gate no longer counts its shares as being sold (ADR 0019,
// amended 2026-10-15, "net of every working sell"): at session 19, whose
// target sells again, the account still holds them, so all 18 are sold.
// Never retired, the same sell would still be counted as being sold, and
// session 19 would sell nothing.
func TestARetiredSellIsNoLongerCountedAsBeingSold(t *testing.T) {
	r := simulate(t, options{closes: flippingCloses(), retire: map[int][]int{18: {17}}})
	sell := releasedBy(t, r.decisions, 17)
	if len(sell) != 1 || sell[0].Side != event.OrderSideSell || sell[0].Quantity != 18 {
		t.Fatalf("setup: session 17 releases %+v, want a sell of 18", sell)
	}
	want := event.BoostReplan{Cause: event.ReplanCauseUnfilled, ClientOrderID: sell[0].ClientOrderID, Released: 18}
	if got := targets(t, r.decisions)[18].Boost.Replan; got != want {
		t.Errorf("session 18 re-plan %+v, want %+v", got, want)
	}
	if got := releasedBy(t, r.decisions, 19); len(got) != 1 || got[0].Side != event.OrderSideSell || got[0].Quantity != 18 {
		t.Errorf("session 19 releases %+v, want all 18 shares sold", got)
	}
	unsent := simulate(t, options{closes: flippingCloses(), unsent: []int{17}})
	if got := releasedBy(t, unsent.decisions, 19); len(got) != 0 {
		t.Errorf("setup: with the sell never retired session 19 releases %+v, want nothing: the sell still counts as being sold", got)
	}
}

// TestARetiredOrderIsRecoveredAsTheFullRun: a run restarted from the
// decision before the retirement, or from the one after it, decides as the
// full run does (ADR 0026, point 5: recovered, never recomputed).
func TestARetiredOrderIsRecoveredAsTheFullRun(t *testing.T) {
	opts := options{closes: flippingCloses(), hold: heldFrom(2), retire: map[int][]int{20: {16}, 19: {17}}}
	for _, n := range []int{18, 19, 20, 24} {
		assertRecoveredAsTheFullRun(t, opts, n)
	}
}

// TestARunWithARetirementIsByteIdenticalAndReplays: a retirement is an
// input like any other, so a rerun writes the same bytes and the journal
// replays to the same decisions, for the adapter's retirement and the
// owner's alike.
func TestARunWithARetirementIsByteIdenticalAndReplays(t *testing.T) {
	assertByteIdenticalAndReplays(t, options{retire: map[int][]int{1: {0}}})
	assertByteIdenticalAndReplays(t, options{closes: flippingCloses(), retire: map[int][]int{18: {17}}, retireBy: "richard"})
}

// TestARetirementOfAnOrderAVenueMayHaveIsRefused: Boost reads a retirement
// only for an order of its own account that a decided session released, as
// released, and that no venue has said anything of; anything else is
// refused, so a retirement never ends an order that may be working (fail
// closed, AGENTS.md). A dry-run sends nothing, so it has nothing to retire.
func TestARetirementOfAnOrderAVenueMayHaveIsRefused(t *testing.T) {
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
	payload := func(o event.OrderReleasedPayload) event.OrderRetiredPayload {
		return event.OrderRetiredPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side,
			Quantity: o.Quantity, LimitPrice: o.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "session over",
			RetiredBy: event.OrderRetiredByAdapter, RetiredAt: at}
	}
	for name, c := range map[string]struct {
		mode    func(*event.TargetWeightConfigurationPayload)
		held    int64
		opts    *options
		change  func(r run, p *event.OrderRetiredPayload)
		version uint32
		want    string
	}{
		"another account":       {change: func(_ run, p *event.OrderRetiredPayload) { p.AccountID = "ira" }, want: "account"},
		"an order not released": {change: func(_ run, p *event.OrderRetiredPayload) { p.ClientOrderID = "tw-nothing" }, want: "did not release"},
		"another size":          {change: func(_ run, p *event.OrderRetiredPayload) { p.Quantity++ }, want: "released as"},
		"an order the venue ruled on": {change: func(r run, p *event.OrderRetiredPayload) {
			*p = payload(first(r, 0)) // the first purchase, filled at session 1
		}, want: "venue"},
		"an order the venue acknowledged": {change: func(r run, p *event.OrderRetiredPayload) {
			*p = payload(first(r, 17)) // sent, and named by IBKR as submitted
		}, want: "venue"},
		"an order the owner cleared": {opts: &options{closes: closes, hold: heldFrom(2), clear: map[int][]int{19: {16}}}, want: "cleared"},
		"an order already retired":   {opts: &options{closes: closes, hold: heldFrom(2), retire: map[int][]int{19: {16}}}, want: "retired"},
		"a dry-run's order":          {mode: boostDryRun, held: 58, want: "dry-run"},
		"another version":            {version: 2, want: "schema version"},
	} {
		t.Run(name, func(t *testing.T) {
			o := opts
			if c.opts != nil {
				o = *c.opts
			}
			o.mutate, o.startHeld = c.mode, c.held
			r := simulate(t, o)
			p := payload(first(r, 16))
			if c.change != nil {
				c.change(r, &p)
			}
			version := event.OrderRetiredSchemaVersion
			if c.version != 0 {
				version = c.version
			}
			err := applyAfter(t, r, event.OrderRetiredEventType, version, "retired:late", at, p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("the retirement %+v: %v, want it refused mentioning %q", p, err, c.want)
			}
		})
	}
}

// TestAVenueReportOnARetiredOrderIsRefused: a retired order never left the
// adapter, so no venue report on it can be true; one that arrives proves
// the retirement wrong and is refused, which stops the run for a person, as
// a report on an order the owner cleared is (fail closed, AGENTS.md).
func TestAVenueReportOnARetiredOrderIsRefused(t *testing.T) {
	closes := flippingCloses()[:22]
	r := simulate(t, options{closes: closes, hold: heldFrom(2), retire: map[int][]int{19: {17}}})
	o := releasedBy(t, r.decisions, 17)[0]
	at := sessionEnd(len(closes) - 1).Add(time.Hour)
	report := event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "ib-" + o.ClientOrderID,
		InstrumentID: o.InstrumentID, Side: o.Side, Status: event.OrderLifecycleStatusSubmitted, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: at}
	if err := applyAfter(t, r, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "late:ack", at, report); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Errorf("a report on the retired order was read, err %v; want it refused as contradicting the retirement", err)
	}
	fill := (&account{}).fill(o, "late-fill", 1, o.LimitPrice, at)
	if err := applyAfter(t, r, event.FillEventType, event.FillTargetWeightVersion, "late-fill", at, fill); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Errorf("a fill of the retired order was read, err %v; want it refused as contradicting the retirement", err)
	}
}

// saleOf is the sale back to 98% released under tag, the first attempt's.
func saleOf(t *testing.T, r run, tag string) event.OrderReleasedPayload {
	t.Helper()
	id := orderplan.SaleClientOrderID(tag, "SPY")
	for _, e := range ofType(r.decisions, event.OrderReleasedEventType) {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		if p.ClientOrderID == id {
			return p
		}
	}
	t.Fatalf("setup: no sale back to 98%% %s released under %s", id, tag)
	return event.OrderReleasedPayload{}
}

// TestARetiredSaleBackTo98IsReattempted: the kill switch refuses Boost's
// exit at session 19, which fires the sale back to 98%, released from
// session 20's broker snapshot, never by a decided session's order, so it
// is not among the orders awaiting the venue's word. The adapter never
// sends it and retires it at session 21. Boost reads the retirement by the
// sale's own rule: it ends with nothing filled, the gate no longer counts
// its shares as being sold, and, since nothing was sent, the attempt is not
// spent (ADR 0019, amended 2026-10-17, reading 4, as the owner's decision
// of 2026-10-22 reads it), so the sale is attempted again at the next
// opportunity, session 21's broker snapshot, under a client order ID of its
// own and for the same shares (ADR 0026, point 2, "Boost", amended
// 2026-10-22).
func TestARetiredSaleBackTo98IsReattempted(t *testing.T) {
	first := orderplan.SaleClientOrderID("kill:19", "SPY")
	r := simulate(t, options{brokers: true, kill: map[int]bool{19: true}, retireIDs: map[int][]string{21: {first}}})
	sold := sales(t, r.decisions)
	if len(sold) != 2 {
		t.Fatalf("sales %+v, want the retired sale and its second attempt", sold)
	}
	if sold[0].ClientOrderID != first || sold[1].ClientOrderID == first {
		t.Errorf("sales %s then %s, want %s then a client order ID of its own", sold[0].ClientOrderID, sold[1].ClientOrderID, first)
	}
	if sold[1].Quantity != sold[0].Quantity || sold[1].Tag != "kill:19" {
		t.Errorf("second attempt %+v, want the same %d shares under kill:19", sold[1], sold[0].Quantity)
	}
	again := saleOf(t, r, "kill:19")
	if again.ClientOrderID != first {
		t.Fatalf("setup: %s", again.ClientOrderID)
	}
	for _, e := range ofType(r.decisions, event.OrderReleasedEventType) {
		var p event.OrderReleasedPayload
		decode(t, e, &p)
		if p.ClientOrderID == sold[1].ClientOrderID && !e.EventTime.Equal(sessionEnd(21)) {
			t.Errorf("second attempt released at %s, want the next opportunity, session 21's broker snapshot", e.EventTime)
		}
	}
}

// TestARetiredUrgentSaleReleasedOutsideADecisionIsReattempted: the margin
// alarm turns urgent at session 20's broker snapshot, outside any decided
// session, and fires the sale back to 98%; the adapter never sends it and
// retires it at session 21, while the alarm is still urgent. The
// retirement is read, the sale's shares are no longer counted as being
// sold, and the next broker snapshot, still urgent, attempts the sale again
// (ADR 0019, amended 2026-10-17, readings 1 and 4, as the owner's decision
// of 2026-10-22 reads them).
func TestARetiredUrgentSaleReleasedOutsideADecisionIsReattempted(t *testing.T) {
	urgent := func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.15 * b.NetLiquidation }
	opts := options{brokers: true, brokerMutate: map[int]func(*event.BrokerSnapshotPayload){20: urgent, 21: urgent}, closes: syntheticCloses()[:22]}
	setup := simulate(t, opts)
	sold := sales(t, setup.decisions)
	if len(sold) != 1 || sold[0].Sale.Kind != event.SaleKindUrgent {
		t.Fatalf("setup: sales %+v, want one urgent sale", sold)
	}
	first := sold[0].ClientOrderID
	opts.retireIDs = map[int][]string{21: {first}}
	r := simulate(t, opts)
	sold = sales(t, r.decisions)
	if len(sold) != 2 || sold[1].ClientOrderID == first || sold[1].Tag != sold[0].Tag {
		t.Fatalf("sales %+v, want the retired urgent sale attempted again under its tag and an ID of its own", sold)
	}
}

// TestAVenueReportOnARetiredSaleIsRefused: a retired sale never left the
// adapter, so a venue's report on it proves the retirement wrong and stops
// the run, as on any retired order (fail closed, AGENTS.md).
func TestAVenueReportOnARetiredSaleIsRefused(t *testing.T) {
	first := orderplan.SaleClientOrderID("kill:19", "SPY")
	r := simulate(t, options{brokers: true, kill: map[int]bool{19: true}, retireIDs: map[int][]string{21: {first}}})
	o := saleOf(t, r, "kill:19")
	at := sessionEnd(len(syntheticCloses()) - 1).Add(time.Hour)
	fill := (&account{}).fill(o, "late-fill", 1, o.LimitPrice, at)
	if err := applyAfter(t, r, event.FillEventType, event.FillTargetWeightVersion, "late-fill", at, fill); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Errorf("a fill of the retired sale was read, err %v; want it refused as contradicting the retirement", err)
	}
}

// TestARetiredOrderSurvivesARecoveryFromAWithheldDecision: the first
// purchase is never sent and is retired at session 1, whose decision is
// withheld for want of an account snapshot, so it records the purchase as
// the watched order, ended. A run restarted from that decision knows the
// purchase was retired, as the full run does: a venue fill naming it is
// refused, and the re-plan at session 2 is the full run's (ADR 0026, point
// 5: recovered, never recomputed).
func TestARetiredOrderSurvivesARecoveryFromAWithheldDecision(t *testing.T) {
	opts := options{retire: map[int][]int{1: {0}}, noSnap: map[int]bool{1: true}}
	full := simulate(t, opts)
	decisions := ofType(full.decisions, event.TargetsDecidedEventType)
	if d := targets(t, full.decisions)[1]; d.Outcome != event.TargetsOutcomeWithheld {
		t.Fatalf("setup: session 1 is %s, want withheld", d.Outcome)
	}
	assertRecoveredAsTheFullRun(t, opts, 1)
	o := releasedBy(t, full.decisions, 0)[0]
	recovery := decisions[1]
	at := sessionEnd(1).Add(time.Hour)
	recovered := simulate(t, options{recovery: &recovery, snapshots: full.inputs, reports: venueReports(full.inputs, 1), closes: syntheticCloses()[:2]})
	fill := (&account{}).fill(o, "late-fill", 1, o.LimitPrice, at)
	for name, r := range map[string]run{"the full run": simulate(t, options{retire: opts.retire, noSnap: opts.noSnap, closes: syntheticCloses()[:2]}), "the recovered run": recovered} {
		if err := applyAfter(t, r, event.FillEventType, event.FillTargetWeightVersion, "late-fill", at, fill); err == nil || !strings.Contains(err.Error(), "retired") {
			t.Errorf("%s read a fill of the retired order, err %v; want it refused as contradicting the retirement", name, err)
		}
	}
}

// TestARetirementOfASaleAVenueReportedOnIsRefused: the sale back to 98% is
// retired by its own rule only while no venue has named it or reported an
// execution of it: one that did was sent, and its retirement is refused (fail
// closed, AGENTS.md).
func TestARetirementOfASaleAVenueReportedOnIsRefused(t *testing.T) {
	first := orderplan.SaleClientOrderID("kill:19", "SPY")
	at := sessionEnd(len(syntheticCloses()) - 1).Add(time.Hour)
	retirement := func(o event.OrderReleasedPayload) event.OrderRetiredPayload {
		return event.OrderRetiredPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side,
			Quantity: o.Quantity, LimitPrice: o.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "session over",
			RetiredBy: event.OrderRetiredByAdapter, RetiredAt: at}
	}
	sent := simulate(t, options{brokers: true, kill: map[int]bool{19: true}})
	o := saleOf(t, sent, "kill:19")
	if err := applyAfter(t, sent, event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:late", at, retirement(o)); err == nil || !strings.Contains(err.Error(), "named") {
		t.Errorf("the retirement of a sale IBKR acknowledged: %v, want it refused as named", err)
	}
	unsent := simulate(t, options{brokers: true, kill: map[int]bool{19: true}, unsentIDs: []string{first}})
	s := &stream{hash: unsent.hash, sequence: uint64(len(unsent.inputs))}
	fill := (&account{}).fill(o, "fill:only", 1, o.LimitPrice, at)
	if _, err := unsent.recorder.Apply(context.Background(), s.envelope(t, event.FillEventType, event.FillTargetWeightVersion, "fill:only", at, fill)); err != nil {
		t.Fatalf("setup: the execution of the sale: %v", err)
	}
	_, err := unsent.recorder.Apply(context.Background(), s.envelope(t, event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:late", at, retirement(o)))
	if err == nil || !strings.Contains(err.Error(), "named") {
		t.Errorf("the retirement of a sale with an execution: %v, want it refused", err)
	}
}

// TestARecoveredRetiredOrderTheGateKnowsIsRefused: a recovered decision
// lists a retired order under the client order ID of a sale a broker
// snapshot released before the recovery, so the gate already knows it; the
// recovery is refused rather than restore one order over another (fail
// closed, AGENTS.md).
func TestARecoveredRetiredOrderTheGateKnowsIsRefused(t *testing.T) {
	cfg := testConfiguration()
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: sessionEnd(0)})
	if err != nil {
		t.Fatal(err)
	}
	rec := journal.NewRecorder(h)
	s := &stream{hash: event.TargetWeightConfigurationHash(cfg)}
	apply := func(e event.Envelope) ([]event.Envelope, error) { return rec.Apply(context.Background(), e) }
	at := sessionEnd(0).Add(-time.Hour)
	if _, err := apply(s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", at, cfg)); err != nil {
		t.Fatal(err)
	}
	broker := (&account{cash: -4000, held: 60}).broker(at, 100)
	broker.ExcessLiquidity = 0.1 * broker.NetLiquidation
	out, err := apply(s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "broker:pre", at, broker))
	if err != nil {
		t.Fatal(err)
	}
	var sale event.OrderReleasedPayload
	for _, e := range ofType(out, event.OrderReleasedEventType) {
		decode(t, e, &sale)
	}
	if sale.ClientOrderID == "" {
		t.Fatalf("setup: the urgent snapshot released no sale: %d decisions", len(out))
	}
	full := simulate(t, options{retire: map[int][]int{1: {0}}})
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[1]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	p.Boost.RetiredOrders = append(p.Boost.RetiredOrders, event.BoostRetiredOrder{ClientOrderID: sale.ClientOrderID, Side: sale.Side,
		Released: sale.Quantity, LimitPrice: sale.LimitPrice, RetiredAt: at})
	_, err = apply(s.envelope(t, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, recovery.ID, at, p))
	if err == nil || !strings.Contains(err.Error(), "recovered decision") || !strings.Contains(err.Error(), sale.ClientOrderID) {
		t.Errorf("a recovered retired order the gate already knows: %v, want the recovery refused naming it", err)
	}
}

// TestARetiredSaleIsRecordedWithItsAttempt: a decision records a retired
// sale back to 98% with its tag and the number of the attempt retired, so a
// run restarted from it gives the next attempt a client order ID of its own
// (ADR 0026, point 2, "Order identity", and point 5: recovered, never
// recomputed).
func TestARetiredSaleIsRecordedWithItsAttempt(t *testing.T) {
	first := orderplan.SaleClientOrderID("kill:19", "SPY")
	r := simulate(t, options{brokers: true, kill: map[int]bool{19: true}, retireIDs: map[int][]string{21: {first}}})
	d := targets(t, r.decisions)[21]
	i := slices.IndexFunc(d.Boost.RetiredOrders, func(o event.BoostRetiredOrder) bool { return o.ClientOrderID == first })
	if i < 0 {
		t.Fatalf("session 21 records retired orders %+v, want the sale", d.Boost.RetiredOrders)
	}
	if got := d.Boost.RetiredOrders[i]; got.SaleTag != "kill:19" || got.SaleAttempt != 1 {
		t.Errorf("the retired sale is recorded as %+v, want tag kill:19 and attempt 1", got)
	}
}

// TestASaleAfterARecoveredRetiredSaleHasAnIDOfItsOwn: a run restarted from
// a decision that records the incident's sale retired as its first attempt
// plans the next attempt under the second attempt's client order ID, never
// the retired order's, as the full run does (ADR 0026, point 2, "Order
// identity", and point 5: recovered, never recomputed).
func TestASaleAfterARecoveredRetiredSaleHasAnIDOfItsOwn(t *testing.T) {
	full := simulate(t, options{brokers: true})
	state := haltedState(t)
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[17]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	retired := orderplan.SaleClientOrderID(state.ID, "SPY")
	p.Boost.RetiredOrders = append(p.Boost.RetiredOrders, event.BoostRetiredOrder{ClientOrderID: retired, Side: event.OrderSideSell,
		Released: 5, LimitPrice: 99, RetiredAt: sessionEnd(17), SaleTag: state.ID, SaleAttempt: 1})
	s := &stream{hash: event.TargetWeightConfigurationHash(testConfiguration())}
	edited := s.envelope(t, recovery.Type, recovery.SchemaVersion, recovery.ID, recovery.EventTime, p)
	edited.Source = recovery.Source
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	r := simulate(t, options{recovery: &edited, snapshots: full.inputs, brokers: true, prelude: []event.Envelope{state}, brokerMutate: urgent})
	sold := sales(t, r.decisions)
	if want := orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 1); len(sold) != 1 || sold[0].ClientOrderID != want {
		t.Fatalf("sales %+v, want one under %s, the second attempt's ID, not the retired %s", sold, want, retired)
	}
}

// TestAFreshRunApprovedAfterARetiredSaleSellsUnderAnIDOfItsOwn: the owner's
// approval carries how many of the incident's sales were retired as never
// sent, so the fresh run's first attempt has the next attempt's client
// order ID, never a retired order's (ADR 0019, amended 2026-10-17, reading
// 4; ADR 0026, point 2, "Order identity").
func TestAFreshRunApprovedAfterARetiredSaleSellsUnderAnIDOfItsOwn(t *testing.T) {
	h, s := configured(t)
	p := approval(false)
	p.IncidentInForce = &event.IncidentInForce{IncidentID: "engine-state:joint:1", State: event.EngineStateHalted, SaleAttempted: new(false), SaleRetiredAttempts: 1}
	mustApply(t, h, s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", sessionEnd(0), p))
	held := &account{cash: -2000, held: 60}
	at := sessionEnd(0)
	mustApply(t, h, s.envelope(t, event.CompletedBarEventType, event.CompletedBarAdjustedVersion, "b0", at, bar(100, at)))
	b := held.broker(at, 100)
	b.ExcessLiquidity = 0.1 * b.NetLiquidation
	out := mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k0", at, b))
	sold := sales(t, out)
	if want := orderplan.SaleAttemptClientOrderID("engine-state:joint:1", "SPY", 1); len(sold) != 1 || sold[0].ClientOrderID != want {
		t.Fatalf("sales %+v, want one under %s, the second attempt's ID", sold, want)
	}
}

// TestARecoveredRunRefusesTheRetirementOfAnOrderAVenueNamed: a venue named
// the watched order and the working one beside it before the decision, so
// the full run refuses their retirement, and a run restarted from the
// decision refuses it as the full run does (ADR 0026, point 2, "Boost",
// amended 2026-10-22, and point 5: recovered, never recomputed).
func TestARecoveredRunRefusesTheRetirementOfAnOrderAVenueNamed(t *testing.T) {
	closes := flippingCloses()[:20]
	opts := options{closes: closes, hold: heldFrom(18), partFill: map[int]int64{17: 5}, cancelAt: map[int]int{17: 30}}
	full := simulate(t, opts)
	decisions := ofType(full.decisions, event.TargetsDecidedEventType)
	n := len(decisions) - 1
	last := targets(t, full.decisions)[n].Boost
	if len(last.WorkingOrders) != 1 || last.Watched.ClientOrderID == "" {
		t.Fatalf("setup: the last decision records %+v, want a working order beside the watched one", last)
	}
	recovery := decisions[n]
	recovered := simulate(t, options{recovery: &recovery, snapshots: full.inputs, reports: venueReports(full.inputs, n), closes: closes})
	at := sessionEnd(len(closes) - 1).Add(time.Hour)
	for _, id := range []string{last.WorkingOrders[0].ClientOrderID, last.Watched.ClientOrderID} {
		var o event.OrderReleasedPayload
		for _, rel := range released(t, full.decisions) {
			if rel.ClientOrderID == id {
				o = rel
			}
		}
		p := event.OrderRetiredPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side,
			Quantity: o.Quantity, LimitPrice: o.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "session over",
			RetiredBy: event.OrderRetiredByAdapter, RetiredAt: at}
		for name, r := range map[string]run{"the full run": full, "the recovered run": recovered} {
			if err := applyAfter(t, r, event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:"+id, at, p); err == nil || !strings.Contains(err.Error(), "venue") {
				t.Errorf("%s: the retirement of %s, which a venue named: %v, want it refused", name, id, err)
			}
		}
	}
}

// TestARecoveredRetiredSaleUnderAnotherAttemptsIDIsRefused: a recovered
// decision recording a retired sale whose client order ID is not the one
// its tag and attempt give is refused, rather than count a retired attempt
// the run never released (fail closed, AGENTS.md).
func TestARecoveredRetiredSaleUnderAnotherAttemptsIDIsRefused(t *testing.T) {
	full := simulate(t, options{brokers: true})
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[17]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	p.Boost.RetiredOrders = append(p.Boost.RetiredOrders, event.BoostRetiredOrder{ClientOrderID: orderplan.SaleClientOrderID("kill:1", "SPY"),
		Side: event.OrderSideSell, Released: 5, LimitPrice: 99, RetiredAt: sessionEnd(17), SaleTag: "kill:1", SaleAttempt: 2})
	h, s := configured(t)
	_, err := h.Apply(context.Background(), s.envelope(t, recovery.Type, recovery.SchemaVersion, recovery.ID, recovery.EventTime, p))
	if err == nil || !strings.Contains(err.Error(), "attempt 2") {
		t.Errorf("a retired sale recorded under another attempt's ID: %v, want the recovery refused", err)
	}
}

// TestARunRecoveredFromAnApprovedRunsDecisionKeepsItsRetiredSales: the
// owner's approval of a fresh run carries one of the incident's sales
// retired as never sent; the fresh run retires nothing itself, and each of
// its decisions records the sale attempt the approval restored, so a run
// restarted from one plans the next attempt under the second attempt's
// client order ID, never the retired order's, as the approved run would
// (ADR 0026, point 2, "Order identity", and point 5: recovered, never
// recomputed).
func TestARunRecoveredFromAnApprovedRunsDecisionKeepsItsRetiredSales(t *testing.T) {
	state := haltedState(t)
	p := approval(false)
	p.IncidentInForce = &event.IncidentInForce{IncidentID: state.ID, State: event.EngineStateHalted, SaleAttempted: new(false), SaleRetiredAttempts: 1}
	s := &stream{hash: event.TargetWeightConfigurationHash(testConfiguration())}
	approved := simulate(t, options{brokers: true, recorded: []event.Envelope{s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", sessionEnd(0), p)}, closes: syntheticCloses()[:18]})
	decisions := ofType(approved.decisions, event.TargetsDecidedEventType)
	recovery := decisions[len(decisions)-1]
	var d event.TargetsDecidedPayload
	decode(t, recovery, &d)
	if want := (event.BoostRestoredSale{IncidentID: state.ID, RetiredAttempts: 1}); d.Boost.RestoredSale == nil || *d.Boost.RestoredSale != want {
		t.Fatalf("the approved run's decision records the restored sale as %+v, want %+v", d.Boost.RestoredSale, want)
	}
	full := simulate(t, options{brokers: true})
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, brokers: true, prelude: []event.Envelope{state}, brokerMutate: urgent})
	sold := sales(t, r.decisions)
	if want := orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 1); len(sold) != 1 || sold[0].ClientOrderID != want {
		t.Fatalf("sales %+v, want one under %s, the second attempt's ID, not the retired %s", sold, want, orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 0))
	}
}

// TestASnapshotBeforeTheRecoveryNeverFiresTheRecordedIncidentsSale: an
// earlier run's recorded engine state puts the gate in its incident, whose
// sale attempts only the recovered decision or the owner's approval states;
// a broker snapshot read before either that would fire the incident's sale
// is refused, so no attempt is planned without the restored record (ADR
// 0026, point 5: recovered, never recomputed; fail closed, AGENTS.md). One
// that fires nothing is read, as before.
func TestASnapshotBeforeTheRecoveryNeverFiresTheRecordedIncidentsSale(t *testing.T) {
	h, s := configured(t)
	mustApply(t, h, haltedState(t))
	at := sessionEnd(0)
	b := (&account{cash: -2000, held: 60}).broker(at, 100)
	mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k0", at, b))
	at = at.Add(time.Minute)
	b = (&account{cash: -2000, held: 60}).broker(at, 100)
	b.ExcessLiquidity = 0.1 * b.NetLiquidation
	out, err := h.Apply(context.Background(), s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k1", at, b))
	if err == nil || !strings.Contains(err.Error(), "recovered decision") {
		t.Fatalf("an urgent snapshot before the recovery: %v, sales %+v; want it refused before any sale", err, sales(t, out))
	}
	// A recorded incident Degraded on another instrument tags no sale of
	// SPY: the urgent sale carries the alarm's tag, this run's own, and goes.
	h, s = configured(t)
	mustApply(t, h, recordedState(t, "engine-state:joint:recorded", event.EngineStatePayload{
		State: event.EngineStateDegraded, Reason: event.EngineStateReasonRiskLimitBreached,
		Detail: "recorded: IEF quantity in dispute", ScopeInstrumentIDs: []string{"IEF"},
	}, event.EngineStateScopedVersion))
	if sold := sales(t, mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k1", at, b))); len(sold) != 1 || sold[0].Tag == "engine-state:joint:recorded" {
		t.Fatalf("an urgent snapshot under a recorded incident on another instrument planned %+v, want the alarm's one sale", sold)
	}
}

// TestARefusedSnapshotBeforeTheRecoveryLeavesTheUrgentSaleToFire: refusing
// a broker snapshot read before the recovered decision changes nothing, so
// the alarm it would have turned urgent is still to come: once the recovered
// decision states the incident's attempts, the next urgent snapshot journals
// the alarm and fires the sale (ADR 0019, amended 2026-10-15; ADR 0027,
// section 6: replay goes on past a refused input; fail closed, AGENTS.md).
func TestARefusedSnapshotBeforeTheRecoveryLeavesTheUrgentSaleToFire(t *testing.T) {
	full := simulate(t, options{brokers: true})
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[17]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	state := haltedState(t)
	p.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: state.ID}
	h, s := configured(t)
	mustApply(t, h, state)
	at := sessionEnd(0)
	mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k0", at, (&account{cash: -2000, held: 60}).broker(at, 100)))
	urgent := func(at time.Time) event.BrokerSnapshotPayload {
		b := (&account{cash: -2000, held: 60}).broker(at, 100)
		b.ExcessLiquidity = 0.1 * b.NetLiquidation
		return b
	}
	at = at.Add(time.Minute)
	if _, err := h.Apply(context.Background(), s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k1", at, urgent(at))); err == nil {
		t.Fatal("setup: the urgent snapshot before the recovery was read, want it refused")
	}
	mustApply(t, h, s.envelope(t, recovery.Type, recovery.SchemaVersion, recovery.ID, recovery.EventTime, p))
	at = at.Add(time.Minute)
	out := mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k2", at, urgent(at)))
	if alarms, sold := ofType(out, event.MarginAlarmEventType), sales(t, out); len(alarms) != 1 || len(sold) != 1 || sold[0].Tag != state.ID {
		t.Fatalf("the urgent snapshot after the recovery emitted alarms %+v and sales %+v, want the alarm and the incident's one sale", alarms, sold)
	}
}

// TestARecoveryThatStatesNoAttemptLeavesTheRecordedIncidentsSaleRefused: a
// recovered decision that restores no sale attempt for the incident the
// recorded engine state restored does not state its attempts, so a broker
// snapshot before the run's first bar that would fire its sale is refused
// still (ADR 0019, amended 2026-10-15: one attempt; fail closed, AGENTS.md).
func TestARecoveryThatStatesNoAttemptLeavesTheRecordedIncidentsSaleRefused(t *testing.T) {
	full := simulate(t, options{brokers: true})
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[17]
	h, s := configured(t)
	mustApply(t, h, recovery)
	mustApply(t, h, haltedState(t))
	at := sessionEnd(0)
	mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k0", at, (&account{cash: -2000, held: 60}).broker(at, 100)))
	at = at.Add(time.Minute)
	b := (&account{cash: -2000, held: 60}).broker(at, 100)
	b.ExcessLiquidity = 0.1 * b.NetLiquidation
	out, err := h.Apply(context.Background(), s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k1", at, b))
	if err == nil || !strings.Contains(err.Error(), "recovered decision") {
		t.Fatalf("an urgent snapshot after a recovery stating no attempt: %v, sales %+v; want it refused", err, sales(t, out))
	}
	// One the observation itself refuses is refused for its own reason.
	stale := (&account{cash: -2000, held: 60}).broker(sessionEnd(0), 100)
	if _, err := h.Apply(context.Background(), s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k2", sessionEnd(0), stale)); err == nil || !strings.Contains(err.Error(), "does not follow") {
		t.Errorf("a snapshot no later than the last: %v, want it refused as not following", err)
	}
}

// TestARestartedRunsOwnSaleAttemptIsRecordedInItsDecisions: a run restarted
// into an earlier run's incident with no approval attempts the incident's
// sale itself; its later decisions record that attempt (restored_sale), so a
// run restarted from one never attempts the sale again under the live
// attempt's client order ID (ADR 0019, amended 2026-10-15: one attempt; ADR
// 0026, point 5: recovered, never recomputed).
func TestARestartedRunsOwnSaleAttemptIsRecordedInItsDecisions(t *testing.T) {
	state := haltedState(t)
	urgentAt := func(n int) map[int]func(*event.BrokerSnapshotPayload) {
		return map[int]func(*event.BrokerSnapshotPayload){n: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	}
	full := simulate(t, options{brokers: true})
	first := simulate(t, options{brokers: true, prelude: []event.Envelope{state}, closes: syntheticCloses()[:18], snapshots: full.inputs, brokerMutate: urgentAt(16)})
	live := orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 0)
	if sold := sales(t, first.decisions); len(sold) != 1 || sold[0].ClientOrderID != live {
		t.Fatalf("setup: the restarted run's sales %+v, want one under %s", sold, live)
	}
	decisions := ofType(first.decisions, event.TargetsDecidedEventType)
	recovery := decisions[len(decisions)-1]
	var d event.TargetsDecidedPayload
	decode(t, recovery, &d)
	if want := (event.BoostRestoredSale{IncidentID: state.ID, Attempted: true}); d.Boost.RestoredSale == nil || *d.Boost.RestoredSale != want {
		t.Fatalf("the decision after the run's own sale records the restored sale as %+v, want %+v", d.Boost.RestoredSale, want)
	}
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, brokers: true, prelude: []event.Envelope{state}, brokerMutate: urgentAt(22)})
	if sold := sales(t, r.decisions); len(sold) != 0 {
		t.Fatalf("the run restarted from that decision sold %+v, want nothing: the sale under %s was attempted", sold, live)
	}
}

// TestARecoveredSaleAttemptTheRunAlreadyHoldsIsRefused: a sale fired by a
// broker snapshot before the recovery writes its tag's attempt record, so a
// recovered decision restoring an attempt under that tag is refused rather
// than overwrite it (ADR 0019, amended 2026-10-15: one attempt; fail
// closed, AGENTS.md).
func TestARecoveredSaleAttemptTheRunAlreadyHoldsIsRefused(t *testing.T) {
	full := simulate(t, options{brokers: true})
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[17]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	h, s := configured(t)
	at := sessionEnd(0)
	b := (&account{cash: -2000, held: 60}).broker(at, 100)
	b.ExcessLiquidity = 0.1 * b.NetLiquidation
	out := mustApply(t, h, s.envelope(t, event.BrokerSnapshotEventType, event.BrokerSnapshotSchemaVersion, "k0", at, b))
	alarms := ofType(out, event.MarginAlarmEventType)
	if len(alarms) != 1 || len(sales(t, out)) != 1 {
		t.Fatalf("setup: the urgent snapshot emitted %+v, want an alarm and its sale", out)
	}
	p.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: alarms[0].ID}
	_, err := h.Apply(context.Background(), s.envelope(t, recovery.Type, recovery.SchemaVersion, recovery.ID, recovery.EventTime, p))
	if err == nil || !strings.Contains(err.Error(), "restored once") {
		t.Errorf("a recovered sale attempt the run already holds: %v, want the recovery refused", err)
	}
}

// TestADecisionRecordsTheSaleAttemptAsTheRunHoldsIt: a fresh run whose
// approval restored the incident's sale as not attempted, with one retired,
// attempts it itself; each later decision records the attempt as the run
// then holds it, so a run restarted from one never attempts the sale again
// under the live attempt's client order ID (ADR 0019, amended 2026-10-15:
// one attempt; ADR 0026, point 2, "Order identity", and point 5: recovered,
// never recomputed).
func TestADecisionRecordsTheSaleAttemptAsTheRunHoldsIt(t *testing.T) {
	state := haltedState(t)
	p := approval(false)
	p.IncidentInForce = &event.IncidentInForce{IncidentID: state.ID, State: event.EngineStateHalted, SaleAttempted: new(false), SaleRetiredAttempts: 1}
	s := &stream{hash: event.TargetWeightConfigurationHash(testConfiguration())}
	urgentAt := func(n int) map[int]func(*event.BrokerSnapshotPayload) {
		return map[int]func(*event.BrokerSnapshotPayload){n: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	}
	full := simulate(t, options{brokers: true})
	approved := simulate(t, options{brokers: true, recorded: []event.Envelope{s.envelope(t, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, "approval", sessionEnd(0), p)},
		closes: syntheticCloses()[:18], snapshots: full.inputs, brokerMutate: urgentAt(16)})
	live := orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 1)
	if sold := sales(t, approved.decisions); len(sold) != 1 || sold[0].ClientOrderID != live {
		t.Fatalf("setup: the approved run's sales %+v, want one under %s", sold, live)
	}
	decisions := ofType(approved.decisions, event.TargetsDecidedEventType)
	recovery := decisions[len(decisions)-1]
	var d event.TargetsDecidedPayload
	decode(t, recovery, &d)
	if want := (event.BoostRestoredSale{IncidentID: state.ID, Attempted: true, RetiredAttempts: 1}); d.Boost.RestoredSale == nil || *d.Boost.RestoredSale != want {
		t.Fatalf("the decision after the sale records the restored sale as %+v, want %+v", d.Boost.RestoredSale, want)
	}
	r := simulate(t, options{recovery: &recovery, snapshots: full.inputs, brokers: true, prelude: []event.Envelope{state}, brokerMutate: urgentAt(22)})
	if sold := sales(t, r.decisions); len(sold) != 0 {
		t.Fatalf("the restarted run sold %+v, want nothing: the sale under %s was attempted", sold, live)
	}
}

// TestARecoveryRestoresTheApprovalsRetiredSalesBeforeTheRunsOwn: a decision
// records both the approval's retired attempt and this run's own retired
// second attempt; the recovery restores the approval's count first, so the
// run's own retirement is counted once and the next attempt has the third
// attempt's client order ID, skipping none (ADR 0026, point 2, "Order
// identity", and point 5: recovered, never recomputed).
func TestARecoveryRestoresTheApprovalsRetiredSalesBeforeTheRunsOwn(t *testing.T) {
	full := simulate(t, options{brokers: true})
	state := haltedState(t)
	recovery := ofType(full.decisions, event.TargetsDecidedEventType)[17]
	var p event.TargetsDecidedPayload
	decode(t, recovery, &p)
	p.Boost.RestoredSale = &event.BoostRestoredSale{IncidentID: state.ID, RetiredAttempts: 1}
	p.Boost.RetiredOrders = append(p.Boost.RetiredOrders, event.BoostRetiredOrder{ClientOrderID: orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 1),
		Side: event.OrderSideSell, Released: 5, LimitPrice: 99, RetiredAt: sessionEnd(17), SaleTag: state.ID, SaleAttempt: 2})
	s := &stream{hash: event.TargetWeightConfigurationHash(testConfiguration())}
	edited := s.envelope(t, recovery.Type, recovery.SchemaVersion, recovery.ID, recovery.EventTime, p)
	edited.Source = recovery.Source
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	r := simulate(t, options{recovery: &edited, snapshots: full.inputs, brokers: true, prelude: []event.Envelope{state}, brokerMutate: urgent})
	sold := sales(t, r.decisions)
	if want := orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 2); len(sold) != 1 || sold[0].ClientOrderID != want {
		t.Fatalf("sales %+v, want one under %s, the third attempt's ID", sold, want)
	}
}

// haltedExitSale restarts the full run from its boosted decision of session
// 17 into an earlier run's Halted incident; the incident refuses Boost's
// exit at session 20, whose decision fires the incident's sale back to 98%
// within the same input. The adapter never sends the sale. It returns the
// run, the incident, the sale released and session 20's decision.
func haltedExitSale(t *testing.T) (r run, state event.Envelope, sale event.OrderReleasedPayload, decision event.Envelope) {
	t.Helper()
	full := simulate(t, options{brokers: true})
	state = haltedState(t)
	r = restartAt17(t, full, options{prelude: []event.Envelope{state}, unsentIDs: []string{orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 0)}})
	rel := released(t, r.decisions)
	if len(rel) != 1 || rel[0].ClientOrderID != orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 0) {
		t.Fatalf("setup: released %+v, want the incident's first sale", rel)
	}
	var at time.Time
	for _, e := range ofType(r.decisions, event.OrderReleasedEventType) {
		at = e.EventTime
	}
	for _, e := range ofType(r.decisions, event.TargetsDecidedEventType) {
		if e.EventTime.Equal(at) {
			decision = e
		}
	}
	if decision.ID == "" {
		t.Fatalf("setup: no decision of the session that released the sale at %s", at)
	}
	return r, state, rel[0], decision
}

// TestADecisionRecordsTheSaleItsOwnSessionAttempts: the incident's sale
// fired by the refusal of the session's own exit is attempted after the
// session's decision is built, and the decision records the attempt as the
// run holds it once the session's orders are processed, so a run restarted
// from it never attempts the sale again (ADR 0019, amended 2026-10-15: one
// attempt; ADR 0026, point 5: recovered, never recomputed).
func TestADecisionRecordsTheSaleItsOwnSessionAttempts(t *testing.T) {
	r, state, _, decision := haltedExitSale(t)
	before := targets(t, r.decisions)[0]
	if s := before.Boost.RestoredSale; s == nil || s.IncidentID != state.ID || s.Attempted {
		t.Fatalf("setup: the first decision records %+v, want the incident's sale not attempted", s)
	}
	var d event.TargetsDecidedPayload
	decode(t, decision, &d)
	if s := d.Boost.RestoredSale; s == nil || s.IncidentID != state.ID || !s.Attempted {
		t.Fatalf("the decision whose session attempted the sale records %+v, want it attempted", s)
	}
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
	again := simulate(t, options{recovery: &decision, snapshots: r.inputs, brokers: true, prelude: []event.Envelope{state}, brokerMutate: urgent})
	if sold := sales(t, again.decisions); len(sold) != 0 {
		t.Fatalf("the run restarted from that decision sold %+v, want nothing: the incident's sale was attempted", sold)
	}
}

// TestARunsOwnSaleRetiredAfterARestartIsAttemptedAgain: the incident's sale
// this run attempted is still unsent when the run restarts from its
// decision; the restarted run holds it as this run's own attempt, released
// and not spent by an earlier run, so its retirement as never sent frees the
// attempt as in the full run, and the sale is attempted again under the
// second attempt's client order ID (ADR 0019, amended 2026-10-17, reading 4,
// as the owner's decision of 2026-10-22 reads it; ADR 0026, point 2, "Order
// identity", and point 5: recovered, never recomputed).
func TestARunsOwnSaleRetiredAfterARestartIsAttemptedAgain(t *testing.T) {
	first, state, sale, decision := haltedExitSale(t)
	retire := map[int]func(apply func(typ string, version uint32, id string, at time.Time, payload any) error){
		21: func(apply func(typ string, version uint32, id string, at time.Time, payload any) error) {
			at := sessionEnd(21).Add(-6 * time.Hour)
			p := event.OrderRetiredPayload{AccountID: sale.AccountID, ClientOrderID: sale.ClientOrderID, InstrumentID: sale.InstrumentID, Side: sale.Side,
				Quantity: sale.Quantity, LimitPrice: sale.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "session over",
				RetiredBy: event.OrderRetiredByAdapter, RetiredAt: at}
			if err := apply(event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:sale", at, p); err != nil {
				t.Errorf("the retirement of the run's own unsent sale after the restart: %v, want it read", err)
			}
		},
	}
	// IBKR still holds the shares the unsent sale would have sold.
	urgent := map[int]func(*event.BrokerSnapshotPayload){22: func(b *event.BrokerSnapshotPayload) {
		b.ExcessLiquidity = 0.1 * b.NetLiquidation
		b.Positions[0].Quantity += sale.Quantity
	}}
	r := simulate(t, options{recovery: &decision, snapshots: first.inputs, brokers: true, prelude: []event.Envelope{state}, intrude: retire, brokerMutate: urgent})
	second := orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 1)
	if sold := sales(t, r.decisions); len(sold) != 1 || sold[0].ClientOrderID != second || sold[0].Tag != state.ID {
		t.Fatalf("sales after the retirement %+v, want the incident's sale attempted again under %s", sold, second)
	}
}

// TestARecoveredOwnSaleUnderAnotherAttemptsIDIsRefused: a recovered decision
// recording the run's own live sale under a client order ID that is not the
// next attempt's is refused, rather than hold an order the run never
// released (ADR 0026, point 2, "Order identity"; fail closed, AGENTS.md).
func TestARecoveredOwnSaleUnderAnotherAttemptsIDIsRefused(t *testing.T) {
	_, state, _, decision := haltedExitSale(t)
	var p event.TargetsDecidedPayload
	decode(t, decision, &p)
	if p.Boost.RestoredSale == nil || p.Boost.RestoredSale.Order == nil {
		t.Fatalf("setup: the decision records %+v, want the run's own live sale", p.Boost.RestoredSale)
	}
	o := *p.Boost.RestoredSale.Order
	o.ClientOrderID = orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 1)
	p.Boost.RestoredSale.Order = &o
	h, s := configured(t)
	_, err := h.Apply(context.Background(), s.envelope(t, decision.Type, decision.SchemaVersion, decision.ID, decision.EventTime, p))
	if err == nil || !strings.Contains(err.Error(), "next attempt") {
		t.Errorf("an own sale recorded under another attempt's ID: %v, want the recovery refused", err)
	}
}

// retireSaleAt retires, at session i, the sale o as never sent, its session
// over, as the adapter does.
func retireSaleAt(t *testing.T, i int, o event.OrderReleasedPayload) map[int]func(apply func(typ string, version uint32, id string, at time.Time, payload any) error) {
	t.Helper()
	return map[int]func(apply func(typ string, version uint32, id string, at time.Time, payload any) error){
		i: func(apply func(typ string, version uint32, id string, at time.Time, payload any) error) {
			at := sessionEnd(i).Add(-6 * time.Hour)
			p := event.OrderRetiredPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side,
				Quantity: o.Quantity, LimitPrice: o.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "session over",
				RetiredBy: event.OrderRetiredByAdapter, RetiredAt: at}
			if err := apply(event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, "retired:"+o.ClientOrderID, at, p); err != nil {
				t.Errorf("the retirement of %s at session %d: %v, want it read", o.ClientOrderID, i, err)
			}
		},
	}
}

// TestARecoveryRestoresTheRunsRetiredSaleBeforeItsLiveOne: a decision
// records the run's retired first attempt of the incident's sale and its
// live second attempt; the recovery restores the retired attempt before the
// live one, so the live one is held as the second attempt, an urgent
// reading fires no sale while it is held, and after its retirement the sale
// is attempted under the third attempt's client order ID (ADR 0019, amended
// 2026-10-15: one attempt; ADR 0026, point 2, "Order identity", and point 5:
// recovered, never recomputed).
func TestARecoveryRestoresTheRunsRetiredSaleBeforeItsLiveOne(t *testing.T) {
	first, state, sale, decision := haltedExitSale(t)
	attempt := func(n int) string { return orderplan.SaleAttemptClientOrderID(state.ID, "SPY", n) }
	// IBKR still holds the shares the unsent sales would have sold.
	urgent := func(sessions ...int) map[int]func(*event.BrokerSnapshotPayload) {
		m := map[int]func(*event.BrokerSnapshotPayload){}
		for _, i := range sessions {
			m[i] = func(b *event.BrokerSnapshotPayload) {
				b.ExcessLiquidity = 0.1 * b.NetLiquidation
				b.Positions[0].Quantity += sale.Quantity
			}
		}
		return m
	}
	second := simulate(t, options{recovery: &decision, snapshots: first.inputs, brokers: true, prelude: []event.Envelope{state},
		intrude: retireSaleAt(t, 21, sale), brokerMutate: urgent(22), unsentIDs: []string{attempt(1)}})
	rel := released(t, second.decisions)
	if len(rel) != 1 || rel[0].ClientOrderID != attempt(1) {
		t.Fatalf("setup: the second run released %+v, want the sale's second attempt %s", rel, attempt(1))
	}
	var recovery event.Envelope
	for _, e := range ofType(second.decisions, event.TargetsDecidedEventType) {
		if e.EventTime.Equal(sessionEnd(23)) {
			recovery = e
		}
	}
	var d event.TargetsDecidedPayload
	decode(t, recovery, &d)
	if d.Boost.RestoredSale == nil || d.Boost.RestoredSale.Order == nil || d.Boost.RestoredSale.Order.ClientOrderID != attempt(1) {
		t.Fatalf("setup: session 23's decision records the restored sale %+v, want the live second attempt %s", d.Boost.RestoredSale, attempt(1))
	}
	if r := d.Boost.RetiredOrders; len(r) != 1 || r[0].ClientOrderID != attempt(0) || r[0].SaleTag != state.ID || r[0].SaleAttempt != 1 {
		t.Fatalf("setup: session 23's decision records retired orders %+v, want the sale's first attempt %s", r, attempt(0))
	}
	held := simulate(t, options{recovery: &recovery, snapshots: second.inputs, brokers: true, prelude: []event.Envelope{state},
		brokerMutate: urgent(24)})
	if sold := sales(t, held.decisions); len(sold) != 0 {
		t.Fatalf("the run restarted from that decision sold %+v while it holds %s, want nothing", sold, attempt(1))
	}
	again := simulate(t, options{recovery: &recovery, snapshots: second.inputs, brokers: true, prelude: []event.Envelope{state},
		intrude: retireSaleAt(t, 24, rel[0]), brokerMutate: urgent(25)})
	if sold := sales(t, again.decisions); len(sold) != 1 || sold[0].ClientOrderID != attempt(2) || sold[0].Tag != state.ID {
		t.Fatalf("sales after the second attempt's retirement %+v, want the sale attempted under the third attempt's %s", sold, attempt(2))
	}
}

// TestAVenueReportOnARestoredOwnSaleEndsItAsInTheFullRun: the incident's
// sale the run released is restored as the run's own after a restart from
// the decision that records it live; the venue's fill of it, its cancel
// after a part fill, and its expiry unfilled are each read as in the run
// that never restarted, so each later decision, and each sale, is the full
// run's: the sale ends, its attempt stays spent, and no decision records it
// live (ADR 0019, amended 2026-10-15: one attempt; ADR 0026, point 5:
// recovered, never recomputed).
func TestAVenueReportOnARestoredOwnSaleEndsItAsInTheFullRun(t *testing.T) {
	cases := []struct {
		name          string
		part          map[int]int64
		filled, ended bool
	}{
		{"filled", nil, true, false},
		{"canceled after a part fill", map[int]int64{21: 5}, true, true},
		{"expired unfilled", map[int]int64{21: 0}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := haltedState(t)
			urgent := map[int]func(*event.BrokerSnapshotPayload){23: func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }}
			full := restartAt17(t, simulate(t, options{brokers: true}), options{prelude: []event.Envelope{state}, partFill: c.part, brokerMutate: urgent})
			rel := released(t, full.decisions)
			if len(rel) != 1 || rel[0].ClientOrderID != orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 0) {
				t.Fatalf("setup: released %+v, want the incident's first sale", rel)
			}
			sale := rel[0]
			var decision event.Envelope
			for _, e := range ofType(full.decisions, event.TargetsDecidedEventType) {
				if e.EventTime.Equal(sessionEnd(20)) {
					decision = e
				}
			}
			var d event.TargetsDecidedPayload
			decode(t, decision, &d)
			if d.Boost.RestoredSale == nil || d.Boost.RestoredSale.Order == nil || d.Boost.RestoredSale.Order.ClientOrderID != sale.ClientOrderID {
				t.Fatalf("setup: session 20's decision records the restored sale %+v, want %s live", d.Boost.RestoredSale, sale.ClientOrderID)
			}
			r := simulate(t, options{recovery: &decision, snapshots: full.inputs, brokers: true, prelude: []event.Envelope{state},
				working: map[int][]event.OrderReleasedPayload{21: {sale}}, partFill: c.part, brokerMutate: urgent})
			var filled, ended bool
			for _, in := range r.inputs {
				filled = filled || in.Type == event.FillEventType && strings.Contains(string(in.Payload), sale.ClientOrderID)
				ended = ended || in.ID == "cancel:"+sale.ClientOrderID
			}
			if filled != c.filled || ended != c.ended {
				t.Fatalf("setup: the venue reported the restored sale filled %t and canceled %t, want %t and %t", filled, ended, c.filled, c.ended)
			}
			after := func(run run) []event.TargetsDecidedPayload {
				var out []event.TargetsDecidedPayload
				for _, e := range ofType(run.decisions, event.TargetsDecidedEventType) {
					if e.EventTime.After(sessionEnd(20)) {
						var p event.TargetsDecidedPayload
						decode(t, e, &p)
						out = append(out, p)
					}
				}
				return out
			}
			want, got := after(full), after(r)
			if len(want) == 0 {
				t.Fatal("setup: the full run decides nothing after session 20")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the restarted run's decisions after the venue's report are %+v, want the full run's %+v", got, want)
			}
			for _, p := range got {
				if s := p.Boost.RestoredSale; s == nil || !s.Attempted || s.Order != nil {
					t.Errorf("a decision after the venue ended the sale records the restored sale as %+v, want it attempted and not live", s)
				}
			}
			late := slices.DeleteFunc(slices.Clone(full.decisions), func(e event.Envelope) bool { return !e.EventTime.After(sessionEnd(20)) })
			if w, g := sales(t, late), sales(t, r.decisions); !reflect.DeepEqual(g, w) {
				t.Errorf("the restarted run's sales %+v, want the full run's %+v", g, w)
			}
		})
	}
}

// TestAVenueReportOnASaleReleasedBeforeTheRecoveredDecisionEndsItAsInTheFullRun:
// the incident's sale fired by an urgent reading at session 18, outside any
// decided session, is still unsent when session 19 decides; session 19's
// decision records it live, with the trigger that fired it, and a run
// restarted from that decision holds it as its own: the venue's fill of it,
// its cancel after a part fill and its expiry unfilled are each read as in
// the run that never restarted, so each later decision is the full run's,
// the sale ends, and no decision records it live after (ADR 0019, amended
// 2026-10-15: one attempt; ADR 0026, point 5: recovered, never recomputed).
func TestAVenueReportOnASaleReleasedBeforeTheRecoveredDecisionEndsItAsInTheFullRun(t *testing.T) {
	cases := []struct {
		name          string
		part          map[int]int64
		filled, ended bool
	}{
		{"filled", nil, true, false},
		{"canceled after a part fill", map[int]int64{21: 5}, true, true},
		{"expired unfilled", map[int]int64{21: 0}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := haltedState(t)
			first := orderplan.SaleAttemptClientOrderID(state.ID, "SPY", 0)
			urgent := func(b *event.BrokerSnapshotPayload) { b.ExcessLiquidity = 0.1 * b.NetLiquidation }
			mutate := map[int]func(*event.BrokerSnapshotPayload){18: urgent, 23: urgent}
			base := simulate(t, options{brokers: true})
			// The sale is released at session 18 and the adapter holds it back
			// until session 21, so session 19's decision finds it unsent.
			probe := restartAt17(t, base, options{prelude: []event.Envelope{state}, brokerMutate: mutate, unsentIDs: []string{first}})
			rel := released(t, probe.decisions)
			if len(rel) != 1 || rel[0].ClientOrderID != first {
				t.Fatalf("setup: released %+v, want the incident's first sale", rel)
			}
			sale := rel[0]
			full := restartAt17(t, base, options{prelude: []event.Envelope{state}, brokerMutate: mutate, unsentIDs: []string{first},
				working: map[int][]event.OrderReleasedPayload{21: {sale}}, partFill: c.part})
			for _, e := range ofType(full.decisions, event.OrderReleasedEventType) {
				if !e.EventTime.Equal(sessionEnd(18)) {
					t.Fatalf("setup: the sale was released at %s, want session 18's broker snapshot", e.EventTime)
				}
			}
			var decision event.Envelope
			for _, e := range ofType(full.decisions, event.TargetsDecidedEventType) {
				if e.EventTime.Equal(sessionEnd(19)) {
					decision = e
				}
			}
			var d event.TargetsDecidedPayload
			decode(t, decision, &d)
			if s := d.Boost.RestoredSale; s == nil || s.Order == nil || s.Order.ClientOrderID != first || s.Order.Trigger != event.SaleTriggerMarginUrgent {
				t.Fatalf("session 19's decision records the restored sale %+v, want %s live under margin-urgent", s, first)
			}
			r := simulate(t, options{recovery: &decision, snapshots: full.inputs, brokers: true, prelude: []event.Envelope{state},
				working: map[int][]event.OrderReleasedPayload{21: {sale}}, partFill: c.part, brokerMutate: mutate})
			var filled, ended bool
			for _, in := range r.inputs {
				filled = filled || in.Type == event.FillEventType && strings.Contains(string(in.Payload), sale.ClientOrderID)
				ended = ended || in.ID == "cancel:"+sale.ClientOrderID
			}
			if filled != c.filled || ended != c.ended {
				t.Fatalf("setup: the venue reported the restored sale filled %t and canceled %t, want %t and %t", filled, ended, c.filled, c.ended)
			}
			after := func(run run) []event.TargetsDecidedPayload {
				var out []event.TargetsDecidedPayload
				for _, e := range ofType(run.decisions, event.TargetsDecidedEventType) {
					if e.EventTime.After(sessionEnd(19)) {
						var p event.TargetsDecidedPayload
						decode(t, e, &p)
						out = append(out, p)
					}
				}
				return out
			}
			want, got := after(full), after(r)
			if len(want) == 0 {
				t.Fatal("setup: the full run decides nothing after session 19")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the restarted run's decisions after the venue's report are %+v, want the full run's %+v", got, want)
			}
			// Session 20's decision still finds the sale unsent; the venue's
			// report reaches the run at session 21.
			for i, p := range got {
				s := p.Boost.RestoredSale
				if live := i == 0; s == nil || !s.Attempted || (s.Order != nil) != live {
					t.Errorf("decision %d after session 19 records the restored sale as %+v, want it attempted and live %t", i, s, live)
				}
			}
			late := slices.DeleteFunc(slices.Clone(full.decisions), func(e event.Envelope) bool { return !e.EventTime.After(sessionEnd(19)) })
			if w, g := sales(t, late), sales(t, r.decisions); !reflect.DeepEqual(g, w) {
				t.Errorf("the restarted run's sales %+v, want the full run's %+v", g, w)
			}
		})
	}
}
