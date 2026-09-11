package event_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validCashMovement() event.CashMovementPayload {
	return event.CashMovementPayload{
		AsOf:         time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
		Amount:       200_000,
		EquityBefore: 890_000,
		Currency:     "USD",
	}
}

func TestCashMovementPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CashMovementPayload)
		wantErr string
	}{
		{name: "valid deposit"},
		{
			name:    "valid withdrawal",
			mutate:  func(p *event.CashMovementPayload) { p.Amount = -100_000 },
			wantErr: "",
		},
		{
			name:    "missing as of",
			mutate:  func(p *event.CashMovementPayload) { p.AsOf = time.Time{} },
			wantErr: "as of is required",
		},
		{
			name:    "zero amount",
			mutate:  func(p *event.CashMovementPayload) { p.Amount = 0 },
			wantErr: "amount must be non-zero",
		},
		{
			name:    "nan amount",
			mutate:  func(p *event.CashMovementPayload) { p.Amount = math.NaN() },
			wantErr: "amount must be finite",
		},
		{
			name:    "infinite amount",
			mutate:  func(p *event.CashMovementPayload) { p.Amount = math.Inf(1) },
			wantErr: "amount must be finite",
		},
		{
			name:    "zero equity before",
			mutate:  func(p *event.CashMovementPayload) { p.EquityBefore = 0 },
			wantErr: "equity before must be positive",
		},
		{
			name:    "negative equity before",
			mutate:  func(p *event.CashMovementPayload) { p.EquityBefore = -1 },
			wantErr: "equity before must be positive",
		},
		{
			name: "withdrawal to exactly zero equity fails closed",
			mutate: func(p *event.CashMovementPayload) {
				p.EquityBefore = 100_000
				p.Amount = -100_000
			},
			wantErr: "would take equity to",
		},
		{
			name: "withdrawal below zero equity fails closed",
			mutate: func(p *event.CashMovementPayload) {
				p.EquityBefore = 100_000
				p.Amount = -150_000
			},
			wantErr: "would take equity to",
		},
		{
			name:    "missing currency",
			mutate:  func(p *event.CashMovementPayload) { p.Currency = "" },
			wantErr: "currency is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validCashMovement()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}
			err := payload.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
