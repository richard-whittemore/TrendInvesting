package event_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validRebalanceCleared() event.RebalanceClearedPayload {
	return event.RebalanceClearedPayload{
		AccountID:      "ira",
		AsOf:           time.Date(2026, time.October, 17, 14, 30, 0, 0, time.UTC),
		DecisionID:     "targets-decided:ira:2026-09-30T20:00:00Z",
		ClientOrderIDs: []string{"ira-buy-1", "ira-buy-2"},
		Operator:       "owner",
		Detail:         "both buys cancelled at IBKR after the venue stopped reporting",
	}
}

// TestRebalanceClearedValidate: the owner's cleared decision names the
// account, when, the rebalance in progress by its decision ID and every
// order of it the owner has dealt with at the broker, strictly ascending,
// and who cleared it and why; nothing about the rebalance is inferred from
// absence (ADR 0026, point 5: a rebalance in progress sends nothing until
// the owner clears it).
func TestRebalanceClearedValidate(t *testing.T) {
	v := validRebalanceCleared()
	if err := v.Validate(); err != nil {
		t.Fatalf("the fixture is invalid: %v", err)
	}
	type P = event.RebalanceClearedPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "as of", v, func(p *P) { p.AsOf = time.Time{} }, "as of")
	expectInvalid(t, "decision", v, func(p *P) { p.DecisionID = "" }, "decision id")
	expectInvalid(t, "no orders", v, func(p *P) { p.ClientOrderIDs = nil }, "client order id")
	expectInvalid(t, "an empty order", v, func(p *P) { p.ClientOrderIDs = []string{"ira-buy-1", ""} }, "client order id")
	expectInvalid(t, "orders out of order", v, func(p *P) { p.ClientOrderIDs = []string{"ira-buy-2", "ira-buy-1"} }, "ascending")
	expectInvalid(t, "a repeated order", v, func(p *P) { p.ClientOrderIDs = []string{"ira-buy-1", "ira-buy-1"} }, "ascending")
	expectInvalid(t, "operator", v, func(p *P) { p.Operator = "" }, "operator")
	expectInvalid(t, "detail", v, func(p *P) { p.Detail = "" }, "detail")
}

func TestRebalanceClearedRoundTrips(t *testing.T) {
	raw, err := json.Marshal(validRebalanceCleared())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"client_order_ids":["ira-buy-1","ira-buy-2"]`) || !strings.Contains(string(raw), `"decision_id":`) {
		t.Errorf("encoded %s", raw)
	}
	if event.RebalanceClearedEventType != "operator.rebalance.cleared" || event.RebalanceClearedSchemaVersion != 1 {
		t.Errorf("type %s version %d", event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion)
	}
}
