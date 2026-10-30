package risklimit_test

import (
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// ADR 0026's IRA cash gate refuses an unrepresentable debit before releasing an order.
func TestBuyCostOverflowRefusesWithoutPanic(t *testing.T) {
	for _, scale := range []int64{0, event.FractionalQuantityScale} {
		t.Run(event.FormatQuantity(1, scale), func(t *testing.T) {
			g := iraGate(t, 0)
			if err := g.SetQuantityScale(scale); err != nil {
				t.Fatal(err)
			}
			o := iraOrder("AAA", event.OrderSideBuy, 20000)
			o.Planned.QuantityScale = scale
			o.Planned.LimitPrice = math.MaxFloat64
			out := checkBook(t, g, risklimit.Book{QuantityScale: scale, CashAvailable: math.MaxFloat64}, o)[0]
			refusedFor(t, out, event.RefusedInsufficientCash)
			if !strings.Contains(out.Refused.Detail, "finite") {
				t.Fatalf("refusal omits arithmetic reason: %s", out.Refused.Detail)
			}
			if err := g.RestoreIncidentState("overflow-incident", event.EngineStateHalted, nil, nil); err != nil {
				t.Fatal(err)
			}
			halted := checkBook(t, g, risklimit.Book{QuantityScale: scale, CashAvailable: math.MaxFloat64}, o)[0]
			refusedFor(t, halted, event.RefusedEngineHalted)
			if g.State() != risklimit.StateHalted || halted.State != nil {
				t.Fatal("overflow loosened the Halted latch")
			}

		})
	}
}
