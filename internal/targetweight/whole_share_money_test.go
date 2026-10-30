package targetweight

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// ADR 0026 preserves D7-L's unrounded fills and float summation order.
func TestWholeShareFillBudgetPreservesLegacyBits(t *testing.T) {
	for _, side := range []string{event.OrderSideBuy, event.OrderSideSell} {
		t.Run(side, func(t *testing.T) {
			raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
			if err != nil {
				t.Fatal(err)
			}
			var cfg event.TargetWeightConfigurationPayload
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Execution.CashRule = event.CashRuleTotal
			at := time.Date(2025, 1, 2, 21, 0, 0, 0, time.UTC)
			h, err := New("momentum/test", cfg, Options{DecideFrom: at})
			if err != nil {
				t.Fatal(err)
			}
			o := event.OrderReleasedPayload{AccountID: cfg.AccountID, ClientOrderID: "whole", InstrumentID: "AAA", Side: side, Quantity: 30, LimitPrice: 500}
			r := h.momentum
			r.orders.Release(o)
			if err := h.gate.RestoreReleased(o, nil, time.Time{}, ""); err != nil {
				t.Fatal(err)
			}
			r.rebalance = &rebalance{budget: orderplan.NewBudget(20000), sold: map[string]int64{}}
			if side == event.OrderSideBuy {
				r.rebalance.budget.Release(o.ClientOrderID, 15000)
			}
			total := 0.0
			for i, price := range []float64{412.3456, 214.18} {
				f := event.TargetWeightFillPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: side, Quantity: 10, Price: price, Commission: 2, FillID: []string{"one", "two"}[i], FilledAt: at}
				payload, err := json.Marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.applyFill(event.Envelope{ID: f.FillID, Type: event.FillEventType, SchemaVersion: f.SchemaVersion(), EventTime: at, Payload: payload}); err != nil {
					t.Fatal(err)
				}
				value := event.QuantityValue(f.Quantity, 0, f.Price)
				if side == event.OrderSideBuy {
					total += value + f.Commission
				} else {
					total += value - f.Commission
				}
			}
			want := 20000 + total
			if side == event.OrderSideBuy {
				r.rebalance.budget.Close(o.ClientOrderID)
				want = 20000 - total
			}
			if got := r.rebalance.budget.Remaining(); math.Float64bits(got) != math.Float64bits(want) {
				t.Fatalf("budget %.17g; want legacy %.17g", got, want)
			}
		})
	}
}
