package floatingpointaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

func TestFusibleShapes(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"add", "_ = x + a*b", 1},
		{"subtract", "_ = x - a*b", 1},
		{"product_on_left", "_ = a*b - x", 1},
		{"accumulate", "x += a*b", 1},
		{"subtract_assign", "x -= a*b", 1},
		{"parentheses", "x += ((a*b))", 1},
		{"unary", "_ = x + -(a*b)", 1},
		{"two_products", "_ = a*b + a*x", 2},
		{"nested_add", "_ = (x + a*b) / 2", 1},
		{"float32", "var v, w float32; v += v*w", 1},
		{"named_float", "type price float64; var v, w price; v += v*w", 1},
		{"integer_percentile", "p := 95; sorted := []int{1}; _ = (p*len(sorted) + 99) / 100", 0},
		{"integer_accumulate", "p := 2; p += p*p", 0},
		{"named_integer", "type count int; var p count; p += p*p", 0},
		{"rounded_product", "_ = x + float64(a*b)", 0},
		{"rounded_accumulate", "x += float64(a*b)", 0},
		{"named_conversion", "type price float64; var v, w price; v += price(v*w)", 0},
		{"outer_conversion_is_not_barrier", "_ = float64(x + a*b)", 1},
		{"constant_product", "_ = x + 2.0*3.0", 0},
		{"product_only", "_ = a*b*x", 0},
		{"function_boundary", "_ = x + product(a, b)", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "shape.go", "package fixture\n"+
				"func product(a, b float64) float64 { return float64(a*b) }\n"+
				"func f(x, a, b float64) { "+tt.body+" }\n", 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
			if _, err := new(types.Config).Check("fixture", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			if got := len(fusibleProducts(file, info)); got != tt.want {
				t.Errorf("fusible products = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGenericFusibleShapes(t *testing.T) {
	tests := []struct {
		name, declarations, constraint, body string
		want                                 int
	}{
		{"generic_float", "type N interface{ ~float32 | ~float64 }", "N", "return x + a*b", 1},
		{"generic_mixed", "", "~int | ~float64", "x += a*b; return x", 1},
		{"generic_subtract", "", "~float32", "return x - a*b", 1},
		{"generic_subtract_assign", "", "~float64", "x -= a*b; return x", 1},
		{"generic_integer", "", "~int | ~int64", "return x + a*b", 0},
		{"generic_rounded", "", "~float32 | ~float64", "return x + T(a*b)", 0},
		{"generic_unary", "", "~float64", "return x + -(a*b)", 1},
		{"generic_constant", "", "~float64", "return x + 2.0*3.0", 0},
		{"embedded_float", "type N interface{ ~int | ~float64 }; type F interface{ N; ~float64 }", "F", "return x + a*b", 1},
		{"intersection_integer", "type N interface{ ~int | ~float64 }; type I interface{ N; ~int }", "I", "return x + a*b", 0},
		{"named_float_term", "type Price float64", "Price | int", "return x + a*b", 1},
		{"float_with_method", "", "interface{ ~float64; M() }", "return x + a*b", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "shape.go", "package fixture\n"+tt.declarations+
				"\nfunc f[T "+tt.constraint+"](x, a, b T) T { "+tt.body+" }\n", 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
			if _, err := new(types.Config).Check("fixture", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			if got := len(fusibleProducts(file, info)); got != tt.want {
				t.Errorf("fusible products = %d, want %d", got, tt.want)
			}
		})
	}
}
