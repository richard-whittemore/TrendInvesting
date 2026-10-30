package reconcile_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// ADR 0026 D7-F and ADR 0019: exact four-place positions have zero tolerance.
func TestFractionalPositionsAndFillReconcileExactly(t *testing.T) {
	b := reconcile.CashBook("snapshot", event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: "ira", AsOf: closeAt, NetLiquidation: 100, Currency: "USD", QuantityScale: 10000, Positions: []event.AccountPosition{held("AAA", 123456)}},
		TotalCash:                       50, SettledCash: 50,
	})
	obs := observed(50, held("AAA", 123456))
	obs.QuantityScale = 10000
	r := classify(t, &b, nil, obs)
	if r.Classification != event.ReconciliationMatched || r.QuantityScale != 10000 {
		t.Fatalf("match: %+v", r)
	}
	fill := event.TargetWeightFillPayload{QuantityScale: 10000, FillID: "fill", InstrumentID: "AAA", Side: event.OrderSideSell, Quantity: 35000, Price: 2.5, Commission: 0.25, FilledAt: closeAt.Add(time.Minute)}
	c := reconcile.FillChange(fill)
	if c.Cash != 850 || c.Shares != -35000 {
		t.Fatalf("fill change: %+v", c)
	}
	obs = observed(58.5, held("AAA", 88456))
	obs.QuantityScale = 10000
	r = classify(t, &b, []reconcile.Change{c}, obs)
	if r.Classification != event.ReconciliationExplained {
		t.Fatalf("fill: %+v", r)
	}
	obs.Positions[0].Quantity++
	r = classify(t, &b, []reconcile.Change{c}, obs)
	if r.Classification != event.ReconciliationUnexplained || component(t, r, "AAA").Residual != 1 {
		t.Fatalf("one ten-thousandth difference: %+v", r)
	}
}

func TestFractionalReconciliationRefusesMixedScalesAndSubTickSplit(t *testing.T) {
	b := &reconcile.Book{AnchorID: "snapshot", AsOf: closeAt, QuantityScale: 10000, Positions: map[string]int64{"AAA": 123456}}
	obs := observed(0, held("AAA", 123456))
	r := classify(t, b, nil, obs)
	if r.Classification != event.ReconciliationUnverifiable {
		t.Fatalf("mixed scales: %+v", r)
	}
	obs.QuantityScale = 10000
	c := reconcile.FillChange(event.TargetWeightFillPayload{FillID: "whole", InstrumentID: "AAA", Quantity: 1, FilledAt: closeAt.Add(time.Minute)})
	r = classify(t, b, []reconcile.Change{c}, obs)
	if r.Classification != event.ReconciliationUnverifiable {
		t.Fatalf("mixed fill: %+v", r)
	}
	c = reconcile.CorporateActionChange("split", event.CorporateActionPayload{Kind: event.CorporateActionKindSplit, InstrumentID: "AAA", EffectiveAt: closeAt.Add(time.Minute), NewShares: 1, OldShares: 10})
	r = classify(t, b, []reconcile.Change{c}, obs)
	if r.Classification != event.ReconciliationUnverifiable {
		t.Fatalf("split silently truncated fractional ticks: %+v", r)
	}
}
