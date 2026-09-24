package event_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

type validatedPayload interface {
	Validate() error
}

func marshalPayloads() []validatedPayload {
	return []validatedPayload{
		validAccountSnapshot(), validAddProposal(), validCampaignUnitAdded(),
		validCompletedBar(), validCampaignOpened(), validPartiallyFilledCampaign(),
		validCampaignExited(), validCampaignExitedByExitChannel(), validCampaignEvaluated(),
		validNotionalAccountCashAdjusted(), validCashMovement(), validConfiguration(),
		validDrawdownStepApplied(), validEngineState(), validExitProposal(),
		validFill(), validStopFill(), validExitFill(), validAddFill(),
		validTradeProposal(), validFixedRiskTradeProposal(), validProposalDeclined(),
		validProposalDeclinedInsufficientCash(), validProposalExpired(),
		validExitProposalExpired(), validAddProposalExpired(),
		validNotionalAccountRebased(), validNotionalAccountRecovered(),
		event.RunCompletedPayload{}, validSetupEvaluated(), validSignal(),
		validCorporateAction(),
		validProtectiveStopSet(), validProtectiveStopSetRaised(), validCampaignUnitsStopped(),
	}
}

// TestMarshalInvariantIncludesEveryPayload pins the fixture inventory to the
// package's declared payload types, so a new type must join the property test.
func TestMarshalInvariantIncludesEveryPayload(t *testing.T) {
	registered := make(map[string]bool)
	for _, p := range marshalPayloads() {
		registered[reflect.TypeOf(p).Name()] = true
	}
	for _, file := range nonTestPackageFiles(t, ".") {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typ := spec.(*ast.TypeSpec)
				if strings.HasSuffix(typ.Name.Name, "Payload") {
					if !registered[typ.Name.Name] {
						t.Errorf("%s needs a valid fixture in marshalPayloads", typ.Name.Name)
					}
					delete(registered, typ.Name.Name)
				}
			}
		}
	}
	for name := range registered {
		t.Errorf("fixture %s is not a declared payload", name)
	}
}

// nonTestPackageFiles parses every non-test .go file in dir and returns their
// ASTs, for tests that walk declarations rather than type-check them.
// go/parser.ParseDir would do this in one call, but it has been deprecated
// since Go 1.25 in favour of golang.org/x/tools/go/packages, which loads and
// type-checks — work this test has no use for and a dependency it need not
// take on. Parsing each file directly keeps the same declaration-only scope
// ParseDir had.
func nonTestPackageFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return files
}

// payloadLeaves visits JSON-visible leaves, including nested structs and every
// slice element; time.Time is a leaf because it supplies its own JSON encoder.
func payloadLeaves(t *testing.T, v reflect.Value, path string, visit func(string, reflect.Value)) {
	t.Helper()
	if v.Type() == reflect.TypeFor[time.Time]() {
		visit(path, v)
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if field.IsExported() && field.Tag.Get("json") != "-" {
				payloadLeaves(t, v.Field(i), path+"."+field.Name, visit)
			}
		}
	case reflect.Slice:
		if v.Len() == 0 {
			original := reflect.New(v.Type()).Elem()
			original.Set(v)
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			defer v.Set(original)
		}
		for i := range v.Len() {
			payloadLeaves(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i), visit)
		}
	case reflect.String, reflect.Int, reflect.Int64, reflect.Bool, reflect.Float64:
		visit(path, v)
	default:
		t.Fatalf("%s: add property generation for %s", path, v.Type())
	}
}

func payloadCopy(p validatedPayload) reflect.Value {
	v := reflect.New(reflect.TypeOf(p)).Elem()
	v.Set(reflect.ValueOf(p))
	return v
}

func requirePayloadMarshals(t *testing.T, p validatedPayload) {
	t.Helper()
	if err := p.Validate(); err != nil {
		t.Fatalf("fixture %T is invalid: %v", p, err)
	}
	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("accepted %T cannot marshal: %v", p, err)
	}
}

func TestPayloadTimestampsRejectUnwritableTimes(t *testing.T) {
	seen := make(map[reflect.Type]bool)
	for _, p := range marshalPayloads() {
		if seen[reflect.TypeOf(p)] {
			continue
		}
		seen[reflect.TypeOf(p)] = true
		requirePayloadMarshals(t, p)
		v := payloadCopy(p)
		payloadLeaves(t, v, v.Type().Name(), func(path string, field reflect.Value) {
			if field.Type() != reflect.TypeFor[time.Time]() {
				return
			}
			for _, kind := range []string{"year", "offset"} {
				t.Run(path+"/"+kind, func(t *testing.T) {
					candidate := payloadCopy(p)
					bad := time.Date(10000, 1, 2, 0, 0, 0, 0, time.UTC)
					if kind == "offset" {
						bad = time.Date(2026, 1, 2, 0, 0, 0, 0, time.FixedZone("out-of-range", 24*60*60))
					}
					// Preserve the expiry's chronology so only the target timestamp
					// is unwritable, rather than relying on another validation error.
					if candidate.Type() == reflect.TypeFor[event.ProposalExpiredPayload]() {
						candidate.FieldByName("Reason").SetString(event.ExpiryReasonInputStreamEnded)
						candidate.FieldByName("PeriodEnd").Set(reflect.ValueOf(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)))
						candidate.FieldByName("EarliestFillAt").Set(reflect.ValueOf(time.Time{}))
						candidate.FieldByName("ExpiredAt").Set(reflect.ValueOf(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)))
						if fieldName(path) == "EarliestFillAt" && kind == "year" {
							bad = time.Date(-1, 1, 2, 0, 0, 0, 0, time.UTC)
						}
					}
					requirePayloadMarshals(t, candidate.Interface().(validatedPayload))
					candidate.FieldByName(fieldName(path)).Set(reflect.ValueOf(bad))
					if _, err := json.Marshal(candidate.Interface()); err == nil {
						t.Fatal("test timestamp must fail JSON encoding")
					}
					if err := candidate.Interface().(validatedPayload).Validate(); err == nil {
						t.Fatal("Validate accepted an unwritable timestamp")
					}
				})
			}
		})
	}
}

func fieldName(path string) string {
	return path[strings.LastIndex(path, ".")+1:]
}

func TestPayloadTimestampsAcceptWritableTimes(t *testing.T) {
	for _, p := range marshalPayloads() {
		for _, year := range []int{0, 9999} {
			for _, offset := range []int{-86399, 0, 86399} {
				t.Run(fmt.Sprintf("%T/year=%d/offset=%d", p, year, offset), func(t *testing.T) {
					v := payloadCopy(p)
					payloadLeaves(t, v, v.Type().Name(), func(_ string, field reflect.Value) {
						if field.Type() != reflect.TypeFor[time.Time]() {
							return
						}
						at := field.Interface().(time.Time)
						if !at.IsZero() {
							field.Set(reflect.ValueOf(time.Date(year, at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), time.FixedZone("writable", offset))))
						}
					})
					requirePayloadMarshals(t, v.Interface().(validatedPayload))
				})
			}
		}
	}
}

// TestValidatedPayloadsMarshal checks the validated-payload-json invariant
// (docs/development.md) against boundaries and reproducible generated values.
func TestValidatedPayloadsMarshal(t *testing.T) {
	for _, p := range marshalPayloads() {
		t.Run(reflect.TypeOf(p).Name(), func(t *testing.T) {
			requirePayloadMarshals(t, p)
			v := payloadCopy(p)
			payloadLeaves(t, v, v.Type().Name(), func(path string, field reflect.Value) {
				t.Run(path, func(t *testing.T) {
					original := reflect.New(field.Type()).Elem()
					original.Set(field)
					defer field.Set(original)
					check := func(value any) bool {
						field.Set(reflect.ValueOf(value).Convert(field.Type()))
						if v.Interface().(validatedPayload).Validate() != nil {
							return true
						}
						if _, err := json.Marshal(v.Interface()); err != nil {
							t.Errorf("Validate accepted %v; json.Marshal: %v", value, err)
							return false
						}
						return true
					}
					for _, value := range marshalBoundaries(t, field) {
						check(value)
					}
					property := func(bits uint64, year int16, offset int32) bool {
						return check(marshalGeneratedValue(t, original, bits, year, offset))
					}
					if err := quick.Check(property, &quick.Config{MaxCount: 100, Rand: rand.New(rand.NewSource(1))}); err != nil {
						t.Error(err)
					}
				})
			})
		})
	}
}

func marshalBoundaries(t *testing.T, v reflect.Value) []any {
	t.Helper()
	if v.Type() == reflect.TypeFor[time.Time]() {
		values := []any{time.Time{}}
		for _, year := range []int{-1, 0, 1, 9999, 10000} {
			for _, offset := range []int{-24 * 3600, -86399, 0, 86399, 24 * 3600} {
				values = append(values, time.Date(year, 1, 2, 0, 0, 0, 0, time.FixedZone("boundary", offset)))
			}
		}
		return values
	}
	switch v.Kind() {
	case reflect.Float64:
		return []any{math.NaN(), math.Inf(-1), math.Inf(1), -math.MaxFloat64, math.MaxFloat64, -1.0, 0.0, math.SmallestNonzeroFloat64, 1.0}
	case reflect.String:
		return []any{"", "\xff\x00\"\\", "valid"}
	case reflect.Int:
		return []any{math.MinInt, -1, 0, 1, math.MaxInt}
	case reflect.Int64:
		return []any{int64(math.MinInt64), int64(-1), int64(0), int64(1), int64(math.MaxInt64)}
	case reflect.Bool:
		return []any{false, true}
	default:
		t.Fatalf("no boundaries for %s", v.Type())
		return nil
	}
}

func marshalGeneratedValue(t *testing.T, v reflect.Value, bits uint64, year int16, offset int32) any {
	t.Helper()
	if v.Type() == reflect.TypeFor[time.Time]() {
		if bits%2 == 0 {
			year = int16(uint16(year) % 10000)
			offset %= 24 * 3600
		}
		return time.Date(int(year), 1, 2, 0, 0, 0, int(bits%1e9), time.FixedZone("generated", int(offset)))
	}
	switch v.Kind() {
	case reflect.Float64:
		return math.Float64frombits(bits)
	case reflect.String:
		return string([]byte{byte(bits), byte(bits >> 8), byte(bits >> 16), byte(bits >> 24)})
	case reflect.Int:
		return int(bits)
	case reflect.Int64:
		return int64(bits)
	case reflect.Bool:
		return bits%2 == 0
	default:
		t.Fatalf("no generator for %s", v.Type())
		return nil
	}
}

func TestJSONStringsMarshal(t *testing.T) {
	property := func(data []byte) bool {
		_, err := json.Marshal(string(data))
		return err == nil
	}
	if !property([]byte{0xff, 0, '"', '\\'}) {
		t.Fatal("string containing invalid UTF-8 and escapes must marshal")
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 100, Rand: rand.New(rand.NewSource(1))}); err != nil {
		t.Fatal(err)
	}
}
