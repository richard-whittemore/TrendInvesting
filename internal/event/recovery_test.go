package event_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validNotionalAccountRecovered() event.NotionalAccountRecoveredPayload {
	return event.NotionalAccountRecoveredPayload{
		AsOf:           time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
		Equity:         1_000_000,
		StartingFigure: 1_000_000,
		NotionalBefore: 640_000,
		StepsCleared:   2,
		Rule:           event.RuleNotionalAccountRecovery,
		ADR:            event.ADRNotionalAccountRecovery,
	}
}

func TestNotionalAccountRecoveredPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.NotionalAccountRecoveredPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing as of",
			mutate:  func(p *event.NotionalAccountRecoveredPayload) { p.AsOf = time.Time{} },
			wantErr: "as of is required",
		},
		{
			name:    "zero equity",
			mutate:  func(p *event.NotionalAccountRecoveredPayload) { p.Equity = 0 },
			wantErr: "equity must be positive",
		},
		{
			name:    "zero starting figure",
			mutate:  func(p *event.NotionalAccountRecoveredPayload) { p.StartingFigure = 0 },
			wantErr: "starting figure must be positive",
		},
		{
			name:    "zero notional before",
			mutate:  func(p *event.NotionalAccountRecoveredPayload) { p.NotionalBefore = 0 },
			wantErr: "notional before must be positive",
		},
		{
			name: "notional before at or above starting figure",
			mutate: func(p *event.NotionalAccountRecoveredPayload) {
				p.NotionalBefore = p.StartingFigure
			},
			wantErr: "must be strictly below",
		},
		{
			name: "equity below starting figure",
			mutate: func(p *event.NotionalAccountRecoveredPayload) {
				p.Equity = p.StartingFigure - 1
			},
			wantErr: "must be at or above",
		},
		{
			name:    "zero steps cleared",
			mutate:  func(p *event.NotionalAccountRecoveredPayload) { p.StepsCleared = 0 },
			wantErr: "steps cleared must be a positive",
		},
		{
			name:    "negative steps cleared",
			mutate:  func(p *event.NotionalAccountRecoveredPayload) { p.StepsCleared = -1 },
			wantErr: "steps cleared must be a positive",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.NotionalAccountRecoveredPayload) { p.Rule = "" },
			wantErr: "rule is required",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.NotionalAccountRecoveredPayload) { p.ADR = "" },
			wantErr: "adr is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validNotionalAccountRecovered()
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
