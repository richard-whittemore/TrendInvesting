package targetweight_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// recorded is a decision's count: one rejection (a), and two orders the
// venue has not ruled on, old released before a and young after it.
func recorded() event.BoostFigures {
	return event.BoostFigures{
		VenueRejections: 1, RejectedInARow: []event.BoostRejectedOrder{{ClientOrderID: "a", VenueOrderID: "ib-a"}},
		UndecidedOrders: []event.BoostUndecidedOrder{
			{ClientOrderID: "old", Side: event.OrderSideBuy, Released: 10, LimitPrice: 101},
			{ClientOrderID: "young", Side: event.OrderSideBuy, Released: 10, LimitPrice: 101, VenueOrderID: "ib-y"},
		},
		UndecidedBeforeRejection: 1,
	}
}

// TestBoostVerdictsCountTheVenuesFirstWordAsTheEngineDoes: Boost's bound of
// two rejections in a row, read from a decision's recorded count and the
// venue's words after it (ADR 0026, point 2, "Boost", amended 2026-10-18;
// point 5: recovered, never recomputed). An acceptance of an order released
// after the latest rejection sets the count back; one of an older order does
// not; a rejection counts one more however late; at two buys stop and the
// count stays there until a fresh run.
func TestBoostVerdictsCountTheVenuesFirstWordAsTheEngineDoes(t *testing.T) {
	var v targetweight.BoostVerdicts
	v.Recover(recorded())
	if v.Rejections() != 1 || v.Stopped() || !slices.Equal(v.InARow(), recorded().RejectedInARow) {
		t.Fatalf("recovered %d %v, stopped %v; want the decision's count", v.Rejections(), v.InARow(), v.Stopped())
	}
	if got := v.Observe("young", false); got != targetweight.VerdictAccepted || v.Rejections() != 0 {
		t.Errorf("an acceptance of an order released after the rejection: %v, count %d; want accepted, 0", got, v.Rejections())
	}
	if got := v.Observe("old", true); got != targetweight.VerdictRejected || v.Rejections() != 1 || v.Stopped() {
		t.Errorf("a late rejection after the count went back: %v, count %d; want one rejection, not stopped", got, v.Rejections())
	}

	v = targetweight.BoostVerdicts{}
	v.Recover(recorded())
	if got := v.Observe("old", false); got != targetweight.VerdictAccepted || v.Rejections() != 1 {
		t.Errorf("an acceptance of an order older than the rejection: %v, count %d; want accepted, still 1", got, v.Rejections())
	}
	if got := v.Observe("young", true); got != targetweight.VerdictRejected || !v.Stopped() || v.Rejections() != 2 {
		t.Errorf("the second rejection: %v, count %d; want buys stopped at 2", got, v.Rejections())
	}
	if want := []event.BoostRejectedOrder{{ClientOrderID: "a", VenueOrderID: "ib-a"}, {ClientOrderID: "young", VenueOrderID: "ib-y"}}; !slices.Equal(v.InARow(), want) {
		t.Errorf("in a row %v; want %v", v.InARow(), want)
	}
	v.Release("late")
	v.Release("later")
	if got := v.Observe("late", true); got != targetweight.VerdictRejectedWhileStopped || v.Rejections() != 2 || v.WhileStopped() != 1 {
		t.Errorf("a rejection once stopped: %v, count %d, while stopped %d; want 2 and one while stopped", got, v.Rejections(), v.WhileStopped())
	}
	if got := v.Observe("later", false); got != targetweight.VerdictAccepted || !v.Stopped() {
		t.Errorf("an acceptance once stopped: %v; buys stay stopped until a fresh run", got)
	}
	if got := v.Observe("late", true); got != targetweight.VerdictNone {
		t.Errorf("a second word on an order: %v; want none", got)
	}
	if got := v.Observe("unknown", true); got != targetweight.VerdictNone {
		t.Errorf("a word on an order no decided session released: %v; want none", got)
	}
}

// TestBoostVerdictsReadAReleaseOnce: an order released is awaited once, so a
// release read twice is one order awaiting the venue's word.
func TestBoostVerdictsReadAReleaseOnce(t *testing.T) {
	var v targetweight.BoostVerdicts
	v.Release("a")
	v.Release("a")
	v.Observe("a", true)
	if got := v.Observe("a", true); got != targetweight.VerdictNone || v.Rejections() != 1 {
		t.Errorf("a release read twice: %v, count %d; want one order, one rejection", got, v.Rejections())
	}
}

// TestBoostLifecycleWord: which venue report is the venue's word on an
// order. Invalid is a rejection; submitted is not enough, since IBKR's
// PreSubmitted maps to it and can precede a rejection; any other report
// shows the venue accepted the order (ADR 0026, point 2, "Boost", amended
// 2026-10-18).
func TestBoostLifecycleWord(t *testing.T) {
	for _, c := range []struct {
		status            string
		rejected, decides bool
	}{
		{event.OrderLifecycleStatusInvalid, true, true},
		{event.OrderLifecycleStatusSubmitted, false, false},
		{event.OrderLifecycleStatusUpdated, false, true},
		{event.OrderLifecycleStatusCanceled, false, true},
		{event.OrderLifecycleStatusCancelPending, false, true},
	} {
		if r, d := targetweight.BoostLifecycleWord(c.status); r != c.rejected || d != c.decides {
			t.Errorf("%s: rejected %v decides %v; want %v %v", c.status, r, d, c.rejected, c.decides)
		}
	}
}

// TestBoostVerdictsClearAnOrderNeverSent: the owner's settlement of an order
// the adapter never sent takes it off the orders awaiting the venue's word,
// counted neither way, and is refused whole for an order the venue named or
// the decision did not release (ADR 0026, point 2, "Boost", amended
// 2026-10-18).
func TestBoostVerdictsClearAnOrderNeverSent(t *testing.T) {
	decision := "targets-decided:joint:x"
	id := orderplan.ClientOrderID(decision, "SPY", event.OrderSideBuy)
	sell := orderplan.ClientOrderID(decision, "SPY", event.OrderSideSell)
	side := func(id string) string {
		if id == sell {
			return event.OrderSideSell
		}
		return event.OrderSideBuy
	}
	var v targetweight.BoostVerdicts
	v.Release(id)
	v.Release("named")
	v.LearnVenueID("named", "ib-n")
	cleared := event.RebalanceClearedPayload{AccountID: "joint", DecisionID: decision, ClientOrderIDs: []string{id}}
	if err := v.ClearNeverSent("clear:1", cleared, "SPY", side); err != nil {
		t.Fatal(err)
	}
	if got := v.Observe(id, true); got != targetweight.VerdictNone || v.Rejections() != 0 {
		t.Errorf("a word on a cleared order: %v; want none", got)
	}
	v.Release("other")
	v.Release(sell)
	for _, c := range []struct {
		ids  []string
		want string
	}{
		{[]string{"named"}, "already named"},
		{[]string{"missing"}, "not an order this run released"},
		{[]string{"other"}, "did not release"},
		{[]string{sell, "missing"}, "not an order this run released"},
	} {
		p := cleared
		p.ClientOrderIDs = c.ids
		if err := v.ClearNeverSent("clear:2", p, "SPY", side); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("clearing %v: %v; want %q", c.ids, err, c.want)
		}
	}
	if got := v.Observe(sell, true); got != targetweight.VerdictRejected {
		t.Errorf("a refused clearance cleared order %s: %v; want it still awaiting the venue's word", sell, got)
	}
}

// TestBoostVerdictsRefuseAClearanceNamingAnOrderTwice: a settlement naming
// one order twice is refused whole, never a panic, and clears nothing (ADR
// 0026, point 2, "Boost", amended 2026-10-18; fail closed).
func TestBoostVerdictsRefuseAClearanceNamingAnOrderTwice(t *testing.T) {
	decision := "targets-decided:joint:x"
	id := orderplan.ClientOrderID(decision, "SPY", event.OrderSideBuy)
	var v targetweight.BoostVerdicts
	v.Release(id)
	p := event.RebalanceClearedPayload{AccountID: "joint", DecisionID: decision, ClientOrderIDs: []string{id, id}}
	err := v.ClearNeverSent("clear:3", p, "SPY", func(string) string { return event.OrderSideBuy })
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a clearance naming %s twice: %v; want a refusal", id, err)
	}
	if got := v.Observe(id, true); got != targetweight.VerdictRejected {
		t.Errorf("a refused clearance cleared the order: %v; want it still awaiting the venue's word", got)
	}
}

// TestBoostVerdictsRetireAnOrderNeverSent: retiring an order the adapter
// never sent takes it off the orders awaiting the venue's word, counted
// neither way, and is refused for an order not awaiting it or one the venue
// has named, which was sent (ADR 0026, point 2, "Boost", amended
// 2026-10-22; fail closed).
func TestBoostVerdictsRetireAnOrderNeverSent(t *testing.T) {
	var v targetweight.BoostVerdicts
	v.Recover(recorded())
	v.Release("unsent")
	if err := v.Retire("retire:1", "unsent"); err != nil {
		t.Fatal(err)
	}
	if got := v.Observe("unsent", true); got != targetweight.VerdictNone || v.Rejections() != 1 {
		t.Errorf("a word on a retired order: %v, count %d; want none, still 1", got, v.Rejections())
	}
	for _, c := range []struct{ id, want string }{
		{"unsent", "not among the orders awaiting"},
		{"missing", "not among the orders awaiting"},
		{"young", "already named"},
	} {
		if err := v.Retire("retire:2", c.id); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("retiring %s: %v; want %q", c.id, err, c.want)
		}
	}
	if err := v.Retire("retire:3", "old"); err != nil {
		t.Fatal(err)
	}
	// old was released before the rejection counted: leaving takes it off
	// the count of older orders, so young's acceptance now sets it back.
	if got := v.Observe("young", false); got != targetweight.VerdictAccepted || v.Rejections() != 0 {
		t.Errorf("an acceptance after the older order retired: %v, count %d; want accepted, 0", got, v.Rejections())
	}
}

// TestRetirementVerdictIsTheEngineRule: the one rule by which a retirement
// as never sent is applied, which the engine, the reporter and the restart
// reading all call (ADR 0026, point 2, "Boost", amended 2026-10-22, and
// point 5: recovered, never recomputed). It must be for the run's account,
// in a run that sends orders, of an order released as the retirement
// describes it and released to a venue, neither cleared nor retired before;
// an order the Boost count holds passes that count's rule and any other may
// never have been named by a venue. It changes nothing.
func TestRetirementVerdictIsTheEngineRule(t *testing.T) {
	sale := event.OrderReleasedPayload{AccountID: "joint", ClientOrderID: "sale", InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 5, LimitPrice: 90, Mode: event.TradingModePaper, Send: true}
	retire := func(id string) event.OrderRetiredPayload {
		return event.OrderRetiredPayload{AccountID: "joint", ClientOrderID: id, InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 5, LimitPrice: 90}
	}
	var v targetweight.BoostVerdicts
	v.Recover(recorded())
	v.Release("unsent")
	released := map[string]event.OrderReleasedPayload{}
	for _, id := range []string{"sale", "unsent", "young", "dry"} {
		o := sale
		o.ClientOrderID = id
		released[id] = o
	}
	dry := released["dry"]
	dry.Mode, dry.Send = event.TradingModeDryRun, false
	released["dry"] = dry
	in := func(ids ...string) func(string) bool { return func(id string) bool { return slices.Contains(ids, id) } }
	state := targetweight.RetirementState{
		AccountID: "joint",
		Released:  func(id string) (event.OrderReleasedPayload, bool) { o, ok := released[id]; return o, ok },
		Verdicts:  &v,
	}
	for _, c := range []struct {
		name  string
		edit  func(*targetweight.RetirementState, *event.OrderRetiredPayload)
		id    string
		held  bool
		error string
	}{
		{"an order the count does not hold, never named", nil, "sale", false, ""},
		{"an order the count holds", nil, "unsent", true, ""},
		{"a dry-run", func(s *targetweight.RetirementState, _ *event.OrderRetiredPayload) { s.DryRun = true }, "sale", false, "dry-run"},
		{"an order a dry-run released", nil, "dry", false, "dry-run"},
		{"another account's", func(_ *targetweight.RetirementState, p *event.OrderRetiredPayload) { p.AccountID = "ira" }, "sale", false, "account"},
		{"an order never released", nil, "missing", false, "did not release"},
		{"an order cleared", func(s *targetweight.RetirementState, _ *event.OrderRetiredPayload) { s.Cleared = in("sale") }, "sale", false, "cleared"},
		{"an order retired", func(s *targetweight.RetirementState, _ *event.OrderRetiredPayload) { s.Retired = in("sale") }, "sale", false, "retired already"},
		{"a retirement not describing the release", func(_ *targetweight.RetirementState, p *event.OrderRetiredPayload) { p.Quantity = 6 }, "sale", false, "was released as"},
		{"an order a venue named", func(s *targetweight.RetirementState, _ *event.OrderRetiredPayload) { s.Named = in("sale") }, "sale", false, "named"},
		{"an order the count holds that the venue named", nil, "young", false, "already named"},
	} {
		s, p := state, retire(c.id)
		if c.edit != nil {
			c.edit(&s, &p)
		}
		held, err := targetweight.RetirementVerdict(s, "retire:1", p)
		switch {
		case c.error == "" && (err != nil || held != c.held):
			t.Errorf("%s: held %v, %v; want held %v, applied", c.name, held, err, c.held)
		case c.error != "" && (err == nil || !strings.Contains(err.Error(), c.error)):
			t.Errorf("%s: %v; want a refusal naming %q", c.name, err, c.error)
		}
	}
	if !v.Holds("unsent") {
		t.Error("the verdict took the order off the count; it changes nothing")
	}
}
