package targetweight

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

func TestSnapshotUnavailableSettledCash(t *testing.T) {
	for _, rule := range []string{event.CashRuleTotal, event.CashRuleSettled} {
		t.Run(rule, func(t *testing.T) {
			raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
			if err != nil {
				t.Fatal(err)
			}
			var cfg event.TargetWeightConfigurationPayload
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Execution.CashRule = rule
			cfg.Execution.ShareQuantity = event.ShareQuantityFractional
			at := time.Date(2025, 1, 2, 21, 0, 0, 0, time.UTC)
			h, err := New("momentum/test", cfg, Options{DecideFrom: at})
			if err != nil {
				t.Fatal(err)
			}
			p := []byte(fmt.Sprintf(`{"account_id":%q,"as_of":"2025-01-02T21:00:00Z","currency":"USD","net_liquidation":1000,"positions":[],"total_cash":500,"quantity_scale":10000}`, cfg.AccountID))
			in := event.Envelope{ID: "snapshot", Type: event.AccountSnapshotEventType, SchemaVersion: 7, EventTime: at, Payload: p}
			out, err := h.momentum.applySnapshot(in)
			if err != nil {
				t.Fatal(err)
			}
			if rule == event.CashRuleTotal {
				if len(out) != 0 || h.momentum.cash(*h.momentum.snapshot) != 500 {
					t.Fatalf("total cash changed: %v", out)
				}
			} else {
				halted := false
				for _, e := range out {
					if e.Type == event.ReconciliationResultEventType {
						var result event.ReconciliationResultPayload
						if err := json.Unmarshal(e.Payload, &result); err != nil {
							t.Fatal(err)
						}
						halted = result.Classification == event.ReconciliationUnverifiable && result.Action == event.ReconciliationActionHalt
					}
				}
				if !halted || h.gate.State() != risklimit.StateHalted {
					t.Fatalf("missing existing unverifiable halt: %v", out)
				}
			}
		})
	}
}

func TestUnavailableSettledCashRulesVersion(t *testing.T) {
	if RulesVersion == "1.30.0" {
		t.Fatal("ADR 0016: accepting a new snapshot needs a rules version")
	}
}

// ADR 0019 and ADR 0026 point 2, Budget: missing settled cash halts
// settled-cash buys even when an earlier snapshot established the cash book.
func TestUnavailableSettledCashWithEstablishedBook(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Execution.CashRule = event.CashRuleSettled
	cfg.Execution.ShareQuantity = event.ShareQuantityFractional
	at := time.Date(2025, 1, 2, 21, 0, 0, 0, time.UTC)
	h, err := New("momentum/test", cfg, Options{DecideFrom: at})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{
			AccountID: cfg.AccountID, AsOf: at, Currency: "USD", NetLiquidation: 1000,
			QuantityScale: 10000, Positions: []event.AccountPosition{},
		}, TotalCash: 500, SettledCash: new(float64(500)),
	}
	raw, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	first := event.Envelope{ID: "snapshot:6", Type: event.AccountSnapshotEventType, SchemaVersion: 6, EventTime: at, Payload: raw}
	if out, err := h.momentum.applySnapshot(first); err != nil || len(out) != 0 {
		t.Fatalf("establish book: %v, %v", out, err)
	}
	previous := h.momentum.snapshot
	snapshot.AsOf = at.Add(time.Hour)
	snapshot.SettledCash = nil
	raw, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	next := event.Envelope{ID: "snapshot:7", Type: event.AccountSnapshotEventType, SchemaVersion: 7, EventTime: snapshot.AsOf, Payload: raw}
	out, err := h.momentum.applySnapshot(next)
	if err != nil {
		t.Fatal(err)
	}
	if h.momentum.snapshot != previous {
		t.Error("unavailable snapshot replaced the stored snapshot")
	}
	if h.gate.State() != risklimit.StateHalted {
		t.Errorf("state = %s", h.gate.State())
	}
	if len(out) != 2 || out[0].Type != event.ReconciliationResultEventType || out[1].Type != event.EngineStateEventType {
		t.Fatalf("want reconciliation and halt alone, got %v", out)
	}
	var result event.ReconciliationResultPayload
	if err := json.Unmarshal(out[0].Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result.Classification != event.ReconciliationUnverifiable || result.Action != event.ReconciliationActionHalt {
		t.Errorf("result = %+v", result)
	}
	const reason = "IBKR sent no settled cash, which the settled-cash rule budgets buys from"
	if !strings.Contains(result.Detail, reason) || !strings.Contains(strings.Join(result.Unverifiable, " "), reason) {
		t.Errorf("missing settled-cash reason: %+v", result)
	}
	if strings.Contains(result.Detail, "total cash could not be read") {
		t.Errorf("misleading reason: %s", result.Detail)
	}
	var state event.EngineStatePayload
	if err := json.Unmarshal(out[1].Payload, &state); err != nil {
		t.Fatal(err)
	}
	if state.State != event.EngineStateHalted || !strings.Contains(state.Detail, reason) {
		t.Errorf("halt = %+v", state)
	}
}
