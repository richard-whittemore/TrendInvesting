package event_test

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// ADR 0026, "Per-execution rounding reserve": D7-L retains its float
// arithmetic, including sub-penny fills and whole-cent boundary cases.
func TestWholeShareMoneyPreservesLegacyBits(t *testing.T) {
	check := func(t *testing.T, q int64, price, fees float64) bool {
		t.Helper()
		wantBuy := event.QuantityValue(q, 0, price) + fees
		wantSale := event.QuantityValue(q, 0, price) - fees
		buy, _ := event.BuyCost(q, 0, price, fees).Float64()
		reservation, _ := event.BuyReservation(q, 0, price, fees, 0).Float64()
		sale := event.SaleProceeds(q, 0, price, fees)
		if math.Float64bits(buy) != math.Float64bits(wantBuy) || math.Float64bits(reservation) != math.Float64bits(wantBuy) || math.Float64bits(sale) != math.Float64bits(wantSale) {
			t.Errorf("q=%d price=%.17g fees=%.17g: buy=%.17g reserve=%.17g sale=%.17g; legacy buy=%.17g sale=%.17g", q, price, fees, buy, reservation, sale, wantBuy, wantSale)
			return false
		}
		return true
	}
	t.Run("cash boundary", func(t *testing.T) { check(t, 82, 214.18, 1) })
	t.Run("sub-penny fill", func(t *testing.T) { check(t, 10, 412.3456, 2) })
	t.Run("whole-cent property", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(627, 2))
		for range 200000 {
			if !check(t, 1+rng.Int64N(10000), float64(1+rng.Int64N(100000))/100, float64(rng.Int64N(1000))/100) {
				return
			}
		}
	})
}
