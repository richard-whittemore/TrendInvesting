package targetweight_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
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
