package event_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validCost() event.CostPayload {
	return event.CostPayload{
		Kind: event.CostCommission, Amount: -1, Currency: "USD",
		AsOf: time.Date(2027, 1, 13, 5, 0, 0, 0, time.UTC), TransactionID: "900000106",
	}
}

func TestCostValidation(t *testing.T) {
	if err := validCost().ValidateSchema(event.CostSchemaVersion); err != nil {
		t.Fatalf("a valid cost: %v", err)
	}
	for _, kind := range event.CostKinds() {
		c := validCost()
		c.Kind = kind
		if err := c.ValidateSchema(event.CostSchemaVersion); err != nil {
			t.Errorf("kind %q: %v", kind, err)
		}
	}
	tests := []struct {
		name   string
		mutate func(*event.CostPayload)
		want   string
	}{
		{"unknown kind", func(c *event.CostPayload) { c.Kind = "dividend" }, "kind"},
		{"no amount", func(c *event.CostPayload) { c.Amount = 0 }, "non-zero"},
		{"nan amount", func(c *event.CostPayload) { c.Amount = math.NaN() }, "finite"},
		{"no currency", func(c *event.CostPayload) { c.Currency = "" }, "currency"},
		{"no time", func(c *event.CostPayload) { c.AsOf = time.Time{} }, "as of"},
		{"no transaction id", func(c *event.CostPayload) { c.TransactionID = "" }, "transaction id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validCost()
			tt.mutate(&c)
			err := c.ValidateSchema(event.CostSchemaVersion)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one naming %q", err, tt.want)
			}
		})
	}
	if err := validCost().ValidateSchema(2); err == nil {
		t.Error("a version this build does not know validated")
	}
}
