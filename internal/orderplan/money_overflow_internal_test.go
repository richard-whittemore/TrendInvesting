package orderplan

import (
	"math"
	"math/big"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// ADR 0026, "Worst-case cost": a debit outside the finite monetary domain
// must refuse the basket before it can commit cash. A panic also fails the test.
func TestBuyCostAndFractionalScalingRefuseUnrepresentableDebits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		quantity    int64
		price, fees float64
		reason      string
	}{
		{"product overflow", 20000, math.MaxFloat64, 0, "buy cost: debit exceeds the finite monetary range"},
		{"fee overflow", 10000, math.MaxFloat64, math.MaxFloat64, "buy cost: debit exceeds the finite monetary range"},
		{"nan price", 20000, math.NaN(), 0, "buy cost: quantity, price and fees must be finite and nonnegative"},
		{"infinite price", 20000, math.Inf(1), 0, "buy cost: quantity, price and fees must be finite and nonnegative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := event.OrderPlannedPayload{
				Quantity: tc.quantity, QuantityScale: event.FractionalQuantityScale,
				FractionalIncrement: "0.0001", LimitPrice: tc.price, RoundingSlack: 0.1,
			}
			if cost := BuyCost(order, tc.fees); !math.IsInf(cost, 1) {
				t.Fatalf("buy cost = %v; want +Inf refusal", cost)
			}
			// ScaleBuys exposes refusal as a nil basket and zero scale.
			kept, skipped, scale := ScaleBuys([]event.OrderPlannedPayload{order}, math.MaxFloat64, tc.fees)
			if kept != nil || skipped != nil || scale != 0 {
				t.Fatalf("scaling returned %v, %v, %v; want nil, nil, 0", kept, skipped, scale)
			}
			// Exercise the ordering helper independently of scaling's preflight
			// reservation, which would otherwise refuse these inputs first.
			orders, exact, monetary, err := fractionalOrdersAtScale(
				[]event.OrderPlannedPayload{order}, []int64{1}, big.NewRat(1, 1), tc.fees)
			if orders != nil || exact != nil || monetary != 0 || err == nil || err.Error() != tc.reason {
				t.Fatalf("ordering returned %v, %v, %v, %v; want empty refusal with %q", orders, exact, monetary, err, tc.reason)
			}
		})
	}
}
