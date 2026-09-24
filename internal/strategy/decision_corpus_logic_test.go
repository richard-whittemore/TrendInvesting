package strategy_test

import (
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// sampleDecisionEnvelope is a minimal, valid-shaped decision envelope for
// exercising decisionHash and canonicalDecisionFields directly, without
// going through stream.run() or a real reducer.
func sampleDecisionEnvelope(id string, sequence uint64, strategyVersion string) event.Envelope {
	return event.Envelope{
		ID:                id,
		Type:              event.CampaignOpenedEventType,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         day(56),
		RecordedAt:        day(56),
		Sequence:          sequence,
		CorrelationID:     "corr-1",
		CausationID:       "cause-1",
		Source:            "reducer",
		StrategyVersion:   strategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       "deadbeef",
		Payload:           json.RawMessage(`{"campaign_id":"c-1"}`),
	}
}

// TestCanonicalDecisionFieldsCoverEveryEnvelopeFieldButStrategyVersion pins
// canonicalDecisionFields' own claim (decision_corpus_test.go) against
// event.CanonicalEnvelopeBytes' actual field set, read back from its own
// canonical JSON rather than restated by hand: the two disagreeing silently
// is exactly the failure mode this test exists to catch (see
// canonicalDecisionFields' doc comment).
func TestCanonicalDecisionFieldsCoverEveryEnvelopeFieldButStrategyVersion(t *testing.T) {
	t.Parallel()

	e := sampleDecisionEnvelope("decision-1", 7, testStrategyVersion)

	var full map[string]json.RawMessage
	if err := json.Unmarshal(event.CanonicalEnvelopeBytes(e), &full); err != nil {
		t.Fatalf("json.Unmarshal(CanonicalEnvelopeBytes) error = %v", err)
	}
	wantKeys := make([]string, 0, len(full)-1)
	for k := range full {
		if k == "strategy_version" {
			continue
		}
		wantKeys = append(wantKeys, k)
	}
	sort.Strings(wantKeys)

	got := canonicalDecisionFields(e)
	gotKeys := make([]string, 0, len(got))
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)

	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("canonicalDecisionFields keys = %v, want exactly CanonicalEnvelopeBytes' fields minus strategy_version = %v", gotKeys, wantKeys)
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("canonicalDecisionFields keys = %v, want exactly CanonicalEnvelopeBytes' fields minus strategy_version = %v", gotKeys, wantKeys)
		}
	}
	if _, ok := got["strategy_version"]; ok {
		t.Fatal("canonicalDecisionFields includes strategy_version; #100 requires it excluded so the hash does not change merely because the version string did")
	}
}

// TestDecisionHashExcludesStrategyVersion is the property #100 asks for by
// name: two runs whose only difference is the StrategyVersion string
// stamped on an otherwise-identical decision hash identically.
func TestDecisionHashExcludesStrategyVersion(t *testing.T) {
	t.Parallel()

	a := []event.Envelope{sampleDecisionEnvelope("decision-1", 1, "turtle-baseline/1.3.0+build-a")}
	b := []event.Envelope{sampleDecisionEnvelope("decision-1", 1, "turtle-baseline/1.3.0+build-b")}

	if got, want := decisionHash(a, nil), decisionHash(b, nil); got != want {
		t.Fatalf("decisionHash differed across StrategyVersion alone: %q vs %q", got, want)
	}
}

// TestDecisionHashDependsOnDecisionContent is decisionHash's converse:
// changing anything ELSE about a decision — here, its payload — must change
// the hash, or the corpus could not catch a rules change that alters what
// is decided.
func TestDecisionHashDependsOnDecisionContent(t *testing.T) {
	t.Parallel()

	a := sampleDecisionEnvelope("decision-1", 1, testStrategyVersion)
	b := a
	b.Payload = json.RawMessage(`{"campaign_id":"c-2"}`)

	if got, want := decisionHash([]event.Envelope{a}, nil), decisionHash([]event.Envelope{b}, nil); got == want {
		t.Fatal("decisionHash was unchanged by a changed payload")
	}
}

// TestDecisionHashDependsOnEmissionOrder: the corpus is asked to hash "in
// emission order" (#100), which only means something if reordering two
// otherwise-identical decisions changes the result.
func TestDecisionHashDependsOnEmissionOrder(t *testing.T) {
	t.Parallel()

	a := sampleDecisionEnvelope("decision-1", 1, testStrategyVersion)
	b := sampleDecisionEnvelope("decision-2", 2, testStrategyVersion)

	forward := decisionHash([]event.Envelope{a, b}, nil)
	backward := decisionHash([]event.Envelope{b, a}, nil)
	if forward == backward {
		t.Fatal("decisionHash did not depend on emission order")
	}
}

// TestDecisionHashIncludesTheRunErrorText: #100 asks the hash to cover "the
// run's error text if any", specifically so a run that used to fail and now
// succeeds (or fails differently) is a corpus change even when the emitted
// decisions — possibly none, on either side — are identical.
func TestDecisionHashIncludesTheRunErrorText(t *testing.T) {
	t.Parallel()

	none := decisionHash(nil, nil)
	failedA := decisionHash(nil, errors.New("predates the last completed bar"))
	failedB := decisionHash(nil, errors.New("unknown or already closed"))

	if none == failedA {
		t.Fatal("decisionHash did not distinguish a successful empty run from a failed one")
	}
	if failedA == failedB {
		t.Fatal("decisionHash did not distinguish two different error texts")
	}
}

// TestDecisionHashIsDeterministic: the corpus only means something if
// hashing the same run twice gives the same answer, independent of Go's map
// iteration order — canonicalDecisionFields returns a map, and
// event.CanonicalBytes sorting its keys is what makes this hold.
func TestDecisionHashIsDeterministic(t *testing.T) {
	t.Parallel()

	envelopes := []event.Envelope{
		sampleDecisionEnvelope("decision-1", 1, testStrategyVersion),
		sampleDecisionEnvelope("decision-2", 2, testStrategyVersion),
	}
	first := decisionHash(envelopes, nil)
	for i := 0; i < 5; i++ {
		if got := decisionHash(envelopes, nil); got != first {
			t.Fatalf("decisionHash was not deterministic: got %q, want %q", got, first)
		}
	}
}
