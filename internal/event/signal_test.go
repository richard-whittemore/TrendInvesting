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

// validSignal returns a payload that satisfies every validation rule so each
// table test only needs to describe its one deviation.
func validSignal() event.SignalPayload {
	return event.SignalPayload{
		InstrumentID:       "AAPL",
		PeriodEnd:          time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC),
		Rule:               event.RuleEntryChannelBreakout,
		ADR:                event.ADREntryChannelBreakout,
		Direction:          event.DirectionLong,
		EntryChannelLength: 55,
		EntryChannelHigh:   150.0,
		BreakoutHigh:       151.0,
		N:                  2.5,
	}
}

func TestSignalPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.SignalPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.SignalPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "zero period end",
			mutate:  func(p *event.SignalPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.SignalPayload) { p.Rule = "" },
			wantErr: "rule is required",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.SignalPayload) { p.ADR = "" },
			wantErr: "adr is required",
		},
		{
			name:    "invalid direction",
			mutate:  func(p *event.SignalPayload) { p.Direction = "short" },
			wantErr: "direction",
		},
		{
			name:    "empty direction",
			mutate:  func(p *event.SignalPayload) { p.Direction = "" },
			wantErr: "direction",
		},
		{
			name:    "zero entry channel length",
			mutate:  func(p *event.SignalPayload) { p.EntryChannelLength = 0 },
			wantErr: "entry channel length must be a positive integer",
		},
		{
			name:    "negative entry channel length",
			mutate:  func(p *event.SignalPayload) { p.EntryChannelLength = -55 },
			wantErr: "entry channel length must be a positive integer",
		},
		{
			name:    "zero entry channel high",
			mutate:  func(p *event.SignalPayload) { p.EntryChannelHigh = 0 },
			wantErr: "entry channel high must be positive",
		},
		{
			name:    "negative entry channel high",
			mutate:  func(p *event.SignalPayload) { p.EntryChannelHigh = -1 },
			wantErr: "entry channel high must be positive",
		},
		{
			name:    "zero breakout high",
			mutate:  func(p *event.SignalPayload) { p.BreakoutHigh = 0 },
			wantErr: "breakout high must be positive",
		},
		{
			name:    "negative breakout high",
			mutate:  func(p *event.SignalPayload) { p.BreakoutHigh = -1 },
			wantErr: "breakout high must be positive",
		},
		{
			// The Turtle Rules p.19: a Breakout "exceeds" the channel high, so
			// a Signal whose breakout high does not strictly exceed its own
			// entry channel high is a contradiction in the payload itself.
			name: "breakout high equal to entry channel high",
			mutate: func(p *event.SignalPayload) {
				p.EntryChannelHigh = 150.0
				p.BreakoutHigh = 150.0
			},
			wantErr: "breakout high must exceed the entry channel high",
		},
		{
			name: "breakout high below entry channel high",
			mutate: func(p *event.SignalPayload) {
				p.EntryChannelHigh = 150.0
				p.BreakoutHigh = 149.0
			},
			wantErr: "breakout high must exceed the entry channel high",
		},
		{
			name:    "zero n",
			mutate:  func(p *event.SignalPayload) { p.N = 0 },
			wantErr: "n must be positive",
		},
		{
			name:    "negative n",
			mutate:  func(p *event.SignalPayload) { p.N = -1 },
			wantErr: "n must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validSignal()
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

func TestSignalPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(p *event.SignalPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{
			name:    "entry channel high",
			apply:   func(p *event.SignalPayload, f float64) { p.EntryChannelHigh = f },
			wantErr: "entry channel high must be finite",
		},
		{
			name:    "breakout high",
			apply:   func(p *event.SignalPayload, f float64) { p.BreakoutHigh = f },
			wantErr: "breakout high must be finite",
		},
		{
			name:    "n",
			apply:   func(p *event.SignalPayload, f float64) { p.N = f },
			wantErr: "n must be finite",
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
				payload := validSignal()
				field.apply(&payload, nf.value)

				err := payload.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
	}
}

func TestSignalPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.SignalPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"instrument id",
		"period end",
		"rule is required",
		"adr is required",
		"direction",
		"entry channel length must be a positive integer",
		"entry channel high must be positive",
		"breakout high must be positive",
		"n must be positive",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestSignalEventConstants(t *testing.T) {
	t.Parallel()

	if event.SignalEventType == "" {
		t.Fatal("SignalEventType must not be empty")
	}
	if event.SignalSchemaVersion == 0 {
		t.Fatal("SignalSchemaVersion must be positive")
	}
	// A Signal must never be confused with a Setup-evaluated event: only the
	// former is a decision to act on (CONTEXT.md: "Signal").
	for _, other := range []string{event.CompletedBarEventType, event.ConfigurationEventType, event.SetupEvaluatedEventType} {
		if event.SignalEventType == other {
			t.Fatalf("SignalEventType %q collides with an existing event type %q", event.SignalEventType, other)
		}
	}
	if event.RuleEntryChannelBreakout == "" {
		t.Fatal("RuleEntryChannelBreakout must not be empty")
	}
	if event.ADREntryChannelBreakout != "0002" {
		t.Fatalf("ADREntryChannelBreakout = %q, want %q (ADR 0002 defines the entry-channel breakout rule itself, not just the Baseline)", event.ADREntryChannelBreakout, "0002")
	}
	if event.DirectionLong != "long" {
		t.Fatalf("DirectionLong = %q, want %q", event.DirectionLong, "long")
	}
}

// Round-trip stability: encoding then decoding a valid payload must reproduce
// the exact same bytes on re-encoding.
func TestSignalPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validSignal()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.SignalPayload
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
func TestSignalPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validSignal())
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
		"rule",
		"adr",
		"direction",
		"entry_channel_length",
		"entry_channel_high",
		"breakout_high",
		"n",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
