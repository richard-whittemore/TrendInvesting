package risklimit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// TestTheFillRuleNeedsNoLimits: the rule a recorded execution of a released
// order is applied by (Gate.ObserveFill) is a pure function of the fill, the
// order it names and the fills already applied, so the reporter reads a
// Boost run's fills by it exactly as the gate does, with no gate and no
// limits (ADR 0019, "Explained means": a fill already applied is never added
// again).
func TestTheFillRuleNeedsNoLimits(t *testing.T) {
	at := time.Date(2024, time.January, 2, 21, 1, 0, 0, time.UTC)
	order := &risklimit.FillOrder{AccountID: "joint", ClientOrderID: "tw-1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 9}
	fill := func(id string, quantity int64) event.TargetWeightFillPayload {
		return event.TargetWeightFillPayload{AccountID: "joint", ClientOrderID: "tw-1", FillID: id, InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: quantity, Price: 100, FilledAt: at}
	}
	applied := map[string]event.TargetWeightFillPayload{}
	if redelivery, err := risklimit.CheckFill(fill("f1", 4), "joint", order, applied); err != nil || redelivery {
		t.Fatalf("a part fill: redelivery %v, %v", redelivery, err)
	}
	order.Reported = 4
	applied["f1"] = fill("f1", 4)
	if redelivery, err := risklimit.CheckFill(fill("f1", 4), "joint", order, applied); err != nil || !redelivery {
		t.Errorf("the same execution again is a redelivery: %v, %v", redelivery, err)
	}
	for name, c := range map[string]struct {
		p       event.TargetWeightFillPayload
		account string
		order   *risklimit.FillOrder
		want    string
	}{
		"past the order's shares":       {fill("f2", 6), "joint", order, "past its 9 shares"},
		"a fill id reused":              {fill("f1", 5), "joint", order, "reused for a different execution"},
		"an order never released":       {fill("f2", 1), "joint", nil, "never released"},
		"another account's gate":        {fill("f2", 1), "ira", order, `this gate is "ira"`},
		"a fill that does not validate": {fill("f2", 0), "joint", order, "quantity"},
	} {
		if _, err := risklimit.CheckFill(c.p, c.account, c.order, applied); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error saying %q", name, err, c.want)
		}
	}
	other := *order
	other.Side = event.OrderSideSell
	if _, err := risklimit.CheckFill(fill("f2", 1), "joint", &other, applied); err == nil || !strings.Contains(err.Error(), "describes buy SPY") {
		t.Errorf("a fill of the other side: %v", err)
	}
}
