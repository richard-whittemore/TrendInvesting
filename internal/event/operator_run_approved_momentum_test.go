package event_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// iraApproval is a fresh IRA run's approval: no Boost latch, and the
// momentum state in force, here no rebalance in progress.
func iraApproval() event.OperatorRunApprovedPayload {
	p := validOperatorRunApproved()
	p.AccountID, p.Boost = "DUIRA", event.BoostStateInForce{}
	p.Momentum = &event.MomentumStateInForce{}
	return p
}

// sellsWorking is a rebalance whose sells were working when the previous
// run ended, its buys never planned.
func sellsWorking() *event.RebalanceInProgress {
	return &event.RebalanceInProgress{
		DecisionID:     "targets-decided:DUIRA:2025-01-31T21:00:00Z",
		DecisionTime:   time.Date(2025, time.January, 31, 21, 0, 0, 0, time.UTC),
		Phase:          event.RebalancePhaseSellsWorking,
		ClientOrderIDs: []string{"DUIRA-1", "DUIRA-2"},
	}
}

// iraApprovalMidRebalance is iraApproval with sellsWorking in progress.
func iraApprovalMidRebalance() event.OperatorRunApprovedPayload {
	p := iraApproval()
	p.Momentum.RebalanceInProgress = sellsWorking()
	return p
}

// TestAnIRAApprovalStatesTheRebalanceInProgress: an IRA has no Boost latch;
// its state in force a fresh run cannot recompute is the rebalance in
// progress, which the approval states, or states that none is (schema
// version 6; ADR 0026, point 5: "Every planned, released and sent order, and
// the IRA phase"). A Boost approval keeps its latch and states no momentum
// state; an approval states exactly one strategy's state.
func TestAnIRAApprovalStatesTheRebalanceInProgress(t *testing.T) {
	if event.OperatorRunApprovedSchemaVersion < 6 {
		t.Fatalf("OperatorRunApprovedSchemaVersion = %d, want 6 once an approval states an IRA's rebalance in progress", event.OperatorRunApprovedSchemaVersion)
	}
	none := iraApproval()
	if err := none.Validate(); err != nil {
		t.Fatalf("an IRA approval stating no rebalance in progress: %v", err)
	}
	if got := none.StrategyID(); got != event.TargetWeightStrategyMomentum {
		t.Errorf("an IRA approval's strategy = %q, want %q", got, event.TargetWeightStrategyMomentum)
	}
	if got := validOperatorRunApproved().StrategyID(); got != event.TargetWeightStrategyBoost {
		t.Errorf("a Boost approval's strategy = %q, want %q", got, event.TargetWeightStrategyBoost)
	}
	working := iraApproval()
	working.Momentum.RebalanceInProgress = sellsWorking()
	if err := working.Validate(); err != nil {
		t.Fatalf("an IRA approval stating a rebalance in progress: %v", err)
	}
	type P = event.OperatorRunApprovedPayload
	rebalance := func(mutate func(*event.RebalanceInProgress)) func(*P) {
		return func(p *P) {
			r := sellsWorking()
			mutate(r)
			p.Momentum = &event.MomentumStateInForce{RebalanceInProgress: r}
		}
	}
	v := iraApproval()
	expectInvalid(t, "both strategies", v, func(p *P) { p.Boost.Latched = new(false) }, "one strategy")
	expectInvalid(t, "neither strategy", v, func(p *P) { p.Momentum = nil }, "boost latched is required")
	expectInvalid(t, "no decision", v, rebalance(func(r *event.RebalanceInProgress) { r.DecisionID = "" }), "decision id")
	expectInvalid(t, "no decision time", v, rebalance(func(r *event.RebalanceInProgress) { r.DecisionTime = time.Time{} }), "decision time")
	expectInvalid(t, "unknown phase", v, rebalance(func(r *event.RebalanceInProgress) { r.Phase = "buys-pending" }), "phase")
	expectInvalid(t, "no orders", v, rebalance(func(r *event.RebalanceInProgress) { r.ClientOrderIDs = nil }), "client order ids")
	expectInvalid(t, "orders out of order", v, rebalance(func(r *event.RebalanceInProgress) { r.ClientOrderIDs = []string{"DUIRA-2", "DUIRA-1"} }), "ascending")
	expectInvalid(t, "an IRA sale back to 98%", v, func(p *P) {
		p.IncidentInForce = &event.IncidentInForce{IncidentID: "engine-state:DUIRA:1", State: event.EngineStateHalted, SaleAttempted: new(true)}
	}, "no sale back to 98%")
	first := iraApproval()
	first.PreviousLog, first.PreviousLogFinalHash = "", ""
	first.StartingBook = &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 10000, Currency: "USD"}
	if err := first.Validate(); err != nil {
		t.Errorf("an IRA's first run approval: %v", err)
	}
	expectInvalid(t, "a first run mid-rebalance", first, rebalance(func(*event.RebalanceInProgress) {}), "first run")
}

// TestDecodeAnIRAApprovalRequiresTheRebalanceKey: the momentum state is read
// only at schema version 6, and states its rebalance in progress, null when
// none, and every key of one it states: no rebalance is taken as none by its
// absence (ADR 0026, point 5). An approval naming no momentum state is a
// Boost approval and needs Boost's latch, so a lost key never reads as an
// IRA approval with no rebalance in progress.
func TestDecodeAnIRAApprovalRequiresTheRebalanceKey(t *testing.T) {
	base := `"account_id":"DUIRA","previous_log":"run-1.log","previous_log_final_hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","incident_id":"","approved_by":"owner","approved_at":"2026-10-17T13:00:00Z","starting_book":null,"incident_in_force":null`
	rebalance := `{"decision_id":"targets-decided:DUIRA:2025-01-31T21:00:00Z","decision_time":"2025-01-31T21:00:00Z","phase":"sells-working","client_order_ids":["DUIRA-1"]}`
	for _, raw := range []string{
		`{` + base + `,"boost":null,"momentum":{"rebalance_in_progress":null}}`,
		`{` + base + `,"boost":null,"momentum":{"rebalance_in_progress":` + rebalance + `}}`,
	} {
		p, err := event.DecodeOperatorRunApproved([]byte(raw))
		if err != nil || p.Momentum == nil {
			t.Errorf("DecodeOperatorRunApproved(%s) = %+v, %v", raw, p.Momentum, err)
		}
	}
	for raw, want := range map[string]string{
		`{` + base + `,"boost":null}`:                                        "boost",
		`{` + base + `,"boost":null,"momentum":{}}`:                          "momentum.rebalance_in_progress",
		`{` + base + `,"boost":null,"momentum":null}`:                        "momentum",
		`{` + base + `,"boost":null,"momentum":{"rebalance_in_progress":5}}`: "object or null",
		`{` + base + `,"boost":null,"momentum":{"rebalance_in_progress":{"decision_id":"d","decision_time":"2025-01-31T21:00:00Z","phase":"sells-working"}}}`: "momentum.rebalance_in_progress.client_order_ids",
		`{` + base + `,"boost":{"latched":false},"momentum":{"rebalance_in_progress":null}}`:                                                                  "one strategy",
	} {
		if _, err := event.DecodeOperatorRunApproved([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DecodeOperatorRunApproved(%s) = %v, want a refusal mentioning %q", raw, err, want)
		}
	}
	ira := `{` + base + `,"boost":null,"momentum":{"rebalance_in_progress":null}}`
	if _, err := event.DecodeRecordedOperatorRunApproved([]byte(ira), 5); err == nil || !strings.Contains(err.Error(), "momentum") {
		t.Errorf("a version 5 approval stating a momentum state: %v", err)
	}
	if err := iraApproval().ValidateSchema(5); err == nil || !strings.Contains(err.Error(), "momentum") {
		t.Errorf("ValidateSchema(5) of an IRA approval: %v", err)
	}
}

// TestABoostApprovalEncodesAsBefore: a Boost approval states no momentum
// key, so its file and payload are those an earlier build wrote; an IRA
// approval encodes its Boost state as null, never as a latch of null.
func TestABoostApprovalEncodesAsBefore(t *testing.T) {
	raw, err := json.Marshal(validOperatorRunApproved())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "momentum") || !strings.Contains(string(raw), `"boost":{"latched":true}`) {
		t.Errorf("a Boost approval encodes as %s", raw)
	}
	working := iraApproval()
	working.Momentum.RebalanceInProgress = sellsWorking()
	raw, err = json.Marshal(working)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"boost":null`) || !strings.Contains(string(raw), `"phase":"sells-working"`) {
		t.Errorf("an IRA approval encodes as %s", raw)
	}
	back, err := event.DecodeOperatorRunApproved(raw)
	if err != nil || back.Momentum == nil || back.Momentum.RebalanceInProgress == nil || back.Momentum.RebalanceInProgress.DecisionID != working.Momentum.RebalanceInProgress.DecisionID {
		t.Errorf("an encoded IRA approval decodes as %+v, %v", back.Momentum, err)
	}
}
