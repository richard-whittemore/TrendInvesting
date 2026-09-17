package sizing_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

type sizingCall struct {
	name string
	fn   any
	args []any
}

func sizingCalls() []sizingCall {
	return []sizingCall{
		{"AverageMoveInN", sizing.AverageMoveInN, []any{110., 100., 5.}},
		{"RealisedResultInUnitN", sizing.RealisedResultInUnitN, []any{100., int64(1), 5., 1.}},
		{"AggregateOpenRisk", sizing.AggregateOpenRisk, []any{[]sizing.UnitOpenRisk{{EntryPrice: 100, ProtectiveStop: 90, Quantity: 10}, {EntryPrice: 110, ProtectiveStop: 95, Quantity: 10}}, 1.}},
		{"NextAddLevel", sizing.NextAddLevel, []any{100., 5., sizing.DirectionLong}},
		{"AddLadder", sizing.AddLadder, []any{100., 5., 4, sizing.DirectionLong}},
		{"ProtectiveStopLevel", sizing.ProtectiveStopLevel, []any{100., 5., 2., sizing.DirectionLong}},
		{"RaisedStop", sizing.RaisedStop, []any{90., 5.}},
		{"SizeUnit", sizing.SizeUnit, []any{sizing.Inputs{Mode: sizing.ModeVolatilityNormalised, NotionalAccount: 1e6, UnitVolatilityFraction: .005, StopMultiple: 2, N: 5, DollarsPerPoint: 1}}},
		{"SizeUnit", sizing.SizeUnit, []any{sizing.Inputs{Mode: sizing.ModeFixedRiskAtStop, NotionalAccount: 1e6, RiskAtStopFraction: .01, StopMultiple: 2, N: 5, DollarsPerPoint: 1}}},
		{"RealisedRiskAtStop", sizing.RealisedRiskAtStop, []any{int64(10), 2., 5., 1., 1e6}},
		{"Product", sizing.Product, []any{2., 5.}},
		{"DrawdownSteppedNotional", sizing.DrawdownSteppedNotional, []any{1e6}},
		{"CashMovementScaledFigure", sizing.CashMovementScaledFigure, []any{1e6, 1e6, 2e6}},
		{"UnitQuantity", sizing.UnitQuantity, []any{1e6, .005, 5., 1.}},
		{"FixedRiskAtStopQuantity", sizing.FixedRiskAtStopQuantity, []any{1e6, .01, 2., 5., 1.}},
		{"RiskAtStop", sizing.RiskAtStop, []any{sizing.ModeVolatilityNormalised, .005, 2., 0.}},
		{"RiskAtStop", sizing.RiskAtStop, []any{sizing.ModeFixedRiskAtStop, .005, 2., .01}},
	}
}

// TestSizingInvariantIncludesEveryExportedFunction makes the finite-result
// contract's inventory falsifiable when the package grows (ADR 0003).
func TestSizingInvariantIncludesEveryExportedFunction(t *testing.T) {
	registered := map[string]bool{}
	for _, c := range sizingCalls() {
		registered[c.name] = true
	}
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", func(i os.FileInfo) bool { return !strings.HasSuffix(i.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range pkgs["sizing"].Files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.IsExported() {
				if !registered[fn.Name.Name] {
					t.Errorf("%s needs a sizingCalls fixture", fn.Name.Name)
				}
				delete(registered, fn.Name.Name)
			}
		}
	}
	for name := range registered {
		t.Errorf("%s is not an exported function", name)
	}
}

func callSizing(c sizingCall) ([]reflect.Value, bool) {
	args := make([]reflect.Value, len(c.args))
	for i, a := range c.args {
		args[i] = reflect.ValueOf(a)
	}
	out := reflect.ValueOf(c.fn).Call(args)
	if len(out) == 1 {
		return out, true
	} // Detect unchecked APIs before their migration.
	last := out[len(out)-1]
	if last.Kind() == reflect.Bool {
		return out[:len(out)-1], last.Bool()
	}
	return out[:len(out)-1], last.IsNil()
}

func floatLeaves(t *testing.T, v reflect.Value, visit func(reflect.Value)) {
	t.Helper()
	switch v.Kind() {
	case reflect.Float64:
		visit(v)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			floatLeaves(t, v.Field(i), visit)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			floatLeaves(t, v.Index(i), visit)
		}
	case reflect.String, reflect.Int, reflect.Int64:
	default:
		t.Fatalf("unsupported sizing value %s", v.Type())
	}
}

// TestSizingSuccessfulResultsAreFinite probes all float leaves with boundary
// values and reproducible IEEE-754 bit patterns, including simultaneous mutations.
func TestSizingSuccessfulResultsAreFinite(t *testing.T) {
	boundary := []float64{0, math.Copysign(0, -1), 1, -1, .005, math.SmallestNonzeroFloat64, 1e-200, 1e200, 1e300, math.MaxFloat64, -math.MaxFloat64, math.Inf(1), math.Inf(-1), math.NaN()}
	for _, fixture := range sizingCalls() {
		t.Run(fixture.name, func(t *testing.T) {
			check := func(c sizingCall) {
				out, success := callSizing(c)
				if !success {
					return
				}
				for _, v := range out {
					floatLeaves(t, v, func(x reflect.Value) {
						if math.IsNaN(x.Float()) || math.IsInf(x.Float(), 0) {
							t.Fatalf("%s(%v) returned %v with success", c.name, c.args, x.Float())
						}
					})
				}
			}
			mutate := func(choose func(int) float64, selected int) sizingCall {
				c := fixture
				c.args = append([]any(nil), fixture.args...)
				leaf := 0
				for i, a := range c.args {
					v := reflect.New(reflect.TypeOf(a)).Elem()
					v.Set(reflect.ValueOf(a))
					if v.Kind() == reflect.Slice {
						cp := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
						reflect.Copy(cp, v)
						v = cp
					}
					floatLeaves(t, v, func(x reflect.Value) {
						if selected < 0 || selected == leaf {
							x.SetFloat(choose(leaf))
						}
						leaf++
					})
					c.args[i] = v.Interface()
				}
				return c
			}
			check(fixture)
			count := 0
			for _, a := range fixture.args {
				floatLeaves(t, reflect.ValueOf(a), func(reflect.Value) { count++ })
			}
			for i := 0; i < count; i++ {
				for _, b := range boundary {
					check(mutate(func(int) float64 { return b }, i))
				}
			}
			rng := rand.New(rand.NewSource(17))
			for i := 0; i < 2000; i++ {
				check(mutate(func(int) float64 {
					if i%2 == 0 {
						return boundary[rng.Intn(len(boundary))]
					}
					return math.Float64frombits(rng.Uint64())
				}, -1))
			}
		})
	}
}

func TestSizingRejectsUnrepresentableResults(t *testing.T) {
	cases := []sizingCall{
		{"AverageMoveInN", sizing.AverageMoveInN, []any{1e300, 1., math.SmallestNonzeroFloat64}},
		{"AverageMoveInN/subtraction", sizing.AverageMoveInN, []any{math.MaxFloat64, -math.MaxFloat64, 1.}},
		{"RealisedResultInUnitN/tiny", sizing.RealisedResultInUnitN, []any{1e300, int64(1), math.SmallestNonzeroFloat64, 1.}},
		{"RealisedResultInUnitN/denormal", sizing.RealisedResultInUnitN, []any{1e308, int64(1), 1e-200, 1e-200}},
		{"RealisedResultInUnitN/denominator-overflow", sizing.RealisedResultInUnitN, []any{1., int64(2), math.MaxFloat64, 1.}},
		{"AggregateOpenRisk/product", sizing.AggregateOpenRisk, []any{[]sizing.UnitOpenRisk{{EntryPrice: 1e308, ProtectiveStop: 1, Quantity: 2}}, 1.}},
		{"AggregateOpenRisk/sum", sizing.AggregateOpenRisk, []any{[]sizing.UnitOpenRisk{{EntryPrice: 1e308, ProtectiveStop: 1, Quantity: 1}, {EntryPrice: 1e308, ProtectiveStop: 1, Quantity: 1}}, 1.}},
		{"NextAddLevel", sizing.NextAddLevel, []any{math.MaxFloat64, math.MaxFloat64, sizing.DirectionLong}},
		{"AddLadder", sizing.AddLadder, []any{1e308, 1e308, 4, sizing.DirectionLong}},
		{"RaisedStop", sizing.RaisedStop, []any{math.MaxFloat64, math.MaxFloat64}},
		{"RealisedRiskAtStop", sizing.RealisedRiskAtStop, []any{int64(1), 2., 1e308, 1., 1.}},
		{"CashMovementScaledFigure", sizing.CashMovementScaledFigure, []any{1., 1e-200, 1e200}},
		{"Product", sizing.Product, []any{1e308, 2.}},
		{"UnitQuantity/cost-overflow", sizing.UnitQuantity, []any{1., .5, 1e308, 2.}},
		{"UnitQuantity/zero-over-zero", sizing.UnitQuantity, []any{math.SmallestNonzeroFloat64, .5, 1e-200, 1e-200}},
		{"FixedRiskAtStopQuantity", sizing.FixedRiskAtStopQuantity, []any{1., .5, 2., 1e308, 1.}},
		{"SizeUnit", sizing.SizeUnit, []any{sizing.Inputs{Mode: sizing.ModeVolatilityNormalised, NotionalAccount: 1e308, UnitVolatilityFraction: .5, StopMultiple: 2, N: 1e308, DollarsPerPoint: 1}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := callSizing(c)
			if ok {
				t.Fatalf("%s returned %v with success; want representability failure", c.name, out)
			}
		})
	}
}
