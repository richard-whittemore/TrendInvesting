package targetweight

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// ADR 0026: an unrepresentable debit cannot mutate the fill book or commit buy cash.
func TestUnrepresentableBuyDebitRefusesBeforeMutation(t *testing.T) {
	for _, mode := range []string{event.ShareQuantityMinimumLot, event.ShareQuantityFractional} {
		t.Run(mode, func(t *testing.T) {
			raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
			if err != nil {
				t.Fatal(err)
			}
			var cfg event.TargetWeightConfigurationPayload
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Execution.ShareQuantity = mode
			at := time.Date(2025, 1, 2, 21, 0, 0, 0, time.UTC)
			h, err := New("momentum/test", cfg, Options{DecideFrom: at})
			if err != nil {
				t.Fatal(err)
			}
			r := h.momentum
			o := event.OrderReleasedPayload{AccountID: cfg.AccountID, ClientOrderID: "overflow", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 30000, QuantityScale: cfg.Execution.QuantityScale(), LimitPrice: math.MaxFloat64}
			r.orders.Release(o)
			if err := h.gate.RestoreReleased(o, nil, time.Time{}, ""); err != nil {
				t.Fatal(err)
			}
			r.rebalance = &rebalance{budget: orderplan.NewBudget(20000), sold: map[string]int64{}}
			r.rebalance.budget.Release(o.ClientOrderID, 10000)
			f := event.TargetWeightFillPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: o.Side, Quantity: 20000, QuantityScale: o.QuantityScale, Price: math.MaxFloat64, FillID: "overflow-fill", FilledAt: at}
			payload, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			in := event.Envelope{ID: f.FillID, Type: event.FillEventType, SchemaVersion: f.SchemaVersion(), EventTime: at, Payload: payload}
			for range 2 {
				if _, err := r.applyFill(in); err == nil || !strings.Contains(err.Error(), "finite") {
					t.Fatalf("want explicit finite-debit refusal on each attempt, got %v", err)
				}
			}
			if got := r.rebalance.budget.Remaining(); got != 10000 {
				t.Fatalf("budget mutated to %v", got)
			}
			r.snapshot = &event.AccountCashSnapshotPayload{}
			r.rebalance.buys = []event.OrderPlannedPayload{{Quantity: 20000, QuantityScale: o.QuantityScale, LimitPrice: math.MaxFloat64}}
			b := &emissions{}
			r.planBuys(b)
			if b.err == nil || !strings.Contains(b.err.Error(), "finite") {
				t.Fatalf("planning must refuse with arithmetic reason, got %v", b.err)
			}
		})
	}
}
