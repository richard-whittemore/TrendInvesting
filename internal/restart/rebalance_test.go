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
// one, which the run refused (an approval comes once), changes nothing:
// only a clearance ends a carried rebalance (ADR 0026, point 5). A log whose
// only approval was refused: TestARefusedApprovalWithNoAcceptanceIsNotReadAsNone.
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
	unnamed := recorded(t, journal.KindInput, "configuration:unnamed", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion,
		map[string]string{"strategy_id": event.TargetWeightStrategyMomentum})
	if _, err := restart.RebalanceAtEnd([]journal.Entry{unnamed}, "run-1.log"); err == nil || !strings.Contains(err.Error(), "cannot be read back") {
		t.Errorf("a configuration that names no account: %v", err)
	}
	if err := restart.CheckRebalanceInProgress([]journal.Entry{unnamed}, event.OperatorRunApprovedPayload{AccountID: iraLogAccount, Momentum: &event.MomentumStateInForce{}}, "run-1.log"); err == nil {
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

// recordedLog is a momentum log built from records alone, as recovery reads
// it: the configuration input naming the account, then entries.
func recordedLog(t *testing.T, entries ...journal.Entry) []journal.Entry {
	t.Helper()
	cfg := recorded(t, journal.KindInput, "configuration:x", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion,
		map[string]string{"strategy_id": event.TargetWeightStrategyMomentum, "account_id": iraLogAccount})
	return append([]journal.Entry{cfg}, entries...)
}

func releasedRecord(t *testing.T, id, tag, side string) journal.Entry {
	o := event.OrderReleasedPayload{AccountID: iraLogAccount, PlanID: "order-planned:" + id, Tag: tag, ClientOrderID: id, InstrumentID: "AAA",
		Side: side, Quantity: 10, LimitPrice: 50, Mode: event.TradingModePaper, Send: true,
		Figures: event.RiskFigures{NetLiquidation: 10000, ReferencePrice: 50, OrdersInDecision: 1}}
	return recorded(t, journal.KindDecision, "risk-order-released:"+id, event.OrderReleasedEventType, o.SchemaVersion(), o)
}

// decidedRecord is January's strategy.targets.decided as the real momentum
// handler records it over the synthetic month; id must be janDecision.
func decidedRecord(t *testing.T, id string) journal.Entry {
	t.Helper()
	run, _ := newHandlerRun(t)
	run.month()
	for _, e := range run.recorder.Entries() {
		if e.Kind == journal.KindDecision && e.Envelope.ID == id {
			return e
		}
	}
	t.Fatalf("setup: the month recorded no decision %s", id)
	return journal.Entry{}
}

// TestRecoveryReadsTheRecordedOrders: the phase, a fill that completes an
// order, and every record recovery must read and cannot, which refuses
// rather than reading as no rebalance in progress.
func TestRecoveryReadsTheRecordedOrders(t *testing.T) {
	sells := recordedLog(t, decidedRecord(t, janDecision), releasedRecord(t, "DUIRA-1", janDecision, event.OrderSideSell), releasedRecord(t, "DUIRA-2", janDecision, event.OrderSideSell))
	got, err := restart.RebalanceAtEnd(sells, "run-1.log")
	if err != nil || got == nil || got.Phase != event.RebalancePhaseSellsWorking || strings.Join(got.ClientOrderIDs, ",") != "DUIRA-1,DUIRA-2" {
		t.Fatalf("sells working: %s, %v", describeRebalance(got), err)
	}
	fill := recorded(t, journal.KindInput, "fill:DUIRA-1", event.FillEventType, event.FillTargetWeightVersion, event.TargetWeightFillPayload{
		AccountID: iraLogAccount, ClientOrderID: "DUIRA-1", FillID: "fill:DUIRA-1", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 10, Price: 50,
		FilledAt: time.Date(2025, time.February, 3, 15, 0, 0, 0, time.UTC)})
	got, err = restart.RebalanceAtEnd(append(sells, fill), "run-1.log")
	if err != nil || got == nil || strings.Join(got.ClientOrderIDs, ",") != "DUIRA-2" {
		t.Fatalf("one sell filled: %s, %v", describeRebalance(got), err)
	}
	bad := func(kind, eventType string, version uint32) journal.Entry {
		return recorded(t, kind, "bad:"+eventType, eventType, version, "not an object")
	}
	for name, entries := range map[string][]journal.Entry{
		"decision":  recordedLog(t, bad(journal.KindDecision, event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion)),
		"release":   recordedLog(t, bad(journal.KindDecision, event.OrderReleasedEventType, 3)),
		"synthetic": recordedLog(t, bad(journal.KindDecision, event.OrderLifecycleEventType, 3)),
		"two":       recordedLog(t, decidedRecord(t, janDecision), releasedRecord(t, "DUIRA-1", janDecision, event.OrderSideSell), releasedRecord(t, "DUIRA-2", "targets-decided:DUIRA:2025-02-28T21:00:00Z", event.OrderSideSell)),
		"undecided": recordedLog(t, releasedRecord(t, "DUIRA-1", janDecision, event.OrderSideSell)),
	} {
		if _, err := restart.RebalanceAtEnd(entries, "run-1.log"); err == nil || !strings.Contains(err.Error(), "cannot be read back") {
			t.Errorf("%s: %v, want a refusal saying it cannot be read back", name, err)
		}
		if _, err := restart.ReadEndState(entries, iraLogAccount, "run-1.log"); err == nil {
			t.Errorf("%s: ReadEndState read the log", name)
		}
	}
}

// TestARecordIsReadOnlyAtAVersionAndShapeThisBuildReads: a decision record
// recovery reads is checked against the schema versions this build reads
// and validated (ADR 0015), so a null payload or an unknown version refuses
// ("cannot be read back"), never reads as an empty record that is skipped.
func TestARecordIsReadOnlyAtAVersionAndShapeThisBuildReads(t *testing.T) {
	release := releasedRecord(t, "DUIRA-1", janDecision, event.OrderSideSell)
	null := release
	null.Envelope.Payload = json.RawMessage(`null`)
	unknown := release
	unknown.Envelope.SchemaVersion = 99
	decidedNull := decidedRecord(t, janDecision)
	decidedNull.Envelope.Payload = json.RawMessage(`null`)
	synthetic := recorded(t, journal.KindDecision, "synthetic-lifecycle:DUIRA-1", event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, event.LimitOrderLifecyclePayload{
		AccountID: iraLogAccount, ClientOrderID: "DUIRA-1", InstrumentID: "AAA", Side: event.OrderSideSell, Status: event.OrderLifecycleStatusCanceled,
		Quantity: 10, LimitPrice: 50, OccurredAt: time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC), Synthetic: true})
	for name, entries := range map[string][]journal.Entry{
		"a release with a null payload":           recordedLog(t, decidedRecord(t, janDecision), null),
		"a release at an unknown version":         recordedLog(t, decidedRecord(t, janDecision), unknown),
		"a decision with a null payload":          recordedLog(t, decidedNull),
		"a synthetic report at a venue's version": recordedLog(t, decidedRecord(t, janDecision), release, synthetic),
	} {
		if _, err := restart.RebalanceAtEnd(entries, "run-1.log"); err == nil || !strings.Contains(err.Error(), "cannot be read back") {
			t.Errorf("%s: %v, want a refusal saying it cannot be read back", name, err)
		}
	}
}

// TestARefusedApprovalWithNoAcceptanceIsNotReadAsNone: a log whose run
// refused the only approval it was given recorded no acceptance, so the
// rebalance that approval named is not known to be carried, nor to be
// ended: the earlier run's state is still in force, and the log is refused
// rather than read as no rebalance in progress (ADR 0026, point 5). A
// refused approval after an accepted one changes nothing
// (TestARefusedApprovalNeverLiftsACarriedRebalance).
func TestARefusedApprovalWithNoAcceptanceIsNotReadAsNone(t *testing.T) {
	run, _ := newHandlerRun(t)
	run.approve("approval:other-configuration", "sha256:other", december())
	if run.errs["approval:other-configuration"] == nil {
		t.Fatal("setup: the approval was not refused")
	}
	if _, err := restart.RebalanceAtEnd(run.recorder.Entries(), "run-1.log"); err == nil || !strings.Contains(err.Error(), "cannot be read back") || !strings.Contains(err.Error(), "approval:other-configuration") {
		t.Fatalf("a log whose only approval was refused: %v, want a refusal naming it", err)
	}
}

// TestAnyUnacceptedApprovalRefusesWhateverItNames: an approval input the run
// was given while it had accepted none, with no acceptance after it, refuses
// whatever account it names or fails to name, and whether or not it
// decodes: the run accepted no approval, so its state is not known from the
// log (ADR 0026, point 5).
func TestAnyUnacceptedApprovalRefusesWhateverItNames(t *testing.T) {
	approval := func(payload string) journal.Entry {
		e := recorded(t, journal.KindInput, "operator-run-approved:x", event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, nil)
		e.Envelope.Payload = json.RawMessage(payload)
		return e
	}
	for name, payload := range map[string]string{
		"no account":      `{"previous_log":"run-0.log"}`,
		"a null account":  `{"account_id":null,"previous_log":"run-0.log"}`,
		"another account": `{"account_id":"DUOTHER","previous_log":"run-0.log"}`,
		"not an object":   `5`,
	} {
		if _, err := restart.RebalanceAtEnd(recordedLog(t, approval(payload)), "run-1.log"); err == nil || !strings.Contains(err.Error(), "cannot be read back") {
			t.Errorf("%s: %v, want a refusal saying it cannot be read back", name, err)
		}
	}
}

// TestARefusedApprovalIsEvidenceOnlyWhenItReads: when the run that refused
// its approval was the account's first, there is no run before it, and the
// refusal says to start again as an IRA's first run; it names the run
// before only from an approval that decodes as an approval and names the
// log's own account; any other, such as one naming another account and a
// log of its own, gives both ways back, never a run nobody showed exists.
func TestARefusedApprovalIsEvidenceOnlyWhenItReads(t *testing.T) {
	valid := func(account, previous string) journal.Entry {
		p := event.OperatorRunApprovedPayload{AccountID: account, PreviousLog: previous, PreviousLogFinalHash: "sha256:" + strings.Repeat("b", 64),
			ApprovedBy: "owner", ApprovedAt: time.Date(2025, time.January, 2, 14, 0, 0, 0, time.UTC), Momentum: &event.MomentumStateInForce{}}
		return recorded(t, journal.KindInput, "operator-run-approved:x", event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, p)
	}
	_, err := restart.RebalanceAtEnd(recordedLog(t, valid("DUOTHER", "missing.log")), "run-1.log")
	if err == nil || strings.Contains(err.Error(), "missing.log") || !strings.Contains(err.Error(), "-first-run") {
		t.Errorf("another account's refused approval: %v, want both ways back and no named run", err)
	}
	_, err = restart.RebalanceAtEnd(recordedLog(t, valid(iraLogAccount, "run-0.log")), "run-1.log")
	if err == nil || !strings.Contains(err.Error(), "run-0.log") || strings.Contains(err.Error(), "-first-run") {
		t.Errorf("the account's own refused approval: %v, want it to name run-0.log", err)
	}
	first := valid(iraLogAccount, "")
	first.Envelope.Payload = json.RawMessage(strings.Replace(string(first.Envelope.Payload), `"starting_book":null`, `"starting_book":{"positions":[],"total_cash":1,"currency":"USD"}`, 1))
	first.Envelope.Payload = json.RawMessage(strings.Replace(string(first.Envelope.Payload), `"previous_log_final_hash":"sha256:`+strings.Repeat("b", 64)+`"`, `"previous_log_final_hash":""`, 1))
	_, err = restart.RebalanceAtEnd(recordedLog(t, first), "run-1.log")
	if err == nil || !strings.Contains(err.Error(), "operator approve -first-run -strategy momentum") || strings.Contains(err.Error(), "run before it") {
		t.Errorf("the account's own refused first-run approval: %v, want operator approve -first-run -strategy momentum", err)
	}
}

// TestABoostLogIsNotReadAsAnIRAs: a Boost log in an IRA's folder is refused
// as not a momentum run's, not as one whose run refused its approval.
func TestABoostLogIsNotReadAsAnIRAs(t *testing.T) {
	boost := recorded(t, journal.KindInput, "configuration:boost", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion,
		map[string]string{"strategy_id": event.TargetWeightStrategyBoost, "account_id": iraLogAccount})
	err := restart.CheckRebalanceInProgress([]journal.Entry{boost}, event.OperatorRunApprovedPayload{AccountID: iraLogAccount, Momentum: &event.MomentumStateInForce{}}, "run-1.log")
	if err == nil || !strings.Contains(err.Error(), "not a momentum run's") {
		t.Errorf("a Boost log: %v, want a refusal saying it is not a momentum run's", err)
	}
}

// TestADamagedConfigurationIsNotReadAsAnotherStrategy: a log whose
// configuration input is missing, does not decode, or names no strategy is
// damaged, so it refuses "cannot be read back" as before; only one whose
// configuration reads and names another strategy is not a momentum run's.
func TestADamagedConfigurationIsNotReadAsAnotherStrategy(t *testing.T) {
	approval := event.OperatorRunApprovedPayload{AccountID: iraLogAccount, Momentum: &event.MomentumStateInForce{}}
	corrupt := recorded(t, journal.KindInput, "configuration:corrupt", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "not an object")
	unnamed := recorded(t, journal.KindInput, "configuration:unnamed", event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion,
		map[string]string{"account_id": iraLogAccount})
	for name, entries := range map[string][]journal.Entry{
		"a corrupt configuration":     {corrupt},
		"a configuration naming none": {unnamed},
		"no configuration input": {recorded(t, journal.KindInput, "session:x", event.SessionClosedEventType, event.SessionClosedSchemaVersion,
			map[string]string{"period_end": "2025-01-02T21:00:00Z"})},
	} {
		err := restart.CheckRebalanceInProgress(entries, approval, "run-1.log")
		if err == nil || !strings.Contains(err.Error(), "cannot be read back") || strings.Contains(err.Error(), "not a momentum run's") {
			t.Errorf("%s: %v, want a refusal saying it cannot be read back", name, err)
		}
	}
}

// ADR 0026 D7-F and ADR 0015: recovery reads the same fractional venue
// reports and synthetic cancellations as the engine; old whole-share reports
// cannot complete an order whose quantities count ten-thousandths.
func TestRecoveryReadsFractionalReportsAtTheirDeclaredVersions(t *testing.T) {
	decision := decidedRecord(t, janDecision)
	var decided event.TargetsDecidedPayload
	if err := json.Unmarshal(decision.Envelope.Payload, &decided); err != nil {
		t.Fatal(err)
	}
	decided.QuantityScale = 10000
	decided.Momentum.MinimumLot = event.MinimumLotFigures{}
	decision = recorded(t, journal.KindDecision, janDecision, event.TargetsDecidedEventType, decided.SchemaVersion(), decided)
	for _, tc := range []struct {
		name     string
		kind     string
		report   string
		quantity int64
		scale    int64
		complete bool
	}{
		{"synthetic cancel", journal.KindDecision, "synthetic", 72500, 10000, true},
		{"venue cancel", journal.KindInput, "lifecycle", 72500, 10000, true},
		{"part fill", journal.KindInput, "fill", 35000, 10000, false},
		{"full fill", journal.KindInput, "fill", 72500, 10000, true},
		{"retirement", journal.KindInput, "retired", 72500, 10000, true},
		{"whole-share lifecycle", journal.KindInput, "lifecycle", 72500, 0, false},
		{"whole-share fill", journal.KindInput, "fill", 72500, 0, false},
		{"whole-share retirement", journal.KindInput, "retired", 72500, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := event.OrderReleasedPayload{AccountID: iraLogAccount, PlanID: "plan", Tag: janDecision, ClientOrderID: "fractional", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 72500, QuantityScale: 10000, LimitPrice: 2, Mode: event.TradingModePaper, Send: true, Figures: event.RiskFigures{NetLiquidation: 10000, ReferencePrice: 2, OrdersInDecision: 1}}
			if tc.report == "synthetic" {
				o.Mode = event.TradingModeDryRun
				o.Send = false
			}
			release := recorded(t, journal.KindDecision, "release", event.OrderReleasedEventType, o.SchemaVersion(), o)
			var report journal.Entry
			switch tc.report {
			case "synthetic", "lifecycle":
				p := event.LimitOrderLifecyclePayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side, Quantity: tc.quantity, QuantityScale: tc.scale, LimitPrice: o.LimitPrice, Status: event.OrderLifecycleStatusCanceled, OccurredAt: afterMonth, OrderID: "venue"}
				if tc.report == "synthetic" {
					p.Synthetic = true
					p.OrderID = ""
					p.Message = "dry-run: never sent"
				}
				report = recorded(t, tc.kind, "report", event.OrderLifecycleEventType, p.SchemaVersion(), p)
			case "fill":
				p := event.TargetWeightFillPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side, Quantity: tc.quantity, QuantityScale: tc.scale, Price: 2, FillID: "execution", FilledAt: afterMonth}
				report = recorded(t, tc.kind, "report", event.FillEventType, p.SchemaVersion(), p)
			case "retired":
				p := event.OrderRetiredPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side, Quantity: tc.quantity, QuantityScale: tc.scale, LimitPrice: o.LimitPrice, Reason: event.OrderRetiredSessionOver, Detail: "synthetic fixture session ended", RetiredBy: event.OrderRetiredByAdapter, RetiredAt: afterMonth}
				report = recorded(t, tc.kind, "report", event.OrderRetiredEventType, p.SchemaVersion(), p)
			}
			got, err := restart.RebalanceAtEnd(recordedLog(t, decision, release, report), "fractional.log")
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != tc.complete {
				t.Fatalf("rebalance %s, want complete %v", describeRebalance(got), tc.complete)
			}
		})
	}
}
