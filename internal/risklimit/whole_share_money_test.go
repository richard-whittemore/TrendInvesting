package risklimit_test

import (
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/orderplan"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// ADR 0026 preserves D7-L's original cash checks and journal evidence.
func TestWholeShareCashBoundaryPreservesPlanningAndRefusal(t *testing.T) {
	o := iraOrder("AAA", event.OrderSideBuy, 82)
	o.Planned.LimitPrice, o.Planned.ReferencePrice, o.Mark = 214.18, 214.18, 214.18
	o.Planned.NetLiquidation = 100000
	const cash = 17563.76
	kept, skipped, _ := orderplan.ScaleBuys([]event.OrderPlannedPayload{o.Planned}, cash, 1)
	if len(kept) != 1 || kept[0].Quantity != 81 || len(skipped) != 0 {
		t.Errorf("planned %+v, skipped %+v; want 81 shares", kept, skipped)
	}
	out := checkBook(t, iraGate(t, 1), risklimit.Book{CashAvailable: cash}, o)[0]
	refusedFor(t, out, event.RefusedInsufficientCash)
	raw, err := json.Marshal(out.Refused.Figures)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["buy_cost"]) != "17563.760000000002" {
		t.Fatalf("buy_cost = %s; want origin/main bytes 17563.760000000002", fields["buy_cost"])
	}
}
