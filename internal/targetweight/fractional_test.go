package targetweight

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// TestFractionalLegacyUpcast is ADR 0015's explicit whole-share migration;
// malformed counts never partially mutate a normalized payload.
func TestFractionalLegacyUpcast(t *testing.T) {
	h := &Handler{cfg: event.TargetWeightConfigurationPayload{Execution: event.ExecutionConfiguration{ShareQuantity: "fractional"}}}
	scale, a, b := int64(0), int64(12), int64(7)
	if err := h.normalizeQuantities(&scale, &a, &b); err != nil || scale != 10000 || a != 120000 || b != 70000 {
		t.Fatalf("upcast: %d %d %d %v", scale, a, b, err)
	}
	for _, bad := range []int64{-1, math.MaxInt64} {
		scale, a, b = 0, 12, bad
		if err := h.normalizeQuantities(&scale, &a, &b); err == nil {
			t.Fatal("invalid count accepted")
		}
		if scale != 0 || a != 12 || b != bad {
			t.Fatal("refused input was partially mutated")
		}
	}
	h.cfg.Execution.ShareQuantity = event.ShareQuantityMinimumLot
	scale, a = 10000, 123456
	if err := h.normalizeQuantities(&scale, &a); err == nil {
		t.Fatal("whole-share account accepted fractional schema")
	}
}

// TestFractionalInputUnits refuses legacy reports and unrepresentable legacy
// observations at the actual handler seams (ADR 0015; ADR 0026 D7-F).
func TestFractionalInputUnits(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Execution.ShareQuantity = event.ShareQuantityFractional
	cfg.Mode = event.TradingModePaper
	cfg.AccountID = "DUfixture"
	at := time.Date(2025, 1, 2, 21, 0, 0, 0, time.UTC)
	h, err := New("momentum/test", cfg, Options{DecideFrom: at})
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(typ string, v uint32, p interface{ Validate() error }) event.Envelope {
		t.Helper()
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return event.Envelope{ID: "synthetic", Type: typ, SchemaVersion: v, EventTime: at, Payload: raw}
	}
	if out, err := h.checkBook(event.Envelope{ID: "mixed-book"}, reconcile.Book{AnchorID: "mixed-book"}, at); err == nil || !strings.Contains(err.Error(), "quantity scale") || len(out) != 0 {
		t.Fatalf("mixed book must refuse without emissions: %v, %v", out, err)
	}
	snapshot := event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: cfg.AccountID, AsOf: at, NetLiquidation: 1000, Currency: "USD", Positions: []event.AccountPosition{{InstrumentID: "AAA", Quantity: -1}}}, SettledCash: 1000, TotalCash: 1000}
	if _, err := h.momentum.applySnapshot(envelope(event.AccountSnapshotEventType, 4, snapshot)); err == nil || !strings.Contains(err.Error(), "four-place") {
		t.Fatalf("legacy negative snapshot: %v", err)
	}
	broker := event.BrokerSnapshotPayload{AccountID: cfg.AccountID, AsOf: at, Unreadable: []string{}, Positions: []event.BrokerPosition{{InstrumentID: "AAA", Quantity: -1, Mark: 10, MarkSet: true}}, OpenOrders: []event.BrokerOrder{}, NetLiquidation: 1000, ExcessLiquidity: 1000}
	if _, err := h.applyBrokerSnapshot(envelope(event.BrokerSnapshotEventType, 1, broker)); err == nil || !strings.Contains(err.Error(), "four-place") {
		t.Fatalf("legacy negative broker snapshot: %v", err)
	}
	rec := event.ReconciliationPayload{AccountID: cfg.AccountID, ReconciliationID: "synthetic", Trigger: event.ReconciliationTriggerMorning, AsOf: at, Unreadable: []string{}, Positions: snapshot.Positions, TotalCash: 1000, Currency: "USD"}
	if _, err := h.applyReconciliation(envelope(event.ReconciliationEventType, 1, rec)); err == nil || !strings.Contains(err.Error(), "four-place") {
		t.Fatalf("legacy negative reconciliation: %v", err)
	}
	life := event.LimitOrderLifecyclePayload{AccountID: cfg.AccountID, ClientOrderID: "synthetic", OrderID: "synthetic-venue", InstrumentID: "AAA", Side: event.OrderSideSell, Status: event.OrderLifecycleStatusCanceled, Quantity: 7, LimitPrice: 10, OccurredAt: at}
	if _, err := h.momentum.applyLifecycle(envelope(event.OrderLifecycleEventType, 2, life)); err == nil || !strings.Contains(err.Error(), "scale") {
		t.Fatalf("legacy lifecycle on fractional account: %v", err)
	}
	fill := event.TargetWeightFillPayload{AccountID: cfg.AccountID, ClientOrderID: "synthetic", FillID: "synthetic-fill", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 3, Price: 10, FilledAt: at}
	if _, err := h.momentum.applyFill(envelope(event.FillEventType, 5, fill)); err == nil || !strings.Contains(err.Error(), "scale") {
		t.Fatalf("legacy fill on fractional account: %v", err)
	}
	retired := event.OrderRetiredPayload{AccountID: cfg.AccountID, ClientOrderID: "synthetic", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 7, LimitPrice: 10, RetiredAt: at, RetiredBy: event.OrderRetiredByAdapter, Reason: event.OrderRetiredSessionOver, Detail: "synthetic session ended"}
	if _, err := h.momentum.applyRetired(envelope(event.OrderRetiredEventType, 1, retired)); err == nil || !strings.Contains(err.Error(), "scale") {
		t.Fatalf("legacy retirement on fractional account: %v", err)
	}
}

// ADR 0026, Worst-case cost and D5: available cash rounds sales down and buys up.
func TestFractionalFillBudgetUsesDirectionalCents(t *testing.T) {
	for _, side := range []string{event.OrderSideBuy, event.OrderSideSell} {
		for _, price := range []float64{10.10, 10.101} {
			t.Run(side+fmt.Sprint(price), func(t *testing.T) {
				raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
				if err != nil {
					t.Fatal(err)
				}
				var cfg event.TargetWeightConfigurationPayload
				if err := json.Unmarshal(raw, &cfg); err != nil {
					t.Fatal(err)
				}
				cfg.Execution.ShareQuantity = event.ShareQuantityFractional
				cfg.Execution.CashRule = event.CashRuleTotal
				at := time.Date(2025, 1, 2, 21, 0, 0, 0, time.UTC)
				h, err := New("momentum/test", cfg, Options{DecideFrom: at})
				if err != nil {
					t.Fatal(err)
				}
				o := event.OrderReleasedPayload{AccountID: cfg.AccountID, ClientOrderID: "fractional", InstrumentID: "AAA", Side: side, Quantity: 145000, QuantityScale: 10000, LimitPrice: price}
				r := h.momentum
				r.orders.Release(o)
				if err := h.gate.RestoreReleased(o, nil, time.Time{}, ""); err != nil {
					t.Fatal(err)
				}
				r.rebalance = &rebalance{budget: orderplan.NewBudget(100), sold: map[string]int64{}}
				if side == event.OrderSideBuy {
					r.rebalance.budget.Release(o.ClientOrderID, 90)
				}
				f := event.TargetWeightFillPayload{AccountID: o.AccountID, ClientOrderID: o.ClientOrderID, InstrumentID: o.InstrumentID, Side: side, Quantity: 72500, QuantityScale: 10000, Price: price, Commission: 0.01, FillID: "fill", FilledAt: at}
				payload, err := json.Marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.applyFill(event.Envelope{ID: "fill", Type: event.FillEventType, SchemaVersion: f.SchemaVersion(), EventTime: at, Payload: payload}); err != nil {
					t.Fatal(err)
				}
				// Close releases the unused reservation, exposing the actual fill debit.
				if side == event.OrderSideBuy {
					r.rebalance.budget.Close(o.ClientOrderID)
				}
				want := 26.76
				if side == event.OrderSideSell {
					want = 173.21
					if price == 10.101 {
						want = 173.22
					}
				} else if price == 10.101 {
					want = 26.75
				}
				if got := r.rebalance.budget.Remaining(); math.Abs(got-want) > 1e-12 {
					t.Fatalf("budget %.8f; want %.2f", got, want)
				}
			})
		}
	}
}
