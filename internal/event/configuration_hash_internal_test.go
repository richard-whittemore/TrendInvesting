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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
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
	if !bytes.Equal(gotA, gotB) {
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
	if !bytes.Equal(gotFirst, gotSecond) {
		t.Fatalf("canonicalJSON differs by map insertion order:\n  first:  %s\n  second: %s", gotFirst, gotSecond)
	}
	const want = `{"alpha":2,"mu":3,"zeta":1}`
	if string(gotFirst) != want {
		t.Fatalf("canonicalJSON(map) = %s, want %s (keys sorted lexicographically)", gotFirst, want)
	}
}

type withHiddenFields struct {
	Visible    string `json:"visible"`
	Hidden     string `json:"-"`
	unexported string
	NoTag      bool
}

// TestCanonicalJSONSkipsUnexportedAndDashTaggedFields confirms
// writeCanonicalStruct's documented field selection: an unexported field is
// never visible to canonicalJSON at all (reflection cannot read it), a field
// tagged `json:"-"` is deliberately excluded the same way encoding/json
// excludes it, and a field with no tag falls back to its Go name.
func TestCanonicalJSONSkipsUnexportedAndDashTaggedFields(t *testing.T) {
	t.Parallel()

	v := withHiddenFields{Visible: "x", Hidden: "should not appear", unexported: "also hidden", NoTag: true}
	got := canonicalJSON(v)
	const want = `{"NoTag":true,"visible":"x"}`
	if string(got) != want {
		t.Fatalf("canonicalJSON(withHiddenFields) = %s, want %s", got, want)
	}
}

// TestCanonicalJSONEncodesSlicesInOrderWithoutSorting confirms array
// position, unlike an object's keys, is significant and left exactly as
// given: sorting a slice the way object keys are sorted would silently
// change what the encoded value means.
func TestCanonicalJSONEncodesSlicesInOrderWithoutSorting(t *testing.T) {
	t.Parallel()

	got := canonicalJSON([]any{3.0, "b", true})
	const want = `[3,"b",true]`
	if string(got) != want {
		t.Fatalf("canonicalJSON(slice) = %s, want %s", got, want)
	}
}

// noExportedFields has fields, but none exported: exactly the shape
// time.Time has (wall, ext, loc are all unexported). writeCanonicalStruct
// would otherwise silently encode this as {} regardless of what hidden
// state it carries.
type noExportedFields struct {
	hidden string
}

// TestCanonicalJSONPanicsOnAStructWithNoExportedFields is the fail-closed
// case a struct field added later without an exported representation would
// hit: canonicalJSON must refuse to render it as {}, naming the type, rather
// than silently producing a hash that cannot distinguish two different
// values of that field.
func TestCanonicalJSONPanicsOnAStructWithNoExportedFields(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("canonicalJSON(noExportedFields{}) did not panic, want it to")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "noExportedFields") {
			t.Errorf("panic message = %q, want it to name the type", msg)
		}
	}()
	canonicalJSON(noExportedFields{hidden: "x"})
}

// TestCanonicalJSONPanicsOnTimeTime is the motivating case: a date or
// timestamp field added to ConfigurationPayload in the future (an
// effective-from, a re-basing date, a Regime Window boundary) as a
// time.Time would otherwise encode as {} for every value, making every
// configuration differing only in that field hash identically — precisely
// the failure ConfigurationHash exists to prevent, and invisible: the
// stability test would still pass, and the sensitivity test would only
// catch it if someone remembered to add a row for the new field.
func TestCanonicalJSONPanicsOnTimeTime(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("canonicalJSON(time.Time{}) did not panic, want it to: time.Time has no exported fields")
		}
	}()
	canonicalJSON(time.Now())
}

func TestCanonicalJSONEncodesUnsignedIntegers(t *testing.T) {
	t.Parallel()

	type payload struct {
		V uint32 `json:"v"`
	}
	got := canonicalJSON(payload{V: 42})
	const want = `{"v":42}`
	if string(got) != want {
		t.Fatalf("canonicalJSON(uint32) = %s, want %s", got, want)
	}
}

// uintOrderA/uintOrderB carry the same `json` tags, including an unsigned
// integer field, in a different declaration order.
type uintOrderA struct {
	Count uint32 `json:"count"`
	Name  string `json:"name"`
}

type uintOrderB struct {
	Name  string `json:"name"`
	Count uint32 `json:"count"`
}

func TestCanonicalJSONUnsignedIntegersAreIndependentOfFieldOrder(t *testing.T) {
	t.Parallel()

	a := uintOrderA{Count: 7, Name: "x"}
	b := uintOrderB{Name: "x", Count: 7}

	gotA := canonicalJSON(a)
	gotB := canonicalJSON(b)
	if !bytes.Equal(gotA, gotB) {
		t.Fatalf("canonicalJSON differs by field declaration order with an unsigned integer field present:\n  a: %s\n  b: %s", gotA, gotB)
	}
}

func TestCanonicalJSONPointerEncodesAsNullOrPointee(t *testing.T) {
	t.Parallel()

	type payload struct {
		V *float64 `json:"v"`
	}

	got := canonicalJSON(payload{V: nil})
	if want := `{"v":null}`; string(got) != want {
		t.Errorf("canonicalJSON(nil pointer) = %s, want %s", got, want)
	}

	value := 2.5
	got = canonicalJSON(payload{V: &value})
	if want := `{"v":2.5}`; string(got) != want {
		t.Errorf("canonicalJSON(non-nil pointer) = %s, want %s", got, want)
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
