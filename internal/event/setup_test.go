package event_test

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validSetupEvaluated returns a payload that satisfies every validation rule
// so each table test only needs to describe its one deviation.
func validSetupEvaluated() event.SetupEvaluatedPayload {
	return event.SetupEvaluatedPayload{
		InstrumentID: "AAPL",
		PeriodEnd:    time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC),
		N:            0.0141,
		NReady:       true,
	}
}

func TestSetupEvaluatedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.SetupEvaluatedPayload)
		wantErr string
	}{
		{name: "valid, ready"},
		{
			name:    "valid, not ready, n is zero",
			mutate:  func(p *event.SetupEvaluatedPayload) { p.N = 0; p.NReady = false },
			wantErr: "",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.SetupEvaluatedPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "zero period end",
			mutate:  func(p *event.SetupEvaluatedPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			name:    "negative n",
			mutate:  func(p *event.SetupEvaluatedPayload) { p.N = -0.0001 },
			wantErr: "n must not be negative",
		},
		{
			// Greptile PR #60 finding 3: twenty flat bars (high==low==close)
			// legitimately complete bar-count warm-up with N==0, which is not
			// a usable volatility reading. NReady means "N is usable", not
			// merely "warm-up complete", so this combination must be
			// rejected rather than handed to a downstream sizing step that
			// would divide by it.
			name:    "ready with zero n is invalid",
			mutate:  func(p *event.SetupEvaluatedPayload) { p.N = 0; p.NReady = true },
			wantErr: "n must be positive when ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validSetupEvaluated()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}
			err := payload.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// A NaN or infinite N must be rejected explicitly, the same way bar.go and
// configuration.go reject non-finite fields: an unreadable N must not reach
// a downstream sizing calculation silently.
func TestSetupEvaluatedPayloadValidateRejectsNonFiniteN(t *testing.T) {
	t.Parallel()

	nonFinite := []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	}

	for _, nf := range nonFinite {
		t.Run(nf.name, func(t *testing.T) {
			t.Parallel()
			payload := validSetupEvaluated()
			payload.N = nf.value

			err := payload.Validate()
			if err == nil || !strings.Contains(err.Error(), "n must be finite") {
				t.Fatalf("Validate() error = %v, want substring %q", err, "n must be finite")
			}
		})
	}
}

func TestSetupEvaluatedPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.SetupEvaluatedPayload
	payload.N = -1

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{"instrument id", "period end", "n must not be negative"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestSetupEvaluatedEventConstants(t *testing.T) {
	t.Parallel()

	if event.SetupEvaluatedEventType == "" {
		t.Fatal("SetupEvaluatedEventType must not be empty")
	}
	if event.SetupEvaluatedSchemaVersion == 0 {
		t.Fatal("SetupEvaluatedSchemaVersion must be positive")
	}
	// This is deliberately not, and must never collide with, a future
	// Signal event type: evaluating N is not a trade decision.
	if event.SetupEvaluatedEventType == event.CompletedBarEventType || event.SetupEvaluatedEventType == event.ConfigurationEventType {
		t.Fatalf("SetupEvaluatedEventType %q collides with an existing event type", event.SetupEvaluatedEventType)
	}
}

// Round-trip stability: encoding then decoding a valid payload must reproduce
// the exact same bytes on re-encoding.
func TestSetupEvaluatedPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validSetupEvaluated()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.SetupEvaluatedPayload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded.Validate() error = %v", err)
	}

	reEncoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-Marshal() error = %v", err)
	}

	if !bytes.Equal(encoded, reEncoded) {
		t.Fatalf("round trip not stable:\n  first:  %s\n  second: %s", encoded, reEncoded)
	}
}

// The JSON wire format uses snake_case tags, matching the envelope's
// convention.
func TestSetupEvaluatedPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validSetupEvaluated())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{"instrument_id", "period_end", "n", "n_ready"} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
