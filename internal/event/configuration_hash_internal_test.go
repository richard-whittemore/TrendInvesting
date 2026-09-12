package event

// This file is deliberately `package event`, not `event_test`: it exercises
// canonicalJSON directly, the unexported helper behind the single exported
// function this ticket adds (ConfigurationHash). #50's "one exported
// function" constrains the derivation's public surface, not what a
// white-box test may reach to prove the canonical encoder's own properties —
// that its object encoding does not depend on Go struct field declaration
// order or map insertion order, and that the schema version genuinely
// participates in the hashed bytes rather than merely being documented as if
// it did (a black-box test cannot show this: ConfigurationSchemaVersion is a
// build-time constant, not something a test can vary). This mirrors
// internal/strategy/invariant_test.go's precedent for the same reason: a
// white-box test reaching an unexported seam because the property under test
// is not observable from the exported API alone.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// fieldOrderA/fieldOrderB carry the same `json` tags in a different
// declaration order, so canonicalJSON's sorted-key output can be checked not
// to depend on Go's struct field order — unlike encoding/json.Marshal, which
// preserves declaration order verbatim.
type fieldOrderA struct {
	Alpha float64 `json:"alpha"`
	Beta  string  `json:"beta"`
	Gamma bool    `json:"gamma"`
}

type fieldOrderB struct {
	Gamma bool    `json:"gamma"`
	Beta  string  `json:"beta"`
	Alpha float64 `json:"alpha"`
}

func TestCanonicalJSONIsIndependentOfStructFieldDeclarationOrder(t *testing.T) {
	t.Parallel()

	a := fieldOrderA{Alpha: 1.5, Beta: "b", Gamma: true}
	b := fieldOrderB{Gamma: true, Beta: "b", Alpha: 1.5}

	gotA := canonicalJSON(a)
	gotB := canonicalJSON(b)
	if string(gotA) != string(gotB) {
		t.Fatalf("canonicalJSON differs by struct field declaration order:\n  a: %s\n  b: %s", gotA, gotB)
	}
}

func TestCanonicalJSONIsIndependentOfMapInsertionOrder(t *testing.T) {
	t.Parallel()

	first := map[string]any{}
	first["zeta"] = 1.0
	first["alpha"] = 2.0
	first["mu"] = 3.0

	second := map[string]any{}
	second["mu"] = 3.0
	second["alpha"] = 2.0
	second["zeta"] = 1.0

	gotFirst := canonicalJSON(first)
	gotSecond := canonicalJSON(second)
	if string(gotFirst) != string(gotSecond) {
		t.Fatalf("canonicalJSON differs by map insertion order:\n  first:  %s\n  second: %s", gotFirst, gotSecond)
	}
	const want = `{"alpha":2,"mu":3,"zeta":1}`
	if string(gotFirst) != want {
		t.Fatalf("canonicalJSON(map) = %s, want %s (keys sorted lexicographically)", gotFirst, want)
	}
}

func TestCanonicalJSONRecursesIntoNestedObjects(t *testing.T) {
	t.Parallel()

	type inner struct {
		Z float64 `json:"z"`
		A float64 `json:"a"`
	}
	type outer struct {
		Name  string `json:"name"`
		Inner inner  `json:"inner"`
	}

	got := canonicalJSON(outer{Name: "x", Inner: inner{Z: 2, A: 1}})
	const want = `{"inner":{"a":1,"z":2},"name":"x"}`
	if string(got) != want {
		t.Fatalf("canonicalJSON(nested) = %s, want %s", got, want)
	}
}

func TestCanonicalJSONFormatsFloatsWithShortestRoundTrip(t *testing.T) {
	t.Parallel()

	type payload struct {
		V float64 `json:"v"`
	}
	tests := []struct {
		value float64
		want  string
	}{
		{0.005, `{"v":0.005}`},
		{2.0, `{"v":2}`},
		{55, `{"v":55}`},
	}
	for _, tt := range tests {
		got := canonicalJSON(payload{V: tt.value})
		if string(got) != tt.want {
			t.Errorf("canonicalJSON(%v) = %s, want %s", tt.value, got, tt.want)
		}
	}
}

// TestConfigurationHashSchemaVersionParticipatesInTheHashedBytes proves the
// spec's central claim directly: the schema version is INSIDE the hashed
// bytes, not stored beside them, so changing it changes the hash even when
// canonicalJSON(payload) is held fixed. ConfigurationSchemaVersion cannot be
// bumped from a test (it is a build-time constant, deliberately — see its
// own doc comment), so this recomputes the hash formula by hand for the
// current version and for one adjacent version, and checks the current one
// agrees with ConfigurationHash's own (black-box-testable) output while the
// adjacent one does not.
func TestConfigurationHashSchemaVersionParticipatesInTheHashedBytes(t *testing.T) {
	t.Parallel()

	payload := ConfigurationPayload{
		StrategyID:             "turtle-baseline",
		SizingMode:             SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2.0,
		EntryChannelLength:     55,
		ExitChannelLength:      20,
		MaxUnits:               4,
		SlippageN:              0.05,
		TierBDistanceInN:       1.0,
		DollarsPerPoint:        1,
		RiskAtStopFraction:     0,
		NotionalAccount: NotionalAccountConfig{
			StartingEquity: 1_000_000,
			RebasingMonth:  1,
			RebasingDay:    1,
		},
	}

	canonical := canonicalJSON(payload)
	hashFor := func(version uint32) string {
		sum := sha256.Sum256([]byte(fmt.Sprintf("configuration/v%d\n", version) + string(canonical)))
		return "sha256:" + hex.EncodeToString(sum[:])
	}

	current := hashFor(ConfigurationSchemaVersion)
	want := ConfigurationHash(payload)
	if current != want {
		t.Fatalf("hand-computed hash at the current schema version = %q, want it to equal ConfigurationHash() = %q", current, want)
	}
	adjacent := hashFor(ConfigurationSchemaVersion + 1)
	if adjacent == current {
		t.Fatal("hashFor(current+1) == hashFor(current); the schema version must change the hash")
	}
}
