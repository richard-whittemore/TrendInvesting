package floatingpointaudit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoFusibleMultiplyAdd enforces docs/development.md's floating-point
// determinism rule (ADR 0017) on production Go files in internal/. It checks local
// expression shapes; cross-statement and inlined-call fusion need the two
// architecture CI suites and sensitive golden fixtures.
func TestNoFusibleMultiplyAdd(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// Register directory reads with Go's test cache: go list's subprocess
	// reads are invisible to it, so new files must invalidate success here.
	if err := filepath.WalkDir(filepath.Join(root, "internal"), func(_ string, _ fs.DirEntry, err error) error {
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "-export", "-compiled", "-json", "./internal/...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("load domain packages: %v\n%s", err, &stderr)
	}
	type listedPackage struct {
		Dir, ImportPath, Export            string
		GoFiles, CgoFiles, CompiledGoFiles []string
	}
	var packages []listedPackage
	exports := make(map[string]string)
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[pkg.ImportPath] = pkg.Export
		if strings.HasPrefix(pkg.Dir, filepath.Join(root, "internal")+string(filepath.Separator)) && len(pkg.GoFiles)+len(pkg.CgoFiles) > 0 {
			packages = append(packages, pkg)
		}
	}
	if len(packages) == 0 {
		t.Fatal("no internal packages loaded")
	}
	for _, pkg := range packages {
		fset := token.NewFileSet()
		var files []*ast.File
		// Original cgo files must participate in test-cache invalidation even
		// though type checking uses the compiler's generated Go representation.
		for _, name := range append(pkg.GoFiles, pkg.CgoFiles...) {
			path := filepath.Join(pkg.Dir, name)
			if _, err := os.ReadFile(path); err != nil {
				t.Fatal(err)
			}
		}
		if len(pkg.CompiledGoFiles) == 0 {
			t.Fatalf("no compiler input for %s", pkg.ImportPath)
		}
		for _, name := range pkg.CompiledGoFiles {
			if !filepath.IsAbs(name) {
				name = filepath.Join(pkg.Dir, name)
			}
			file, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
		}
		config := types.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			if exports[path] == "" {
				return nil, fmt.Errorf("missing export data for %s", path)
			}
			return os.Open(exports[path])
		})}
		info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
		if _, err := config.Check(pkg.ImportPath, fset, files, info); err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			for _, pos := range fusibleProducts(file, info) {
				position := fset.Position(pos)
				where := strings.TrimPrefix(position.String(), root+string(filepath.Separator))
				t.Errorf("%s: fusible floating-point multiply-add; round the product explicitly (docs/development.md: Floating-point determinism)", where)
			}
		}
	}
}

// fusibleProducts locates unrounded runtime floating-point products that are
// operands of +, -, += or -=, per docs/development.md and ADR 0017. Parentheses
// and signs are transparent; conversions and calls are expression boundaries.
func fusibleProducts(file *ast.File, info *types.Info) []token.Pos {
	var positions []token.Pos
	check := func(expr ast.Expr) {
		for {
			switch e := expr.(type) {
			case *ast.ParenExpr:
				expr = e.X
			case *ast.UnaryExpr:
				if e.Op != token.ADD && e.Op != token.SUB {
					return
				}
				expr = e.X
			default:
				product, ok := expr.(*ast.BinaryExpr)
				if !ok || product.Op != token.MUL {
					return
				}
				value := info.Types[product]
				if value.Value == nil && len(floatingTerms(value.Type)) > 0 {
					positions = append(positions, product.OpPos)
				}
				return
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.BinaryExpr:
			if n.Op == token.ADD || n.Op == token.SUB {
				check(n.X)
				check(n.Y)
			}
		case *ast.AssignStmt:
			if n.Tok == token.ADD_ASSIGN || n.Tok == token.SUB_ASSIGN {
				for _, rhs := range n.Rhs {
					check(rhs)
				}
			}
		}
		return true
	})
	return positions
}

// floatingTerms retains the floating-point portion of a type set, per the
// determinism rule in docs/development.md (ADR 0017). Unions admit either term;
// embedded interfaces intersect terms. A mixed constraint needs rounding
// because any permitted float instantiation can fuse.
func floatingTerms(typ types.Type) []*types.Term {
	if param, ok := typ.(*types.TypeParam); ok {
		return floatingTerms(param.Constraint())
	}
	switch t := typ.Underlying().(type) {
	case *types.Basic:
		if t.Info()&types.IsFloat != 0 {
			return []*types.Term{types.NewTerm(false, typ)}
		}
	case *types.Union:
		var terms []*types.Term
		for i := 0; i < t.Len(); i++ {
			term := t.Term(i)
			for _, floating := range floatingTerms(term.Type()) {
				terms = append(terms, types.NewTerm(term.Tilde() || floating.Tilde(), floating.Type()))
			}
		}
		return terms
	case *types.Interface:
		terms := []*types.Term{
			types.NewTerm(true, types.Typ[types.Float32]),
			types.NewTerm(true, types.Typ[types.Float64]),
		}
		for i := 0; i < t.NumEmbeddeds(); i++ {
			terms = intersectFloatingTerms(terms, floatingTerms(t.EmbeddedType(i)))
		}
		var permitted []*types.Term
		for _, term := range terms {
			if term.Tilde() || types.Satisfies(term.Type(), t) {
				permitted = append(permitted, term)
			}
		}
		return permitted
	}
	return nil
}

func intersectFloatingTerms(left, right []*types.Term) []*types.Term {
	var terms []*types.Term
	for _, a := range left {
		for _, b := range right {
			switch {
			case types.Identical(a.Type(), b.Type()):
				terms = append(terms, types.NewTerm(a.Tilde() && b.Tilde(), a.Type()))
			case a.Tilde() && types.Identical(a.Type(), b.Type().Underlying()):
				terms = append(terms, b)
			case b.Tilde() && types.Identical(a.Type().Underlying(), b.Type()):
				terms = append(terms, a)
			}
		}
	}
	return terms
}
