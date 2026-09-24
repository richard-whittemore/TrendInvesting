package event_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds the tests for OrderLifecycleEventType: a venue's report
// that one of this system's orders changed state without executing.

func validOrderLifecycle() event.OrderLifecyclePayload {
	return event.OrderLifecyclePayload{
		InstrumentID: "AAPL",
		OrderID:      "7",
		Tag:          "proposal:AAPL:2014-06-06T20:00:00.000000000Z",
		Status:       event.OrderLifecycleStatusSubmitted,
		Quantity:     100,
		StopPrice:    24.5,
		OccurredAt:   time.Date(2014, 6, 6, 20, 0, 0, 0, time.UTC),
		Message:      "",
	}
}

func TestOrderLifecyclePayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.OrderLifecyclePayload)
		wantErr string
	}{
		{name: "valid submission"},
		{name: "a sell order carries a negative quantity", mutate: func(p *event.OrderLifecyclePayload) { p.Quantity = -100 }},
		{name: "a message is free text", mutate: func(p *event.OrderLifecyclePayload) { p.Message = "Order expired" }},
		{name: "updated", mutate: func(p *event.OrderLifecyclePayload) { p.Status = event.OrderLifecycleStatusUpdated }},
		{name: "cancel pending", mutate: func(p *event.OrderLifecyclePayload) { p.Status = event.OrderLifecycleStatusCancelPending }},
		{name: "canceled", mutate: func(p *event.OrderLifecyclePayload) { p.Status = event.OrderLifecycleStatusCanceled }},
		{name: "invalid", mutate: func(p *event.OrderLifecyclePayload) { p.Status = event.OrderLifecycleStatusInvalid }},
		{name: "missing instrument", mutate: func(p *event.OrderLifecyclePayload) { p.InstrumentID = "" }, wantErr: "instrument id is required"},
		{name: "missing order id", mutate: func(p *event.OrderLifecyclePayload) { p.OrderID = "" }, wantErr: "order id is required"},
		{name: "missing tag", mutate: func(p *event.OrderLifecyclePayload) { p.Tag = "" }, wantErr: "tag is required"},
		{name: "missing status", mutate: func(p *event.OrderLifecyclePayload) { p.Status = "" }, wantErr: "status is required"},
		{
			// A fill is an execution.fill, never a lifecycle status: a
			// position changes only from a recorded fill.
			name:    "a fill is not a lifecycle status",
			mutate:  func(p *event.OrderLifecyclePayload) { p.Status = "filled" },
			wantErr: `status "filled" is not a recognised order lifecycle status`,
		},
		{
			name:    "a partial fill is not a lifecycle status",
			mutate:  func(p *event.OrderLifecyclePayload) { p.Status = "partially-filled" },
			wantErr: "not a recognised order lifecycle status",
		},
		{name: "zero quantity", mutate: func(p *event.OrderLifecyclePayload) { p.Quantity = 0 }, wantErr: "quantity must not be zero"},
		{name: "zero stop price", mutate: func(p *event.OrderLifecyclePayload) { p.StopPrice = 0 }, wantErr: "stop price must be positive"},
		{name: "negative stop price", mutate: func(p *event.OrderLifecyclePayload) { p.StopPrice = -1 }, wantErr: "stop price must be positive"},
		{name: "non-finite stop price", mutate: func(p *event.OrderLifecyclePayload) { p.StopPrice = math.Inf(1) }, wantErr: "stop price must be finite"},
		{name: "missing time", mutate: func(p *event.OrderLifecyclePayload) { p.OccurredAt = time.Time{} }, wantErr: "occurred at is required"},
		{
			name:    "unwritable time",
			mutate:  func(p *event.OrderLifecyclePayload) { p.OccurredAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
			wantErr: "occurred at cannot be written as RFC 3339",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := validOrderLifecycle()
			if tt.mutate != nil {
				tt.mutate(&p)
			}
			err := p.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}
