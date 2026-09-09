package event_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validPayload and validPayloadHash are a matched pair: the hash is the
// SHA-256 of the payload bytes, hex-encoded, computed independently of the
// implementation (`printf '%s' '{"symbol":"AAPL"}' | shasum -a 256`).
const (
	validPayload     = `{"symbol":"AAPL"}`
	validPayloadHash = "81c8d84ddf020b1584fa351351da6f46b756261e048fe93502b5f5c3fdc1e526"
)

func validEnvelope() event.Envelope {
	now := time.Date(2026, time.August, 29, 20, 0, 0, 0, time.UTC)
	return event.Envelope{
		ID:                "evt-1",
		Type:              "market.bar.completed",
		SchemaVersion:     1,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         now,
		RecordedAt:        now,
		Sequence:          1,
		Source:            "lean-adapter",
		StrategyVersion:   "turtle-baseline-1.0.0",
		ConfigurationHash: "cfg-7f3a",
		PayloadHash:       validPayloadHash,
		Payload:           json.RawMessage(validPayload),
	}
}

func TestEnvelopeValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.Envelope)
		wantErr string
	}{
		{name: "valid"},
		{name: "missing id", mutate: func(e *event.Envelope) { e.ID = "" }, wantErr: "event id"},
		{name: "missing type", mutate: func(e *event.Envelope) { e.Type = "" }, wantErr: "event type"},
		{name: "zero schema", mutate: func(e *event.Envelope) { e.SchemaVersion = 0 }, wantErr: "schema version"},
		{name: "zero envelope version", mutate: func(e *event.Envelope) { e.EnvelopeVersion = 0 }, wantErr: "envelope version is required"},
		{
			name:    "envelope version above current",
			mutate:  func(e *event.Envelope) { e.EnvelopeVersion = event.CurrentEnvelopeVersion + 1 },
			wantErr: "newer build",
		},
		{
			// The only integer below CurrentEnvelopeVersion (1) is 0, so this
			// necessarily coincides with the zero-version input above; the
			// point of this row is to confirm the below-current message is
			// also reported, aggregated alongside the required-field one, not
			// that a distinct input value exists yet. Once a future ticket
			// raises CurrentEnvelopeVersion, nonzero values also hit this
			// branch on their own.
			name:    "envelope version below current",
			mutate:  func(e *event.Envelope) { e.EnvelopeVersion = 0 },
			wantErr: "no upcaster registered",
		},
		{name: "zero event time", mutate: func(e *event.Envelope) { e.EventTime = time.Time{} }, wantErr: "event time"},
		{name: "zero recorded time", mutate: func(e *event.Envelope) { e.RecordedAt = time.Time{} }, wantErr: "recorded time"},
		{name: "zero sequence", mutate: func(e *event.Envelope) { e.Sequence = 0 }, wantErr: "sequence"},
		{name: "invalid payload", mutate: func(e *event.Envelope) { e.Payload = []byte(`{"symbol":`) }, wantErr: "valid JSON"},
		{name: "missing source", mutate: func(e *event.Envelope) { e.Source = "" }, wantErr: "event source"},
		{name: "missing strategy version", mutate: func(e *event.Envelope) { e.StrategyVersion = "" }, wantErr: "strategy version"},
		{name: "missing configuration hash", mutate: func(e *event.Envelope) { e.ConfigurationHash = "" }, wantErr: "configuration hash"},
		{name: "missing payload hash", mutate: func(e *event.Envelope) { e.PayloadHash = "" }, wantErr: "payload hash"},
		{
			name:    "mismatched payload hash",
			mutate:  func(e *event.Envelope) { e.PayloadHash = strings.Repeat("0", len(validPayloadHash)) },
			wantErr: "payload hash does not match payload",
		},
		{
			name:    "tampered payload",
			mutate:  func(e *event.Envelope) { e.Payload = json.RawMessage(`{"symbol":"MSFT"}`) },
			wantErr: "payload hash does not match payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			envelope := validEnvelope()
			if tt.mutate != nil {
				tt.mutate(&envelope)
			}
			err := envelope.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// A journal entry with no provenance at all must name every missing field in
// one error, so an operator sees the whole gap rather than fixing it one
// rejection at a time.
func TestEnvelopeValidateAggregatesMissingProvenance(t *testing.T) {
	t.Parallel()

	envelope := validEnvelope()
	envelope.Source = ""
	envelope.StrategyVersion = ""
	envelope.ConfigurationHash = ""
	envelope.PayloadHash = ""

	err := envelope.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{"event source", "strategy version", "configuration hash", "payload hash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

// A tampered payload must be rejected even when the envelope is otherwise
// complete, and the integrity check must not be reported for a payload hash
// that is merely absent.
func TestEnvelopeValidateMissingPayloadHashIsNotReportedAsMismatch(t *testing.T) {
	t.Parallel()

	envelope := validEnvelope()
	envelope.PayloadHash = ""

	err := envelope.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if strings.Contains(err.Error(), "does not match") {
		t.Errorf("Validate() error = %v, want no mismatch complaint for an absent payload hash", err)
	}
}

func TestHashPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "known vector",
			payload: validPayload,
			want:    validPayloadHash,
		},
		{
			name:    "empty object",
			payload: `{}`,
			want:    "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
		},
		{
			name:    "empty payload",
			payload: "",
			want:    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := event.HashPayload(json.RawMessage(tt.payload)); got != tt.want {
				t.Fatalf("HashPayload(%q) = %q, want %q", tt.payload, got, tt.want)
			}
		})
	}
}

// The helper hashes the bytes exactly as stored: it is the single definition
// shared by producers and by validation, so whitespace that a producer emitted
// is part of what is attested.
func TestHashPayloadHashesBytesAsStored(t *testing.T) {
	t.Parallel()

	compact := event.HashPayload(json.RawMessage(`{"symbol":"AAPL"}`))
	spaced := event.HashPayload(json.RawMessage(`{"symbol": "AAPL"}`))
	if compact == spaced {
		t.Fatal("HashPayload() ignored byte-level differences; it must hash the payload as stored")
	}
}

// An envelope built with the helper validates, which is what lets a producer
// stamp integrity without duplicating the hash definition.
func TestHashPayloadProducesAValidatingEnvelope(t *testing.T) {
	t.Parallel()

	envelope := validEnvelope()
	envelope.Payload = json.RawMessage(`{"symbol":"MSFT","quantity":100}`)
	envelope.PayloadHash = event.HashPayload(envelope.Payload)

	if err := envelope.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
