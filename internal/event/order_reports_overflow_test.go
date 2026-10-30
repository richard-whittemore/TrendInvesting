package event_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// ADR 0026's finite buy debit is accepted by one rule for live orders and
// recorded reports (ADR 0027); a refusal changes no order or fill identity.
func TestOrderReportsRefusesOverflowingBuyDebitBeforeMutation(t *testing.T) {
	for _, mode := range []string{event.ShareQuantityMinimumLot, event.ShareQuantityFractional} {
		for _, cause := range []string{"product", "commission"} {
			t.Run(mode+"/"+cause, func(t *testing.T) {
				scale := (event.ExecutionConfiguration{ShareQuantity: mode}).QuantityScale()
				quantity := 2 * event.QuantityUnits(scale)
				r := event.NewOrderReports()
				r.Release(event.OrderReleasedPayload{AccountID: "ira", ClientOrderID: "buy", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: quantity, QuantityScale: scale, LimitPrice: 10})
				p := event.TargetWeightFillPayload{AccountID: "ira", ClientOrderID: "buy", FillID: "execution", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: quantity, QuantityScale: scale, Price: math.MaxFloat64, FilledAt: time.Date(2025, 2, 3, 15, 0, 0, 0, time.UTC)}
				if cause == "commission" {
					p.Price = math.MaxFloat64 / 2
					p.Commission = math.MaxFloat64
				}
				if err := p.ValidateSchema(p.SchemaVersion()); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if _, duplicate, err := r.Fill(p); err == nil || duplicate || !strings.Contains(err.Error(), "finite monetary range") {
						t.Fatalf("overflow must be refused on every attempt: duplicate=%v, err=%v", duplicate, err)
					}
					if o, _ := r.Order("buy"); o.Terminal || o.Filled != 0 || o.Venue || len(o.Fills) != 0 || len(r.Working()) != 1 {
						t.Fatalf("overflow mutated order: %+v", o)
					}
				}
				p.Price, p.Commission = 10, 0
				if o, duplicate, err := r.Fill(p); err != nil || duplicate || !o.Terminal || o.Filled != quantity {
					t.Fatalf("refused fill must not reserve its identity: order=%+v, duplicate=%v, err=%v", o, duplicate, err)
				}
			})
		}
	}
}
