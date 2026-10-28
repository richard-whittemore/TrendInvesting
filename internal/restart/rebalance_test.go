package restart_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// A restart mid-rebalance sends nothing for that decision time until the
// owner clears it (ADR 0026, point 5), so an IRA's approval states the
// rebalance its log ends with in progress, or none. Every log here is
// recorded by the real momentum handler (handlerRun), so the state read
// back is the one the run held.

const iraLogAccount = "DUIRA"

var janDecision = "targets-decided:" + iraLogAccount + ":2025-01-31T21:00:00Z"

// month applies the synthetic month (cmd/engine's dry-run fixture,
// invented prices only) to run, the run's account in place of the
// fixture's, and returns the orders it released.
func (r *handlerRun) month() []event.OrderReleasedPayload {
	r.t.Helper()
	file, err := os.Open("../../cmd/engine/testdata/momentum-month.jsonl")
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var in struct {
			Type          string          `json:"type"`
			SchemaVersion uint32          `json:"schema_version"`
			ID            string          `json:"id"`
			EventTime     time.Time       `json:"event_time"`
			Payload       json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &in); err != nil {
			r.t.Fatal(err)
		}
		raw := bytes.ReplaceAll(in.Payload, []byte(`"account_id":"trad-ira"`), []byte(`"account_id":"`+iraLogAccount+`"`))
		r.apply(in.ID, in.Type, in.SchemaVersion, r.hash, in.EventTime, json.RawMessage(raw))
		if err := r.errs[in.ID]; err != nil {
			r.t.Fatalf("setup: the month's %s was refused: %v", in.ID, err)
		}
	}
	var out []event.OrderReleasedPayload
	for _, e := range r.recorder.Entries() {
		if e.Kind == journal.KindDecision && e.Envelope.Type == event.OrderReleasedEventType {
			var p event.OrderReleasedPayload
			if err := json.Unmarshal(e.Envelope.Payload, &p); err != nil {
				r.t.Fatal(err)
			}
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		r.t.Fatal("setup: the month released no order")
	}
	return out
}

var afterMonth = time.Date(2025, time.February, 3, 15, 0, 0, 0, time.UTC)

func (r *handlerRun) cancel(o event.OrderReleasedPayload) {
	r.apply("cancel:"+o.ClientOrderID, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, r.hash, afterMonth, event.LimitOrderLifecyclePayload{
		AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, OrderID: "ib-" + o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side,
		Status: event.OrderLifecycleStatusCanceled, Quantity: o.Quantity, LimitPrice: o.LimitPrice, OccurredAt: afterMonth})
}

func (r *handlerRun) retire(o event.OrderReleasedPayload) {
	r.apply("retired:"+o.ClientOrderID, event.OrderRetiredEventType, event.OrderRetiredSchemaVersion, r.hash, afterMonth, event.OrderRetiredPayload{
		AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side, Quantity: o.Quantity, LimitPrice: o.LimitPrice,
		Reason: event.OrderRetiredSessionOver, Detail: "the session was over", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: afterMonth})
}

func (r *handlerRun) clear(account, decision string, orders []string) {
	r.apply("cleared:"+decision+":"+account, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, r.hash, afterMonth, event.RebalanceClearedPayload{
		AccountID: account, AsOf: afterMonth, DecisionID: decision, ClientOrderIDs: orders, Operator: "owner", Detail: "cancelled at IBKR"})
}

func (r *handlerRun) approve(id, hash string, rebalance *event.RebalanceInProgress) {
	at := time.Date(2025, time.January, 2, 9, 0, 0, 0, time.UTC)
	r.apply(id, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, hash, at, event.OperatorRunApprovedPayload{
		AccountID: iraLogAccount, PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("b", 64),
		ApprovedBy: "owner", ApprovedAt: at, Momentum: &event.MomentumStateInForce{RebalanceInProgress: rebalance}})
}

func ids(orders []event.OrderReleasedPayload) []string {
	out := make([]string, len(orders))
	for i, o := range orders {
		out[i] = o.ClientOrderID
	}
	return out
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

// december is a rebalance an earlier run left in progress.
func december() *event.RebalanceInProgress {
	return &event.RebalanceInProgress{DecisionID: "targets-decided:DUIRA:2024-12-31T21:00:00Z", DecisionTime: time.Date(2024, time.December, 31, 21, 0, 0, 0, time.UTC),
		Phase: event.RebalancePhaseSellsWorking, ClientOrderIDs: []string{"DUIRA-0"}}
}

func TestTheRebalanceInProgressIsReadFromTheLog(t *testing.T) {
	buysWorking := func(orders []event.OrderReleasedPayload) *event.RebalanceInProgress {
		return &event.RebalanceInProgress{DecisionID: janDecision, DecisionTime: time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC),
			Phase: event.RebalancePhaseBuysWorking, ClientOrderIDs: ids(orders)}
	}
	for name, c := range map[string]func(r *handlerRun) *event.RebalanceInProgress{
		"buys working, no venue report": func(r *handlerRun) *event.RebalanceInProgress { return buysWorking(r.month()) },
		"one buy cancelled": func(r *handlerRun) *event.RebalanceInProgress {
			orders := r.month()
			r.cancel(orders[0])
			return buysWorking(orders[1:])
		},
		"every buy cancelled or retired": func(r *handlerRun) *event.RebalanceInProgress {
			orders := r.month()
			r.cancel(orders[0])
			for _, o := range orders[1:] {
				r.retire(o)
			}
			return nil
		},
		"cleared": func(r *handlerRun) *event.RebalanceInProgress {
			orders := r.month()
			r.clear(iraLogAccount, janDecision, ids(orders))
			return nil
		},
		"a clearance short of an order": func(r *handlerRun) *event.RebalanceInProgress {
			orders := r.month()
			r.clear(iraLogAccount, janDecision, ids(orders)[:1])
			return buysWorking(orders)
		},
		"another account's clearance": func(r *handlerRun) *event.RebalanceInProgress {
			orders := r.month()
			r.clear("DUOTHER", janDecision, ids(orders))
			return buysWorking(orders)
		},
		"carried": func(r *handlerRun) *event.RebalanceInProgress {
			r.approve("approval", r.hash, december())
			return december()
		},
		"carried, cleared": func(r *handlerRun) *event.RebalanceInProgress {
			r.approve("approval", r.hash, december())
			r.clear(iraLogAccount, december().DecisionID, december().ClientOrderIDs)
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			run, _ := newHandlerRun(t)
			want := c(run)
			entries := run.recorder.Entries()
			end, err := restart.ReadEndState(entries, iraLogAccount, "run-1.log")
			if err != nil {
				t.Fatal(err)
			}
			if !end.Momentum {
				t.Fatal("a momentum log is not read as one")
			}
			if got := describeRebalance(end.Rebalance); got != describeRebalance(want) {
				t.Fatalf("rebalance in progress = %s, want %s", got, describeRebalance(want))
			}
			approval, err := restart.ProposeApproval(end, freshIRARequest())
			if err != nil {
				t.Fatal(err)
			}
			if approval.Momentum == nil || approval.Boost.Latched != nil || describeRebalance(approval.Momentum.RebalanceInProgress) != describeRebalance(want) {
				t.Fatalf("proposed %+v, want an IRA approval stating %s", approval, describeRebalance(want))
			}
			if err := restart.CheckRebalanceInProgress(entries, approval, "run-1.log"); err != nil {
				t.Errorf("the engine refuses the approval operator approve proposes: %v", err)
			}
		})
	}
}

// TestADryRunLogEndsWithNoRebalance: a dry-run ends each order it releases
// with its own synthetic cancel (ADR 0026, point 3, "Mode").
func TestADryRunLogEndsWithNoRebalance(t *testing.T) {
	run, _ := newHandlerRunIn(t, event.TradingModeDryRun)
	run.month()
	if got, err := restart.RebalanceAtEnd(run.recorder.Entries(), "run-1.log"); err != nil || got != nil {
		t.Fatalf("a dry-run month: %s, %v, want none", describeRebalance(got), err)
	}
}

// TestTheEngineRefusesAnApprovalOfAnotherRebalance: an approval naming a
// rebalance the log does not end with in progress, stating none where one
// is, or Boost-shaped for a momentum log is refused.
func TestTheEngineRefusesAnApprovalOfAnotherRebalance(t *testing.T) {
	mid, _ := newHandlerRun(t)
	orders := mid.month()
	done, _ := newHandlerRun(t)
	for _, o := range done.month() {
		done.cancel(o)
	}
	approval := func(r *event.RebalanceInProgress) event.OperatorRunApprovedPayload {
		return event.OperatorRunApprovedPayload{AccountID: iraLogAccount, Momentum: &event.MomentumStateInForce{RebalanceInProgress: r}}
	}
	stated := &event.RebalanceInProgress{DecisionID: janDecision, DecisionTime: time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC), Phase: event.RebalancePhaseBuysWorking, ClientOrderIDs: ids(orders)}
	other := *stated
	other.ClientOrderIDs = []string{"DUIRA-9"}
	for name, c := range map[string]struct {
		entries  []journal.Entry
		approval event.OperatorRunApprovedPayload
		want     string
	}{
		"none stated, one in progress": {mid.recorder.Entries(), approval(nil), "ends mid-rebalance"},
		"one stated, none in progress": {done.recorder.Entries(), approval(stated), "cannot find"},
		"another stated":               {mid.recorder.Entries(), approval(&other), "cannot find"},
		"Boost-shaped":                 {mid.recorder.Entries(), event.OperatorRunApprovedPayload{AccountID: iraLogAccount, Boost: event.BoostStateInForce{Latched: new(false)}}, "Boost-shaped"},
	} {
		if err := restart.CheckRebalanceInProgress(c.entries, c.approval, "run-1.log"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, c.want)
		}
	}
}

// TestARefusedApprovalNeverLiftsACarriedRebalance: the log commits an input
// the run refused as well as one it read. An approval after the accepted
// one, which the run refused (an approval comes once), and one for another
// configuration change nothing: only a clearance ends a carried rebalance
// (ADR 0026, point 5).
func TestARefusedApprovalNeverLiftsACarriedRebalance(t *testing.T) {
	run, _ := newHandlerRun(t)
	run.approve("approval", run.hash, december())
	run.approve("approval:refused", run.hash, nil)
	if run.errs["approval"] != nil || run.errs["approval:refused"] == nil {
		t.Fatalf("setup: the handler's verdicts were %v", run.errs)
	}
	got, err := restart.RebalanceAtEnd(run.recorder.Entries(), "run-1.log")
	if err != nil || describeRebalance(got) != describeRebalance(december()) {
		t.Fatalf("after a refused second approval: %s, %v, want %s still in progress", describeRebalance(got), err, describeRebalance(december()))
	}
	foreign, _ := newHandlerRun(t)
	foreign.approve("approval", "sha256:other", december())
	if got, err := restart.RebalanceAtEnd(foreign.recorder.Entries(), "run-1.log"); err != nil || got != nil {
		t.Fatalf("an approval of another configuration: %s, %v, want it not read", describeRebalance(got), err)
	}
}

// TestTheLogsOwnAccountDecides: the rebalance is read for the account the
// log's configuration names; an approval for another account than the
// log's is refused, as is a log with no configuration or one whose
// configuration cannot be read back.
func TestTheLogsOwnAccountDecides(t *testing.T) {
	mid, _ := newHandlerRun(t)
	mid.month()
	entries := mid.recorder.Entries()
	if err := restart.CheckRebalanceInProgress(entries, event.OperatorRunApprovedPayload{AccountID: "DUOTHER", Momentum: &event.MomentumStateInForce{}}, "run-1.log"); err == nil || !strings.Contains(err.Error(), "DUOTHER") {
		t.Errorf("an approval for another account: %v", err)
	}
	if _, err := restart.ReadEndState(entries, "DUOTHER", "run-1.log"); err == nil || !strings.Contains(err.Error(), "DUOTHER") {
		t.Errorf("operator approve for another account than the log's: %v", err)
	}
	if _, err := restart.RebalanceAtEnd(entries[1:], "run-1.log"); err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Errorf("a log with no configuration input: %v", err)
	}
	bad := recorded(t, journal.KindInput, "configuration:bad", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "not an object")
	if _, err := restart.RebalanceAtEnd([]journal.Entry{bad}, "run-1.log"); err == nil || !strings.Contains(err.Error(), "configuration:bad") {
		t.Errorf("a configuration input that cannot be decoded: %v", err)
	}
	invalid := recorded(t, journal.KindInput, "configuration:invalid", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion,
		map[string]string{"strategy_id": event.TargetWeightStrategyMomentum, "account_id": iraLogAccount})
	if _, err := restart.RebalanceAtEnd([]journal.Entry{invalid}, "run-1.log"); err == nil || !strings.Contains(err.Error(), "cannot be read back") {
		t.Errorf("a configuration the handler refuses: %v", err)
	}
	if _, err := restart.ReadEndState([]journal.Entry{invalid}, iraLogAccount, "run-1.log"); err == nil {
		t.Error("ReadEndState read a log whose configuration cannot be read back")
	}
	if err := restart.CheckRebalanceInProgress([]journal.Entry{invalid}, event.OperatorRunApprovedPayload{AccountID: iraLogAccount, Momentum: &event.MomentumStateInForce{}}, "run-1.log"); err == nil {
		t.Error("CheckRebalanceInProgress read a log whose configuration cannot be read back")
	}
}

// TestABoostLogRefusesAnIRAApprovalAndAnIRAApprovalStartsNoLatch: an IRA
// approval states no latch, so it is refused for a Boost log; a log holding
// only an IRA approval ends with no latch from it.
func TestABoostLogRefusesAnIRAApprovalAndAnIRAApprovalStartsNoLatch(t *testing.T) {
	ira := event.OperatorRunApprovedPayload{AccountID: "joint", Momentum: &event.MomentumStateInForce{}}
	if err := restart.CheckApprovedLatch(nil, ira, "run-1.log"); err == nil || !strings.Contains(err.Error(), "no latch") {
		t.Errorf("an IRA approval for a Boost log: %v", err)
	}
	approval := recorded(t, journal.KindInput, "operator-run-approved:x", event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion,
		event.OperatorRunApprovedPayload{AccountID: iraLogAccount, PreviousLog: "run-0.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("b", 64),
			ApprovedBy: "owner", ApprovedAt: time.Date(2025, time.January, 2, 14, 0, 0, 0, time.UTC), Momentum: &event.MomentumStateInForce{}})
	end, err := restart.ReadEndState([]journal.Entry{approval}, iraLogAccount, "run-1.log")
	if err != nil || end.Latched || end.Momentum {
		t.Errorf("a log holding only an IRA approval: %+v, %v", end, err)
	}
}
