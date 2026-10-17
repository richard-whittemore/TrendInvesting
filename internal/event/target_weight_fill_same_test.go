package event_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func TestTargetWeightFillSameExecution(t *testing.T) {
	at := time.Date(2026, 3, 2, 21, 0, 0, 0, time.UTC)
	p := event.TargetWeightFillPayload{AccountID: "I", ClientOrderID: "c", FillID: "f", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 2, Price: 3, FilledAt: at}
	moved := p
	moved.FilledAt = at.In(time.FixedZone("EST", -5*3600))
	if !p.SameExecution(moved) {
		t.Error("one instant in another offset is the same execution")
	}
	other := p
	other.Quantity++
	if p.SameExecution(other) {
		t.Error("a different quantity is a different execution")
	}
}
