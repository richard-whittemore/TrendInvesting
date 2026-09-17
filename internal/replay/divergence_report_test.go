package replay_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// fieldEnvelope builds a decision envelope at sequence, carrying an
// arbitrary payload — a superset of decisionEnvelope (equivalence_test.go)
// that lets a caller name its own sequence, so field-path tests can build
// several envelopes that would otherwise collide on the fixed sequence 1
// decisionEnvelope always uses.
func fieldEnvelope(id string, sequence uint64, payload string) event.Envelope {
	e := decisionEnvelope(id, payload)
	e.Sequence = sequence
	return e
}

// TestDiffReportsNoDivergenceForIdenticalStreams: the baseline case for the
// reporter, mirroring TestEquivalentReportsNoDivergenceForIdenticalStreams
// one layer up.
func TestDiffReportsNoDivergenceForIdenticalStreams(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`), fieldEnvelope("d-2", 2, `{"a":2}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`), fieldEnvelope("d-2", 2, `{"a":2}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report != nil {
		t.Fatalf("Diff() = %+v, want nil", report)
	}
}

// TestDiffReportsExactSequenceIDTypeAndNestedFieldPath is the ticket's
// second required case: differing in one field of one event reports that
// event's sequence, id, type, and the exact field path with both values.
// The payload nests the differing field two levels deep — under
// "protective_stop", the ticket's own example — so a reporter that only
// ever compared whole envelopes, or only ever looked one level into the
// payload, would fail this: it has to actually descend.
func TestDiffReportsExactSequenceIDTypeAndNestedFieldPath(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{
		fieldEnvelope("d-1", 1, `{"a":1}`),
		fieldEnvelope("d-2", 2, `{"protective_stop":{"level":93.5,"unit_index":1}}`),
	}
	got := []event.Envelope{
		fieldEnvelope("d-1", 1, `{"a":1}`),
		fieldEnvelope("d-2", 2, `{"protective_stop":{"level":94.75,"unit_index":1}}`),
	}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	if report.Sequence != 2 {
		t.Errorf("Sequence = %d, want 2", report.Sequence)
	}
	if report.EventID != "d-2" {
		t.Errorf("EventID = %q, want %q", report.EventID, "d-2")
	}
	if report.EventType != "test.decision" {
		t.Errorf("EventType = %q, want %q", report.EventType, "test.decision")
	}
	if report.FieldPath != "payload.protective_stop.level" {
		t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.protective_stop.level")
	}
	if report.Want != 93.5 || report.Got != 94.75 {
		t.Fatalf("Want/Got = %v/%v, want 93.5/94.75", report.Want, report.Got)
	}
	if report.EndedStream != "" {
		t.Fatalf("EndedStream = %q, want empty for a field difference", report.EndedStream)
	}
}

// TestDiffReportsTopLevelFieldDivergence: the differing field need not be in
// the payload at all — recorded_at is the ticket's own example of a
// top-level field.
func TestDiffReportsTopLevelFieldDivergence(t *testing.T) {
	t.Parallel()

	w := fieldEnvelope("d-1", 1, `{"a":1}`)
	g := w
	g.RecordedAt = w.RecordedAt.Add(1)

	report, err := replay.Diff([]event.Envelope{w}, []event.Envelope{g})
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	if report.FieldPath != "recorded_at" {
		t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "recorded_at")
	}
}

// TestDiffReportsArrayIndexInPath: the ticket's own instruction on arrays —
// index in the path. Three elements, differing only at index 2, so a
// reporter that hardcoded index 0 or matched elements by content rather than
// position would fail this.
func TestDiffReportsArrayIndexInPath(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"units":[{"qty":100},{"qty":100},{"qty":100}]}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"units":[{"qty":100},{"qty":100},{"qty":250}]}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	if report.FieldPath != "payload.units[2].qty" {
		t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.units[2].qty")
	}
	if report.Want != 100.0 || report.Got != 250.0 {
		t.Fatalf("Want/Got = %v/%v, want 100/250", report.Want, report.Got)
	}
}

// TestDiffReportsAFieldPresentOnOneSideOnly: the ticket's own instruction on
// a field present on one side only. Both directions are asserted: a key
// want has that got lacks, and the reverse.
func TestDiffReportsAFieldPresentOnOneSideOnly(t *testing.T) {
	t.Parallel()

	t.Run("missing from got", func(t *testing.T) {
		t.Parallel()
		want := []event.Envelope{fieldEnvelope("d-1", 1, `{"level":1,"note":"only on want"}`)}
		got := []event.Envelope{fieldEnvelope("d-1", 1, `{"level":1}`)}

		report, err := replay.Diff(want, got)
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if report == nil {
			t.Fatal("Diff() = nil, want a report")
		}
		if report.FieldPath != "payload.note" {
			t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.note")
		}
		if report.Want != "only on want" {
			t.Fatalf("Want = %v, want %q", report.Want, "only on want")
		}
		if got := fmt.Sprintf("%v", report.Got); got != "<absent>" {
			t.Fatalf("Got = %#v (%q), want the absent-field marker", report.Got, got)
		}
	})

	t.Run("missing from want", func(t *testing.T) {
		t.Parallel()
		want := []event.Envelope{fieldEnvelope("d-1", 1, `{"level":1}`)}
		got := []event.Envelope{fieldEnvelope("d-1", 1, `{"level":1,"note":"only on got"}`)}

		report, err := replay.Diff(want, got)
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if report == nil {
			t.Fatal("Diff() = nil, want a report")
		}
		if report.FieldPath != "payload.note" {
			t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.note")
		}
		if report.Got != "only on got" {
			t.Fatalf("Got = %v, want %q", report.Got, "only on got")
		}
		if want := fmt.Sprintf("%v", report.Want); want != "<absent>" {
			t.Fatalf("Want = %#v (%q), want the absent-field marker", report.Want, want)
		}
	})
}

// TestDiffReportsWhereTheShorterStreamEnds is the ticket's third required
// case, both directions: got longer than want, and want longer than got.
// Divergence already models this with a nil Want or Got (#20); this test
// asserts Report reuses that rather than attempting a field diff on it.
func TestDiffReportsWhereTheShorterStreamEnds(t *testing.T) {
	t.Parallel()

	t.Run("got is longer", func(t *testing.T) {
		t.Parallel()
		want := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`)}
		got := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`), fieldEnvelope("d-2", 2, `{"a":2}`)}

		report, err := replay.Diff(want, got)
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if report == nil {
			t.Fatal("Diff() = nil, want a report")
		}
		if report.EndedStream != "want" {
			t.Fatalf("EndedStream = %q, want %q", report.EndedStream, "want")
		}
		if report.Sequence != 2 || report.EventID != "d-2" {
			t.Fatalf("Sequence/EventID = %d/%q, want 2/%q (got's extra event)", report.Sequence, report.EventID, "d-2")
		}
		if report.FieldPath != "" {
			t.Fatalf("FieldPath = %q, want empty for a length mismatch", report.FieldPath)
		}
	})

	t.Run("want is longer", func(t *testing.T) {
		t.Parallel()
		want := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`), fieldEnvelope("d-2", 2, `{"a":2}`)}
		got := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`)}

		report, err := replay.Diff(want, got)
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if report == nil {
			t.Fatal("Diff() = nil, want a report")
		}
		if report.EndedStream != "got" {
			t.Fatalf("EndedStream = %q, want %q", report.EndedStream, "got")
		}
		if report.Sequence != 2 || report.EventID != "d-2" {
			t.Fatalf("Sequence/EventID = %d/%q, want 2/%q (want's un-replayed event)", report.Sequence, report.EventID, "d-2")
		}
	})
}

// TestDiffReportsTheFirstDisagreeingSequenceWhenOrderDiffers is the ticket's
// fourth required case: the same two events, reordered, must report the
// first sequence at which the streams disagree, not silently match d-1 and
// d-2 up by content across positions.
func TestDiffReportsTheFirstDisagreeingSequenceWhenOrderDiffers(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`), fieldEnvelope("d-2", 2, `{"a":2}`)}
	got := []event.Envelope{fieldEnvelope("d-2", 1, `{"a":2}`), fieldEnvelope("d-1", 2, `{"a":1}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	if report.Sequence != 1 {
		t.Fatalf("Sequence = %d, want 1 (the FIRST disagreeing position)", report.Sequence)
	}
	if report.FieldPath != "id" {
		t.Fatalf("FieldPath = %q, want %q (the envelopes at position 0 differ by id)", report.FieldPath, "id")
	}
	if report.Want != "d-1" || report.Got != "d-2" {
		t.Fatalf("Want/Got = %v/%v, want d-1/d-2", report.Want, report.Got)
	}
}

// TestDiffReportsAOneULPFloatDivergenceDistinguishably is the ticket's
// single most important case: 118.2875 and 118.28750000000001 are one bit
// apart (verified below) and must both survive rendering distinguishably —
// in the FieldDivergence value itself, in the human-readable String(), and
// in the machine-readable MarshalJSON() output. A reporter that rendered
// floats with, say, "%.4f" or a fixed-precision %g would print "118.2875"
// for both and hide the exact defect class (a fused multiply-add differing
// in the last bits across architectures, docs/development.md) this ticket
// exists to catch.
func TestDiffReportsAOneULPFloatDivergenceDistinguishably(t *testing.T) {
	t.Parallel()

	const wantLevel = "118.2875"
	const gotLevel = "118.28750000000001"
	if wantLevel == gotLevel {
		t.Fatal("test setup: the two literals are identical strings, this test would prove nothing")
	}

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"protective_stop":{"level":`+wantLevel+`}}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"protective_stop":{"level":`+gotLevel+`}}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report: 118.2875 and 118.28750000000001 are different float64 values")
	}
	if report.FieldPath != "payload.protective_stop.level" {
		t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.protective_stop.level")
	}
	wantFloat, _ := report.Want.(float64)
	gotFloat, _ := report.Got.(float64)
	if wantFloat == gotFloat {
		t.Fatalf("Want == Got == %v: the divergence was rounded away before it reached the report", wantFloat)
	}

	// The human-readable line must render the two values distinguishably.
	line := report.String()
	if !strings.Contains(line, wantLevel) || !strings.Contains(line, gotLevel) {
		t.Fatalf("String() = %q, want both %q and %q to appear distinguishably", line, wantLevel, gotLevel)
	}

	// The machine-readable form must too — CI reads this one.
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	if !strings.Contains(string(encoded), wantLevel) || !strings.Contains(string(encoded), gotLevel) {
		t.Fatalf("MarshalJSON = %s, want both %q and %q to appear distinguishably", encoded, wantLevel, gotLevel)
	}
}

// TestDiffDistinguishesIntegersFloat64WouldCollapse is the integer
// counterpart to the ULP test above: 9007199254740992 (2^53) and
// 9007199254740993 (2^53+1) are different valid JSON integers, but the
// second is not exactly representable as a float64 and rounds down to the
// first — verified below via strconv.ParseFloat, the exact conversion a
// plain json.Unmarshal into `any` performs. A reporter that decoded payload
// numbers as float64 would see these as equal and fall all the way back to
// reporting the whole payload rather than naming "quantity", which is
// strictly worse than not reporting at all: it turns "I could not tell"
// into "there is no difference". envelopeTree decodes with UseNumber
// instead, so the field walk compares the literal digits and never makes
// that mistake.
func TestDiffDistinguishesIntegersFloat64WouldCollapse(t *testing.T) {
	t.Parallel()

	const wantQuantity = "9007199254740992"
	const gotQuantity = "9007199254740993"
	wantAsFloat, err := strconv.ParseFloat(wantQuantity, 64)
	if err != nil {
		t.Fatalf("strconv.ParseFloat(%q) error = %v", wantQuantity, err)
	}
	gotAsFloat, err := strconv.ParseFloat(gotQuantity, 64)
	if err != nil {
		t.Fatalf("strconv.ParseFloat(%q) error = %v", gotQuantity, err)
	}
	if wantAsFloat != gotAsFloat {
		t.Fatalf("test setup: %s and %s do not collide as float64 (%v vs %v); this test would prove nothing", wantQuantity, gotQuantity, wantAsFloat, gotAsFloat)
	}

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"quantity":`+wantQuantity+`}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"quantity":`+gotQuantity+`}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report: 9007199254740992 and 9007199254740993 are different integers")
	}
	if report.FieldPath != "payload.quantity" {
		t.Fatalf("FieldPath = %q, want %q (a reporter that collapsed the two through float64 would fall back to %q instead)", report.FieldPath, "payload.quantity", "payload")
	}

	wantNumber, ok := report.Want.(json.Number)
	if !ok {
		t.Fatalf("Want = %#v, want a json.Number", report.Want)
	}
	gotNumber, ok := report.Got.(json.Number)
	if !ok {
		t.Fatalf("Got = %#v, want a json.Number", report.Got)
	}
	if wantNumber.String() != wantQuantity || gotNumber.String() != gotQuantity {
		t.Fatalf("Want/Got = %s/%s, want the exact literals %s/%s untouched", wantNumber, gotNumber, wantQuantity, gotQuantity)
	}

	line := report.String()
	if !strings.Contains(line, wantQuantity) || !strings.Contains(line, gotQuantity) {
		t.Fatalf("String() = %q, want both %q and %q to appear distinguishably", line, wantQuantity, gotQuantity)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	if !strings.Contains(string(encoded), wantQuantity) || !strings.Contains(string(encoded), gotQuantity) {
		t.Fatalf("MarshalJSON = %s, want both %q and %q to appear distinguishably", encoded, wantQuantity, gotQuantity)
	}
}

// TestDiffTreatsIdenticalHugeIntegersAsEqualAndKeepsWalking: a bare integer
// literal outside int64's own range (so numberIsFloat64Safe cannot even ask
// float64 about it) is compared by its literal digits, the same as any
// other number — identical on both sides here — so it must not itself be
// reported as a divergence, and the walk must continue past it to the field
// that actually differs.
func TestDiffTreatsIdenticalHugeIntegersAsEqualAndKeepsWalking(t *testing.T) {
	t.Parallel()

	const beyondInt64 = "99999999999999999999" // 20 nines; math.MaxInt64 has 19 digits
	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"a_quantity":`+beyondInt64+`,"z_other":1}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"a_quantity":`+beyondInt64+`,"z_other":2}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report: z_other differs")
	}
	if report.FieldPath != "payload.z_other" {
		t.Fatalf("FieldPath = %q, want %q (the identical a_quantity must compare equal and be walked past)", report.FieldPath, "payload.z_other")
	}
	if report.Want != 1.0 || report.Got != 2.0 {
		t.Fatalf("Want/Got = %v/%v, want 1/2", report.Want, report.Got)
	}
}

// TestDiffDistinguishesDecimalLiteralsFloat64WouldCollapse is the decimal
// counterpart to TestDiffDistinguishesIntegersFloat64WouldCollapse:
// 9007199254740992.0 and 9007199254740993.0 are different valid JSON
// literals, both within float64's exponent range, but the second is not
// exactly representable and rounds to the first. Round 1 fixed this for
// the bare-integer spelling of the same defect but treated every literal
// containing "." or "e"/"E" as automatically float64-safe on the strength
// of its shape alone, which this decimal spelling exposes as false: shape
// was never the right test, a round trip through float64 is.
func TestDiffDistinguishesDecimalLiteralsFloat64WouldCollapse(t *testing.T) {
	t.Parallel()

	const wantQuantity = "9007199254740992.0"
	const gotQuantity = "9007199254740993.0"
	wantAsFloat, err := strconv.ParseFloat(wantQuantity, 64)
	if err != nil {
		t.Fatalf("strconv.ParseFloat(%q) error = %v", wantQuantity, err)
	}
	gotAsFloat, err := strconv.ParseFloat(gotQuantity, 64)
	if err != nil {
		t.Fatalf("strconv.ParseFloat(%q) error = %v", gotQuantity, err)
	}
	if wantAsFloat != gotAsFloat {
		t.Fatalf("test setup: %s and %s do not collide as float64 (%v vs %v); this test would prove nothing", wantQuantity, gotQuantity, wantAsFloat, gotAsFloat)
	}

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"quantity":`+wantQuantity+`}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"quantity":`+gotQuantity+`}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report: 9007199254740992.0 and 9007199254740993.0 are different decimal literals")
	}
	if report.FieldPath != "payload.quantity" {
		t.Fatalf("FieldPath = %q, want %q (a reporter that collapsed the two through float64 would fall back to %q instead)", report.FieldPath, "payload.quantity", "payload")
	}

	wantNumber, ok := report.Want.(json.Number)
	if !ok {
		t.Fatalf("Want = %#v, want a json.Number", report.Want)
	}
	gotNumber, ok := report.Got.(json.Number)
	if !ok {
		t.Fatalf("Got = %#v, want a json.Number", report.Got)
	}
	if wantNumber.String() != wantQuantity || gotNumber.String() != gotQuantity {
		t.Fatalf("Want/Got = %s/%s, want the exact literals %s/%s untouched", wantNumber, gotNumber, wantQuantity, gotQuantity)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	if !strings.Contains(string(encoded), wantQuantity) || !strings.Contains(string(encoded), gotQuantity) {
		t.Fatalf("MarshalJSON = %s, want both %q and %q to appear distinguishably", encoded, wantQuantity, gotQuantity)
	}
}

// TestDiffTreatsOrdinaryDecimalsAsFloat64 guards against the round-trip
// check in numberIsFloat64Safe overcorrecting into treating every decimal
// literal as unsafe. 118.2875 has no finite binary fraction equal to it —
// no ordinary decimal does, in general — but it round-trips through
// float64 and Go's own shortest formatting back to the same literal, which
// is the property numberIsFloat64Safe actually needs, so it must still be
// compared and reported as float64, not as a json.Number.
func TestDiffTreatsOrdinaryDecimalsAsFloat64(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"level":118.2875}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"level":118.29}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	if _, ok := report.Want.(float64); !ok {
		t.Fatalf("Want = %#v (%T), want a float64", report.Want, report.Want)
	}
	if _, ok := report.Got.(float64); !ok {
		t.Fatalf("Got = %#v (%T), want a float64", report.Got, report.Got)
	}
}

// TestDiffReportsOverflowingExponentLiteralExactly: 1e400 is syntactically
// valid JSON (json.Valid checks syntax, not numeric range) but overflows
// float64 to +Inf. numberIsFloat64Safe's round trip rejects it the same
// way it rejects any other literal float64 cannot carry — Float64() itself
// errors on it — so it is compared and reported as its exact digits, never
// converted, and Report.MarshalJSON never has to encode a non-JSON +Inf.
func TestDiffReportsOverflowingExponentLiteralExactly(t *testing.T) {
	t.Parallel()

	const overflow = "1e400"
	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"level":1}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"extra":`+overflow+`,"level":1}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	if report.FieldPath != "payload.extra" {
		t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.extra")
	}
	gotNumber, ok := report.Got.(json.Number)
	if !ok {
		t.Fatalf("Got = %#v, want a json.Number carrying the literal, not a float64 +Inf", report.Got)
	}
	if gotNumber.String() != overflow {
		t.Fatalf("Got = %s, want the exact literal %s", gotNumber, overflow)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v, want no error: a dropped Float64 error would have let +Inf reach canonicalJSON and fail here", err)
	}
	if !strings.Contains(string(encoded), overflow) {
		t.Fatalf("MarshalJSON = %s, want it to contain the exact literal %s", encoded, overflow)
	}
}

// TestDiffReportsUnderflowingExponentLiteralExactly: 1e-400 underflows
// float64 to 0 without strconv.ParseFloat itself reporting an error — Go
// treats underflow-to-zero as the correctly-rounded result, not a range
// error — so a check that trusted Float64()'s error alone would miss it.
// numberIsFloat64Safe's round trip catches it anyway: formatting 0 back
// out does not reproduce "1e-400", which is the actual failure mode this
// guards, not merely the failure mode of a discarded error.
func TestDiffReportsUnderflowingExponentLiteralExactly(t *testing.T) {
	t.Parallel()

	const underflow = "1e-400"
	if f, err := strconv.ParseFloat(underflow, 64); err != nil || f != 0 {
		t.Fatalf("test setup: strconv.ParseFloat(%q) = %v, %v, want 0, nil (underflow-to-zero without an error)", underflow, f, err)
	}

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"level":1}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"extra":`+underflow+`,"level":1}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	if report.FieldPath != "payload.extra" {
		t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.extra")
	}
	gotNumber, ok := report.Got.(json.Number)
	if !ok {
		t.Fatalf("Got = %#v, want a json.Number carrying the literal %s, not float64 0", report.Got, underflow)
	}
	if gotNumber.String() != underflow {
		t.Fatalf("Got = %s, want the exact literal %s", gotNumber, underflow)
	}
}

// TestDiffRefusesAPayloadWithTrailingDataAfterItsJSONValue: envelopeTree
// decodes with a json.Decoder rather than json.Unmarshal (so it can call
// UseNumber), and a Decoder, unlike Unmarshal, does not by itself refuse
// trailing bytes after the one value it decodes. Field checks for that
// itself, so a payload that is not exactly one JSON value is refused the
// same way Unmarshal would refuse it, rather than being silently narrowed
// to its first value.
//
// "closing brace" and "closing bracket" are the case json.Decoder.More
// itself gets wrong: More reports whether the decoder can read another
// value, and returns false the moment the next non-whitespace byte is a
// bare "}" or "]" — exactly what trails a payload's real value here — so a
// trailing-data guard built on More alone would wave both through as
// having nothing left to read, rather than refusing them.
func TestDiffRefusesAPayloadWithTrailingDataAfterItsJSONValue(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"a second value":  `{"a":1} 2`,
		"closing brace":   `{"a":1}}`,
		"closing bracket": `{"a":1}]`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := []event.Envelope{fieldEnvelope("d-1", 1, payload)}
			got := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`)}
			if _, err := replay.Diff(want, got); err == nil {
				t.Fatal("Diff() error = nil, want the trailing data refused")
			}
		})
	}
}

// TestDiffFallsBackToThePayloadWhenDecodedValuesAreEqual:
// CanonicalEnvelopeBytes hashes the payload's raw bytes, not its decoded
// shape (see that function's own doc comment), so it is possible for
// Equivalent to report a divergence that the decoded field walk cannot
// localise any further — the payload's raw bytes differ (insignificant
// whitespace here) but decode to an equal value, and every other envelope
// field, including PayloadHash, is forced equal so the walk has nothing
// else to find. Field falls back to reporting the payload as a whole, by
// its raw bytes, rather than silently claiming no field differs for an
// envelope Equivalent has already said differs.
func TestDiffFallsBackToThePayloadWhenDecodedValuesAreEqual(t *testing.T) {
	t.Parallel()

	base := fieldEnvelope("d-1", 1, `{"a":1}`)
	w, g := base, base
	w.Payload = json.RawMessage(`{"a":1}`)
	g.Payload = json.RawMessage(`{"a": 1}`)
	// Forced equal so the only remaining difference the field walk could
	// find is the payload's decoded shape, which is equal.
	w.PayloadHash, g.PayloadHash = "forced-equal-hash", "forced-equal-hash"

	report, err := replay.Diff([]event.Envelope{w}, []event.Envelope{g})
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report: the raw payload bytes differ (whitespace), so Equivalent must have found a divergence")
	}
	if report.FieldPath != "payload" {
		t.Fatalf("FieldPath = %q, want the payload fallback %q", report.FieldPath, "payload")
	}
	if report.Want != `{"a":1}` || report.Got != `{"a": 1}` {
		t.Fatalf("Want/Got = %q/%q, want the raw payload bytes of each side", report.Want, report.Got)
	}
}

// TestDiffPicksTheAlphabeticallyFirstDifferingKeyDeterministically guards
// against a reporter that walks a decoded JSON object in Go's (unspecified,
// randomised) map iteration order: with two keys both differing, the
// reported path must always be the lexicographically first one, on every
// run, not whichever happened to be visited first.
func TestDiffPicksTheAlphabeticallyFirstDifferingKeyDeterministically(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"alpha":1,"beta":1,"gamma":1,"delta":1,"epsilon":1}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"alpha":2,"beta":2,"gamma":2,"delta":2,"epsilon":2}`)}

	for i := 0; i < 50; i++ {
		report, err := replay.Diff(want, got)
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if report == nil {
			t.Fatal("Diff() = nil, want a report")
		}
		if report.FieldPath != "payload.alpha" {
			t.Fatalf("iteration %d: FieldPath = %q, want %q (the alphabetically first differing key)", i, report.FieldPath, "payload.alpha")
		}
	}
}

// TestReportStringOnNilIsNoDivergence: a defensive convenience so a caller
// holding a possibly-nil *Report can always call String() without a guard.
func TestReportStringOnNilIsNoDivergence(t *testing.T) {
	t.Parallel()
	var report *replay.Report
	if got := report.String(); got != "no divergence" {
		t.Fatalf("(*Report)(nil).String() = %q, want %q", got, "no divergence")
	}
}

// TestFieldReturnsNilForNilOrLengthMismatchDivergence exercises Field
// directly (it is exported, and Diff's own use of it never reaches these
// guards, since Explain filters a length mismatch before calling Field):
// a nil Divergence, and each direction of a length mismatch, all report
// "nothing to find" rather than attempting to decode a payload that is not
// there.
func TestFieldReturnsNilForNilOrLengthMismatchDivergence(t *testing.T) {
	t.Parallel()

	if field, err := replay.Field(nil); err != nil || field != nil {
		t.Fatalf("Field(nil) = %+v, %v, want nil, nil", field, err)
	}

	w := fieldEnvelope("d-1", 1, `{"a":1}`)
	if field, err := replay.Field(&replay.Divergence{Index: 0, Want: &w, Got: nil}); err != nil || field != nil {
		t.Fatalf("Field(Got nil) = %+v, %v, want nil, nil", field, err)
	}
	if field, err := replay.Field(&replay.Divergence{Index: 0, Want: nil, Got: &w}); err != nil || field != nil {
		t.Fatalf("Field(Want nil) = %+v, %v, want nil, nil", field, err)
	}
}

// TestDiffReturnsAnErrorWhenAPayloadCannotBeDecoded: Field/Diff do not call
// Envelope.Validate, so a structurally-differing envelope whose payload is
// not valid JSON can reach the field-level walk. Both directions are
// asserted, since envelopeTree is called once for each side of a
// Divergence and either call can be the one that fails.
func TestDiffReturnsAnErrorWhenAPayloadCannotBeDecoded(t *testing.T) {
	t.Parallel()

	t.Run("want is undecodable", func(t *testing.T) {
		t.Parallel()
		want := []event.Envelope{fieldEnvelope("d-1", 1, `not valid json`)}
		got := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`)}
		if _, err := replay.Diff(want, got); err == nil {
			t.Fatal("Diff() error = nil, want a decode failure")
		}
	})

	t.Run("got is undecodable", func(t *testing.T) {
		t.Parallel()
		want := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`)}
		got := []event.Envelope{fieldEnvelope("d-1", 1, `not valid json`)}
		if _, err := replay.Diff(want, got); err == nil {
			t.Fatal("Diff() error = nil, want a decode failure")
		}
	})
}

// TestDiffReportsArrayLengthMismatchWithinPayload: an array nested in a
// payload can differ in length as well as in content, in either direction,
// and the element(s) beyond the shorter array are reported as absent rather
// than compared to nothing.
func TestDiffReportsArrayLengthMismatchWithinPayload(t *testing.T) {
	t.Parallel()

	t.Run("got has an extra element", func(t *testing.T) {
		t.Parallel()
		want := []event.Envelope{fieldEnvelope("d-1", 1, `{"units":[1,2]}`)}
		got := []event.Envelope{fieldEnvelope("d-1", 1, `{"units":[1,2,3]}`)}

		report, err := replay.Diff(want, got)
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if report == nil {
			t.Fatal("Diff() = nil, want a report")
		}
		if report.FieldPath != "payload.units[2]" {
			t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.units[2]")
		}
		if fmt.Sprintf("%v", report.Want) != "<absent>" {
			t.Fatalf("Want = %#v, want the absent-field marker", report.Want)
		}
		if report.Got != 3.0 {
			t.Fatalf("Got = %v, want 3", report.Got)
		}
	})

	t.Run("want has an extra element", func(t *testing.T) {
		t.Parallel()
		want := []event.Envelope{fieldEnvelope("d-1", 1, `{"units":[1,2,3]}`)}
		got := []event.Envelope{fieldEnvelope("d-1", 1, `{"units":[1,2]}`)}

		report, err := replay.Diff(want, got)
		if err != nil {
			t.Fatalf("Diff() error = %v", err)
		}
		if report == nil {
			t.Fatal("Diff() = nil, want a report")
		}
		if report.FieldPath != "payload.units[2]" {
			t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.units[2]")
		}
		if report.Want != 3.0 {
			t.Fatalf("Want = %v, want 3", report.Want)
		}
		if fmt.Sprintf("%v", report.Got) != "<absent>" {
			t.Fatalf("Got = %#v, want the absent-field marker", report.Got)
		}
	})
}

// TestDiffSkipsAnEqualArrayAndFindsTheNextDifference: an array that
// compares equal element by element must not itself be reported as
// differing — the walk continues past it to the field that actually
// differs, alphabetically later in the same payload object.
func TestDiffSkipsAnEqualArrayAndFindsTheNextDifference(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"arr":[1,2,3],"z":1}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"arr":[1,2,3],"z":2}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	if report.FieldPath != "payload.z" {
		t.Fatalf("FieldPath = %q, want %q (the equal array must be skipped)", report.FieldPath, "payload.z")
	}
}

// TestReportStringForEndedStream covers String()'s other branch: a length
// mismatch renders as which stream ended, not as a field difference.
func TestReportStringForEndedStream(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"a":1}`), fieldEnvelope("d-2", 2, `{"a":2}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	line := report.String()
	if !strings.Contains(line, "want") || !strings.Contains(line, "d-2") {
		t.Fatalf("String() = %q, want it to name the ended stream and the extra event", line)
	}
}

// TestAbsentFieldRendersDistinctlyInStringAndJSON: a field that does not
// exist at all must be shown as absent in both the human-readable and
// machine-readable reports, via WantAbsent/GotAbsent rather than by
// rendering some particular text into the value itself — see
// TestDiffDistinguishesAnAbsentFieldFromItsOwnMarkerText for why the
// latter breaks the moment a real value happens to equal that text.
func TestAbsentFieldRendersDistinctlyInStringAndJSON(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{"note":"present"}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}

	if !report.GotAbsent {
		t.Fatal("GotAbsent = false, want true: note does not exist on the got side")
	}
	if report.WantAbsent {
		t.Fatal("WantAbsent = true, want false: note is present on the want side")
	}
	if line := report.String(); !strings.Contains(line, "absent") {
		t.Fatalf("String() = %q, want it to say the field is absent", line)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	var decoded struct {
		GotAbsent bool            `json:"got_absent"`
		Got       json.RawMessage `json:"got"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(encoded) error = %v", err)
	}
	if !decoded.GotAbsent {
		t.Fatalf("decoded got_absent = false, want true")
	}
	if decoded.Got != nil {
		t.Fatalf("decoded got = %s, want it omitted: there is nothing to show for a field that does not exist", decoded.Got)
	}
}

// TestDiffDistinguishesAnAbsentFieldFromItsOwnMarkerText: a field can be
// genuinely present with the value "<absent>" — the exact text a reporter
// might otherwise use to render a field that does not exist at all.
// WantAbsent/GotAbsent, not the rendered text, are what a caller must
// check to tell the two apart; encodeValue no longer has a text rendering
// for "missing" to collide with a real value in the first place.
func TestDiffDistinguishesAnAbsentFieldFromItsOwnMarkerText(t *testing.T) {
	t.Parallel()

	want := []event.Envelope{fieldEnvelope("d-1", 1, `{}`)}
	got := []event.Envelope{fieldEnvelope("d-1", 1, `{"x":"<absent>"}`)}

	report, err := replay.Diff(want, got)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if report == nil {
		t.Fatal("Diff() = nil, want a report")
	}
	if report.FieldPath != "payload.x" {
		t.Fatalf("FieldPath = %q, want %q", report.FieldPath, "payload.x")
	}
	if !report.WantAbsent {
		t.Fatal("WantAbsent = false, want true: x does not exist on the want side at all")
	}
	if report.GotAbsent {
		t.Fatal("GotAbsent = true, want false: x is present on the got side; its value merely equals the absent marker's own text")
	}
	if report.Got != "<absent>" {
		t.Fatalf("Got = %v, want the literal string %q", report.Got, "<absent>")
	}

	line := report.String()
	if strings.Contains(line, `"<absent>"`) {
		t.Fatalf("String() = %q, renders the present value the same way as an actually-missing field would be", line)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	var decoded struct {
		WantAbsent bool            `json:"want_absent"`
		GotAbsent  bool            `json:"got_absent"`
		Want       json.RawMessage `json:"want"`
		Got        json.RawMessage `json:"got"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(encoded) error = %v", err)
	}
	if !decoded.WantAbsent {
		t.Fatal("decoded want_absent = false, want true")
	}
	if decoded.GotAbsent {
		t.Fatal("decoded got_absent = true, want false")
	}
	if decoded.Want != nil {
		t.Fatalf("decoded want = %s, want it omitted", decoded.Want)
	}
	if string(decoded.Got) != `"<absent>"` {
		t.Fatalf("decoded got = %s, want the JSON string %q", decoded.Got, `"<absent>"`)
	}
}
