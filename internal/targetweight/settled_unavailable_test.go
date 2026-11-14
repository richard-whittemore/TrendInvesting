package targetweight

import (
	"encoding/json"
	"fmt"
	"os"
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
