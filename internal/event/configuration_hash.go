package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ConfigurationHash returns the identity ADR 0012 requires every experiment
// run to be retained under (#50, ADR 0016): the same configuration always
// produces the same hash, and any parameter change — including a schema
// bump — produces a different one.
//
// It hashes "configuration/v" + ConfigurationSchemaVersion + "\n" + the
// canonical JSON encoding of payload (see canonicalJSON), SHA-256, hex
// encoded and prefixed "sha256:" so a reader can tell the algorithm from the
// string. Only the payload and its schema version feed the hash — not the
// strategy version, not the run span, not the data provenance tag. Those are
// run-level facts recorded beside the hash (#39), so the same configuration
// run over a different span or a different dataset is still recognisably
// the same configuration.
//
// The schema version is inside the hashed bytes, not stored beside them,
// precisely so that a schema bump — a field added to the payload — changes
// every hash even when the numbers a schema-3 and a schema-4 configuration
// happen to share are identical: the strategy's behaviour space changed
// with the new field, so it is a different configuration, not a
// re-statement of the old one.
//
// This is the single place this project derives a configuration hash from.
// Reducer.NewReducer calls it, rather than deriving one itself, so a caller
// can never construct a Reducer whose stored hash disagrees with what this
// function would compute for the same payload.
func ConfigurationHash(payload ConfigurationPayload) string {
	canonical := canonicalJSON(payload)
	prefix := fmt.Sprintf("configuration/v%d\n", ConfigurationSchemaVersion)
	sum := sha256.Sum256(append([]byte(prefix), canonical...))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canonicalJSON renders v as canonical JSON for hashing: object keys sorted
// lexicographically, no insignificant whitespace, float64 values rendered
// with Go's shortest round-trip formatting (strconv.FormatFloat(v, 'g', -1,
// 64)), and nested objects/maps canonicalised recursively.
//
// It builds the object and number encoding itself, rather than delegating
// to encoding/json.Marshal, specifically so the result depends on neither
// encoding/json's own float formatting (which has changed across Go
// versions) nor on map iteration order: a hash that is not byte-for-byte the
// same on every Go version and for every equivalent map literal is not a
// hash ADR 0012's "same configuration, same hash" can be trusted against.
// Individual string and bool leaves are still encoded via encoding/json
// (json.Marshal on a bare string), because JSON string quoting/escaping is
// not one of the two things this function exists to avoid.
//
// v is expected to be built only from: struct values (exported fields keyed
// by their `json` tag, `-` skipped), maps with string keys, slices/arrays,
// string, bool, any signed integer kind, and float32/float64.
// ConfigurationPayload satisfies this by construction, so ConfigurationHash
// can call this unconditionally; anything else — including an unsigned
// integer or a pointer, neither of which any field in this codebase's
// configuration payloads uses — panics rather than silently producing a
// non-canonical encoding.
func canonicalJSON(v any) []byte {
	var buf bytes.Buffer
	writeCanonical(&buf, reflect.ValueOf(v))
	return buf.Bytes()
}

func writeCanonical(buf *bytes.Buffer, v reflect.Value) {
	// A map[string]any's values (see writeCanonicalMap) come back from
	// reflect.Value.MapIndex with static kind Interface, wrapping whatever
	// concrete value was actually stored; unwrap it once (a Go interface
	// value's dynamic type is never itself an interface, so this never needs
	// to repeat) so the switch below sees the real kind. A nil interface
	// value renders as JSON null.
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			buf.WriteString("null")
			return
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Struct:
		writeCanonicalStruct(buf, v)
	case reflect.Map:
		writeCanonicalMap(buf, v)
	case reflect.Slice, reflect.Array:
		writeCanonicalSlice(buf, v)
	case reflect.String:
		writeCanonicalString(buf, v.String())
	case reflect.Bool:
		if v.Bool() {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case reflect.Float32, reflect.Float64:
		buf.WriteString(strconv.FormatFloat(v.Float(), 'g', -1, 64))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.WriteString(strconv.FormatInt(v.Int(), 10))
	default:
		panic(fmt.Sprintf("event: canonicalJSON: unsupported kind %s for value %v", v.Kind(), v))
	}
}

// canonicalField is one struct field or map entry pending canonical
// encoding, keyed by its rendered JSON name so both callers below can share
// one sort-and-write routine.
type canonicalField struct {
	name string
	val  reflect.Value
}

func writeCanonicalFields(buf *bytes.Buffer, fields []canonicalField) {
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeCanonicalString(buf, f.name)
		buf.WriteByte(':')
		writeCanonical(buf, f.val)
	}
	buf.WriteByte('}')
}

// writeCanonicalStruct encodes v's exported fields as a canonical JSON
// object, keyed by each field's `json` tag name (falling back to the Go
// field name when no tag is present, and skipping a field tagged `-`) —
// independent of the order the fields were declared in, unlike
// encoding/json.Marshal.
func writeCanonicalStruct(buf *bytes.Buffer, v reflect.Value) {
	t := v.Type()
	fields := make([]canonicalField, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" { // unexported
			continue
		}
		name := sf.Name
		if tag := sf.Tag.Get("json"); tag != "" {
			tagName, _, _ := strings.Cut(tag, ",")
			if tagName == "-" {
				continue
			}
			if tagName != "" {
				name = tagName
			}
		}
		fields = append(fields, canonicalField{name: name, val: v.Field(i)})
	}
	writeCanonicalFields(buf, fields)
}

// writeCanonicalMap encodes v as a canonical JSON object, sorted by key —
// independent of the map's iteration order, unlike encoding/json.Marshal's
// own (already-sorted, but not relied on here) map handling.
func writeCanonicalMap(buf *bytes.Buffer, v reflect.Value) {
	if v.Type().Key().Kind() != reflect.String {
		panic(fmt.Sprintf("event: canonicalJSON: map key kind %s is not supported; only string keys are", v.Type().Key().Kind()))
	}
	keys := v.MapKeys()
	fields := make([]canonicalField, 0, len(keys))
	for _, k := range keys {
		fields = append(fields, canonicalField{name: k.String(), val: v.MapIndex(k)})
	}
	writeCanonicalFields(buf, fields)
}

// writeCanonicalSlice encodes v as a JSON array, preserving element order:
// unlike object keys, array position is significant and must not be sorted.
func writeCanonicalSlice(buf *bytes.Buffer, v reflect.Value) {
	buf.WriteByte('[')
	for i := 0; i < v.Len(); i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeCanonical(buf, v.Index(i))
	}
	buf.WriteByte(']')
}

// writeCanonicalString writes s as a quoted JSON string via
// encoding/json.Marshal. String quoting/escaping is not one of the two
// nondeterminism sources this encoder exists to avoid (map ordering and
// float formatting), so delegating to the standard library here is safe.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	encoded, err := json.Marshal(s)
	if err != nil {
		// Unreachable: json.Marshal never fails to encode a Go string.
		panic(fmt.Sprintf("event: canonicalJSON: marshal string %q: %v", s, err))
	}
	buf.Write(encoded)
}
