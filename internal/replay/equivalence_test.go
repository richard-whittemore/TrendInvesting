package replay_test

import (
	"encoding/json"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// decisionEnvelope is a minimal valid decision envelope, identified by id and
// carrying payload so two decisions with different content compare unequal.
func decisionEnvelope(id, payload string) event.Envelope {
	raw := json.RawMessage(payload)
	return event.Envelope{
		ID:                id,
		Type:              "test.decision",
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         envelope(1).EventTime,
		RecordedAt:        envelope(1).RecordedAt,
		Sequence:          1,
		Source:            "reducer",
		StrategyVersion:   "test-strategy-1.0.0",
		ConfigurationHash: "cfg-test",
		PayloadHash:       event.HashPayload(raw),
		Payload:           raw,
	}
}

// TestEquivalentReportsNoDivergenceForIdenticalStreams: the baseline case,
// with two independently built but content-identical envelopes — Equivalent
// must not report a divergence just because the two Go values are distinct
// allocations.
func TestEquivalentReportsNoDivergenceForIdenticalStreams(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{decisionEnvelope("d-1", `{"a":1}`), decisionEnvelope("d-2", `{"a":2}`)}
	got := []event.Envelope{decisionEnvelope("d-1", `{"a":1}`), decisionEnvelope("d-2", `{"a":2}`)}

	if d := replay.Equivalent(want, got); d != nil {
		t.Fatalf("Equivalent() = %+v, want nil", d)
	}
}

// TestEquivalentReportsNoDivergenceForBothEmpty: two empty streams are
// trivially equivalent.
func TestEquivalentReportsNoDivergenceForBothEmpty(t *testing.T) {
	t.Parallel()

	if d := replay.Equivalent(nil, nil); d != nil {
		t.Fatalf("Equivalent(nil, nil) = %+v, want nil", d)
	}
}

// TestEquivalentFindsTheFirstContentDivergence: a mismatch at index 1, with
// index 0 identical, is reported at index 1 and names both envelopes.
func TestEquivalentFindsTheFirstContentDivergence(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{decisionEnvelope("d-1", `{"a":1}`), decisionEnvelope("d-2", `{"a":2}`)}
	got := []event.Envelope{decisionEnvelope("d-1", `{"a":1}`), decisionEnvelope("d-2", `{"a":999}`)}

	d := replay.Equivalent(want, got)
	if d == nil {
		t.Fatal("Equivalent() = nil, want a divergence")
	}
	if d.Index != 1 {
		t.Fatalf("Index = %d, want 1", d.Index)
	}
	if d.Want == nil || d.Got == nil {
		t.Fatalf("Want/Got = %v/%v, want both non-nil for a content mismatch", d.Want, d.Got)
	}
	if d.Want.ID != "d-2" || d.Got.ID != "d-2" {
		t.Fatalf("Want.ID/Got.ID = %q/%q, want both %q", d.Want.ID, d.Got.ID, "d-2")
	}
}

// TestEquivalentReportsGotLongerThanWant: got holds an extra decision beyond
// what was recorded — reported with Want nil, since nothing at that index
// was expected.
func TestEquivalentReportsGotLongerThanWant(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{decisionEnvelope("d-1", `{"a":1}`)}
	got := []event.Envelope{decisionEnvelope("d-1", `{"a":1}`), decisionEnvelope("d-2", `{"a":2}`)}

	d := replay.Equivalent(want, got)
	if d == nil {
		t.Fatal("Equivalent() = nil, want a divergence")
	}
	if d.Index != 1 {
		t.Fatalf("Index = %d, want 1", d.Index)
	}
	if d.Want != nil {
		t.Fatalf("Want = %+v, want nil (nothing recorded at this index)", d.Want)
	}
	if d.Got == nil || d.Got.ID != "d-2" {
		t.Fatalf("Got = %+v, want the extra decision d-2", d.Got)
	}
}

// TestEquivalentReportsWantLongerThanGot: want held a decision the replay
// never produced — reported with Got nil.
func TestEquivalentReportsWantLongerThanGot(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{decisionEnvelope("d-1", `{"a":1}`), decisionEnvelope("d-2", `{"a":2}`)}
	got := []event.Envelope{decisionEnvelope("d-1", `{"a":1}`)}

	d := replay.Equivalent(want, got)
	if d == nil {
		t.Fatal("Equivalent() = nil, want a divergence")
	}
	if d.Index != 1 {
		t.Fatalf("Index = %d, want 1", d.Index)
	}
	if d.Got != nil {
		t.Fatalf("Got = %+v, want nil (the replay produced nothing here)", d.Got)
	}
	if d.Want == nil || d.Want.ID != "d-2" {
		t.Fatalf("Want = %+v, want the missing decision d-2", d.Want)
	}
}
