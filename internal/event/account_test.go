package event_test

import (
	"encoding/json"
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
		AsOf:          time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC),
		Equity:        1_000_000,
		AvailableCash: 500_000,
		Currency:      "USD",
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
		{
			// Zero is a legitimate reading (every dollar already deployed),
			// unlike Equity: an account can have no spare cash at all.
			name:    "zero available cash",
			mutate:  func(p *event.AccountSnapshotPayload) { p.AvailableCash = 0 },
			wantErr: "",
		},
		{
			name:    "negative available cash",
			mutate:  func(p *event.AccountSnapshotPayload) { p.AvailableCash = -1 },
			wantErr: "available cash must not be negative",
		},
		{
			name:    "nan available cash",
			mutate:  func(p *event.AccountSnapshotPayload) { p.AvailableCash = math.NaN() },
			wantErr: "available cash must be finite",
		},
		{
			name:    "positive infinite available cash",
			mutate:  func(p *event.AccountSnapshotPayload) { p.AvailableCash = math.Inf(1) },
			wantErr: "available cash must be finite",
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

// TestAccountSnapshotPayloadAvailableCashIsRequiredInJSON pins the one thing
// a struct-level check cannot see: a record that OMITS available_cash decodes
// it as the float64 zero, which is indistinguishable from a snapshot
// deliberately reporting no spare cash. A malformed payload would then
// silently decline every affordable Unit (ADR 0010) instead of being refused,
// so presence is tracked through decoding and an absent field is rejected —
// while an explicit 0 stays a legitimate reading.
func TestAccountSnapshotPayloadAvailableCashIsRequiredInJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		encoded string
		wantErr string
	}{
		{
			name:    "omitted available cash",
			encoded: `{"as_of":"2026-01-02T00:00:00Z","equity":1000000,"currency":"USD"}`,
			wantErr: "available cash is required",
		},
		{
			name:    "null available cash",
			encoded: `{"as_of":"2026-01-02T00:00:00Z","equity":1000000,"available_cash":null,"currency":"USD"}`,
			wantErr: "available cash is required",
		},
		{
			name:    "explicit zero available cash",
			encoded: `{"as_of":"2026-01-02T00:00:00Z","equity":1000000,"available_cash":0,"currency":"USD"}`,
			wantErr: "",
		},
		{
			name:    "explicit available cash",
			encoded: `{"as_of":"2026-01-02T00:00:00Z","equity":1000000,"available_cash":500000,"currency":"USD"}`,
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var payload event.AccountSnapshotPayload
			if err := json.Unmarshal([]byte(tt.encoded), &payload); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
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

// TestAccountSnapshotPayloadRoundTripsThroughJSON confirms the presence
// tracking above does not change what a well-formed record decodes to: a
// payload built in Go, encoded and decoded, is the payload that went in.
func TestAccountSnapshotPayloadRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	original := validAccountSnapshot()
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded event.AccountSnapshotPayload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded != original {
		t.Fatalf("round trip = %+v, want %+v", decoded, original)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

// TestAccountSnapshotPayloadRejectsMalformedJSON pins that a decoding failure
// is surfaced rather than swallowed by the presence tracking.
func TestAccountSnapshotPayloadRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	var payload event.AccountSnapshotPayload
	if err := json.Unmarshal([]byte(`{"available_cash":"lots"}`), &payload); err == nil {
		t.Fatal("json.Unmarshal() error = nil, want a decoding error")
	}
}
