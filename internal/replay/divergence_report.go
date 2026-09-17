package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// FieldDivergence names the exact field two envelopes disagree on first,
// refining Divergence's own "which envelope differs" to "which field, and
// what each side had". Path is dotted for an object field
// ("recorded_at", "payload.protective_stop.level") and bracket-indexed for
// an array element ("payload.units[2].price"); array position is never
// treated as a named field, since sorting it away would report the wrong
// element as "the same". Want and Got are the two values found there,
// decoded from JSON: an ordinary number is a float64, an integer literal
// float64 cannot carry exactly is a json.Number holding its exact digits
// instead (see envelopeTree and diffNumber), and a path present on only one
// side is an absentField.
type FieldDivergence struct {
	Path string
	Want any
	Got  any
}

// absentField marks the side of a FieldDivergence where the path does not
// exist at all: a key missing from an object, or an index beyond a shorter
// array. It is distinct from a field that is present and explicitly JSON
// null, which decodes as a plain nil and is reported as such. encodeValue
// renders it as the JSON string "<absent>" — distinct from JSON null — in
// both the human-readable (String) and machine-readable (MarshalJSON)
// reports; it does not implement json.Marshaler itself, since every path
// that renders a value goes through encodeValue rather than calling
// encoding/json on a Want/Got value directly.
type absentField struct{}

func (absentField) String() string { return "<absent>" }

// Field derives the FieldDivergence for d: the first field at which d.Want
// and d.Got disagree.
//
// It returns (nil, nil) when d is nil (Equivalent found no divergence) or
// when d models a length mismatch (Want or Got nil): there is no field to
// point to when an event has no counterpart on the other side, only the
// point at which one stream ended, and Divergence already states that,
// reused here rather than re-derived.
//
// When both are present, Field always returns a non-nil result: Equivalent
// found these two envelopes' canonical bytes to differ, so there is always
// something to report. The one case that needs a fallback is a payload
// whose raw bytes differ (an insignificant-whitespace or alternate but
// equal-valued number literal) but whose decoded values compare equal field
// for field — CanonicalEnvelopeBytes hashes the payload's raw bytes, not its
// decoded shape (see that function's own doc comment), so this is a real
// possibility, not a defensive dead branch. In that case Field reports the
// whole payload, by its raw bytes, rather than silently finding no field
// difference for an envelope Equivalent has already said differs.
func Field(d *Divergence) (*FieldDivergence, error) {
	if d == nil || d.Want == nil || d.Got == nil {
		return nil, nil
	}
	want, err := envelopeTree(*d.Want)
	if err != nil {
		return nil, fmt.Errorf("replay: decode envelope %s for field-level diff: %w", d.Want.ID, err)
	}
	got, err := envelopeTree(*d.Got)
	if err != nil {
		return nil, fmt.Errorf("replay: decode envelope %s for field-level diff: %w", d.Got.ID, err)
	}

	if path, wantVal, gotVal, found := diffAny("", want, got); found {
		return &FieldDivergence{Path: path, Want: wantVal, Got: gotVal}, nil
	}
	return &FieldDivergence{Path: "payload", Want: string(d.Want.Payload), Got: string(d.Got.Payload)}, nil
}

// envelopeTree builds the same fields CanonicalEnvelopeBytes hashes (ADR
// 0016), except payload is decoded into its native JSON shape instead of
// kept as the raw string CanonicalEnvelopeBytes hashes, so a caller can walk
// into it. Timestamps are rendered as RFC 3339 in UTC, the same treatment
// CanonicalEnvelopeBytes gives them, so two envelopes recording the same
// instant in different zones compare equal here too.
//
// The payload is decoded with json.Decoder.UseNumber, so every JSON number
// becomes a json.Number carrying its exact literal digits rather than a
// plain json.Unmarshal's float64, which would round two distinct integers
// above 2^53 to the same value and make them indistinguishable to the field
// walk. diffNumber is what later decides, field by field, whether float64
// is safe to report or whether the literal itself must be.
func envelopeTree(e event.Envelope) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(e.Payload))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, fmt.Errorf("replay: envelope %s: payload has trailing data after its JSON value", e.ID)
	}
	return map[string]any{
		"id":                 e.ID,
		"type":               e.Type,
		"envelope_version":   e.EnvelopeVersion,
		"schema_version":     e.SchemaVersion,
		"event_time":         e.EventTime.UTC().Format(time.RFC3339Nano),
		"recorded_at":        e.RecordedAt.UTC().Format(time.RFC3339Nano),
		"sequence":           e.Sequence,
		"correlation_id":     e.CorrelationID,
		"causation_id":       e.CausationID,
		"source":             e.Source,
		"strategy_version":   e.StrategyVersion,
		"configuration_hash": e.ConfigurationHash,
		"payload_hash":       e.PayloadHash,
		"payload":            payload,
	}, nil
}

// diffAny reports the first field at which want and got disagree, walking
// objects and arrays recursively; found is false when the two are equal all
// the way down. A pair of decoded JSON numbers is routed to diffNumber
// rather than compared here (see its own doc comment for why). Every other
// value is compared with reflect.DeepEqual, the exact-value discipline this
// project already applies to every derived float (see
// ProtectiveStopSetPayload.Validate): two float64 values one bit apart are
// unequal, never rounded together.
func diffAny(path string, want, got any) (foundPath string, wantVal, gotVal any, found bool) {
	if wantMap, ok := want.(map[string]any); ok {
		if gotMap, ok := got.(map[string]any); ok {
			return diffMap(path, wantMap, gotMap)
		}
	}
	if wantSlice, ok := want.([]any); ok {
		if gotSlice, ok := got.([]any); ok {
			return diffSlice(path, wantSlice, gotSlice)
		}
	}
	if wantNum, ok := want.(json.Number); ok {
		if gotNum, ok := got.(json.Number); ok {
			return diffNumber(path, wantNum, gotNum)
		}
	}
	if reflect.DeepEqual(want, got) {
		return "", nil, nil, false
	}
	return path, want, got, true
}

// diffNumber compares two decoded JSON numbers and reports the pair in the
// representation a caller can trust.
//
// When both sides round-trip through float64 without changing value
// (numberIsFloat64Safe), they are compared and reported as float64 —
// exactly what every other numeric field in this reporter has always done,
// including the one-ULP float divergence this reporter exists to catch.
// When either side does not — an integer literal above float64's 53-bit
// mantissa, where two distinct integers (2^53 and 2^53+1, for instance)
// convert to the identical float64 — comparing the converted floats would
// call them equal, and reporting them would print the same text for two
// different values. Both the comparison and the reported values use the
// literal token itself in that case, never a lossy float64 conversion of
// it.
func diffNumber(path string, want, got json.Number) (foundPath string, wantVal, gotVal any, found bool) {
	if numberIsFloat64Safe(want) && numberIsFloat64Safe(got) {
		wantFloat, _ := want.Float64()
		gotFloat, _ := got.Float64()
		if wantFloat == gotFloat {
			return "", nil, nil, false
		}
		return path, wantFloat, gotFloat, true
	}
	if want == got {
		return "", nil, nil, false
	}
	return path, want, got, true
}

// numberIsFloat64Safe reports whether n converts to float64 and back
// without changing value. Every literal with a decimal point or an
// exponent is JSON's own float syntax — this reporter already trusts those
// to float64 (encodeValue) — so it is always safe. A bare integer literal
// is safe only when converting it to float64 and back to int64 reproduces
// the same integer; one whose magnitude exceeds float64's exact range, or
// int64's range entirely, is not, and diffNumber falls back to comparing
// and reporting its literal digits instead.
func numberIsFloat64Safe(n json.Number) bool {
	if strings.ContainsAny(string(n), ".eE") {
		return true
	}
	i, err := n.Int64()
	if err != nil {
		return false
	}
	return int64(float64(i)) == i
}

// normalizeLoneNumber renders a value reported on its own — one side of an
// absent-field pair, where there is no counterpart to weigh a lossy
// conversion against — the same way diffNumber renders a pair: float64 when
// that loses nothing, the literal json.Number when it would.
func normalizeLoneNumber(v any) any {
	if n, ok := v.(json.Number); ok && numberIsFloat64Safe(n) {
		f, _ := n.Float64()
		return f
	}
	return v
}

// diffMap compares two decoded JSON objects key by key, in SORTED key
// order, so the field reported is deterministic regardless of decode order
// (encoding/json's map decoding order is unspecified). A key present on
// only one side is reported at that key's path with absentField on the
// other (this function's own doc comment on CONTEXT.md "a field present on
// one side only").
func diffMap(path string, want, got map[string]any) (foundPath string, wantVal, gotVal any, found bool) {
	keys := make(map[string]struct{}, len(want)+len(got))
	for k := range want {
		keys[k] = struct{}{}
	}
	for k := range got {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	for _, key := range sorted {
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		wantChild, wantOK := want[key]
		gotChild, gotOK := got[key]
		switch {
		case !wantOK:
			return childPath, absentField{}, normalizeLoneNumber(gotChild), true
		case !gotOK:
			return childPath, normalizeLoneNumber(wantChild), absentField{}, true
		default:
			if p, w, g, found := diffAny(childPath, wantChild, gotChild); found {
				return p, w, g, true
			}
		}
	}
	return "", nil, nil, false
}

// diffSlice compares two decoded JSON arrays element by element, reporting
// the array INDEX in the path rather than sorting elements: array position
// is significant (CONTEXT.md orders a Campaign's Units, a journal's records,
// by position, never by value), so element 2 of want is compared to element
// 2 of got, never matched up by content. An index beyond the shorter array
// is reported as absentField, the same treatment diffMap gives a missing
// key.
func diffSlice(path string, want, got []any) (foundPath string, wantVal, gotVal any, found bool) {
	longest := len(want)
	if len(got) > longest {
		longest = len(got)
	}
	for i := 0; i < longest; i++ {
		childPath := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case i >= len(want):
			return childPath, absentField{}, normalizeLoneNumber(got[i]), true
		case i >= len(got):
			return childPath, normalizeLoneNumber(want[i]), absentField{}, true
		default:
			if p, w, g, found := diffAny(childPath, want[i], got[i]); found {
				return p, w, g, true
			}
		}
	}
	return "", nil, nil, false
}

// Report is where two decision streams — in practice, two journals'
// decisions (journal.Split) — first disagree: nil for no divergence,
// otherwise the sequence, id and type of the event compared, and either the
// field the two envelopes disagree on (FieldPath, Want, Got) or which
// stream ended first (EndedStream) — never both. Divergence's own nil
// Want/Got is what decides which case applies (see Explain), reused rather
// than re-modelled.
type Report struct {
	// Sequence, EventID and EventType name the compared event: the
	// envelope's own Sequence (the decision's position in its output
	// stream, ADR 0016 — never the journal's interleaved record number),
	// its ID and its Type.
	Sequence  uint64
	EventID   string
	EventType string

	// EndedStream is "want" or "got" — the stream that has nothing at this
	// position — when the two streams differ only in length. Empty exactly
	// when FieldPath names a real field difference instead.
	EndedStream string

	// FieldPath, Want and Got are set exactly when EndedStream is empty: the
	// field the two envelopes disagree on first, and what each side had.
	FieldPath string
	Want      any
	Got       any
}

// Explain turns a Divergence into a Report: the field within the compared
// envelopes that differs, or which stream ended first. It returns (nil,
// nil) for a nil Divergence.
func Explain(d *Divergence) (*Report, error) {
	if d == nil {
		return nil, nil
	}
	switch {
	case d.Got == nil:
		return &Report{Sequence: d.Want.Sequence, EventID: d.Want.ID, EventType: d.Want.Type, EndedStream: "got"}, nil
	case d.Want == nil:
		return &Report{Sequence: d.Got.Sequence, EventID: d.Got.ID, EventType: d.Got.Type, EndedStream: "want"}, nil
	}

	field, err := Field(d)
	if err != nil {
		return nil, err
	}
	return &Report{
		Sequence:  d.Want.Sequence,
		EventID:   d.Want.ID,
		EventType: d.Want.Type,
		FieldPath: field.Path,
		Want:      field.Want,
		Got:       field.Got,
	}, nil
}

// Diff compares two decision streams and reports where they first disagree:
// nil for no divergence, otherwise a Report naming the sequence, event id
// and type, and either the differing field and both values or which stream
// ended first. It is Equivalent and Explain composed, the single entry
// point a caller comparing two journals needs.
func Diff(want, got []event.Envelope) (*Report, error) {
	return Explain(Equivalent(want, got))
}

// String renders a Report as one human-readable line.
func (r *Report) String() string {
	if r == nil {
		return "no divergence"
	}
	if r.EndedStream != "" {
		return fmt.Sprintf("the %s stream ends here; the other continues with sequence %d, event %s (%s)",
			r.EndedStream, r.Sequence, r.EventID, r.EventType)
	}
	return fmt.Sprintf("sequence %d, event %s (%s): %s differs — want %s, got %s",
		r.Sequence, r.EventID, r.EventType, r.FieldPath, renderValue(r.Want), renderValue(r.Got))
}

// MarshalJSON renders Report for a machine-readable consumer such as CI.
// Want and Got are encoded through event.CanonicalBytes (ADR 0016) rather
// than encoding/json's own float formatting, so a value that must be
// distinguishable in the report — 118.2875 against 118.28750000000001, the
// one defect class this project has actually shipped (docs/development.md,
// "never leave a multiply-add fusible") — still is once JSON has
// round-tripped it.
func (r Report) MarshalJSON() ([]byte, error) {
	wire := struct {
		Sequence    uint64          `json:"sequence"`
		EventID     string          `json:"event_id"`
		EventType   string          `json:"event_type"`
		EndedStream string          `json:"ended_stream,omitempty"`
		FieldPath   string          `json:"field_path,omitempty"`
		Want        json.RawMessage `json:"want,omitempty"`
		Got         json.RawMessage `json:"got,omitempty"`
	}{
		Sequence:    r.Sequence,
		EventID:     r.EventID,
		EventType:   r.EventType,
		EndedStream: r.EndedStream,
		FieldPath:   r.FieldPath,
	}
	if r.EndedStream == "" {
		wire.Want = encodeValue(r.Want)
		wire.Got = encodeValue(r.Got)
	}
	return json.Marshal(wire)
}

// renderValue and encodeValue share one formatter for both the
// human-readable (String) and machine-readable (MarshalJSON) reports, so
// the two never disagree about whether a value round-trips.
func renderValue(v any) string {
	return string(encodeValue(v))
}

// encodeValue renders v as a single JSON value through event.CanonicalBytes
// (ADR 0016's canonical encoder), the one place in this project a float64
// is already proven to render with Go's shortest round-trip formatting
// (strconv.FormatFloat(v, 'g', -1, 64)) rather than a second, independently
// maintained formatter that could drift from it.
//
// CanonicalBytes takes a map of named fields, so v is wrapped as the sole
// field "v" and the wrapper stripped back off: canonicalJSON never emits
// insignificant whitespace, so the result is always exactly `{"v":<value>}`
// and the strip is unconditional.
func encodeValue(v any) json.RawMessage {
	if _, ok := v.(absentField); ok {
		return json.RawMessage(`"<absent>"`)
	}
	if n, ok := v.(json.Number); ok {
		// json.Number's underlying string is exactly the literal digits
		// UseNumber decoded it from — the entire reason to carry it this way
		// instead of a float64 — so it is written out verbatim as a JSON
		// number rather than through CanonicalBytes, whose reflect-driven
		// encoder sees json.Number's underlying string kind and would quote
		// it, and which formats floats via a float64 conversion that is
		// exactly the lossy step this exists to avoid.
		return json.RawMessage(n.String())
	}
	const prefix = `{"v":`
	wrapped := event.CanonicalBytes(map[string]any{"v": v})
	return json.RawMessage(wrapped[len(prefix) : len(wrapped)-1])
}
