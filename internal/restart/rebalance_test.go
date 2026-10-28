package restart_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// A restart mid-rebalance sends nothing for that decision time until the
// owner clears it (ADR 0026, point 5), so an IRA's approval states the
// rebalance its log ends with in progress, or none, read from the log by
// the rule the engine and the reporter read orders by (event.OrderReports).

const iraLogAccount = "DUIRA"

var janDecision = "targets-decided:" + iraLogAccount + ":2025-01-31T21:00:00Z"

func momentumConfigEntry(t *testing.T) journal.Entry {
	return recorded(t, journal.KindInput, "configuration:x", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion,
		map[string]any{"strategy_id": event.TargetWeightStrategyMomentum, "account_id": iraLogAccount})
}

func janDecided(t *testing.T) journal.Entry {
	return recorded(t, journal.KindDecision, janDecision, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, event.TargetsDecidedPayload{
		AccountID: iraLogAccount, StrategyID: event.TargetWeightStrategyMomentum, DecisionTime: time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC),
		Outcome: event.TargetsOutcomeDecided,
	})
}

func releasedOrder(id, side string, quantity int64) event.OrderReleasedPayload {
	return event.OrderReleasedPayload{AccountID: iraLogAccount, PlanID: "order-planned:" + id, Tag: janDecision, ClientOrderID: id, InstrumentID: "AAA", Side: side, Quantity: quantity, LimitPrice: 50, Mode: event.TradingModePaper, Send: true}
}

func releaseEntry(t *testing.T, o event.OrderReleasedPayload) journal.Entry {
	return recorded(t, journal.KindDecision, "risk-order-released:"+o.ClientOrderID, event.OrderReleasedEventType, o.SchemaVersion(), o)
}

func lifecycleEntry(t *testing.T, kind string, o event.OrderReleasedPayload, status string, synthetic bool) journal.Entry {
	p := event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side,
		Status: status, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: time.Date(2025, time.February, 3, 15, 0, 0, 0, time.UTC), Synthetic: synthetic}
	if !synthetic {
		p.OrderID = "ib-" + o.ClientOrderID
	}
	return recorded(t, kind, "lifecycle:"+status+":"+o.ClientOrderID, event.OrderLifecycleEventType, p.SchemaVersion(), p)
}

func fillEntry(t *testing.T, o event.OrderReleasedPayload) journal.Entry {
	p := event.TargetWeightFillPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, FillID: "fill:" + o.ClientOrderID, InstrumentID: o.InstrumentID,
		Side: o.Side, Quantity: o.Quantity, Price: 50, FilledAt: time.Date(2025, time.February, 3, 15, 1, 0, 0, time.UTC)}
	return recorded(t, journal.KindInput, "fill:"+o.ClientOrderID, event.FillEventType, event.FillTargetWeightVersion, p)
}

func clearanceEntry(t *testing.T, decision string, orders ...string) journal.Entry {
	return recorded(t, journal.KindInput, "operator.rebalance.cleared:x", event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, event.RebalanceClearedPayload{
		AccountID: iraLogAccount, AsOf: time.Date(2025, time.February, 4, 15, 0, 0, 0, time.UTC), DecisionID: decision, ClientOrderIDs: orders, Operator: "owner", Detail: "cancelled at IBKR",
	})
}

func carriedApprovalEntry(t *testing.T, r *event.RebalanceInProgress) journal.Entry {
	p := event.OperatorRunApprovedPayload{AccountID: iraLogAccount, PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("b", 64),
		ApprovedBy: "owner", ApprovedAt: time.Date(2025, time.January, 2, 14, 0, 0, 0, time.UTC), Momentum: &event.MomentumStateInForce{RebalanceInProgress: r}}
	return recorded(t, journal.KindInput, "operator-run-approved:x", event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, p)
}

func TestTheRebalanceInProgressIsReadFromTheLog(t *testing.T) {
	sell1, sell2, buy := releasedOrder("DUIRA-1", event.OrderSideSell, 10), releasedOrder("DUIRA-2", event.OrderSideSell, 5), releasedOrder("DUIRA-3", event.OrderSideBuy, 7)
	base := []journal.Entry{momentumConfigEntry(t), janDecided(t), releaseEntry(t, sell1), releaseEntry(t, sell2)}
	earlier := &event.RebalanceInProgress{DecisionID: "targets-decided:DUIRA:2024-12-31T21:00:00Z", DecisionTime: time.Date(2024, time.December, 31, 21, 0, 0, 0, time.UTC),
		Phase: event.RebalancePhaseBuysWorking, ClientOrderIDs: []string{"DUIRA-0"}}
	for name, c := range map[string]struct {
		entries []journal.Entry
		want    *event.RebalanceInProgress
	}{
		"no order":        {[]journal.Entry{momentumConfigEntry(t), janDecided(t)}, nil},
		"sells working":   {base, &event.RebalanceInProgress{DecisionID: janDecision, DecisionTime: time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC), Phase: event.RebalancePhaseSellsWorking, ClientOrderIDs: []string{"DUIRA-1", "DUIRA-2"}}},
		"one sell filled": {append(append([]journal.Entry{}, base...), fillEntry(t, sell1)), &event.RebalanceInProgress{DecisionID: janDecision, DecisionTime: time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC), Phase: event.RebalancePhaseSellsWorking, ClientOrderIDs: []string{"DUIRA-2"}}},
		"buys working": {append(append([]journal.Entry{}, base...), fillEntry(t, sell1), lifecycleEntry(t, journal.KindInput, sell2, event.OrderLifecycleStatusCanceled, false), releaseEntry(t, buy)),
			&event.RebalanceInProgress{DecisionID: janDecision, DecisionTime: time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC), Phase: event.RebalancePhaseBuysWorking, ClientOrderIDs: []string{"DUIRA-3"}}},
		"all terminal":     {append(append([]journal.Entry{}, base...), fillEntry(t, sell1), fillEntry(t, sell2), releaseEntry(t, buy), fillEntry(t, buy)), nil},
		"cleared":          {append(append([]journal.Entry{}, base...), clearanceEntry(t, janDecision, "DUIRA-1", "DUIRA-2")), nil},
		"bad clearance":    {append(append([]journal.Entry{}, base...), clearanceEntry(t, janDecision, "DUIRA-1")), &event.RebalanceInProgress{DecisionID: janDecision, DecisionTime: time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC), Phase: event.RebalancePhaseSellsWorking, ClientOrderIDs: []string{"DUIRA-1", "DUIRA-2"}}},
		"dry-run":          {append(append([]journal.Entry{}, base...), lifecycleEntry(t, journal.KindDecision, sell1, event.OrderLifecycleStatusCanceled, true), lifecycleEntry(t, journal.KindDecision, sell2, event.OrderLifecycleStatusCanceled, true)), nil},
		"carried":          {[]journal.Entry{momentumConfigEntry(t), carriedApprovalEntry(t, earlier)}, earlier},
		"carried, cleared": {[]journal.Entry{momentumConfigEntry(t), carriedApprovalEntry(t, earlier), clearanceEntry(t, earlier.DecisionID, "DUIRA-0")}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			end, err := restart.ReadEndState(c.entries, iraLogAccount, "run-1.log")
			if err != nil {
				t.Fatal(err)
			}
			if !end.Momentum {
				t.Fatal("a momentum log is not read as one")
			}
			if got, want := describeRebalance(end.Rebalance), describeRebalance(c.want); got != want {
				t.Fatalf("rebalance in progress = %s, want %s", got, want)
			}
			approval, err := restart.ProposeApproval(end, freshIRARequest())
			if err != nil {
				t.Fatal(err)
			}
			if approval.Momentum == nil || approval.Boost.Latched != nil || describeRebalance(approval.Momentum.RebalanceInProgress) != describeRebalance(c.want) {
				t.Fatalf("proposed %+v, want an IRA approval stating %s", approval, describeRebalance(c.want))
			}
			if err := restart.CheckRebalanceInProgress(c.entries, approval, "run-1.log"); err != nil {
				t.Errorf("the engine refuses the approval operator approve proposes: %v", err)
			}
		})
	}
}

func describeRebalance(r *event.RebalanceInProgress) string {
	if r == nil {
		return "none"
	}
	return r.DecisionID + " " + r.DecisionTime.UTC().Format(time.RFC3339) + " " + r.Phase + " " + strings.Join(r.ClientOrderIDs, ",")
}

func freshIRARequest() restart.ApprovalRequest {
	r := freshRequest()
	r.AccountID = iraLogAccount
	return r
}

// TestTheEngineRefusesAnApprovalOfAnotherRebalance: an approval naming a
// rebalance the log does not end with in progress, stating none where one
// is, or Boost-shaped for a momentum log is refused.
func TestTheEngineRefusesAnApprovalOfAnotherRebalance(t *testing.T) {
	sell := releasedOrder("DUIRA-1", event.OrderSideSell, 10)
	mid := []journal.Entry{momentumConfigEntry(t), janDecided(t), releaseEntry(t, sell)}
	done := []journal.Entry{momentumConfigEntry(t), janDecided(t), releaseEntry(t, sell), fillEntry(t, sell)}
	approval := func(r *event.RebalanceInProgress) event.OperatorRunApprovedPayload {
		return event.OperatorRunApprovedPayload{AccountID: iraLogAccount, Momentum: &event.MomentumStateInForce{RebalanceInProgress: r}}
	}
	stated := &event.RebalanceInProgress{DecisionID: janDecision, DecisionTime: time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC), Phase: event.RebalancePhaseSellsWorking, ClientOrderIDs: []string{"DUIRA-1"}}
	other := *stated
	other.ClientOrderIDs = []string{"DUIRA-9"}
	for name, c := range map[string]struct {
		entries  []journal.Entry
		approval event.OperatorRunApprovedPayload
		want     string
	}{
		"none stated, one in progress": {mid, approval(nil), "ends mid-rebalance"},
		"one stated, none in progress": {done, approval(stated), "cannot find"},
		"another stated":               {mid, approval(&other), "cannot find"},
		"Boost-shaped":                 {mid, event.OperatorRunApprovedPayload{AccountID: iraLogAccount, Boost: event.BoostStateInForce{Latched: new(false)}}, "Boost-shaped"},
	} {
		if err := restart.CheckRebalanceInProgress(c.entries, c.approval, "run-1.log"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, c.want)
		}
	}
}

// TestARetiredOrderEndsAndAnotherAccountsClearanceDoesNot: an order the
// adapter retired as never sent is terminal, as the run reads it; a
// clearance for another account clears nothing.
func TestARetiredOrderEndsAndAnotherAccountsClearanceDoesNot(t *testing.T) {
	sell := releasedOrder("DUIRA-1", event.OrderSideSell, 10)
	retired := recorded(t, journal.KindInput, "retired:DUIRA-1", event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, event.OrderRetiredPayload{
		AccountID: iraLogAccount, ClientOrderID: sell.ClientOrderID, InstrumentID: sell.InstrumentID, Side: sell.Side, Quantity: sell.Quantity, LimitPrice: sell.LimitPrice,
		Reason: event.OrderRetiredSessionOver, Detail: "the session was over", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: time.Date(2025, time.February, 4, 13, 30, 0, 0, time.UTC),
	})
	got, err := restart.RebalanceAtEnd([]journal.Entry{momentumConfigEntry(t), janDecided(t), releaseEntry(t, sell), retired}, "run-1.log")
	if err != nil || got != nil {
		t.Fatalf("a retired sell: %s, %v, want none in progress", describeRebalance(got), err)
	}
	foreign := clearanceEntry(t, janDecision, "DUIRA-1")
	foreign.Envelope.Payload = []byte(strings.Replace(string(foreign.Envelope.Payload), iraLogAccount, "OTHER", 1))
	got, err = restart.RebalanceAtEnd([]journal.Entry{momentumConfigEntry(t), janDecided(t), releaseEntry(t, sell), foreign}, "run-1.log")
	if err != nil || got == nil {
		t.Fatalf("another account's clearance: %s, %v, want the rebalance still in progress", describeRebalance(got), err)
	}
}

// TestALogTheRuleCannotReadIsRefused: a record that is evidence of an order
// or a decision and cannot be decoded, a log holding orders of two
// rebalances not terminal, or orders of a decision it does not hold, is
// refused naming the log, never read as no rebalance in progress.
func TestALogTheRuleCannotReadIsRefused(t *testing.T) {
	sell := releasedOrder("DUIRA-1", event.OrderSideSell, 10)
	other := releasedOrder("DUIRA-2", event.OrderSideSell, 10)
	other.Tag = "targets-decided:DUIRA:2025-02-28T21:00:00Z"
	bad := func(kind, eventType string, version uint32) journal.Entry {
		return recorded(t, kind, "bad:"+eventType, eventType, version, "not an object")
	}
	for name, c := range map[string]struct {
		entries []journal.Entry
		want    string
	}{
		"decision":  {[]journal.Entry{momentumConfigEntry(t), bad(journal.KindDecision, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion)}, "bad:" + event.TargetsDecidedEventType},
		"release":   {[]journal.Entry{momentumConfigEntry(t), bad(journal.KindDecision, event.OrderReleasedEventType, 3)}, "bad:" + event.OrderReleasedEventType},
		"synthetic": {[]journal.Entry{momentumConfigEntry(t), bad(journal.KindDecision, event.OrderLifecycleEventType, 3)}, "bad:" + event.OrderLifecycleEventType},
		"approval":  {[]journal.Entry{momentumConfigEntry(t), bad(journal.KindInput, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion)}, "bad:" + event.OperatorRunApprovedEventType},
		"two":       {[]journal.Entry{momentumConfigEntry(t), janDecided(t), releaseEntry(t, sell), releaseEntry(t, other)}, "two rebalances"},
		"undecided": {[]journal.Entry{momentumConfigEntry(t), releaseEntry(t, sell)}, "not that decision"},
	} {
		if _, err := restart.RebalanceAtEnd(c.entries, "run-1.log"); err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "run-1.log") {
			t.Errorf("%s: %v, want a refusal naming %q in run-1.log", name, err, c.want)
		}
		if _, err := restart.ReadEndState(c.entries, iraLogAccount, "run-1.log"); err == nil {
			t.Errorf("%s: ReadEndState read the log", name)
		}
		approval := event.OperatorRunApprovedPayload{AccountID: iraLogAccount, Momentum: &event.MomentumStateInForce{}}
		if err := restart.CheckRebalanceInProgress(c.entries, approval, "run-1.log"); err == nil {
			t.Errorf("%s: CheckRebalanceInProgress read the log", name)
		}
	}
}

// TestABoostLogRefusesAnIRAApprovalAndAnIRAApprovalStartsNoLatch: an IRA
// approval states no latch, so it is refused for a Boost log; a Boost log
// whose own approval was an IRA's (never written by the engine, which
// checks the shape) ends with no latch from it.
func TestABoostLogRefusesAnIRAApprovalAndAnIRAApprovalStartsNoLatch(t *testing.T) {
	ira := event.OperatorRunApprovedPayload{AccountID: "joint", Momentum: &event.MomentumStateInForce{}}
	if err := restart.CheckApprovedLatch(nil, ira, "run-1.log"); err == nil || !strings.Contains(err.Error(), "no latch") {
		t.Errorf("an IRA approval for a Boost log: %v", err)
	}
	end, err := restart.ReadEndState([]journal.Entry{carriedApprovalEntry(t, nil)}, iraLogAccount, "run-1.log")
	if err != nil || end.Latched || end.Momentum {
		t.Errorf("a log holding only an IRA approval: %+v, %v", end, err)
	}
}

// TestARefusedApprovalNeverLiftsACarriedRebalance: the log commits an input
// the run refused as well as one it read, and the run reads an approval only
// as its second input, straight after the configuration (Handler's
// decodeApproval). A later approval stating none, which the run refused,
// therefore leaves the carried rebalance in force: only a clearance ends it
// (ADR 0026, point 5).
func TestARefusedApprovalNeverLiftsACarriedRebalance(t *testing.T) {
	earlier := &event.RebalanceInProgress{DecisionID: "targets-decided:DUIRA:2024-12-31T21:00:00Z", DecisionTime: time.Date(2024, time.December, 31, 21, 0, 0, 0, time.UTC),
		Phase: event.RebalancePhaseSellsWorking, ClientOrderIDs: []string{"DUIRA-0"}}
	refused := carriedApprovalEntry(t, nil)
	refused.Envelope.ID = "operator-run-approved:refused"
	entries := []journal.Entry{momentumConfigEntry(t), carriedApprovalEntry(t, earlier), janDecided(t), refused}
	got, err := restart.RebalanceAtEnd(entries, "run-1.log")
	if err != nil || describeRebalance(got) != describeRebalance(earlier) {
		t.Fatalf("after a refused second approval: %s, %v, want %s still in progress", describeRebalance(got), err, describeRebalance(earlier))
	}
	// An approval for another configuration is one the run refused too.
	foreign := carriedApprovalEntry(t, earlier)
	foreign.Envelope.ConfigurationHash = "sha256:other"
	if got, err := restart.RebalanceAtEnd([]journal.Entry{momentumConfigEntry(t), foreign}, "run-1.log"); err != nil || got != nil {
		t.Fatalf("an approval of another configuration: %s, %v, want it not read", describeRebalance(got), err)
	}
}

// TestTheLogsOwnAccountDecides: the rebalance is read for the account the
// log's configuration names, so a log recorded for another account is never
// read as one with no rebalance in progress; an approval for another account
// than the log's is refused.
func TestTheLogsOwnAccountDecides(t *testing.T) {
	sell := releasedOrder("DUIRA-1", event.OrderSideSell, 10)
	mid := []journal.Entry{momentumConfigEntry(t), janDecided(t), releaseEntry(t, sell)}
	approval := event.OperatorRunApprovedPayload{AccountID: "DUOTHER", Momentum: &event.MomentumStateInForce{}}
	if err := restart.CheckRebalanceInProgress(mid, approval, "run-1.log"); err == nil || !strings.Contains(err.Error(), "DUOTHER") {
		t.Errorf("an approval for another account: %v", err)
	}
	if _, err := restart.RebalanceAtEnd([]journal.Entry{janDecided(t), releaseEntry(t, sell)}, "run-1.log"); err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Errorf("a log with no configuration input: %v", err)
	}
	badConfig := recorded(t, journal.KindInput, "configuration:bad", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "not an object")
	if _, err := restart.RebalanceAtEnd([]journal.Entry{badConfig}, "run-1.log"); err == nil || !strings.Contains(err.Error(), "configuration:bad") {
		t.Errorf("a configuration input that cannot be decoded: %v", err)
	}
	if _, err := restart.ReadEndState(mid, "DUOTHER", "run-1.log"); err == nil || !strings.Contains(err.Error(), "DUOTHER") {
		t.Errorf("operator approve for another account than the log's: %v", err)
	}
}
