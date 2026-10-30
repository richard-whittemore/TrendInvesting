package targetweight

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
)

// ADR 0026: a working buy retains its allowance across partial executions;
// only termination releases unused slack. The fixture is synthetic.
func TestRoundingSlackHeldUntilLastExecution(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	slack := 0.37
	cfg.Execution.ShareQuantity, cfg.Execution.MaxFeesPerOrder = event.ShareQuantityFractional, 0
	cfg.Execution.RoundingSlackPerOrder = &slack
	cfg.Mode, cfg.AccountID = event.TradingModePaper, "DUfixture"
	at := time.Date(2025, 1, 2, 21, 0, 0, 0, time.UTC)
	h, err := New("momentum/test", cfg, Options{DecideFrom: at})
	if err != nil {
		t.Fatal(err)
	}
	// ADR 0016 freezes the effective rule even if the caller reuses its input.
	slack = 1
	h.gate.RecordDecision("decision")
	r := h.momentum
	r.snapshot = &event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: cfg.AccountID, QuantityScale: 10000, AsOf: at, NetLiquidation: 10000, Currency: "USD"}, TotalCash: 73.60, SettledCash: 73.60}
	r.marks["AAA"] = event.MarkRecordedPayload{InstrumentID: "AAA", Price: 10.10, AsOf: at}
	o := event.OrderPlannedPayload{AccountID: cfg.AccountID, Tag: "decision", ClientOrderID: "buy", InstrumentID: "AAA", Side: event.OrderSideBuy, Phase: event.OrderSideBuy, OrderType: event.OrderTypeLimit,
		Quantity: 72500, QuantityScale: 10000, FractionalIncrement: "0.0001", LimitPrice: 10.10, ReferencePrice: 10.10, PriceBand: 0.05, NetLiquidation: 10000, TargetWeight: 0.01}
	r.rebalance = &rebalance{decisionID: "decision", session: at, snapshot: *r.snapshot, buys: []event.OrderPlannedPayload{o}, budget: orderplan.NewBudget(73.60), withheld: map[string]bool{}, prices: map[string]float64{"AAA": 10.10}}
	b := &emissions{h: h, at: at, in: event.Envelope{ID: "trigger"}}
	r.planBuys(b)
	if _, err := b.result(); err != nil {
		t.Fatal(err)
	}
	if got := r.rebalance.budget.Remaining(); got != 0 {
		t.Fatalf("release failed to reserve cost and configured slack: remaining %v", got)
	}
	for i := range 4 {
		f := event.TargetWeightFillPayload{AccountID: cfg.AccountID, ClientOrderID: "buy", FillID: fmt.Sprint(i), InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 18125, QuantityScale: 10000, Price: 10.10, FilledAt: at}
		payload, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.applyFill(event.Envelope{ID: f.FillID, Type: event.FillEventType, SchemaVersion: f.SchemaVersion(), EventTime: at, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		want := 0.0
		if i == 3 {
			want = 0.36 // $73.60 cash minus four $18.31 executions.
		}
		if got := r.rebalance.budget.Remaining(); math.Abs(got-want) > 1e-12 {
			t.Fatalf("fill %d: remaining %.12f, want %.2f", i+1, got, want)
		}
	}
}
