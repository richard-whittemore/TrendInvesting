package event_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validNotionalAccountRebased returns a payload that satisfies every
// validation rule, so each table test only needs to describe its one
// deviation (account_test.go's pattern).
func validNotionalAccountRebased() event.NotionalAccountRebasedPayload {
	return event.NotionalAccountRebasedPayload{
		AsOf:                   time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC),
		PreviousStartingFigure: 1_000_000,
		NewStartingFigure:      950_000,
		Equity:                 950_000,
		Rule:                   event.RuleNotionalAccountRebase,
		ADR:                    event.ADRNotionalAccountRebase,
	}
}

func TestNotionalAccountRebasedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.NotionalAccountRebasedPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing as of",
			mutate:  func(p *event.NotionalAccountRebasedPayload) { p.AsOf = time.Time{} },
			wantErr: "as of is required",
		},
		{
			name:    "zero previous starting figure",
			mutate:  func(p *event.NotionalAccountRebasedPayload) { p.PreviousStartingFigure = 0 },
			wantErr: "previous starting figure must be positive",
		},
		{
			name:    "zero new starting figure",
			mutate:  func(p *event.NotionalAccountRebasedPayload) { p.NewStartingFigure = 0; p.Equity = 0 },
			wantErr: "new starting figure must be positive",
		},
		{
			name:    "zero equity",
			mutate:  func(p *event.NotionalAccountRebasedPayload) { p.Equity = 0; p.NewStartingFigure = 0 },
			wantErr: "equity must be positive",
		},
		{
			name: "new starting figure does not match equity",
			mutate: func(p *event.NotionalAccountRebasedPayload) {
				p.NewStartingFigure = 950_000
				p.Equity = 960_000
			},
			wantErr: "must equal equity",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.NotionalAccountRebasedPayload) { p.Rule = "" },
			wantErr: "rule is required",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.NotionalAccountRebasedPayload) { p.ADR = "" },
			wantErr: "adr is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validNotionalAccountRebased()
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
