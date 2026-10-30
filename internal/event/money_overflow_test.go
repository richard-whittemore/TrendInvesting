package event_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// ADR 0026's no-borrowing invariant requires an unrepresentable debit to fail closed.
func TestBuyMoneyRejectsUnrepresentableDebit(t *testing.T) {
	for _, scale := range []int64{0, event.FractionalQuantityScale} {
		for _, tc := range []struct {
			name               string
			quantity           int64
			price, fees, slack float64
		}{
			{"product overflow", 20000, math.MaxFloat64, 0, 0.1},
			{"fee overflow", 10000, math.MaxFloat64, math.MaxFloat64, 0.1},
			{"nan price", 1, math.NaN(), 0, 0.1},
			{"infinite price", 1, math.Inf(1), 0, 0.1},
			{"nan fees", 1, 1, math.NaN(), 0.1},
			{"infinite fees", 1, 1, math.Inf(1), 0.1},
		} {
			t.Run(tc.name+event.FormatQuantity(1, scale), func(t *testing.T) {
				if cost, err := event.BuyCost(tc.quantity, scale, tc.price, tc.fees); err == nil || cost != nil {
					t.Fatalf("cost %v, error %v; want explicit refusal", cost, err)
				}
				if cost, err := event.BuyReservation(tc.quantity, scale, tc.price, tc.fees, tc.slack); err == nil || cost != nil {
					t.Fatalf("reservation %v, error %v; want explicit refusal", cost, err)
				}
			})
		}
	}
	for _, scale := range []int64{0, event.FractionalQuantityScale} {
		for _, slack := range []float64{math.NaN(), math.Inf(1)} {
			if _, err := event.BuyReservation(1, scale, 1, 0, slack); err == nil {
				t.Fatal("non-finite slack accepted")
			}
		}
	}
}

func TestBuyCostRefusesUnsupportedQuantityScale(t *testing.T) {
	const reason = "quantity scale 1 is not legacy whole shares (0) or four-place fixed point (10000)"
	cost, err := event.BuyCost(20000, 1, math.MaxFloat64, 0)
	if cost != nil || err == nil || err.Error() != reason {
		t.Fatalf("cost %v, error %v; want nil and %q", cost, err, reason)
	}
}
