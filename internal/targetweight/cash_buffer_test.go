package targetweight

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// ADR 0026, owner decision 2026-10-31: reserve from available cash after
// proceeds, before scaling. Synthetic money and instruments only.
func TestCashBufferBuyPhase(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		cash, proceeds, want float64
		skipped              bool
	}{
		{"below_buffer", 50, 0, 0, true},
		{"proceeds_do_not_erase_shortfall", 50, 100, 50, false},
		{"exact_fit", 9902.10, 0, 9802.10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
			if err != nil {
				t.Fatal(err)
			}
			var cfg event.TargetWeightConfigurationPayload
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Execution.ShareQuantity = event.ShareQuantityFractional
			cfg.Execution.MaxFeesPerOrder = 2
			if err := json.Unmarshal([]byte(`{"cash_buffer_fraction":0.01}`), &cfg.Execution); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2025, 1, 2, 21, 0, 0, 0, time.UTC)
			h, err := New("momentum/test", cfg, Options{DecideFrom: at})
			if err != nil {
				t.Fatal(err)
			}
			// ADR 0016: the run owns its recorded rule, even if its caller
			// changes the source configuration after construction.
			*cfg.Execution.CashBufferFraction = 0.05
			h.gate.RecordDecision("decision")
			r := h.momentum
			r.snapshot = &event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: cfg.AccountID, QuantityScale: 10000, AsOf: at, NetLiquidation: 10000, Currency: "USD"}, SettledCash: tc.cash, TotalCash: tc.cash}
			r.marks["AAA"] = event.MarkRecordedPayload{InstrumentID: "AAA", Price: 100, AsOf: at}
			o := event.OrderPlannedPayload{AccountID: cfg.AccountID, Tag: "decision", ClientOrderID: "buy", InstrumentID: "AAA", Side: event.OrderSideBuy, Phase: event.OrderSideBuy, OrderType: event.OrderTypeLimit, Quantity: 980000, QuantityScale: 10000, FractionalIncrement: "0.0001", LimitPrice: 100, ReferencePrice: 100, PriceBand: 0.05, NetLiquidation: 10000, TargetWeight: 0.98}
			r.rebalance = &rebalance{decisionID: "decision", session: at, snapshot: *r.snapshot, buys: []event.OrderPlannedPayload{o}, budget: orderplan.NewBudget(tc.cash), withheld: map[string]bool{}, prices: map[string]float64{"AAA": 100}}
			r.rebalance.budget.AddProceeds(tc.proceeds)
			b := &emissions{h: h, at: at, in: event.Envelope{ID: "trigger"}}
			r.planBuys(b)
			out, err := b.result()
			if err != nil {
				t.Fatal(err)
			}
			planned, skipped := 0, 0
			for _, e := range out {
				switch e.Type {
				case event.OrderPlannedEventType:
					planned++
					var p event.OrderPlannedPayload
					if err := json.Unmarshal(e.Payload, &p); err != nil {
						t.Fatal(err)
					}
					if cost := orderplan.BuyCost(p, 2); cost > tc.want {
						t.Errorf("planned cost %v exceeds buffered budget %v", cost, tc.want)
					}
					if tc.name == "exact_fit" && p.Quantity != o.Quantity {
						t.Errorf("exact-fit buy scaled: %d, want %d", p.Quantity, o.Quantity)
					}
				case event.OrderSkippedEventType:
					skipped++
					var p event.OrderSkippedPayload
					if err := json.Unmarshal(e.Payload, &p); err != nil {
						t.Fatal(err)
					}
					if p.Reason == "" {
						t.Error("skip lacks reason")
					}
				}
			}
			if tc.skipped && (planned != 0 || skipped != 1) {
				t.Errorf("below buffer: planned %d skipped %d; want 0/1", planned, skipped)
			}
			if !tc.skipped && planned != 1 {
				t.Errorf("want one buy, got %d", planned)
			}
		})
	}
}
