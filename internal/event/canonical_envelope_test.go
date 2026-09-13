package event_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// canonicalSubject is the envelope the canonical-bytes tests vary one field
// at a time from.
func canonicalSubject() event.Envelope {
	payload := json.RawMessage(`{"b":2,"a":1}`)
	return event.Envelope{
		ID:                "decision:AAPL:2026-02-27",
		Type:              event.SetupEvaluatedEventType,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     event.SetupEvaluatedSchemaVersion,
		EventTime:         time.Date(2026, time.February, 27, 0, 0, 0, 0, time.UTC),
		RecordedAt:        time.Date(2026, time.February, 27, 1, 2, 3, 0, time.UTC),
		Sequence:          7,
		CorrelationID:     "bar:AAPL:2026-02-27",
		CausationID:       "bar:AAPL:2026-02-27",
		Source:            "reducer",
		StrategyVersion:   "turtle-baseline/1.1.0+dev",
		ConfigurationHash: "sha256:abc",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// TestCanonicalBytesIsIndependentOfInsertionOrder: the exported encoder a
// journal canonicalises its header with is the same one the envelope uses,
// so a project that must not have two definitions of "the canonical bytes
// of a value" does not acquire one.
func TestCanonicalBytesIsIndependentOfInsertionOrder(t *testing.T) {
	t.Parallel()

	first := event.CanonicalBytes(map[string]any{"b": 2, "a": "x", "c": 1.5})
	second := event.CanonicalBytes(map[string]any{"c": 1.5, "a": "x", "b": 2})
	if !bytes.Equal(first, second) {
		t.Fatalf("canonical bytes depend on insertion order:\n %s\n %s", first, second)
	}
	if want := `{"a":"x","b":2,"c":1.5}`; string(first) != want {
		t.Fatalf("CanonicalBytes = %s, want %s", first, want)
	}
}

func TestCanonicalEnvelopeBytesIsStableForTheSameEnvelope(t *testing.T) {
	t.Parallel()

	first := event.CanonicalEnvelopeBytes(canonicalSubject())
	second := event.CanonicalEnvelopeBytes(canonicalSubject())
	if !bytes.Equal(first, second) {
		t.Fatalf("canonical bytes differ between two encodings of the same envelope:\n %s\n %s", first, second)
	}
	if len(first) == 0 {
		t.Fatal("canonical bytes are empty")
	}
}

// TestCanonicalEnvelopeBytesSurvivesAJSONRoundTrip is the property a journal
// verifier depends on: the bytes hashed when the record was written must be
// the bytes recomputed after the record is read back from the file.
func TestCanonicalEnvelopeBytesSurvivesAJSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := canonicalSubject()
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded event.Envelope
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !bytes.Equal(event.CanonicalEnvelopeBytes(original), event.CanonicalEnvelopeBytes(decoded)) {
		t.Fatalf("canonical bytes changed across a JSON round trip:\n before %s\n after  %s",
			event.CanonicalEnvelopeBytes(original), event.CanonicalEnvelopeBytes(decoded))
	}
}

// TestCanonicalEnvelopeBytesIgnoresTheZoneAnInstantIsStatedIn: a time read
// back from a journal is UTC, and an in-memory one need not be. Two envelopes
// naming the same instant must hash the same, or every journal written by a
// process in a non-UTC zone would fail its own verification.
func TestCanonicalEnvelopeBytesIgnoresTheZoneAnInstantIsStatedIn(t *testing.T) {
	t.Parallel()

	utc := canonicalSubject()
	elsewhere := canonicalSubject()
	elsewhere.EventTime = elsewhere.EventTime.In(time.FixedZone("UTC-5", -5*60*60))
	elsewhere.RecordedAt = elsewhere.RecordedAt.In(time.FixedZone("UTC+9", 9*60*60))

	if !bytes.Equal(event.CanonicalEnvelopeBytes(utc), event.CanonicalEnvelopeBytes(elsewhere)) {
		t.Fatalf("the same instant canonicalised differently in another zone:\n %s\n %s",
			event.CanonicalEnvelopeBytes(utc), event.CanonicalEnvelopeBytes(elsewhere))
	}
}

// TestCanonicalEnvelopeBytesChangesWithEveryField: a chain over these bytes
// is only tamper-evidence for the fields they cover, so every field of the
// envelope must be covered.
func TestCanonicalEnvelopeBytesChangesWithEveryField(t *testing.T) {
	t.Parallel()

	base := event.CanonicalEnvelopeBytes(canonicalSubject())

	tests := []struct {
		field  string
		mutate func(*event.Envelope)
	}{
		{"id", func(e *event.Envelope) { e.ID = "other" }},
		{"type", func(e *event.Envelope) { e.Type = event.SignalEventType }},
		{"envelope version", func(e *event.Envelope) { e.EnvelopeVersion = 2 }},
		{"schema version", func(e *event.Envelope) { e.SchemaVersion = 9 }},
		{"event time", func(e *event.Envelope) { e.EventTime = e.EventTime.Add(time.Nanosecond) }},
		{"recorded at", func(e *event.Envelope) { e.RecordedAt = e.RecordedAt.Add(time.Nanosecond) }},
		{"sequence", func(e *event.Envelope) { e.Sequence = 8 }},
		{"correlation id", func(e *event.Envelope) { e.CorrelationID = "other" }},
		{"causation id", func(e *event.Envelope) { e.CausationID = "other" }},
		{"source", func(e *event.Envelope) { e.Source = "simulator" }},
		{"strategy version", func(e *event.Envelope) { e.StrategyVersion = "turtle-baseline/1.2.0+dev" }},
		{"configuration hash", func(e *event.Envelope) { e.ConfigurationHash = "sha256:def" }},
		{"payload hash", func(e *event.Envelope) { e.PayloadHash = "0000" }},
		{"payload", func(e *event.Envelope) { e.Payload = json.RawMessage(`{"b":2,"a":2}`) }},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()
			altered := canonicalSubject()
			tt.mutate(&altered)
			if bytes.Equal(base, event.CanonicalEnvelopeBytes(altered)) {
				t.Fatalf("changing %s left the canonical bytes unchanged: %s", tt.field, base)
			}
		})
	}
}

// TestCanonicalEnvelopeBytesCarriesThePayloadBytesAsStored: the payload is
// attested by PayloadHash over the bytes exactly as stored (see
// event.HashPayload), so the chain must see those same bytes rather than a
// re-encoding of them.
func TestCanonicalEnvelopeBytesCarriesThePayloadBytesAsStored(t *testing.T) {
	t.Parallel()

	spaced := canonicalSubject()
	spaced.Payload = json.RawMessage(`{"a": 1, "b": 2}`)
	spaced.PayloadHash = event.HashPayload(spaced.Payload)

	compact := canonicalSubject()
	compact.Payload = json.RawMessage(`{"a":1,"b":2}`)
	compact.PayloadHash = event.HashPayload(compact.Payload)

	if bytes.Equal(event.CanonicalEnvelopeBytes(spaced), event.CanonicalEnvelopeBytes(compact)) {
		t.Fatal("two different payload byte strings canonicalised identically; the chain would not attest the bytes PayloadHash attests")
	}
}
