package event_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validAccountSnapshot returns a snapshot payload that satisfies every
// validation rule, so each table test only needs to describe its one
// deviation.
func validAccountSnapshot() event.AccountSnapshotPayload {
	return event.AccountSnapshotPayload{
		AsOf:     time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC),
		Equity:   1_000_000,
		Currency: "USD",
	}
}

func TestAccountSnapshotPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.AccountSnapshotPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing as of",
			mutate:  func(p *event.AccountSnapshotPayload) { p.AsOf = time.Time{} },
			wantErr: "as of is required",
		},
		{
			name:    "zero equity",
			mutate:  func(p *event.AccountSnapshotPayload) { p.Equity = 0 },
			wantErr: "equity must be positive",
		},
		{
			name:    "negative equity",
			mutate:  func(p *event.AccountSnapshotPayload) { p.Equity = -1 },
			wantErr: "equity must be positive",
		},
		{
			name:    "nan equity",
			mutate:  func(p *event.AccountSnapshotPayload) { p.Equity = math.NaN() },
			wantErr: "equity must be finite",
		},
		{
			name:    "positive infinite equity",
			mutate:  func(p *event.AccountSnapshotPayload) { p.Equity = math.Inf(1) },
			wantErr: "equity must be finite",
		},
		{
			name:    "negative infinite equity",
			mutate:  func(p *event.AccountSnapshotPayload) { p.Equity = math.Inf(-1) },
			wantErr: "equity must be finite",
		},
		{
			name:    "missing currency",
			mutate:  func(p *event.AccountSnapshotPayload) { p.Currency = "" },
			wantErr: "currency is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validAccountSnapshot()
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
