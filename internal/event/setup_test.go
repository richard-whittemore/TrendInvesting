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
// so each table test only needs to describe its one deviation. Tier is
// TierNone with a positive distance: a Setup that is ready to be evaluated
// but not currently approaching or meeting its entry condition.
func validSetupEvaluated() event.SetupEvaluatedPayload {
	return event.SetupEvaluatedPayload{
		InstrumentID:       "AAPL",
		PeriodEnd:          time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC),
		N:                  0.0141,
		NReady:             true,
		EntryChannelHigh:   150.0,
		EntryChannelReady:  true,
		Tier:               event.TierNone,
		DistanceToEntryInN: 5.0,
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
			name: "valid, not ready, n is zero",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.N = 0
				p.NReady = false
				// #9: when either input is not ready, the Entry Channel
				// fields, Tier, and distance must all report their
				// "not evaluable" zero values too (see the type's doc
				// comment) — this case exercises the whole combination,
				// not just N.
				p.EntryChannelHigh = 0
				p.EntryChannelReady = false
				p.Tier = event.TierNone
				p.DistanceToEntryInN = 0
			},
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
		{
			name:    "negative entry channel high",
			mutate:  func(p *event.SetupEvaluatedPayload) { p.EntryChannelHigh = -1 },
			wantErr: "entry channel high must not be negative",
		},
		{
			name: "entry channel high zero while ready is invalid",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.EntryChannelHigh = 0
				p.EntryChannelReady = true
			},
			wantErr: "entry channel high must be positive when ready",
		},
		{
			name: "entry channel high nonzero while not ready is invalid",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.EntryChannelReady = false
				p.Tier = event.TierNone
				p.DistanceToEntryInN = 0
				// EntryChannelHigh deliberately left at its ready value
				// (150) to trigger exactly this one violation.
			},
			wantErr: "entry channel high must be zero while not ready",
		},
		{
			name:    "invalid tier value",
			mutate:  func(p *event.SetupEvaluatedPayload) { p.Tier = "C" },
			wantErr: "not a recognised tier",
		},
		{
			name: "tier set while n not ready is invalid",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.N = 0
				p.NReady = false
				p.DistanceToEntryInN = 0
				p.Tier = event.TierA
			},
			wantErr: "tier must be empty while n or the entry channel is not ready",
		},
		{
			name: "tier set while entry channel not ready is invalid",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.EntryChannelReady = false
				p.DistanceToEntryInN = 0
				p.Tier = event.TierB
			},
			wantErr: "tier must be empty while n or the entry channel is not ready",
		},
		{
			name: "tier a requires a negative distance",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.Tier = event.TierA
				p.DistanceToEntryInN = 5.0
			},
			wantErr: "tier a requires a negative distance",
		},
		{
			name: "tier a with negative distance is valid",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.Tier = event.TierA
				p.DistanceToEntryInN = -2.0
			},
			wantErr: "",
		},
		{
			name: "tier b requires a positive distance",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.Tier = event.TierB
				p.DistanceToEntryInN = -1.0
			},
			wantErr: "tier b requires a positive distance",
		},
		{
			name: "tier b with positive distance is valid",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.Tier = event.TierB
				p.DistanceToEntryInN = 0.5
			},
			wantErr: "",
		},
		{
			name: "negative distance without tier a is invalid",
			mutate: func(p *event.SetupEvaluatedPayload) {
				p.Tier = event.TierNone
				p.DistanceToEntryInN = -3.0
			},
			wantErr: "a negative distance to entry in n implies a breakout and must be tier a",
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

// A NaN or infinite value must be rejected explicitly, the same way bar.go
// and configuration.go reject non-finite fields: an unreadable value must
// not reach a downstream sizing calculation silently.
func TestSetupEvaluatedPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(p *event.SetupEvaluatedPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{
			name:    "n",
			apply:   func(p *event.SetupEvaluatedPayload, f float64) { p.N = f },
			wantErr: "n must be finite",
		},
		{
			name:    "entry channel high",
			apply:   func(p *event.SetupEvaluatedPayload, f float64) { p.EntryChannelHigh = f },
			wantErr: "entry channel high must be finite",
		},
		{
			name:    "distance to entry in n",
			apply:   func(p *event.SetupEvaluatedPayload, f float64) { p.DistanceToEntryInN = f },
			wantErr: "distance to entry in n must be finite",
		},
	}

	nonFinite := []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	}

	for _, field := range fields {
		for _, nf := range nonFinite {
			t.Run(field.name+" "+nf.name, func(t *testing.T) {
				t.Parallel()
				payload := validSetupEvaluated()
				field.apply(&payload, nf.value)

				err := payload.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
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
	// This must never collide with the Signal event type: evaluating a
	// Setup is not the same as a decision to act on one (CONTEXT.md:
	// "Signal").
	for _, other := range []string{event.CompletedBarEventType, event.ConfigurationEventType, event.SignalEventType} {
		if event.SetupEvaluatedEventType == other {
			t.Fatalf("SetupEvaluatedEventType %q collides with an existing event type %q", event.SetupEvaluatedEventType, other)
		}
	}
}

// TestSetupEvaluatedSchemaVersionBumpedForEntryChannelFields pins #9's
// explicit schema bump: EntryChannelHigh, EntryChannelReady, Tier, and
// DistanceToEntryInN are new fields on an existing payload, so the schema
// version must change (docs/development.md: a schema change is explicit in
// this project, never a silent field addition).
func TestSetupEvaluatedSchemaVersionBumpedForEntryChannelFields(t *testing.T) {
	t.Parallel()

	if event.SetupEvaluatedSchemaVersion != 2 {
		t.Fatalf("SetupEvaluatedSchemaVersion = %d, want 2", event.SetupEvaluatedSchemaVersion)
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

	for _, key := range []string{
		"instrument_id",
		"period_end",
		"n",
		"n_ready",
		"entry_channel_high",
		"entry_channel_ready",
		"tier",
		"distance_to_entry_in_n",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
